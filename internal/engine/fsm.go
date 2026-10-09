// Package engine holds the orchestrator core: the explicit run lifecycle,
// stage invocation, and (later) the pipeline runner, pack registry, and memory
// component.
//
// Lifecycle vocabulary keeps terminal decisions separate from local deletion:
//   - pause (StatePaused*) is non-terminal and resumable via continue/advance.
//   - EventCancel is a terminal abort. The worktree and branch stay for review.
//   - Branch deletion (worktree.DeleteBranch) is an explicit, idempotent cleanup
//     action that lives outside this FSM — it operates on already-terminal runs
//     and is audited separately. The FSM has no edge for it because it does not
//     change run state.
//
// StateDone keeps the worktree and branch until explicit deletion.
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
	EventStopUser       RunEvent = "stop_user"   // stop at a durable checkpoint after a pause request
	EventContinue       RunEvent = "continue"    // resume an open-questions or user-stop pause
	EventAdvance        RunEvent = "advance"     // pass a gate → next stage runs
	EventAskToEdit      RunEvent = "ask_to_edit" // request changes at a plan gate → the planner re-runs with the remarks
	EventPlanEdited     RunEvent = "plan_edited" // revoke source-write until the new revision is approved
	EventRequestFix     RunEvent = "request_fix" // re-enter fix from final human review
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
		EventContinue:   StateRunning,
		EventPlanEdited: StatePausedGate,
		EventCancel:     StateCancelled,
	},
	StatePausedUserStop: {
		EventContinue:   StateRunning,
		EventPlanEdited: StatePausedGate,
		EventCancel:     StateCancelled,
	},
	StatePausedGate: {
		EventPlanEdited: StatePausedGate,
		EventAdvance:    StateRunning, // next stage is a fresh invocation
		EventAskToEdit:  StateRunning, // request changes: the approval stage re-runs with the remarks
		EventCancel:     StateCancelled,
	},
	StateAwaitingFinalReview: {
		EventPlanEdited: StatePausedGate,
		EventRequestFix: StateRunning,
		EventApprove:    StateDone, // memory commits at run-done
		EventCancel:     StateCancelled,
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
