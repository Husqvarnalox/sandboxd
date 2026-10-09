package job

import (
	"errors"
	"testing"
	"time"
)

var allStates = []State{
	StateQueued, StateStarting, StateRunning,
	StateSucceeded, StateFailed, StateCancelled, StateTimedOut,
}

func TestTransitionTable(t *testing.T) {
	allowed := map[[2]State]bool{
		{StateQueued, StateStarting}:    true,
		{StateQueued, StateCancelled}:   true,
		{StateStarting, StateRunning}:   true,
		{StateStarting, StateFailed}:    true,
		{StateStarting, StateCancelled}: true,
		{StateRunning, StateSucceeded}:  true,
		{StateRunning, StateFailed}:     true,
		{StateRunning, StateCancelled}:  true,
		{StateRunning, StateTimedOut}:   true,
	}
	for _, from := range allStates {
		for _, to := range allStates {
			want := allowed[[2]State{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestTerminalStatesAreImmutable(t *testing.T) {
	for _, from := range allStates {
		if !from.Terminal() {
			continue
		}
		for _, to := range allStates {
			if CanTransition(from, to) {
				t.Errorf("terminal state %s must not transition to %s", from, to)
			}
		}
	}
}

func TestJobLifecycle(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	j := New("id", Spec{Command: []string{"true"}}, now)

	if err := j.Transition(StateStarting, now); err != nil {
		t.Fatal(err)
	}
	if err := j.Transition(StateRunning, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := j.Snapshot().StartedAt; !got.Equal(now.Add(time.Second)) {
		t.Fatalf("StartedAt = %v", got)
	}

	// Terminal states must go through Finish so exit info lands atomically.
	err := j.Transition(StateSucceeded, now)
	var te *TransitionError
	if !errors.As(err, &te) || !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Transition to terminal state: got %v, want TransitionError", err)
	}

	out := Outcome{ExitCode: 3, Exited: true, Reason: ReasonNonZeroExit, Stats: Stats{PIDsCurrent: 1}}
	if err := j.Finish(StateFailed, out, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	s := j.Snapshot()
	if s.State != StateFailed || s.ExitCode != 3 || !s.Exited || s.Reason != ReasonNonZeroExit {
		t.Fatalf("snapshot after Finish = %+v", s)
	}
	if !s.FinishedAt.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("FinishedAt = %v", s.FinishedAt)
	}

	// Nothing moves a terminal job.
	if err := j.Finish(StateCancelled, Outcome{}, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Finish on terminal job: got %v", err)
	}
	if err := j.Transition(StateRunning, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Transition on terminal job: got %v", err)
	}
	j.UpdateStats(Stats{PIDsCurrent: 99})
	if j.Snapshot().Stats.PIDsCurrent != 1 {
		t.Fatal("UpdateStats must not change a finished job")
	}
}

func TestFinishRejectsNonTerminal(t *testing.T) {
	j := New("id", Spec{}, time.Now())
	if err := j.Finish(StateRunning, Outcome{}, time.Now()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("got %v", err)
	}
	if j.State() != StateQueued {
		t.Fatalf("state changed to %s", j.State())
	}
}

func TestChangedFiresOncePerTransition(t *testing.T) {
	j := New("id", Spec{}, time.Now())
	ch := j.Changed()
	select {
	case <-ch:
		t.Fatal("Changed fired before any transition")
	default:
	}
	if err := j.Transition(StateStarting, time.Now()); err != nil {
		t.Fatal(err)
	}
	<-ch
	ch2 := j.Changed()
	if ch2 == ch {
		t.Fatal("Changed must return a fresh channel after firing")
	}
	select {
	case <-ch2:
		t.Fatal("new channel fired without a transition")
	default:
	}
}

func TestRequestCancelFirstWins(t *testing.T) {
	j := New("id", Spec{}, time.Now())
	select {
	case <-j.Cancelled():
		t.Fatal("cancelled before request")
	default:
	}
	if !j.RequestCancel(ReasonUser) {
		t.Fatal("first request must return true")
	}
	if j.RequestCancel(ReasonShutdown) {
		t.Fatal("second request must return false")
	}
	<-j.Cancelled()
	if r := j.CancelReason(); r != ReasonUser {
		t.Fatalf("reason = %q", r)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	spec := Spec{Command: []string{"a", "b"}, Env: map[string]string{"K": "V"}}
	j := New("id", spec, time.Now())
	s := j.Snapshot()
	s.Spec.Command[0] = "x"
	s.Spec.Env["K"] = "changed"
	if j.Spec.Command[0] != "a" || j.Spec.Env["K"] != "V" {
		t.Fatal("snapshot shares memory with the job")
	}
}

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidID(id) {
			t.Fatalf("generated id %q is not canonical", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{
		"", "..", "../etc", "123e4567-e89b-12d3-a456-426614174000", // v1, not v4
		"123E4567-E89B-42D3-A456-426614174000", // uppercase
		"123e4567-e89b-42d3-a456-42661417400",  // short
		"123e4567-e89b-42d3-a456-426614174000/", "123e4567-e89b-42d3-a456-4266141740000",
	} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func BenchmarkTransition(b *testing.B) {
	now := time.Now()
	for i := 0; i < b.N; i++ {
		j := New("id", Spec{}, now)
		_ = j.Transition(StateStarting, now)
		_ = j.Transition(StateRunning, now)
		_ = j.Finish(StateSucceeded, Outcome{Exited: true}, now)
	}
}
