package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

type fakeTask struct {
	id        string
	run       func(ctx context.Context)
	runs      atomic.Int32
	discarded atomic.Int32
}

func (f *fakeTask) ID() string { return f.id }
func (f *fakeTask) Run(ctx context.Context) {
	f.runs.Add(1)
	if f.run != nil {
		f.run(ctx)
	}
}
func (f *fakeTask) Discard() { f.discarded.Add(1) }

func newTask(id string, run func(ctx context.Context)) *fakeTask {
	return &fakeTask{id: id, run: run}
}

// blocker returns a task that signals started and then waits for release or
// context cancellation.
func blocker(id string, started chan<- struct{}, release <-chan struct{}) *fakeTask {
	return newTask(id, func(ctx context.Context) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
}

func shutdown(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNewPanicsOnBadConfig(t *testing.T) {
	for _, cfg := range []Config{{}, {MaxConcurrent: 1}, {MaxQueue: 1}, {MaxConcurrent: -1, MaxQueue: 1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%+v) did not panic", cfg)
				}
			}()
			New(cfg)
		}()
	}
}

func TestFIFOOrder(t *testing.T) {
	const n = 100
	s := New(Config{MaxConcurrent: 1, MaxQueue: n})
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		err := s.Submit(newTask(fmt.Sprint(i), func(context.Context) {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			wg.Done()
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Start()
	s.Start() // idempotent
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("order[%d] = %d", i, got)
		}
	}
	shutdown(t, s)
}

func TestConcurrencyLimit(t *testing.T) {
	const limit, total = 4, 40
	s := New(Config{MaxConcurrent: limit, MaxQueue: total})
	var cur, high atomic.Int32
	release := make(chan struct{})
	var done sync.WaitGroup
	done.Add(total)
	for i := range total {
		s.Submit(newTask(fmt.Sprint(i), func(context.Context) {
			defer done.Done()
			n := cur.Add(1)
			for {
				h := high.Load()
				if n <= h || high.CompareAndSwap(h, n) {
					break
				}
			}
			<-release
			cur.Add(-1)
		}))
	}
	s.Start()
	waitFor(t, func() bool { return s.Stats().Running == limit })
	if st := s.Stats(); st.Queued != total-limit {
		t.Fatalf("stats = %+v", st)
	}
	close(release)
	done.Wait()
	if h := high.Load(); h != limit {
		t.Fatalf("high-water mark = %d, want %d", h, limit)
	}
	shutdown(t, s)
}

func TestQueueFull(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 2})
	s.Start()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	if err := s.Submit(blocker("run", started, release)); err != nil {
		t.Fatal(err)
	}
	<-started
	for _, id := range []string{"a", "b"} {
		if err := s.Submit(newTask(id, nil)); err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
	}
	if err := s.Submit(newTask("c", nil)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
	close(release)
	shutdown(t, s)
}

func TestDuplicateID(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 4})
	s.Start()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	s.Submit(blocker("x", started, release))
	<-started
	if err := s.Submit(newTask("x", nil)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate of running task: %v", err)
	}
	s.Submit(newTask("y", nil))
	if err := s.Submit(newTask("y", nil)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate of queued task: %v", err)
	}
	close(release)
	shutdown(t, s)
}

func TestIDReusableAfterCompletion(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 4})
	s.Start()
	done := make(chan struct{})
	s.Submit(newTask("x", func(context.Context) { close(done) }))
	<-done
	waitFor(t, func() bool { return s.Submit(newTask("x", nil)) == nil })
	shutdown(t, s)
}

func TestCancel(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 4})
	s.Start()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	running := blocker("running", started, release)
	queued := newTask("queued", nil)
	after := newTask("after", func(context.Context) {})
	s.Submit(running)
	<-started
	s.Submit(queued)
	s.Submit(after)

	if s.Cancel("unknown") {
		t.Error("Cancel(unknown) = true")
	}
	if s.Cancel("running") {
		t.Error("Cancel(running) = true")
	}
	if !s.Cancel("queued") {
		t.Fatal("Cancel(queued) = false")
	}
	if s.Cancel("queued") {
		t.Error("second Cancel(queued) = true")
	}
	if st := s.Stats(); st.Queued != 1 {
		t.Fatalf("stats = %+v", st)
	}
	close(release)
	waitFor(t, func() bool { return after.runs.Load() == 1 })
	shutdown(t, s)
	if queued.runs.Load() != 0 || queued.discarded.Load() != 0 {
		t.Fatalf("cancelled task ran=%d discarded=%d", queued.runs.Load(), queued.discarded.Load())
	}
}

func TestShutdown(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 8})
	s.Start()
	started := make(chan struct{}, 1)
	ctxDone := make(chan struct{})
	running := newTask("running", func(ctx context.Context) {
		started <- struct{}{}
		<-ctx.Done()
		close(ctxDone)
	})
	s.Submit(running)
	<-started
	var queued []*fakeTask
	for i := range 3 {
		q := newTask(fmt.Sprint("q", i), nil)
		queued = append(queued, q)
		s.Submit(q)
	}

	shutdown(t, s)
	shutdown(t, s) // idempotent
	<-ctxDone

	for _, q := range queued {
		if q.runs.Load() != 0 || q.discarded.Load() != 1 {
			t.Errorf("%s: ran=%d discarded=%d", q.id, q.runs.Load(), q.discarded.Load())
		}
	}
	if running.discarded.Load() != 0 {
		t.Error("running task was discarded")
	}
	if err := s.Submit(newTask("late", nil)); !errors.Is(err, ErrStopped) {
		t.Fatalf("Submit after Shutdown: %v", err)
	}
	if st := s.Stats(); st.Queued != 0 || st.Running != 0 {
		t.Fatalf("stats = %+v", st)
	}
	s.Start() // must not resurrect workers
}

func TestShutdownBeforeStart(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxQueue: 4})
	q := newTask("q", nil)
	s.Submit(q)
	shutdown(t, s)
	s.Start()
	if q.runs.Load() != 0 || q.discarded.Load() != 1 {
		t.Fatalf("ran=%d discarded=%d", q.runs.Load(), q.discarded.Load())
	}
}

func TestShutdownContextExpires(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueue: 1})
	s.Start()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	stubborn := newTask("stubborn", func(context.Context) { // ignores cancellation
		started <- struct{}{}
		<-release
	})
	s.Submit(stubborn)
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	close(release)
	shutdown(t, s)
}

func TestStats(t *testing.T) {
	s := New(Config{MaxConcurrent: 2, MaxQueue: 5})
	if st := s.Stats(); st != (Stats{MaxConcurrent: 2, MaxQueue: 5}) {
		t.Fatalf("initial stats = %+v", st)
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	for i := range 4 {
		s.Submit(blocker(fmt.Sprint(i), started, release))
	}
	if st := s.Stats(); st.Queued != 4 || st.Running != 0 {
		t.Fatalf("before start = %+v", st)
	}
	s.Start()
	<-started
	<-started
	waitFor(t, func() bool { return s.Stats() == Stats{Queued: 2, Running: 2, MaxConcurrent: 2, MaxQueue: 5} })
	close(release)
	waitFor(t, func() bool { st := s.Stats(); return st.Queued == 0 && st.Running == 0 })
	shutdown(t, s)
}

func TestConcurrentSubmitCancelShutdown(t *testing.T) {
	s := New(Config{MaxConcurrent: 3, MaxQueue: 64})
	s.Start()

	const goroutines, perG = 8, 200
	var all []*fakeTask
	var wg sync.WaitGroup
	var mu sync.Mutex
	var cancelled atomic.Int32
	var halfOnce sync.Once
	half := make(chan struct{})
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perG {
				if g == 0 && i == perG/2 {
					halfOnce.Do(func() { close(half) })
				}
				task := newTask(fmt.Sprintf("%d-%d", g, i), func(context.Context) {})
				if err := s.Submit(task); err != nil {
					continue
				}
				mu.Lock()
				all = append(all, task)
				mu.Unlock()
				if i%3 == 0 && s.Cancel(task.id) {
					cancelled.Add(1)
					task.discarded.Add(100) // marks "removed by caller"
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-half
		shutdown(t, s)
	}()
	wg.Wait()

	for _, task := range all {
		runs, disc := task.runs.Load(), task.discarded.Load()
		switch {
		case disc >= 100:
			if runs != 0 || disc != 100 {
				t.Errorf("%s: cancelled but ran=%d discarded=%d", task.id, runs, disc)
			}
		case runs+disc != 1:
			t.Errorf("%s: ran=%d discarded=%d", task.id, runs, disc)
		}
	}
}

func BenchmarkSubmitRun(b *testing.B) {
	s := New(Config{MaxConcurrent: 4, MaxQueue: 1024})
	s.Start()
	var wg sync.WaitGroup
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		wg.Add(1)
		task := newTask(fmt.Sprint(i), func(context.Context) { wg.Done() })
		i++
		for s.Submit(task) != nil {
			runtime.Gosched() // queue momentarily full; let the workers drain
		}
	}
	wg.Wait()
	b.StopTimer()
	s.Shutdown(context.Background())
}
