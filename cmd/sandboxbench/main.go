// Command sandboxbench drives a running sandboxd with many jobs and reports
// latency percentiles measured from the client side.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type options struct {
	url         string
	jobs        int
	concurrency int
	command     []string
	timeout     time.Duration
	memoryMB    int
	cpuMillis   int
	pids        int
	cancelAfter time.Duration
	delete      bool
}

type jobView struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	CreatedAt  string  `json:"created_at"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

type sample struct {
	state                           string
	submit, queueWait, run, toFinal time.Duration
	cancel, del                     time.Duration
	hasQueue, hasRun                bool
	hasCancel, hasDelete            bool
	err                             error
}

func main() {
	var o options
	var command string
	var extra stringList
	flag.StringVar(&o.url, "url", "http://localhost:8080", "sandboxd base URL")
	flag.IntVar(&o.jobs, "jobs", 100, "number of jobs to run")
	flag.IntVar(&o.concurrency, "concurrency", 10, "jobs in flight at once")
	flag.StringVar(&command, "command", "/bin/true", "command, split on whitespace")
	flag.Var(&extra, "arg", "extra argument appended verbatim (repeatable)")
	flag.DurationVar(&o.timeout, "timeout", 30*time.Second, "per-job timeout")
	flag.IntVar(&o.memoryMB, "memory-mb", 64, "per-job memory limit")
	flag.IntVar(&o.cpuMillis, "cpu-millis", 500, "per-job CPU limit")
	flag.IntVar(&o.pids, "pids", 16, "per-job pids limit")
	flag.DurationVar(&o.cancelAfter, "cancel-after", 0, "cancel each job this long after submission (0 = never)")
	flag.BoolVar(&o.delete, "delete", false, "delete jobs afterwards and measure DELETE latency")
	flag.Parse()
	o.command = append(strings.Fields(command), extra...)
	o.url = strings.TrimRight(o.url, "/")
	if len(o.command) == 0 || o.jobs <= 0 || o.concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "sandboxbench: need a command, --jobs > 0 and --concurrency > 0")
		os.Exit(2)
	}
	if err := run(o, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxbench:", err)
		os.Exit(1)
	}
}

func run(o options, out io.Writer) error {
	client := &http.Client{}
	var rejected atomic.Int64
	samples := make([]sample, o.jobs)
	work := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range o.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				samples[i] = runOne(client, o, &rejected)
			}
		}()
	}
	for i := range o.jobs {
		work <- i
	}
	close(work)
	wg.Wait()
	wall := time.Since(start)
	report(out, o, samples, rejected.Load(), wall)
	return nil
}

func runOne(c *http.Client, o options, rejected *atomic.Int64) sample {
	var s sample
	body, _ := json.Marshal(map[string]any{
		"command":         o.command,
		"timeout_seconds": int(o.timeout.Seconds()),
		"resources":       map[string]int{"memory_mb": o.memoryMB, "cpu_millis": o.cpuMillis, "pids": o.pids},
	})
	backoff := 100 * time.Millisecond
	var created jobView
	var submitted time.Time
	for {
		submitted = time.Now()
		resp, err := c.Post(o.url+"/v1/jobs", "application/json", bytes.NewReader(body))
		if err != nil {
			s.err = err
			return s
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			rejected.Add(1)
			time.Sleep(backoff)
			backoff = min(time.Duration(float64(backoff)*1.5), 2*time.Second)
			continue
		}
		s.submit = time.Since(submitted)
		if resp.StatusCode != http.StatusCreated {
			s.err = fmt.Errorf("submit: %s: %s", resp.Status, bytes.TrimSpace(data))
			return s
		}
		if err := json.Unmarshal(data, &created); err != nil {
			s.err = fmt.Errorf("decode submit response: %w", err)
			return s
		}
		break
	}

	var cancelAt time.Time
	var cancelDone chan struct{}
	if o.cancelAfter > 0 {
		cancelDone = make(chan struct{})
		go func() {
			defer close(cancelDone)
			time.Sleep(time.Until(submitted.Add(o.cancelAfter)))
			cancelAt = time.Now()
			resp, err := c.Post(o.url+"/v1/jobs/"+created.ID+"/cancel", "", nil)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					cancelAt = time.Time{} // already finished: no cancel latency
				}
			} else {
				cancelAt = time.Time{}
			}
		}()
	}

	if err := follow(c, o.url, created.ID); err != nil {
		s.err = err
		return s
	}
	terminal := time.Now()
	s.toFinal = terminal.Sub(submitted)
	if cancelDone != nil {
		<-cancelDone
		if !cancelAt.IsZero() && terminal.After(cancelAt) {
			s.cancel, s.hasCancel = terminal.Sub(cancelAt), true
		}
	}

	final, err := getJob(c, o.url, created.ID)
	if err != nil {
		s.err = err
		return s
	}
	s.state = final.State
	createdAt, _ := time.Parse(time.RFC3339Nano, final.CreatedAt)
	if final.StartedAt != nil {
		startedAt, _ := time.Parse(time.RFC3339Nano, *final.StartedAt)
		s.queueWait, s.hasQueue = startedAt.Sub(createdAt), true
		if final.FinishedAt != nil {
			finishedAt, _ := time.Parse(time.RFC3339Nano, *final.FinishedAt)
			s.run, s.hasRun = finishedAt.Sub(startedAt), true
		}
	}

	if o.delete {
		req, _ := http.NewRequest(http.MethodDelete, o.url+"/v1/jobs/"+created.ID, nil)
		t0 := time.Now()
		resp, err := c.Do(req)
		if err != nil {
			s.err = err
			return s
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			s.del, s.hasDelete = time.Since(t0), true
		}
	}
	return s
}

// follow reads the job's SSE log stream until a terminal state event.
func follow(c *http.Client, base, id string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/jobs/"+id+"/logs?follow=true", nil)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("follow %s: %s", id, resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "state":
			var st struct{ State string }
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st) == nil && terminal(st.State) {
				return nil
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("follow %s: %w", id, err)
	}
	return fmt.Errorf("follow %s: stream ended without a terminal state", id)
}

func terminal(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled", "timed_out":
		return true
	}
	return false
}

func getJob(c *http.Client, base, id string) (jobView, error) {
	var j jobView
	resp, err := c.Get(base + "/v1/jobs/" + id)
	if err != nil {
		return j, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return j, fmt.Errorf("get %s: %s", id, resp.Status)
	}
	return j, json.NewDecoder(resp.Body).Decode(&j)
}

func report(out io.Writer, o options, samples []sample, rejected int64, wall time.Duration) {
	states := map[string]int{}
	var errs []error
	var submit, queue, runD, final, cancel, del []time.Duration
	for _, s := range samples {
		if s.err != nil {
			errs = append(errs, s.err)
			continue
		}
		states[s.state]++
		submit = append(submit, s.submit)
		final = append(final, s.toFinal)
		if s.hasQueue {
			queue = append(queue, s.queueWait)
		}
		if s.hasRun {
			runD = append(runD, s.run)
		}
		if s.hasCancel {
			cancel = append(cancel, s.cancel)
		}
		if s.hasDelete {
			del = append(del, s.del)
		}
	}
	completed := len(samples) - len(errs)
	fmt.Fprintf(out, "jobs: %d submitted, %d completed, %d errors, %d rejected (429)\n", len(samples), completed, len(errs), rejected)
	for _, st := range slices.Sorted(maps.Keys(states)) {
		fmt.Fprintf(out, "  %-10s %d\n", st, states[st])
	}
	fmt.Fprintf(out, "wall time: %s, throughput: %.2f jobs/s\n\n", wall.Round(time.Millisecond), float64(completed)/wall.Seconds())

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "metric\tn\tp50\tp95\tp99\tmax\t")
	row := func(name string, d []time.Duration) {
		if len(d) == 0 {
			return
		}
		slices.Sort(d)
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t\n", name, len(d), pct(d, 50), pct(d, 95), pct(d, 99), d[len(d)-1].Round(time.Microsecond))
	}
	row("submit rtt", submit)
	row("queue wait", queue)
	row("run duration", runD)
	row("submit->terminal", final)
	row("cancel latency", cancel)
	row("delete latency", del)
	tw.Flush()
	for i, err := range errs {
		if i == 5 {
			fmt.Fprintf(out, "... and %d more errors\n", len(errs)-5)
			break
		}
		fmt.Fprintln(out, "error:", err)
	}
}

// pct uses the nearest-rank method on sorted durations.
func pct(sorted []time.Duration, p int) time.Duration {
	rank := (p*len(sorted) + 99) / 100
	return sorted[max(rank, 1)-1].Round(time.Microsecond)
}
