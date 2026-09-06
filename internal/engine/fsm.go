// Package engine holds the orchestrator core: the explicit run lifecycle,
// stage invocation, and (later) the pipeline runner, pack registry, and memory
// component.
//
// F.6.1 lifecycle vocabulary — these are distinct verbs, never conflated into a
// single ambiguous "cancel":
//   - pause (StatePaused*) is non-terminal and resumable via continue/advance.
//   - EventCancel is a terminal abort: the run moves to StateCancelled and the
//     worktree is torn down, but the agentum/<run-id> branch and any committed
//     recovery work survive for review. Branch deletion is NOT part of cancel.
//   - Branch deletion (worktree.DeleteBranch) is an explicit, idempotent cleanup
//     action that lives outside this FSM — it operates on already-terminal runs
//     and is audited separately. The FSM has no edge for it because it does not
//     change run state.
//
// StateDone mirrors cancel's teardown contract: RemoveWorktree only. The branch
// + result_commit remain resolvable so a human can review base_commit..result.
package engine

import "fmt"

// RunState is the explicit lifecycle of a run. Paused states double as the
// stop-point taxonomy: humans act only at these points.
type RunState string

const (
	StateCreated             RunState = "created"
	StateRunning             RunState = "running"
	StatePausedOpenQuestions RunState = "paused_open_questions"
	StatePausedGate          RunState = "paused_gate"
	StatePausedUserStop      RunState = "paused_user_stop"
	StateAwaitingFinalReview RunState = "awaiting_final_review"
	StateDone                RunState = "done"
	StateFailed              RunState = "failed"
	StateCancelled           RunState = "cancelled"
)

// RunEvent is a named input to the FSM.
type RunEvent string

const (
	EventStart          RunEvent = "start"
	EventStopOpenQ      RunEvent = "stop_open_questions"
	EventStopGate       RunEvent = "stop_gate"
	EventStopUser       RunEvent = "stop_user"
	EventContinue       RunEvent = "continue" // resume an open-questions or user-stop pause
	EventAdvance        RunEvent = "advance"  // pass a gate → next stage runs
	EventReachFinalGate RunEvent = "reach_final_gate"
	EventApprove        RunEvent = "approve" // final approval → commit memory, then done
	EventFail           RunEvent = "fail"
	EventCancel         RunEvent = "cancel"
)

// transitions is the explicit table: map[from]map[event]to. Any (state, event)
// not present is illegal and rejected. Terminal states have no outgoing edges.
var transitions = map[RunState]map[RunEvent]RunState{
	StateCreated: {
		EventStart:  StateRunning,
		EventCancel: StateCancelled,
	},
	StateRunning: {
		EventStopOpenQ:      StatePausedOpenQuestions,
		EventStopGate:       StatePausedGate,
		EventStopUser:       StatePausedUserStop,
		EventReachFinalGate: StateAwaitingFinalReview,
		EventFail:           StateFailed,
		EventCancel:         StateCancelled,
	},
	StatePausedOpenQuestions: {
		EventContinue: StateRunning, // session-id resume
		EventCancel:   StateCancelled,
	},
	StatePausedUserStop: {
		EventContinue: StateRunning, // session-id resume (non-destructive)
		EventCancel:   StateCancelled,
	},
	StatePausedGate: {
		EventAdvance: StateRunning, // next stage is a fresh invocation
		EventCancel:  StateCancelled,
	},
	StateAwaitingFinalReview: {
		EventApprove: StateDone, // memory commits at run-done
		EventCancel:  StateCancelled,
	},
}

// ErrIllegalTransition is returned when an event is not valid for the state.
type ErrIllegalTransition struct {
	From  RunState
	Event RunEvent
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("engine: illegal transition %s --%s-->", e.From, e.Event)
}

// Next returns the resulting state for (from, event), or an error if illegal.
func Next(from RunState, event RunEvent) (RunState, error) {
	if m, ok := transitions[from]; ok {
		if to, ok := m[event]; ok {
			return to, nil
		}
	}
	return "", &ErrIllegalTransition{From: from, Event: event}
}

// IsTerminal reports whether no further transitions are possible.
func IsTerminal(s RunState) bool {
	switch s {
	case StateDone, StateFailed, StateCancelled:
		return true
	}
	return false
}

// IsPaused reports whether the run is at a stop point awaiting a human.
func IsPaused(s RunState) bool {
	switch s {
	case StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop:
		return true
	}
	return false
}
