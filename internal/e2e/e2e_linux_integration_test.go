//go:build linux && integration

// Package e2e drives the full daemon stack (cgroup tree, Linux runtime,
// scheduler, executor, HTTP API) through its public HTTP interface.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"

	"github.com/Husqvarnalox/sandboxd/internal/api"
	"github.com/Husqvarnalox/sandboxd/internal/cgroup"
	"github.com/Husqvarnalox/sandboxd/internal/executor"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/metrics"
	"github.com/Husqvarnalox/sandboxd/internal/runtime/linux"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

const (
	maxConcurrent = 2
	maxQueue      = 2
	pollInterval  = 50 * time.Millisecond
	smallBodyMax  = 4096
)

type stack struct {
	ex    *executor.Executor
	tree  *cgroup.Tree
	srv   *httptest.Server // default body limit
	small *httptest.Server // MaxBodyBytes = smallBodyMax
}

// newStack wires the real components the way cmd/sandboxd/serve.go does.
func newStack(t *testing.T) *stack {
	t.Helper()
	rootfs := os.Getenv("SANDBOXD_TEST_ROOTFS")
	if rootfs == "" || os.Geteuid() != 0 {
		t.Skip("e2e tests need root and SANDBOXD_TEST_ROOTFS (see scripts/test-linux.sh)")
	}
	cgRoot := os.Getenv("SANDBOXD_TEST_CGROUP_ROOT")
	if cgRoot == "" {
		cgRoot = cgroup.DefaultRoot
	}
	stateBase := os.Getenv("SANDBOXD_TEST_STATE")
	if stateBase == "" {
		stateBase = t.TempDir()
	}
	state := filepath.Join(stateBase, fmt.Sprintf("e2e-%d", os.Getpid()))
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })

	tree, err := cgroup.Open(cgRoot, fmt.Sprintf("sandboxd-e2e-%d", os.Getpid()))
	if err != nil {
		t.Fatalf("open cgroup tree: %v", err)
	}
	t.Cleanup(func() { removeTree(t, tree) })

	log := slog.New(slog.DiscardHandler)
	rt, err := linux.New(linux.Config{
		Rootfs:      rootfs,
		Tree:        tree,
		InitPath:    initBinary(t),
		UserNS:      true,
		HostUIDBase: 100000,
		SandboxUID:  65534,
		SandboxGID:  65534,
		Logger:      log,
	})
	if err != nil {
		t.Fatalf("linux.New: %v", err)
	}

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	sched := scheduler.New(scheduler.Config{MaxConcurrent: maxConcurrent, MaxQueue: maxQueue})
	m.RegisterScheduler(reg, sched.Stats)
	ex := executor.New(executor.Config{
		Limits:          job.DefaultLimits(),
		KillGracePeriod: time.Second,
		StateDir:        state,
		StatsInterval:   200 * time.Millisecond,
	}, rt, sched, m, log)
	sched.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := ex.Shutdown(ctx); err != nil {
			t.Errorf("executor shutdown: %v", err)
		}
	})

	handler := func(maxBody int64) http.Handler {
		return api.New(api.Config{
			Executor: ex,
			Ready: func() error {
				if !ex.Accepting() {
					return errors.New("shutting down")
				}
				if err := rt.Check(); err != nil {
					return errors.New("sandbox runtime unavailable")
				}
				return nil
			},
			Metrics:      metrics.Handler(reg),
			Version:      api.VersionInfo{Version: "e2e", Commit: "none", GoVersion: goruntime.Version()},
			MaxBodyBytes: maxBody,
			Logger:       log,
		})
	}
	s := &stack{ex: ex, tree: tree, srv: httptest.NewServer(handler(0)), small: httptest.NewServer(handler(smallBodyMax))}
	t.Cleanup(s.srv.Close)
	t.Cleanup(s.small.Close)
	return s
}

// initBinary copies the test binary to a world-executable directory: with
// user namespaces init is exec'd as the unprivileged mapped uid.
func initBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "sandboxd-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sandboxd-init")
	if err := os.WriteFile(p, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// removeTree removes the test subtree once the runtime has removed every job
// cgroup beneath it.
func removeTree(t *testing.T, tree *cgroup.Tree) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := unix.Rmdir(tree.Path())
		if err == nil || err == unix.ENOENT {
			return
		}
		if time.Now().After(deadline) {
			ids, _ := tree.List()
			t.Errorf("remove test cgroup tree %s: %v (children: %v)", tree.Path(), err, ids)
			return
		}
		time.Sleep(pollInterval)
	}
}

// --- HTTP helpers ---------------------------------------------------------

type jobJSON struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	ExitCode   *int    `json:"exit_code"`
	Reason     string  `json:"reason"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
	Stats      struct {
		MemoryPeakBytes *int64 `json:"memory_peak_bytes"`
		OOMKills        int    `json:"oom_kills"`
	} `json:"stats"`
}

type errJSON struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	var e errJSON
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("decode error body %q: %v", r.body, err)
	}
	return e.Error.Code
}

func (r resp) job(t *testing.T) jobJSON {
	t.Helper()
	var j jobJSON
	if err := json.Unmarshal(r.body, &j); err != nil {
		t.Fatalf("decode job %q: %v", r.body, err)
	}
	return j
}

func do(t *testing.T, base, method, path string, body any) resp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp{status: res.StatusCode, header: res.Header, body: data}
}

func expect(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d (body %s)", r.status, status, r.body)
	}
	if code != "" {
		if got := r.errCode(t); got != code {
			t.Fatalf("error code %q, want %q", got, code)
		}
	}
}

type req map[string]any

func submit(t *testing.T, s *stack, body req) jobJSON {
	t.Helper()
	r := do(t, s.srv.URL, "POST", "/v1/jobs", body)
	expect(t, r, http.StatusCreated, "")
	return r.job(t)
}

func sh(script string) req { return req{"command": []string{"sh", "-c", script}} }

func get(t *testing.T, s *stack, id string) jobJSON {
	t.Helper()
	r := do(t, s.srv.URL, "GET", "/v1/jobs/"+id, nil)
	expect(t, r, http.StatusOK, "")
	return r.job(t)
}

var terminal = map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "timed_out": true}

// waitFor polls the job until cond holds or timeout expires.
func waitFor(t *testing.T, s *stack, id string, timeout time.Duration, what string, cond func(jobJSON) bool) jobJSON {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j := get(t, s, id)
		if cond(j) {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: timed out after %v waiting for %s (state %s)", id, timeout, what, j.State)
		}
		time.Sleep(pollInterval)
	}
}

func waitTerminal(t *testing.T, s *stack, id string, timeout time.Duration) jobJSON {
	t.Helper()
	return waitFor(t, s, id, timeout, "a terminal state", func(j jobJSON) bool { return terminal[j.State] })
}

func waitRunning(t *testing.T, s *stack, id string) {
	t.Helper()
	waitFor(t, s, id, 10*time.Second, "running", func(j jobJSON) bool {
		if terminal[j.State] {
			t.Fatalf("job %s finished (%s/%s) before running", id, j.State, j.Reason)
		}
		return j.State == "running"
	})
}

func expectFinal(t *testing.T, j jobJSON, state, reason string) {
	t.Helper()
	if j.State != state || j.Reason != reason {
		t.Fatalf("job %s: %s/%s, want %s/%s", j.ID, j.State, j.Reason, state, reason)
	}
}

type sseEvent struct {
	name string
	data map[string]any
}

// readSSE reads the log stream of a job until the server ends it.
func readSSE(t *testing.T, s *stack, id string, follow bool, timeout time.Duration) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	url := fmt.Sprintf("%s/v1/jobs/%s/logs?follow=%t", s.srv.URL, id, follow)
	r, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("GET logs: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET logs: status %d, content type %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.name != "" {
				events = append(events, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Fatalf("decode SSE data %q: %v", line, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read log stream: %v (events so far: %v)", err, events)
	}
	return events
}

// output concatenates the text of all events of one stream.
func output(events []sseEvent, stream string) string {
	var b strings.Builder
	for _, e := range events {
		if e.name == stream {
			s, _ := e.data["data"].(string)
			b.WriteString(s)
		}
	}
	return b.String()
}

func lastState(t *testing.T, events []sseEvent) string {
	t.Helper()
	if len(events) == 0 || events[len(events)-1].name != "state" {
		t.Fatalf("log stream does not end with a state event: %v", events)
	}
	st, _ := events[len(events)-1].data["state"].(string)
	return st
}

// procsMatching returns the pids whose command line is exactly cmd.
func procsMatching(cmd string) []string {
	dirs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	var pids []string
	for _, p := range dirs {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.TrimRight(strings.ReplaceAll(string(data), "\x00", " "), " ") == cmd {
			pids = append(pids, filepath.Base(filepath.Dir(p)))
		}
	}
	return pids
}

// --- tests ----------------------------------------------------------------

// TestE2E runs every case against one stack, sequentially: the queue case
// needs exact control over the scheduler, and graceful shutdown is last.
func TestE2E(t *testing.T) {
	s := newStack(t)
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, *stack)
	}{
		{"APISmoke", testAPISmoke},
		{"FollowStreaming", testFollowStreaming},
		{"MemoryLimit", testMemoryLimit},
		{"Timeout", testTimeout},
		{"CancelDescendants", testCancelDescendants},
		{"QueueFull", testQueueFull},
		{"Validation", testValidation},
		{"Artifacts", testArtifacts},
		{"GracefulShutdown", testGracefulShutdown},
	} {
		// Each case uses fresh jobs, so a failure does not stop the rest.
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, s) })
	}
}

func testAPISmoke(t *testing.T, s *stack) {
	expect(t, do(t, s.srv.URL, "GET", "/health", nil), http.StatusOK, "")
	expect(t, do(t, s.srv.URL, "GET", "/ready", nil), http.StatusOK, "")
	r := do(t, s.srv.URL, "GET", "/version", nil)
	expect(t, r, http.StatusOK, "")
	var v map[string]string
	if err := json.Unmarshal(r.body, &v); err != nil || v["version"] == "" || v["go_version"] == "" {
		t.Fatalf("version body %s (err %v)", r.body, err)
	}

	j := submit(t, s, sh("echo hello; echo err >&2; exit 0"))
	if j.ID == "" || j.State != "queued" && j.State != "starting" && j.State != "running" {
		t.Fatalf("unexpected submit response %+v", j)
	}
	j = waitTerminal(t, s, j.ID, 15*time.Second)
	expectFinal(t, j, "succeeded", "")
	if j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("exit code %v, want 0", j.ExitCode)
	}

	ev := readSSE(t, s, j.ID, false, 10*time.Second)
	if got := output(ev, "stdout"); got != "hello\n" {
		t.Errorf("stdout %q, want %q", got, "hello\n")
	}
	if got := output(ev, "stderr"); got != "err\n" {
		t.Errorf("stderr %q, want %q", got, "err\n")
	}
	if st := lastState(t, ev); st != "succeeded" {
		t.Errorf("final state event %q", st)
	}

	r = do(t, s.srv.URL, "GET", "/metrics", nil)
	expect(t, r, http.StatusOK, "")
	if !bytes.Contains(r.body, []byte("sandboxd_")) {
		t.Error("metrics have no sandboxd_ series")
	}
	if bytes.Contains(r.body, []byte(j.ID)) {
		t.Error("metrics contain a job id")
	}

	r = do(t, s.srv.URL, "GET", "/v1/jobs", nil)
	expect(t, r, http.StatusOK, "")
	var list struct{ Jobs []jobJSON }
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lj := range list.Jobs {
		found = found || lj.ID == j.ID
	}
	if !found {
		t.Errorf("job %s missing from list %s", j.ID, r.body)
	}

	expect(t, do(t, s.srv.URL, "DELETE", "/v1/jobs/"+j.ID, nil), http.StatusNoContent, "")
	expect(t, do(t, s.srv.URL, "GET", "/v1/jobs/"+j.ID, nil), http.StatusNotFound, "NOT_FOUND")
}

func testFollowStreaming(t *testing.T, s *stack) {
	j := submit(t, s, sh("for i in 1 2 3; do echo line$i; sleep 0.2; done"))
	ev := readSSE(t, s, j.ID, true, 15*time.Second)
	if got, want := output(ev, "stdout"), "line1\nline2\nline3\n"; got != want {
		t.Errorf("followed stdout %q, want %q", got, want)
	}
	if st := lastState(t, ev); st != "succeeded" {
		t.Errorf("final state event %q, want succeeded", st)
	}
}

func testMemoryLimit(t *testing.T, s *stack) {
	// /dev/shm is a tmpfs inside the sandbox; its pages are charged to the
	// job's cgroup, so writing 64 MiB under a 16 MiB limit must OOM.
	body := sh("dd if=/dev/zero of=/dev/shm/fill bs=1M count=64")
	body["resources"] = req{"memory_mb": 16}
	j := submit(t, s, body)
	j = waitTerminal(t, s, j.ID, 20*time.Second)
	expectFinal(t, j, "failed", "oom_killed")
	if j.Stats.OOMKills < 1 {
		t.Errorf("oom_kills %d, want >= 1", j.Stats.OOMKills)
	}
}

func testTimeout(t *testing.T, s *stack) {
	body := sh("sleep 30")
	body["timeout_seconds"] = 1
	j := submit(t, s, body)
	j = waitTerminal(t, s, j.ID, 10*time.Second)
	expectFinal(t, j, "timed_out", "timeout")
	if j.StartedAt == nil || j.FinishedAt == nil {
		t.Fatalf("missing timestamps: %+v", j)
	}
	started, err1 := time.Parse(time.RFC3339Nano, *j.StartedAt)
	finished, err2 := time.Parse(time.RFC3339Nano, *j.FinishedAt)
	if err1 != nil || err2 != nil {
		t.Fatalf("parse timestamps: %v %v", err1, err2)
	}
	// 1s timeout + 1s kill grace, plus scheduling slack.
	if d := finished.Sub(started); d < time.Second || d > 3500*time.Millisecond {
		t.Errorf("job ran %v, want about 1s (+ up to 1s grace)", d)
	}
}

func testCancelDescendants(t *testing.T, s *stack) {
	const sleeper = "sleep 137"
	body := sh(sleeper + " & " + sleeper + " & " + sleeper + "; wait")
	body["resources"] = req{"pids": 16}
	j := submit(t, s, body)
	waitRunning(t, s, j.ID)
	// All three sleeps should be visible before cancelling.
	deadline := time.Now().Add(5 * time.Second)
	for len(procsMatching(sleeper)) < 3 && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
	}
	if n := len(procsMatching(sleeper)); n != 3 {
		t.Fatalf("found %d %q processes before cancel, want 3", n, sleeper)
	}

	expect(t, do(t, s.srv.URL, "POST", "/v1/jobs/"+j.ID+"/cancel", nil), http.StatusAccepted, "")
	j = waitTerminal(t, s, j.ID, 10*time.Second)
	expectFinal(t, j, "cancelled", "user")

	cg := filepath.Join(s.tree.Path(), j.ID)
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(cg)
		left := procsMatching(sleeper)
		if errors.Is(err, os.ErrNotExist) && len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after cancel: cgroup stat err %v, %q pids left %v", err, sleeper, left)
		}
		time.Sleep(pollInterval)
	}

	expect(t, do(t, s.srv.URL, "POST", "/v1/jobs/"+j.ID+"/cancel", nil), http.StatusConflict, "JOB_FINISHED")
	expect(t, do(t, s.srv.URL, "POST", "/v1/jobs/j_doesnotexist/cancel", nil), http.StatusNotFound, "NOT_FOUND")
}

func testQueueFull(t *testing.T, s *stack) {
	var ids []string
	t.Cleanup(func() {
		for _, id := range ids {
			do(t, s.srv.URL, "POST", "/v1/jobs/"+id+"/cancel", nil)
		}
		for _, id := range ids {
			waitTerminal(t, s, id, 10*time.Second)
		}
	})
	for range maxConcurrent {
		j := submit(t, s, sh("sleep 30"))
		ids = append(ids, j.ID)
		waitRunning(t, s, j.ID)
	}
	for range maxQueue {
		j := submit(t, s, sh("sleep 30"))
		ids = append(ids, j.ID)
		if j.State != "queued" {
			t.Fatalf("job %s state %s, want queued", j.ID, j.State)
		}
	}
	r := do(t, s.srv.URL, "POST", "/v1/jobs", sh("sleep 30"))
	expect(t, r, http.StatusTooManyRequests, "QUEUE_FULL")
	if r.header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	queued := ids[maxConcurrent]
	r = do(t, s.srv.URL, "POST", "/v1/jobs/"+queued+"/cancel", nil)
	expect(t, r, http.StatusAccepted, "")
	// A queued job is finished by the cancel request itself.
	j := get(t, s, queued)
	expectFinal(t, j, "cancelled", "user")
	if j.ExitCode != nil || j.StartedAt != nil {
		t.Errorf("cancelled queued job has exit_code %v, started_at %v", j.ExitCode, j.StartedAt)
	}
}

func testValidation(t *testing.T, s *stack) {
	for _, tc := range []struct {
		name   string
		base   string
		body   any
		status int
		code   string
	}{
		{"empty command", s.srv.URL, req{"command": []string{}}, 400, "INVALID_REQUEST"},
		{"memory too small", s.srv.URL, req{"command": []string{"true"}, "resources": req{"memory_mb": 1}}, 400, "INVALID_REQUEST"},
		{"unknown field", s.srv.URL, req{"command": []string{"true"}, "bogus": 1}, 400, "INVALID_REQUEST"},
		{"body too large", s.small.URL, []byte(`{"command":["` + strings.Repeat("a", 5000) + `"]}`), 413, "REQUEST_TOO_LARGE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, do(t, tc.base, "POST", "/v1/jobs", tc.body), tc.status, tc.code)
		})
	}
}

func testArtifacts(t *testing.T, s *stack) {
	running := submit(t, s, sh("sleep 30"))
	t.Cleanup(func() {
		do(t, s.srv.URL, "POST", "/v1/jobs/"+running.ID+"/cancel", nil)
		waitTerminal(t, s, running.ID, 10*time.Second)
	})
	expect(t, do(t, s.srv.URL, "GET", "/v1/jobs/"+running.ID+"/artifacts/out.txt", nil), http.StatusConflict, "JOB_NOT_FINISHED")

	j := submit(t, s, sh("mkdir -p /workspace/artifacts && echo artifact-content > /workspace/artifacts/out.txt && ln -s /etc/passwd /workspace/artifacts/leak"))
	j = waitTerminal(t, s, j.ID, 15*time.Second)
	expectFinal(t, j, "succeeded", "")

	base := "/v1/jobs/" + j.ID + "/artifacts/"
	r := do(t, s.srv.URL, "GET", base+"out.txt", nil)
	expect(t, r, http.StatusOK, "")
	if string(r.body) != "artifact-content\n" {
		t.Errorf("artifact body %q", r.body)
	}
	for _, p := range []string{"..%2Fetc%2Fpasswd", "../etc/passwd", "leak"} {
		r := do(t, s.srv.URL, "GET", base+p, nil)
		if r.status != http.StatusNotFound || bytes.Contains(r.body, []byte("root:")) {
			t.Errorf("GET %s: status %d body %q, want 404", p, r.status, r.body)
		}
	}
}

func testGracefulShutdown(t *testing.T, s *stack) {
	j := submit(t, s, sh("sleep 100"))
	waitRunning(t, s, j.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ex.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	j = get(t, s, j.ID)
	expectFinal(t, j, "cancelled", "shutdown")
	expect(t, do(t, s.srv.URL, "POST", "/v1/jobs", sh("true")), http.StatusServiceUnavailable, "SHUTTING_DOWN")
	expect(t, do(t, s.srv.URL, "GET", "/ready", nil), http.StatusServiceUnavailable, "NOT_READY")
}
