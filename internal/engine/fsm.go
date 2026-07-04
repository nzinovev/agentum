// Package engine holds the orchestrator core: the explicit task lifecycle,
// stage invocation, and (later) the pipeline runner, pack registry, and memory
// component.
package engine

import "fmt"

// TaskState is the explicit lifecycle of a task. Paused states double as the
// stop-point taxonomy: humans act only at these points.
type TaskState string

const (
	StateCreated              TaskState = "created"
	StateRunning              TaskState = "running"
	StatePausedOpenQuestions  TaskState = "paused_open_questions"
	StatePausedGate           TaskState = "paused_gate"
	StatePausedUserStop       TaskState = "paused_user_stop"
	StateAwaitingMemoryCommit TaskState = "awaiting_memory_commit"
	StateDone                 TaskState = "done"
	StateFailed               TaskState = "failed"
	StateCancelled            TaskState = "cancelled"
)

// TaskEvent is a named input to the FSM.
type TaskEvent string

const (
	EventStart          TaskEvent = "start"
	EventStopOpenQ      TaskEvent = "stop_open_questions"
	EventStopGate       TaskEvent = "stop_gate"
	EventStopUser       TaskEvent = "stop_user"
	EventContinue       TaskEvent = "continue" // resume an open-questions or user-stop pause
	EventAdvance        TaskEvent = "advance"  // pass a gate → next stage runs
	EventReachFinalGate TaskEvent = "reach_final_gate"
	EventApprove        TaskEvent = "approve" // final approval → commit memory, then done
	EventFail           TaskEvent = "fail"
	EventCancel         TaskEvent = "cancel"
)

// transitions is the explicit table: map[from]map[event]to. Any (state, event)
// not present is illegal and rejected. Terminal states have no outgoing edges.
var transitions = map[TaskState]map[TaskEvent]TaskState{
	StateCreated: {
		EventStart:  StateRunning,
		EventCancel: StateCancelled,
	},
	StateRunning: {
		EventStopOpenQ:      StatePausedOpenQuestions,
		EventStopGate:       StatePausedGate,
		EventStopUser:       StatePausedUserStop,
		EventReachFinalGate: StateAwaitingMemoryCommit,
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
	StateAwaitingMemoryCommit: {
		EventApprove: StateDone, // memory commits at task-done
		EventCancel:  StateCancelled,
	},
}

// ErrIllegalTransition is returned when an event is not valid for the state.
type ErrIllegalTransition struct {
	From  TaskState
	Event TaskEvent
}

func (e *ErrIllegalTransition) Error() string {
	return fmt.Sprintf("engine: illegal transition %s --%s-->", e.From, e.Event)
}

// Next returns the resulting state for (from, event), or an error if illegal.
func Next(from TaskState, event TaskEvent) (TaskState, error) {
	if m, ok := transitions[from]; ok {
		if to, ok := m[event]; ok {
			return to, nil
		}
	}
	return "", &ErrIllegalTransition{From: from, Event: event}
}

// IsTerminal reports whether no further transitions are possible.
func IsTerminal(s TaskState) bool {
	switch s {
	case StateDone, StateFailed, StateCancelled:
		return true
	}
	return false
}

// IsPaused reports whether the task is at a stop point awaiting a human.
func IsPaused(s TaskState) bool {
	switch s {
	case StatePausedOpenQuestions, StatePausedGate, StatePausedUserStop:
		return true
	}
	return false
}
