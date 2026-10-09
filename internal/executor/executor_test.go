package executor

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/logbuf"
	"github.com/Husqvarnalox/sandboxd/internal/metrics"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
	"github.com/Husqvarnalox/sandboxd/internal/runtime/runtimetest"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

const testDeadline = 20 * time.Second

// errorRecorder is a slog.Handler that remembers error-level messages so tests
// can assert that no lifecycle bug was logged.
type errorRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (h *errorRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *errorRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *errorRecorder) WithGroup(string) slog.Handler            { return h }
func (h *errorRecorder) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.mu.Lock()
		h.msgs = append(h.msgs, r.Message)
		h.mu.Unlock()
	}
	return nil
}

func (h *errorRecorder) errors() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.msgs)
}

type harness struct {
	ex   *Executor
	rt   *runtimetest.Runtime
	m    *metrics.Metrics
	dir  string
	logs *errorRecorder
}

type options struct {
	sched  scheduler.Config
	config func(*Config)
}

// behaviours maps Command[0] to a fake behaviour.
func newHarness(t *testing.T, opts options, behaviours map[string]runtimetest.Behavior) *harness {
	t.Helper()
	if opts.sched.MaxConcurrent == 0 {
		opts.sched = scheduler.Config{MaxConcurrent: 4, MaxQueue: 64}
	}
	limits := job.DefaultLimits()
	limits.MinTimeout = time.Millisecond
	cfg := Config{
		Limits:          limits,
		MaxLogBytes:     1 << 20,
		KillGracePeriod: 50 * time.Millisecond,
		StateDir:        t.TempDir(),
		StatsInterval:   5 * time.Millisecond,
	}
	if opts.config != nil {
		opts.config(&cfg)
	}
	rt := runtimetest.New(func(s runtime.Spec) runtimetest.Behavior { return behaviours[s.Command[0]] })
	sched := scheduler.New(opts.sched)
	m := metrics.New(prometheus.NewRegistry())
	rec := &errorRecorder{}
	ex := New(cfg, rt, sched, m, slog.New(rec))
	sched.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
		defer cancel()
		if err := ex.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return &harness{ex: ex, rt: rt, m: m, dir: cfg.StateDir, logs: rec}
}

func (h *harness) submit(t *testing.T, cmd string, mod ...func(*job.Spec)) job.Snapshot {
	t.Helper()
	spec := job.Spec{Command: []string{cmd}, Timeout: time.Minute}
	for _, f := range mod {
		f(&spec)
	}
	snap, err := h.ex.Submit(spec)
	if err != nil {
		t.Fatalf("submit %s: %v", cmd, err)
	}
	return snap
}

func (h *harness) waitState(t *testing.T, id string, done func(job.State) bool) job.Snapshot {
	t.Helper()
	deadline := time.NewTimer(testDeadline)
	defer deadline.Stop()
	for {
		ch, err := h.ex.Changed(id)
		if err != nil {
			t.Fatalf("changed %s: %v", id, err)
		}
		snap, err := h.ex.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if done(snap.State) {
			return snap
		}
		select {
		case <-ch:
		case <-deadline.C:
			t.Fatalf("job %s stuck in %s", id, snap.State)
		}
	}
}

func (h *harness) waitTerminal(t *testing.T, id string) job.Snapshot {
	t.Helper()
	snap := h.waitState(t, id, job.State.Terminal)
	h.assertNoLifecycleErrors(t)
	return snap
}

func (h *harness) assertNoLifecycleErrors(t *testing.T) {
	t.Helper()
	for _, msg := range h.logs.errors() {
		if strings.Contains(msg, "lifecycle bug") {
			t.Fatalf("lifecycle error logged: %s", msg)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func historyText(t *testing.T, h *harness, id string) string {
	t.Helper()
	logs, err := h.ex.Logs(id)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range logs.History() {
		b.Write(c.Data)
	}
	return b.String()
}

func TestOutcomes(t *testing.T) {
	stats := job.Stats{CPUUsage: 1500 * time.Millisecond, MemoryPeak: 4 << 20, MemoryPeakKnown: true, PIDsCurrent: 1}
	tests := []struct {
		name      string
		b         runtimetest.Behavior
		state     job.State
		reason    string
		exitCode  int
		exited    bool
		started   bool
		closeFail float64
	}{
		{name: "success", b: runtimetest.Behavior{Stdout: []byte("hello\n"), Stats: stats}, state: job.StateSucceeded, exited: true, started: true},
		{name: "non-zero", b: runtimetest.Behavior{ExitCode: 3}, state: job.StateFailed, reason: job.ReasonNonZeroExit, exitCode: 3, exited: true, started: true},
		{name: "signaled", b: runtimetest.Behavior{ExitCode: 137, Signal: 9}, state: job.StateFailed, reason: job.ReasonSignaled, exitCode: 137, exited: true, started: true},
		{name: "oom", b: runtimetest.Behavior{ExitCode: 137, Signal: 9, OOMKilled: true}, state: job.StateFailed, reason: job.ReasonOOMKilled, exitCode: 137, exited: true, started: true},
		{name: "start error", b: runtimetest.Behavior{StartErr: errors.New("mount /secret/host/path: EPERM")}, state: job.StateFailed, reason: job.ReasonStartFailed},
		{name: "wait error", b: runtimetest.Behavior{WaitErr: errors.New("wait4: ECHILD")}, state: job.StateFailed, reason: job.ReasonRuntimeError, started: true},
		{name: "close error", b: runtimetest.Behavior{CloseErr: errors.New("rmdir: EBUSY")}, state: job.StateSucceeded, exited: true, started: true, closeFail: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": tt.b})
			snap := h.waitTerminal(t, h.submit(t, "/bin/x").ID)
			if snap.State != tt.state || snap.Reason != tt.reason || snap.ExitCode != tt.exitCode || snap.Exited != tt.exited {
				t.Fatalf("got state=%s reason=%q exit=%d exited=%v", snap.State, snap.Reason, snap.ExitCode, snap.Exited)
			}
			if got := !snap.StartedAt.IsZero(); got != tt.started {
				t.Fatalf("started=%v, want %v", got, tt.started)
			}
			if tt.state == job.StateSucceeded && tt.b.Stats != (job.Stats{}) && snap.Stats != tt.b.Stats {
				t.Fatalf("stats = %+v, want %+v", snap.Stats, tt.b.Stats)
			}
			if string(tt.b.Stdout) != historyText(t, h, snap.ID) {
				t.Fatalf("logs = %q", historyText(t, h, snap.ID))
			}
			logs, _ := h.ex.Logs(snap.ID)
			waitFor(t, "logs closed", logs.Closed)
			if p := h.rt.Proc(snap.ID); p != nil {
				waitFor(t, "close", func() bool { return p.Closed() == 1 })
			}
			if got := testutil.ToFloat64(h.m.CleanupFailures); got != tt.closeFail {
				t.Fatalf("cleanup failures = %v", got)
			}
			if got := testutil.ToFloat64(h.m.JobsTotal.WithLabelValues(string(tt.state))); got != 1 {
				t.Fatalf("jobs_total{%s} = %v", tt.state, got)
			}
			if tt.reason == job.ReasonOOMKilled && testutil.ToFloat64(h.m.OOMTotal) != 1 {
				t.Fatal("oom counter not incremented")
			}
		})
	}
}

func TestValidationErrorPassesThrough(t *testing.T) {
	h := newHarness(t, options{}, nil)
	_, err := h.ex.Submit(job.Spec{})
	var ve *job.ValidationError
	if !errors.As(err, &ve) || ve.Field != "command" {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	tests := []struct {
		name   string
		b      runtimetest.Behavior
		killed bool
	}{
		{"honours TERM", runtimetest.Behavior{RunUntilSignal: true}, false},
		{"ignores TERM", runtimetest.Behavior{RunUntilSignal: true, IgnoreTerm: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": tt.b})
			snap := h.waitTerminal(t, h.submit(t, "/bin/x", func(s *job.Spec) { s.Timeout = 20 * time.Millisecond }).ID)
			if snap.State != job.StateTimedOut || snap.Reason != job.ReasonTimeout {
				t.Fatalf("got %s/%s", snap.State, snap.Reason)
			}
			p := h.rt.Proc(snap.ID)
			if sigs := p.Signals(); len(sigs) != 1 || sigs[0] != syscall.SIGTERM {
				t.Fatalf("signals = %v", sigs)
			}
			if p.Killed() != tt.killed {
				t.Fatalf("killed = %v", p.Killed())
			}
			if testutil.ToFloat64(h.m.TimeoutTotal) != 1 {
				t.Fatal("timeout counter not incremented")
			}
		})
	}
}

func TestCancelQueuedNeverStarts(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, options{sched: scheduler.Config{MaxConcurrent: 1, MaxQueue: 4}}, map[string]runtimetest.Behavior{
		"/bin/block": {Release: release},
	})
	blocker := h.submit(t, "/bin/block")
	h.waitState(t, blocker.ID, func(s job.State) bool { return s == job.StateRunning })
	queued := h.submit(t, "/bin/other")

	if err := h.ex.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	snap := h.waitTerminal(t, queued.ID)
	if snap.State != job.StateCancelled || snap.Reason != job.ReasonUser || snap.Exited {
		t.Fatalf("got %+v", snap)
	}
	if err := h.ex.Cancel(queued.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("second cancel: %v", err)
	}
	close(release)
	if h.waitTerminal(t, blocker.ID).State != job.StateSucceeded {
		t.Fatal("blocker did not succeed")
	}
	for _, s := range h.rt.Starts() {
		if s.ID == queued.ID {
			t.Fatal("cancelled queued job was started")
		}
	}
	if testutil.ToFloat64(h.m.CancelTotal.WithLabelValues(job.ReasonUser)) != 1 {
		t.Fatal("cancel counter not incremented")
	}
}

func TestCancelRunning(t *testing.T) {
	h := newHarness(t, options{config: func(c *Config) { c.KillGracePeriod = 200 * time.Millisecond }},
		map[string]runtimetest.Behavior{"/bin/x": {RunUntilSignal: true, IgnoreTerm: true}})
	id := h.submit(t, "/bin/x").ID
	h.waitState(t, id, func(s job.State) bool { return s == job.StateRunning })
	for range 2 {
		if err := h.ex.Cancel(id); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}
	snap := h.waitTerminal(t, id)
	if snap.State != job.StateCancelled || snap.Reason != job.ReasonUser {
		t.Fatalf("got %s/%s", snap.State, snap.Reason)
	}
	p := h.rt.Proc(id)
	if sigs := p.Signals(); len(sigs) != 1 || sigs[0] != syscall.SIGTERM || !p.Killed() {
		t.Fatalf("signals=%v killed=%v", sigs, p.Killed())
	}
}

func TestCancelHonouredTERMIsNotKilled(t *testing.T) {
	h := newHarness(t, options{config: func(c *Config) { c.KillGracePeriod = time.Minute }},
		map[string]runtimetest.Behavior{"/bin/x": {RunUntilSignal: true}})
	id := h.submit(t, "/bin/x").ID
	h.waitState(t, id, func(s job.State) bool { return s == job.StateRunning })
	if err := h.ex.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if snap := h.waitTerminal(t, id); snap.State != job.StateCancelled || h.rt.Proc(id).Killed() {
		t.Fatalf("state=%s killed=%v", snap.State, h.rt.Proc(id).Killed())
	}
}

func TestCancelDuringStart(t *testing.T) {
	h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": {BlockStart: true}})
	id := h.submit(t, "/bin/x").ID
	waitFor(t, "start call", func() bool { return len(h.rt.Starts()) == 1 })
	if err := h.ex.Cancel(id); err != nil {
		t.Fatal(err)
	}
	snap := h.waitTerminal(t, id)
	if snap.State != job.StateCancelled || snap.Reason != job.ReasonUser || !snap.StartedAt.IsZero() {
		t.Fatalf("got %+v", snap)
	}
	if testutil.ToFloat64(h.m.StartFailures) != 0 {
		t.Fatal("cancelled start counted as start failure")
	}
}

func TestShutdown(t *testing.T) {
	h := newHarness(t, options{sched: scheduler.Config{MaxConcurrent: 2, MaxQueue: 4}}, map[string]runtimetest.Behavior{
		"/bin/run":   {RunUntilSignal: true, IgnoreTerm: true},
		"/bin/start": {BlockStart: true},
	})
	running := h.submit(t, "/bin/run").ID
	starting := h.submit(t, "/bin/start").ID
	queued := h.submit(t, "/bin/queued").ID
	h.waitState(t, running, func(s job.State) bool { return s == job.StateRunning })
	h.waitState(t, starting, func(s job.State) bool { return s == job.StateStarting })

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := h.ex.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if h.ex.Accepting() {
		t.Fatal("still accepting")
	}
	for _, id := range []string{running, starting, queued} {
		snap, _ := h.ex.Get(id)
		if snap.State != job.StateCancelled || snap.Reason != job.ReasonShutdown {
			t.Fatalf("job %s: %s/%s", id, snap.State, snap.Reason)
		}
	}
	p := h.rt.Proc(running)
	if sigs := p.Signals(); len(sigs) != 1 || sigs[0] != syscall.SIGTERM || !p.Killed() {
		t.Fatalf("running job: signals=%v killed=%v", p.Signals(), p.Killed())
	}
	for _, s := range h.rt.Starts() {
		if s.ID == queued {
			t.Fatal("queued job was started")
		}
	}
	if _, err := h.ex.Submit(job.Spec{Command: []string{"/bin/true"}}); !errors.Is(err, ErrStopped) {
		t.Fatalf("submit after shutdown: %v", err)
	}
	h.assertNoLifecycleErrors(t)
}

func TestDelete(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": {Release: release, Stdout: []byte("out")}})
	id := h.submit(t, "/bin/x").ID
	h.waitState(t, id, func(s job.State) bool { return s == job.StateRunning })
	if err := h.ex.Delete(id); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("delete running: %v", err)
	}
	close(release)
	h.waitTerminal(t, id)
	logs, _ := h.ex.Logs(id)
	if err := h.ex.Delete(id); err != nil {
		t.Fatal(err)
	}
	if !logs.Closed() {
		t.Fatal("logs not closed")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "jobs", id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("job dir still present: %v", err)
	}
	for _, err := range []error{h.ex.Delete(id), h.ex.Cancel(id), func() error { _, err := h.ex.Get(id); return err }()} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	}
	if _, err := h.ex.Get("not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
}

func TestArtifacts(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": {Release: release}})
	id := h.submit(t, "/bin/x").ID
	h.waitState(t, id, func(s job.State) bool { return s == job.StateRunning })
	if _, err := h.ex.OpenArtifact(id, "out.txt"); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("artifact while running: %v", err)
	}
	close(release)
	h.waitTerminal(t, id)

	ws := filepath.Join(h.dir, "jobs", id, "workspace")
	art := filepath.Join(ws, "artifacts")
	mustNoErr(t, os.MkdirAll(filepath.Join(art, "nested", "dir"), 0o755))
	mustNoErr(t, os.WriteFile(filepath.Join(art, "out.txt"), []byte("result"), 0o644))
	mustNoErr(t, os.WriteFile(filepath.Join(art, "nested", "dir", "f.txt"), []byte("deep"), 0o644))
	mustNoErr(t, os.WriteFile(filepath.Join(ws, "outside.txt"), []byte("ws"), 0o644))
	mustNoErr(t, os.Symlink("/etc/passwd", filepath.Join(art, "evil")))
	mustNoErr(t, os.Symlink("../../../../../../../../etc/passwd", filepath.Join(art, "evil-rel")))
	mustNoErr(t, os.Symlink(h.dir, filepath.Join(art, "evil-dir")))

	for rel, want := range map[string]string{"out.txt": "result", "nested/dir/f.txt": "deep"} {
		f, err := h.ex.OpenArtifact(id, rel)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		data, err := os.ReadFile(f.Name())
		f.Close()
		if err != nil || string(data) != want {
			t.Fatalf("%s: %q %v", rel, data, err)
		}
	}
	for _, rel := range []string{
		"evil", "evil-rel", "evil-dir/jobs", "../outside.txt", "nested/../../outside.txt", "..",
		"", "/etc/passwd", "nested", "missing", "a\x00b", `..\outside.txt`,
	} {
		f, err := h.ex.OpenArtifact(id, rel)
		if err == nil {
			f.Close()
			t.Fatalf("%q: opened", rel)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%q: err = %v, want fs.ErrNotExist", rel, err)
		}
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestListOrdering(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": {Release: release}})
	var want []string
	for range 5 {
		want = append(want, h.submit(t, "/bin/x").ID)
	}
	var got []string
	for _, s := range h.ex.List() {
		got = append(got, s.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestQueueFull(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, options{sched: scheduler.Config{MaxConcurrent: 1, MaxQueue: 1}},
		map[string]runtimetest.Behavior{"/bin/x": {Release: release}})
	first := h.submit(t, "/bin/x").ID
	h.waitState(t, first, func(s job.State) bool { return s == job.StateRunning })
	h.submit(t, "/bin/x")
	if _, err := h.ex.Submit(job.Spec{Command: []string{"/bin/x"}}); !errors.Is(err, scheduler.ErrQueueFull) {
		t.Fatalf("err = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(h.dir, "jobs"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("job dirs = %d, %v", len(entries), err)
	}
	if len(h.ex.List()) != 2 {
		t.Fatal("rejected job kept in table")
	}
	if testutil.ToFloat64(h.m.QueueRejected) != 1 {
		t.Fatal("queue rejection not counted")
	}
}

func TestEnvDoesNotLeakDaemonEnvironment(t *testing.T) {
	t.Setenv("SANDBOXD_TEST_CANARY", "leaked")
	h := newHarness(t, options{}, nil)
	snap := h.submit(t, "/bin/x", func(s *job.Spec) { s.Env = map[string]string{"FOO": "bar", "PATH": "/custom"} })
	h.waitTerminal(t, snap.ID)
	starts := h.rt.Starts()
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	want := []string{"FOO=bar", "HOME=/workspace", "PATH=/custom"}
	if !slices.Equal(starts[0].Env, want) {
		t.Fatalf("env = %q, want %q", starts[0].Env, want)
	}
	for _, kv := range starts[0].Env {
		if strings.Contains(kv, "CANARY") || strings.Contains(kv, "leaked") {
			t.Fatalf("daemon environment leaked: %q", kv)
		}
	}
	if s := starts[0]; s.Hostname != "sandbox" || s.WorkDir != job.DefaultWorkDir ||
		s.Workspace != filepath.Join(h.dir, "jobs", snap.ID, "workspace") {
		t.Fatalf("spec = %+v", s)
	}
}

func TestDefaultEnv(t *testing.T) {
	if got := buildEnv(job.Spec{}); !slices.Equal(got, []string{"HOME=/workspace", "PATH=" + defaultPath}) {
		t.Fatalf("env = %q", got)
	}
}

func TestStatsRefreshedWhileRunning(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, options{}, map[string]runtimetest.Behavior{"/bin/x": {Release: release, Stats: job.Stats{PIDsCurrent: 3}}})
	id := h.submit(t, "/bin/x").ID
	waitFor(t, "stats", func() bool {
		s, _ := h.ex.Get(id)
		return s.State == job.StateRunning && s.Stats.PIDsCurrent == 3
	})
	close(release)
	h.waitTerminal(t, id)
}

func TestLargeOutputIsBoundedAndDoesNotDeadlock(t *testing.T) {
	const total = 20 << 20
	h := newHarness(t, options{config: func(c *Config) { c.MaxLogBytes = 1 << 20 }},
		map[string]runtimetest.Behavior{"/bin/x": {LargeOutput: total, Stderr: []byte("done")}})
	snap := h.waitTerminal(t, h.submit(t, "/bin/x").ID)
	if snap.State != job.StateSucceeded {
		t.Fatalf("state = %s", snap.State)
	}
	logs, _ := h.ex.Logs(snap.ID)
	if logs.Size() > 1<<20 {
		t.Fatalf("retained %d bytes", logs.Size())
	}
	if got := logs.Truncated() + int64(logs.Size()); got != total+4 {
		t.Fatalf("retained+truncated = %d, want %d", got, total+4)
	}
	// The two pumps run concurrently, so stderr's position relative to the
	// stdout tail is not fixed; it must merely be retained.
	if !slices.ContainsFunc(logs.History(), func(c logbuf.Chunk) bool {
		return c.Stream == logbuf.Stderr && string(c.Data) == "done"
	}) {
		t.Fatal("stderr chunk missing from history")
	}
}

func TestCancelExitRace(t *testing.T) {
	h := newHarness(t, options{sched: scheduler.Config{MaxConcurrent: 8, MaxQueue: 128}}, nil)
	for i := range 100 {
		id := h.submit(t, "/bin/x").ID
		if i%2 == 0 {
			h.waitState(t, id, func(s job.State) bool { return s != job.StateQueued })
		}
		if err := h.ex.Cancel(id); err != nil && !errors.Is(err, ErrFinished) {
			t.Fatalf("cancel: %v", err)
		}
		snap := h.waitTerminal(t, id)
		if snap.State != job.StateSucceeded && snap.State != job.StateCancelled {
			t.Fatalf("iteration %d: state %s", i, snap.State)
		}
	}
	for _, msg := range h.logs.errors() {
		t.Errorf("unexpected error log: %s", msg)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	h := newHarness(t, options{config: func(c *Config) { c.KillGracePeriod = 5 * time.Millisecond }}, map[string]runtimetest.Behavior{
		"/bin/ok":      {Stdout: []byte("x")},
		"/bin/fail":    {ExitCode: 1},
		"/bin/nostart": {StartErr: errors.New("boom")},
		"/bin/hang":    {RunUntilSignal: true, IgnoreTerm: true},
	})
	base := goruntime.NumGoroutine()
	var ids []string
	for i := range 40 {
		cmd := []string{"/bin/ok", "/bin/fail", "/bin/nostart", "/bin/hang"}[i%4]
		ids = append(ids, h.submit(t, cmd, func(s *job.Spec) { s.Timeout = 10 * time.Millisecond }).ID)
	}
	for _, id := range ids {
		h.waitTerminal(t, id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d > %d\n%s", goruntime.NumGoroutine(), base, buf[:goruntime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCleanupStale(t *testing.T) {
	h := newHarness(t, options{}, nil)
	id, _ := job.NewID()
	jobs := filepath.Join(h.dir, "jobs")
	mustNoErr(t, os.MkdirAll(filepath.Join(jobs, id, "workspace"), 0o755))
	mustNoErr(t, os.MkdirAll(filepath.Join(jobs, "keep-me"), 0o755))
	if err := h.ex.CleanupStale(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(jobs, id)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("stale job dir not removed")
	}
	if _, err := os.Stat(filepath.Join(jobs, "keep-me")); err != nil {
		t.Fatal("foreign entry removed")
	}
}
