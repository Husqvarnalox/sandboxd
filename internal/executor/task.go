package executor

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/logbuf"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

// task adapts one job to scheduler.Task. Every path through Run and Discard
// ends with the job in a terminal state and its log buffer closed, in that
// order, so a log follower always sees the final state before its stream ends.
type task struct {
	e   *Executor
	ent *entry
}

type waitResult struct {
	res runtime.Result
	err error
}

func (t *task) ID() string { return t.ent.job.ID }

func (t *task) Discard() {
	j := t.ent.job
	j.RequestCancel(job.ReasonShutdown)
	t.e.finish(j, job.StateCancelled, job.Outcome{Reason: j.CancelReason()})
	t.ent.logs.Close()
}

func (t *task) Run(ctx context.Context) {
	defer t.ent.logs.Close()
	e, j := t.e, t.ent.job

	select {
	case <-ctx.Done():
		j.RequestCancel(job.ReasonShutdown)
	default:
	}
	if cancelled(j) {
		e.finish(j, job.StateCancelled, job.Outcome{Reason: j.CancelReason()})
		return
	}

	begin := time.Now()
	if err := j.Transition(job.StateStarting, begin); err != nil {
		e.log.Error("start job: lifecycle bug", "job_id", j.ID, "err", err)
		e.finish(j, job.StateFailed, job.Outcome{Reason: job.ReasonRuntimeError})
		return
	}
	e.m.QueueWait.Observe(begin.Sub(j.CreatedAt).Seconds())

	proc, err := t.start(ctx)
	e.m.StartLatency.Observe(time.Since(begin).Seconds())
	if err != nil {
		if ctx.Err() != nil {
			j.RequestCancel(job.ReasonShutdown)
		}
		if cancelled(j) {
			e.log.Debug("start interrupted by cancellation", "job_id", j.ID, "err", err)
			e.finish(j, job.StateCancelled, job.Outcome{Reason: j.CancelReason()})
			return
		}
		e.m.StartFailures.Inc()
		e.log.Error("start job", "job_id", j.ID, "err", err)
		e.finish(j, job.StateFailed, job.Outcome{Reason: job.ReasonStartFailed})
		return
	}

	if err := j.Transition(job.StateRunning, time.Now()); err != nil {
		// Unreachable unless the lifecycle table changes; still tear the
		// sandbox down so nothing is leaked.
		e.log.Error("run job: lifecycle bug", "job_id", j.ID, "err", err)
		j.RequestCancel(job.ReasonShutdown)
	}
	t.supervise(ctx, proc)
}

func cancelled(j *job.Job) bool {
	select {
	case <-j.Cancelled():
		return true
	default:
		return false
	}
}

// start calls the runtime with a context that is cancelled by a cancel
// request or by shutdown, so a slow sandbox setup can be abandoned.
func (t *task) start(ctx context.Context) (runtime.Process, error) {
	j := t.ent.job
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-j.Cancelled():
			cancel()
		case <-startCtx.Done():
		}
	}()
	return t.e.rt.Start(startCtx, runtime.Spec{
		ID:        j.ID,
		Command:   j.Spec.Command,
		Env:       buildEnv(j.Spec),
		WorkDir:   j.Spec.WorkDir,
		Workspace: filepath.Join(t.ent.dir, "workspace"),
		Resources: j.Spec.Resources,
		Hostname:  t.e.cfg.Hostname,
	})
}

func (t *task) supervise(ctx context.Context, proc runtime.Process) {
	e, j := t.e, t.ent.job

	var pumps sync.WaitGroup
	pump := func(r io.Reader, s logbuf.Stream) {
		defer pumps.Done()
		if _, err := io.CopyBuffer(t.ent.logs.Writer(s), r, make([]byte, pumpBufferSize)); err != nil {
			e.log.Debug("copy job output", "job_id", j.ID, "stream", s, "err", err)
		}
	}
	pumps.Add(2)
	go pump(proc.Stdout(), logbuf.Stdout)
	go pump(proc.Stderr(), logbuf.Stderr)

	waitCh := make(chan waitResult, 1)
	go func() {
		r, err := proc.Wait()
		waitCh <- waitResult{r, err}
	}()

	timeout := time.NewTimer(j.Spec.Timeout)
	defer timeout.Stop()
	ticker := time.NewTicker(e.cfg.StatsInterval)
	defer ticker.Stop()

	var (
		stats  job.Stats
		forced job.State // zero unless the job is being terminated by us
		reason string
		wr     waitResult
		exited bool
	)
	refresh := func() {
		s, err := proc.Stats()
		if err != nil {
			e.log.Debug("read job stats", "job_id", j.ID, "err", err)
			return
		}
		stats = s
		j.UpdateStats(s)
	}

	if cancelled(j) {
		forced, reason = job.StateCancelled, j.CancelReason()
	}
	// The first arm to win decides the outcome. In particular, if the timeout
	// fires in the same instant the workload exits, select may pick either
	// ready case: a timer win makes the job timed_out even though the process
	// was already gone (and exited 0). Terminating an exited process is
	// harmless, and the reported state is still one the client could have
	// observed, so no attempt is made to break the tie further.
	for forced == "" && !exited {
		select {
		case wr = <-waitCh:
			exited = true
		case <-ticker.C:
			refresh()
		case <-timeout.C:
			forced, reason = job.StateTimedOut, job.ReasonTimeout
		case <-j.Cancelled():
			forced, reason = job.StateCancelled, j.CancelReason()
		case <-ctx.Done():
			j.RequestCancel(job.ReasonShutdown)
			forced, reason = job.StateCancelled, j.CancelReason()
		}
	}
	if !exited {
		wr, exited = t.terminate(proc, waitCh)
	}

	drained := waitTimeout(&pumps, drainTimeout)
	if !drained {
		e.log.Warn("job output still open after exit; detaching", "job_id", j.ID)
	}
	if exited {
		refresh()
	}

	state, out := t.outcome(forced, reason, wr, exited, stats)
	e.finish(j, state, out)

	if !exited {
		// The process could not be confirmed dead. Close must follow Wait, so
		// hand cleanup to a goroutine that waits for it.
		e.m.CleanupFailures.Inc()
		go func() {
			<-waitCh
			t.close(proc)
		}()
		return
	}
	t.close(proc)
}

func (t *task) close(proc runtime.Process) {
	if err := proc.Close(); err != nil {
		t.e.m.CleanupFailures.Inc()
		t.e.log.Error("release sandbox", "job_id", t.ent.job.ID, "err", err)
	}
}

// terminate asks the workload to stop, then kills the whole sandbox after the
// grace period. It reports false if even the kill did not end the process
// within killTimeout.
func (t *task) terminate(proc runtime.Process, waitCh <-chan waitResult) (waitResult, bool) {
	e, id := t.e, t.ent.job.ID
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		e.log.Debug("signal job", "job_id", id, "err", err)
	}
	grace := time.NewTimer(e.cfg.KillGracePeriod)
	defer grace.Stop()
	select {
	case wr := <-waitCh:
		return wr, true
	case <-grace.C:
	}
	if err := proc.Kill(); err != nil {
		e.log.Warn("kill job", "job_id", id, "err", err)
	}
	deadline := time.NewTimer(killTimeout)
	defer deadline.Stop()
	select {
	case wr := <-waitCh:
		return wr, true
	case <-deadline.C:
		e.log.Error("job did not exit after kill", "job_id", id)
		return waitResult{}, false
	}
}

func (t *task) outcome(forced job.State, reason string, wr waitResult, exited bool, stats job.Stats) (job.State, job.Outcome) {
	e, id := t.e, t.ent.job.ID
	out := job.Outcome{Stats: stats}
	if exited && wr.err == nil {
		out.Exited = true
		out.ExitCode = wr.res.ExitCode
		if wr.res.OOMKilled {
			e.m.OOMTotal.Inc()
		}
	}
	if exited && wr.err != nil {
		e.log.Error("wait for job", "job_id", id, "err", wr.err)
	}
	if forced != "" {
		out.Reason = reason
		return forced, out
	}
	switch {
	case wr.err != nil:
		out.Reason = job.ReasonRuntimeError
		return job.StateFailed, out
	case wr.res.OOMKilled:
		out.Reason = job.ReasonOOMKilled
		return job.StateFailed, out
	case wr.res.ExitCode == 0:
		return job.StateSucceeded, out
	case wr.res.Signal != 0:
		out.Reason = job.ReasonSignaled
		return job.StateFailed, out
	default:
		out.Reason = job.ReasonNonZeroExit
		return job.StateFailed, out
	}
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
