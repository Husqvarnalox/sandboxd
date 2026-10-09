package job

import (
	"errors"
	"fmt"
)

// State is the lifecycle state of a job. Terminal states are immutable.
type State string

const (
	StateQueued    State = "queued"
	StateStarting  State = "starting"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
	StateTimedOut  State = "timed_out"
)

// Reasons attached to a job when it finishes without a plain exit 0, or when
// cancellation is requested. They are stable strings exposed through the API.
const (
	ReasonUser         = "user"
	ReasonShutdown     = "shutdown"
	ReasonTimeout      = "timeout"
	ReasonOOMKilled    = "oom_killed"
	ReasonNonZeroExit  = "non_zero_exit"
	ReasonSignaled     = "signaled"
	ReasonStartFailed  = "start_failed"
	ReasonRuntimeError = "runtime_error"
)

var ErrInvalidTransition = errors.New("invalid state transition")

// TransitionError reports a rejected transition. It unwraps to
// ErrInvalidTransition.
type TransitionError struct {
	From, To State
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid state transition %s -> %s", e.From, e.To)
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

var transitions = map[State]map[State]bool{
	StateQueued: {
		StateStarting:  true,
		StateCancelled: true,
	},
	StateStarting: {
		StateRunning:   true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StateRunning: {
		StateSucceeded: true,
		StateFailed:    true,
		StateCancelled: true,
		StateTimedOut:  true,
	},
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	switch s {
	case StateQueued, StateStarting, StateRunning,
		StateSucceeded, StateFailed, StateCancelled, StateTimedOut:
		return true
	}
	return false
}

// Terminal reports whether s is a final state. Terminal states never change.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateTimedOut:
		return true
	}
	return false
}

func (s State) String() string { return string(s) }

// CanTransition reports whether from -> to is an allowed lifecycle transition.
func CanTransition(from, to State) bool {
	return transitions[from][to]
}
