// Package runner drives a task through its pack's stages: it composes the
// adapter (F.2), the pack loader (F.5), the worktree service and routing block
// (PR2), the models resolver (F.4), and the engine FSM (F.1) into the loop that
// makes a task actually run (04 §7.2). A job worker (internal/jobs) claims jobs
// and calls the Runner as its Handler.
package runner

import (
	"fmt"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/pack"
)

// Action is what the runner does after evaluating a stage outcome.
type Action int

const (
	// ActionAdvance: the stage auto-completed; continue to the next stage. The
	// task stays `running` — no FSM transition, only current_stage moves.
	ActionAdvance Action = iota
	// ActionPause: stop the loop and surface the FSMEvent; the task moves to a
	// paused state and awaits a human (continue/advance/cancel).
	ActionPause
	// ActionFinal: the terminal stage completed; fire reach_final_gate so the
	// task moves to awaiting_memory_commit ahead of final approval.
	ActionFinal
)

// Decision is the result of evaluating a stage's outcome against its gate
// (04 §7.4). It is the single source of truth for "what does the runner do next."
type Decision struct {
	Action Action

	// FSMEvent is fed to engine.Next. Empty for ActionAdvance (auto-advance
	// stays in `running`). Set for ActionPause and ActionFinal.
	FSMEvent engine.TaskEvent

	// StopReason is recorded on the stage_invocation row. Empty unless pausing.
	StopReason string

	// NextStage is the stage id to advance to (ActionAdvance only), taken from
	// the stage's first transition. Empty otherwise.
	NextStage string
}

// StageInput bundles everything Evaluate needs. Keeping it explicit (rather than
// passing the whole Result) makes the table-driven tests readable and pins the
// evaluator to the fields that actually drive the decision.
type StageInput struct {
	// Parsed result.json (the file-derived fields). Nil when the file was
	// missing or failed strict parsing.
	Result *agent.ResultJSON

	// The stage definition from the resolved pack, including its gate and
	// transitions. StageID is the pack key (e.g. "spec").
	Stage   pack.Stage
	StageID string

	// Clean reports whether the worktree has no changes outside the result's
	// declared edit_targets. Drives auto_if_clean.
	Clean bool

	// AdapterError is set when the adapter returned EventError (the run could
	// not produce a valid result).
	AdapterError bool

	// ParseError is set when result.json was missing or failed strict parsing.
	// Distinct from AdapterError: the agent ran and exited, but its output was
	// unusable.
	ParseError bool
}

// Evaluate maps a stage's outcome to a Decision. It is pure and table-tested.
// The mapping follows 04 §7.4; error/parse/timeout outcomes reuse the
// paused_user_stop shape (per §5.3) rather than introducing a new FSM state.
func Evaluate(input StageInput) (Decision, error) {
	// Adapter error: the run itself failed. Retryable stop-point, not task
	// failure — the user retries (fresh invocation; there is no session to
	// resume from a crashed run).
	if input.AdapterError {
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopUser, StopReason: "adapter_error"}, nil
	}
	// Parse error: the agent ran but result.json was missing or invalid. Same
	// retryable shape; the user reviews the worktree and retries.
	if input.ParseError {
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopUser, StopReason: "parse_error"}, nil
	}
	if input.Result == nil {
		// Defensive: no error flag and no result is a programming error.
		return Decision{}, fmt.Errorf("runner: evaluate called with neither result nor error")
	}

	// Blocked with open questions → pause for human answers (session-id resume).
	if input.Result.Status == "blocked" && len(input.Result.OpenQuestions) > 0 {
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopOpenQ, StopReason: "open_questions"}, nil
	}

	complete := input.Result.Status == "complete" || input.Result.Status == "partial"

	// A terminal stage (no transitions) that completes reaches the final gate,
	// regardless of its declared gate value — the awaiting_memory_commit state
	// IS the final approval.
	if complete && input.Stage.Terminal() {
		return Decision{Action: ActionFinal, FSMEvent: engine.EventReachFinalGate}, nil
	}

	if !complete {
		// Blocked without open_questions, or an unknown status: treat as needing
		// human review rather than guessing. Strict-by-default.
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopGate, StopReason: "gate"}, nil
	}

	// Complete, non-terminal: route by the stage's gate.
	switch input.Stage.Gate {
	case pack.GateAuto:
		return advance(input)

	case pack.GateAutoIfClean:
		if input.Clean {
			return advance(input)
		}
		// Not clean: the agent touched files beyond its declared edit_targets,
		// so surface for review even though the gate would otherwise auto-pass.
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopGate, StopReason: "gate"}, nil

	case pack.GateAutoOnApproval,
		pack.GateHumanApproval,
		pack.GateHumanFinal,
		pack.GateHumanEdit:
		// All human gates: stop for review. auto_on_approval advances on
		// explicit continue (the approval), same as the rest.
		return Decision{Action: ActionPause, FSMEvent: engine.EventStopGate, StopReason: "gate"}, nil

	default:
		return Decision{}, fmt.Errorf("runner: unknown gate %q on stage %q", input.Stage.Gate, input.StageID)
	}
}

// advance builds an ActionAdvance decision, reading the next stage from the
// stage's first transition. Conditions (Epic 4) are not evaluated here — the
// first transition wins at MVP.
func advance(input StageInput) (Decision, error) {
	if len(input.Stage.Transitions) == 0 {
		return Decision{}, fmt.Errorf("runner: stage %q has no transition to advance along", input.StageID)
	}
	return Decision{Action: ActionAdvance, NextStage: input.Stage.Transitions[0].To}, nil
}
