package job

import (
	"sync"
	"time"
)

// Stats are resource usage figures read from the job's cgroup. Fields that the
// kernel does not expose are reported through the *Known flags rather than as
// a misleading zero.
type Stats struct {
	CPUUsage        time.Duration
	MemoryCurrent   int64
	MemoryPeak      int64
	MemoryPeakKnown bool
	PIDsCurrent     int
	OOMKills        int
}

// Outcome is everything the executor knows when a job reaches a terminal
// state. It is applied atomically with the state transition so observers never
// see a terminal job without its exit information.
type Outcome struct {
	ExitCode int
	// Exited is false when the workload never produced an exit status (start
	// failure, or cancellation before start).
	Exited bool
	Reason string
	Stats  Stats
}

// Job is the mutable record for one submitted job. The executor is the only
// writer; everything else observes it through Snapshot.
//
// Cancellation is a one-shot request recorded on the job itself rather than
// derived from a context, so that user cancel, timeout and daemon shutdown all
// flow through the same path and the request survives whichever goroutine
// happens to be driving the job at that moment.
type Job struct {
	ID        string
	Spec      Spec
	CreatedAt time.Time

	mu         sync.Mutex
	state      State
	exitCode   int
	exited     bool
	reason     string
	startedAt  time.Time
	finishedAt time.Time
	stats      Stats
	changed    chan struct{}

	cancelOnce   sync.Once
	cancelCh     chan struct{}
	cancelReason string
}

// New creates a job in StateQueued.
func New(id string, spec Spec, now time.Time) *Job {
	return &Job{
		ID:        id,
		Spec:      spec,
		CreatedAt: now,
		state:     StateQueued,
		changed:   make(chan struct{}),
		cancelCh:  make(chan struct{}),
	}
}

// State returns the current state.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// Transition moves the job to a non-terminal state. Use Finish for terminal
// states so exit information is recorded in the same critical section.
func (j *Job) Transition(to State, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if to.Terminal() {
		return &TransitionError{From: j.state, To: to}
	}
	return j.transitionLocked(to, now)
}

// Finish moves the job to a terminal state and records the outcome.
func (j *Job) Finish(to State, out Outcome, now time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !to.Terminal() {
		return &TransitionError{From: j.state, To: to}
	}
	if err := j.transitionLocked(to, now); err != nil {
		return err
	}
	j.exitCode = out.ExitCode
	j.exited = out.Exited
	j.reason = out.Reason
	j.stats = out.Stats
	return nil
}

func (j *Job) transitionLocked(to State, now time.Time) error {
	if !CanTransition(j.state, to) {
		return &TransitionError{From: j.state, To: to}
	}
	j.state = to
	if to == StateRunning {
		j.startedAt = now
	}
	if to.Terminal() {
		j.finishedAt = now
	}
	close(j.changed)
	j.changed = make(chan struct{})
	return nil
}

// Changed returns a channel that is closed on the next state transition.
// Callers re-arm by calling Changed again after it fires.
func (j *Job) Changed() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.changed
}

// UpdateStats records the latest resource usage while the job runs.
func (j *Job) UpdateStats(s Stats) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.Terminal() {
		return
	}
	j.stats = s
}

// RequestCancel records a cancellation request. Only the first request is
// kept; it returns true if this call was the first.
func (j *Job) RequestCancel(reason string) bool {
	first := false
	j.cancelOnce.Do(func() {
		j.mu.Lock()
		j.cancelReason = reason
		j.mu.Unlock()
		close(j.cancelCh)
		first = true
	})
	return first
}

// Cancelled is closed once cancellation has been requested.
func (j *Job) Cancelled() <-chan struct{} { return j.cancelCh }

// CancelReason returns the reason of the first cancellation request, or "".
func (j *Job) CancelReason() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelReason
}

// Snapshot is an immutable view of a job for the API and logs.
type Snapshot struct {
	ID         string
	Spec       Spec
	State      State
	ExitCode   int
	Exited     bool
	Reason     string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Stats      Stats
}

// Snapshot returns a consistent copy of the job's observable fields.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	spec := j.Spec
	spec.Command = append([]string(nil), j.Spec.Command...)
	if j.Spec.Env != nil {
		spec.Env = make(map[string]string, len(j.Spec.Env))
		for k, v := range j.Spec.Env {
			spec.Env[k] = v
		}
	}
	return Snapshot{
		ID:         j.ID,
		Spec:       spec,
		State:      j.state,
		ExitCode:   j.exitCode,
		Exited:     j.exited,
		Reason:     j.reason,
		CreatedAt:  j.CreatedAt,
		StartedAt:  j.startedAt,
		FinishedAt: j.finishedAt,
		Stats:      j.stats,
	}
}
