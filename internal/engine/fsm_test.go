package engine

import (
	"errors"
	"slices"
	"testing"
)

// legalTransitions enumerates every (from, event) -> to that the FSM must
// accept. It mirrors the transitions map in fsm.go; this is the contract the
// runner and API depend on. If the map changes, this table changes in lockstep.
func TestNext_LegalTransitions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from  TaskState
		event TaskEvent
		want  TaskState
	}{
		{StateCreated, EventStart, StateRunning},
		{StateCreated, EventCancel, StateCancelled},

		{StateRunning, EventStopOpenQ, StatePausedOpenQuestions},
		{StateRunning, EventStopGate, StatePausedGate},
		{StateRunning, EventStopUser, StatePausedUserStop},
		{StateRunning, EventReachFinalGate, StateAwaitingMemoryCommit},
		{StateRunning, EventFail, StateFailed},
		{StateRunning, EventCancel, StateCancelled},

		{StatePausedOpenQuestions, EventContinue, StateRunning},
		{StatePausedOpenQuestions, EventCancel, StateCancelled},

		{StatePausedUserStop, EventContinue, StateRunning},
		{StatePausedUserStop, EventCancel, StateCancelled},

		{StatePausedGate, EventAdvance, StateRunning},
		{StatePausedGate, EventCancel, StateCancelled},

		{StateAwaitingMemoryCommit, EventApprove, StateDone},
		{StateAwaitingMemoryCommit, EventCancel, StateCancelled},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"--"+string(tc.event)+"-->", func(t *testing.T) {
			t.Parallel()
			got, err := Next(tc.from, tc.event)
			if err != nil {
				t.Fatalf("Next(%q, %q) unexpected error: %v", tc.from, tc.event, err)
			}
			if got != tc.want {
				t.Fatalf("Next(%q, %q) = %q, want %q", tc.from, tc.event, got, tc.want)
			}
		})
	}
}

// TestNext_RejectsIllegal exercises the contract the API relies on: a second
// EventStart on an already-running task must fail and surface as a 409, and a
// transition out of a terminal state must fail too. Both must be the typed
// ErrIllegalTransition so the HTTP layer can errors.As it.
func TestNext_RejectsIllegal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		from  TaskState
		event TaskEvent
	}{
		{"start on running", StateRunning, EventStart},
		{"start on terminal done", StateDone, EventStart},
		{"approve on running (must reach final gate first)", StateRunning, EventApprove},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Next(tc.from, tc.event)
			if err == nil {
				t.Fatalf("Next(%q, %q) expected illegal, got nil", tc.from, tc.event)
			}
			var ill *ErrIllegalTransition
			if !errors.As(err, &ill) {
				t.Fatalf("error must be *ErrIllegalTransition, got %T (%v)", err, err)
			}
			if ill.From != tc.from || ill.Event != tc.event {
				t.Fatalf("ErrIllegalTransition fields = (%q,%q), want (%q,%q)", ill.From, ill.Event, tc.from, tc.event)
			}
		})
	}
}

// TestNext_NoSpuriousEdges walks every state against every event and asserts
// that the accepted set exactly matches the transitions map — no more, no fewer.
// This catches both missing rejections and accidentally-added edges.
func TestNext_NoSpuriousEdges(t *testing.T) {
	t.Parallel()
	allStates := []TaskState{
		StateCreated, StateRunning,
		StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop,
		StateAwaitingMemoryCommit,
		StateDone, StateFailed, StateCancelled,
	}
	allEvents := []TaskEvent{
		EventStart, EventStopOpenQ, EventStopGate, EventStopUser,
		EventContinue, EventAdvance, EventReachFinalGate,
		EventApprove, EventFail, EventCancel,
	}
	for _, s := range allStates {
		legalForState := map[TaskEvent]bool{}
		for ev := range transitions[s] {
			legalForState[ev] = true
		}
		for _, ev := range allEvents {
			_, err := Next(s, ev)
			legal := legalForState[ev]
			switch {
			case legal && err != nil:
				t.Errorf("Next(%q, %q) expected legal, got error: %v", s, ev, err)
			case !legal && err == nil:
				t.Errorf("Next(%q, %q) expected illegal, got nil", s, ev)
			}
		}
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	terminal := []TaskState{StateDone, StateFailed, StateCancelled}
	nonTerminal := []TaskState{
		StateCreated, StateRunning,
		StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop,
		StateAwaitingMemoryCommit,
	}
	for _, s := range terminal {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false, want true", s)
		}
		if _, ok := transitions[s]; ok && len(transitions[s]) > 0 {
			t.Errorf("terminal state %q must have no outgoing transitions", s)
		}
	}
	for _, s := range nonTerminal {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true, want false", s)
		}
	}
	if len(terminal) != 3 || len(nonTerminal) != 6 {
		// guard against silently dropping a state when the enum grows
		t.Logf("state set changed: terminal=%d nonTerminal=%d — update this test", len(terminal), len(nonTerminal))
	}
}

func TestIsPaused(t *testing.T) {
	t.Parallel()
	paused := []TaskState{StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop}
	for _, s := range paused {
		if !IsPaused(s) {
			t.Errorf("IsPaused(%q) = false, want true", s)
		}
	}
	all := []TaskState{
		StateCreated, StateRunning,
		StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop,
		StateAwaitingMemoryCommit,
		StateDone, StateFailed, StateCancelled,
	}
	for _, s := range all {
		want := slices.Contains(paused, s)
		if got := IsPaused(s); got != want {
			t.Errorf("IsPaused(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestErrIllegalTransition_Message(t *testing.T) {
	t.Parallel()
	e := &ErrIllegalTransition{From: StateRunning, Event: EventStart}
	want := "engine: illegal transition running --start-->"
	if e.Error() != want {
		t.Fatalf("Error() = %q, want %q", e.Error(), want)
	}
}
