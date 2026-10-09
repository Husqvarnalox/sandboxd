// Package executor owns the job table and drives each job through its
// lifecycle on top of a runtime.Runtime and a scheduler.Scheduler.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/logbuf"
	"github.com/Husqvarnalox/sandboxd/internal/metrics"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

// Config controls job execution. Zero values get defaults in New.
type Config struct {
	// Limits bound accepted specs; the zero value means job.DefaultLimits().
	Limits job.Limits
	// MaxLogBytes is the per-job log retention across both streams.
	MaxLogBytes int
	// KillGracePeriod is the time between SIGTERM and the forced kill.
	KillGracePeriod time.Duration
	// StateDir holds job workspaces at <StateDir>/jobs/<id>/workspace.
	StateDir string
	// Hostname is the sandbox hostname.
	Hostname string
	// StatsInterval is how often resource usage is refreshed while running.
	StatsInterval time.Duration
}

var (
	ErrNotFound    = errors.New("job not found")
	ErrFinished    = errors.New("job already finished")
	ErrNotFinished = errors.New("job not finished")
	ErrStopped     = errors.New("executor is shutting down")
)

const (
	defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

	pumpBufferSize = 32 << 10

	// After process exit the pipes normally hit EOF immediately (the runtime
	// guarantees nothing in the sandbox survives init). These bounds only
	// keep a misbehaving runtime from pinning a job in a non-terminal state.
	drainTimeout = 5 * time.Second
	killTimeout  = 10 * time.Second
)

// Executor is safe for concurrent use.
type Executor struct {
	cfg   Config
	rt    runtime.Runtime
	sched *scheduler.Scheduler
	m     *metrics.Metrics
	log   *slog.Logger

	accepting atomic.Bool

	mu   sync.Mutex
	jobs map[string]*entry
}

type entry struct {
	job  *job.Job
	logs *logbuf.Buffer
	dir  string // <StateDir>/jobs/<id>
}

// New returns an Executor that accepts jobs immediately. The caller starts
// and stops sched through the Executor's lifetime (Shutdown stops it).
func New(cfg Config, rt runtime.Runtime, sched *scheduler.Scheduler, m *metrics.Metrics, log *slog.Logger) *Executor {
	if cfg.Limits == (job.Limits{}) {
		cfg.Limits = job.DefaultLimits()
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = 4 << 20
	}
	if cfg.KillGracePeriod <= 0 {
		cfg.KillGracePeriod = 5 * time.Second
	}
	if cfg.Hostname == "" {
		cfg.Hostname = "sandbox"
	}
	if cfg.StatsInterval <= 0 {
		cfg.StatsInterval = time.Second
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Executor{cfg: cfg, rt: rt, sched: sched, m: m, log: log, jobs: make(map[string]*entry)}
	e.accepting.Store(true)
	return e
}

func (e *Executor) jobsDir() string { return filepath.Join(e.cfg.StateDir, "jobs") }

// CleanupStale removes job directories left by a previous daemon instance.
// Only entries named like job ids are touched.
func (e *Executor) CleanupStale() error {
	entries, err := os.ReadDir(e.jobsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read jobs dir: %w", err)
	}
	var errs []error
	for _, de := range entries {
		name := de.Name()
		if !job.ValidID(name) {
			e.log.Warn("skipping unexpected entry in jobs dir", "name", name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(e.jobsDir(), name)); err != nil {
			errs = append(errs, fmt.Errorf("remove stale job dir %s: %w", name, err))
			continue
		}
		e.log.Info("removed stale job dir", "job_id", name)
	}
	return errors.Join(errs...)
}

// Submit validates spec, registers the job and queues it.
func (e *Executor) Submit(spec job.Spec) (job.Snapshot, error) {
	if !e.accepting.Load() {
		return job.Snapshot{}, ErrStopped
	}
	if err := spec.Validate(e.cfg.Limits); err != nil {
		return job.Snapshot{}, err
	}
	id, err := job.NewID()
	if err != nil {
		return job.Snapshot{}, err
	}
	dir := filepath.Join(e.jobsDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		return job.Snapshot{}, fmt.Errorf("create workspace for job %s: %w", id, err)
	}
	ent := &entry{job: job.New(id, spec, time.Now()), logs: logbuf.New(e.cfg.MaxLogBytes), dir: dir}

	e.mu.Lock()
	e.jobs[id] = ent
	e.mu.Unlock()

	if err := e.sched.Submit(&task{e: e, ent: ent}); err != nil {
		e.mu.Lock()
		delete(e.jobs, id)
		e.mu.Unlock()
		ent.logs.Close()
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			e.log.Error("remove rejected job dir", "job_id", id, "err", rmErr)
		}
		switch {
		case errors.Is(err, scheduler.ErrQueueFull):
			e.m.QueueRejected.Inc()
			return job.Snapshot{}, err
		case errors.Is(err, scheduler.ErrStopped):
			return job.Snapshot{}, ErrStopped
		}
		return job.Snapshot{}, fmt.Errorf("submit job %s: %w", id, err)
	}
	return ent.job.Snapshot(), nil
}

func (e *Executor) lookup(id string) (*entry, error) {
	if !job.ValidID(id) {
		return nil, ErrNotFound
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return ent, nil
}

func (e *Executor) Get(id string) (job.Snapshot, error) {
	ent, err := e.lookup(id)
	if err != nil {
		return job.Snapshot{}, err
	}
	return ent.job.Snapshot(), nil
}

// List returns every known job ordered by creation time, then id.
func (e *Executor) List() []job.Snapshot {
	e.mu.Lock()
	jobs := make([]*job.Job, 0, len(e.jobs))
	for _, ent := range e.jobs {
		jobs = append(jobs, ent.job)
	}
	e.mu.Unlock()

	snaps := make([]job.Snapshot, len(jobs))
	for i, j := range jobs {
		snaps[i] = j.Snapshot()
	}
	slices.SortFunc(snaps, func(a, b job.Snapshot) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return snaps
}

// Cancel requests cancellation. A queued job is finished immediately; a
// starting or running job is terminated by its task. Repeated cancels of a
// live job succeed.
func (e *Executor) Cancel(id string) error {
	ent, err := e.lookup(id)
	if err != nil {
		return err
	}
	j := ent.job
	if j.State().Terminal() {
		return ErrFinished
	}
	j.RequestCancel(job.ReasonUser)
	// Exactly one of scheduler.Cancel, Task.Run and Task.Discard ever owns a
	// job, so finishing here cannot race with the task.
	if e.sched.Cancel(id) {
		e.finish(j, job.StateCancelled, job.Outcome{Reason: j.CancelReason()})
		ent.logs.Close()
	}
	return nil
}

// Delete removes a finished job's files and metadata.
func (e *Executor) Delete(id string) error {
	ent, err := e.lookup(id)
	if err != nil {
		return err
	}
	if !ent.job.State().Terminal() {
		return ErrNotFinished
	}
	if err := os.RemoveAll(ent.dir); err != nil {
		e.log.Error("delete job dir", "job_id", id, "err", err)
		return fmt.Errorf("delete job %s: %w", id, err)
	}
	e.mu.Lock()
	delete(e.jobs, id)
	e.mu.Unlock()
	ent.logs.Close()
	return nil
}

func (e *Executor) Logs(id string) (*logbuf.Buffer, error) {
	ent, err := e.lookup(id)
	if err != nil {
		return nil, err
	}
	return ent.logs, nil
}

// Changed returns a channel closed on the job's next state transition.
func (e *Executor) Changed(id string) (<-chan struct{}, error) {
	ent, err := e.lookup(id)
	if err != nil {
		return nil, err
	}
	return ent.job.Changed(), nil
}

// OpenArtifact opens <workspace>/artifacts/<rel> of a finished job. Anything
// that is not a regular file inside the workspace, including symlinks that
// resolve outside it, is reported as fs.ErrNotExist.
func (e *Executor) OpenArtifact(id, rel string) (*os.File, error) {
	ent, err := e.lookup(id)
	if err != nil {
		return nil, err
	}
	if !ent.job.State().Terminal() {
		return nil, ErrNotFinished
	}
	if !validArtifactPath(rel) {
		return nil, fmt.Errorf("open artifact: invalid path: %w", fs.ErrNotExist)
	}
	// os.Root resolves every component with openat-style semantics beneath the
	// workspace and refuses symlinks (absolute or relative) that escape it, so
	// a workload cannot plant a link to a host file.
	root, err := os.OpenRoot(filepath.Join(ent.dir, "workspace"))
	if err != nil {
		return nil, fmt.Errorf("open workspace of job %s: %w", id, err)
	}
	defer root.Close()
	f, err := root.Open(path.Join("artifacts", rel))
	if err != nil {
		e.log.Debug("open artifact", "job_id", id, "err", err)
		return nil, fmt.Errorf("open artifact: %w", fs.ErrNotExist)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("open artifact: not a regular file: %w", fs.ErrNotExist)
	}
	return f, nil
}

func validArtifactPath(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsRune(rel, 0) || strings.ContainsRune(rel, '\\') {
		return false
	}
	for elem := range strings.SplitSeq(rel, "/") {
		if elem == ".." {
			return false
		}
	}
	return true
}

func (e *Executor) Accepting() bool { return e.accepting.Load() }

// Shutdown stops accepting jobs, cancels queued ones and terminates running
// ones (SIGTERM, then kill after the grace period). It returns ctx.Err() if
// jobs are still being torn down when ctx expires.
func (e *Executor) Shutdown(ctx context.Context) error {
	e.accepting.Store(false)
	return e.sched.Shutdown(ctx)
}

// finish applies a terminal state. A TransitionError means a lifecycle bug;
// it is logged rather than allowed to crash the daemon.
func (e *Executor) finish(j *job.Job, st job.State, out job.Outcome) {
	if err := j.Finish(st, out, time.Now()); err != nil {
		e.log.Error("finish job: lifecycle bug", "job_id", j.ID, "err", err)
		return
	}
	snap := j.Snapshot()
	e.m.JobsTotal.WithLabelValues(string(st)).Inc()
	switch st {
	case job.StateCancelled:
		e.m.CancelTotal.WithLabelValues(out.Reason).Inc()
	case job.StateTimedOut:
		e.m.TimeoutTotal.Inc()
	}
	attrs := []any{"job_id", j.ID, "state", st, "command", snap.Spec.Command[0], "reason", out.Reason}
	if !snap.StartedAt.IsZero() {
		d := snap.FinishedAt.Sub(snap.StartedAt)
		e.m.JobDuration.Observe(d.Seconds())
		attrs = append(attrs, "duration_ms", d.Milliseconds())
	}
	if out.Exited {
		e.m.JobCPU.Observe(out.Stats.CPUUsage.Seconds())
		if out.Stats.MemoryPeakKnown {
			e.m.JobMemoryPeak.Observe(float64(out.Stats.MemoryPeak))
		}
		attrs = append(attrs, "exit_code", out.ExitCode)
	}
	e.log.Info("job finished", attrs...)
}

func buildEnv(spec job.Spec) []string {
	env := map[string]string{"PATH": defaultPath, "HOME": job.DefaultWorkDir}
	for k, v := range spec.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}
