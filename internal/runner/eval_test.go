package runner

import (
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/pack"
)

// nextStage is a stage with a single unconditional transition (the MVP shape).
func nextStage(to string) pack.Stage {
	return pack.Stage{Gate: pack.GateAuto, Transitions: []pack.Transition{{To: to}}}
}

// terminalStage has no transitions.
func terminalStage(gate pack.Gate) pack.Stage { return pack.Stage{Gate: gate} }

func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      StageInput
		wantAction Action
		wantEvent  engine.TaskEvent // empty for ActionAdvance
		wantStop   string           // stop_reason
		wantNext   string           // next stage (ActionAdvance)
		wantErr    bool
	}{
		{
			name: "blocked with open questions → pause for answers (resume)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusBlocked, OpenQuestions: []string{"which DB?"}},
				Stage:  nextStage("impl"),
			},
			wantAction: ActionPause, wantEvent: engine.EventStopOpenQ, wantStop: "open_questions",
		},
		{
			name: "blocked without open questions → gate review (strict default)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusBlocked},
				Stage:  nextStage("impl"),
			},
			wantAction: ActionPause, wantEvent: engine.EventStopGate, wantStop: "gate",
		},
		{
			name: "complete + auto gate → advance to next stage",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  nextStage("impl"),
			},
			wantAction: ActionAdvance, wantNext: "impl",
		},
		{
			name: "complete + auto_if_clean + clean → advance",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoIfClean, Transitions: []pack.Transition{{To: "review"}}},
				Clean:  true,
			},
			wantAction: ActionAdvance, wantNext: "review",
		},
		{
			name: "complete + auto_if_clean + dirty → gate review",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoIfClean, Transitions: []pack.Transition{{To: "review"}}},
				Clean:  false,
			},
			wantAction: ActionPause, wantEvent: engine.EventStopGate, wantStop: "gate",
		},
		{
			name: "complete + human_approval → gate review",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateHumanApproval, Transitions: []pack.Transition{{To: "review"}}},
			},
			wantAction: ActionPause, wantEvent: engine.EventStopGate, wantStop: "gate",
		},
		{
			name: "complete + auto_on_approval → gate review (advances on explicit continue)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoOnApproval, Transitions: []pack.Transition{{To: "review"}}},
			},
			wantAction: ActionPause, wantEvent: engine.EventStopGate, wantStop: "gate",
		},
		{
			name: "complete + terminal stage → reach final gate",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  terminalStage(pack.GateHumanFinal),
			},
			wantAction: ActionFinal, wantEvent: engine.EventReachFinalGate,
		},
		{
			name: "partial + terminal stage → reach final gate (partial still finishes)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusPartial},
				Stage:  terminalStage(pack.GateHumanFinal),
			},
			wantAction: ActionFinal, wantEvent: engine.EventReachFinalGate,
		},
		{
			name: "partial + auto gate → advance (operator chose auto; respect it)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusPartial},
				Stage:  nextStage("review"),
			},
			wantAction: ActionAdvance, wantNext: "review",
		},
		{
			name: "adapter error → retryable pause (paused_user_stop shape, §5.3)",
			input: StageInput{
				Result:       &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:        nextStage("impl"),
				AdapterError: true,
			},
			wantAction: ActionPause, wantEvent: engine.EventStopUser, wantStop: "adapter_error",
		},
		{
			name: "parse error → retryable pause",
			input: StageInput{
				Result:     &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:      nextStage("impl"),
				ParseError: true,
			},
			wantAction: ActionPause, wantEvent: engine.EventStopUser, wantStop: "parse_error",
		},
		{
			name: "adapter error takes precedence over a complete result",
			input: StageInput{
				Result:       &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:        nextStage("impl"),
				AdapterError: true, ParseError: true,
			},
			wantAction: ActionPause, wantEvent: engine.EventStopUser, wantStop: "adapter_error",
		},
		{
			name:    "no result and no error → programming error",
			input:   StageInput{Stage: nextStage("impl")},
			wantErr: true,
		},
		{
			name: "advance on a stage with no transitions → error (guard against bad pack)",
			input: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAuto}, // terminal, but auto gate
			},
			// Terminal + complete returns ActionFinal first, so this shouldn't
			// reach advance().Construct the case directly via a non-terminal
			// marker: covered by the explicit advance() unit test below.
			wantAction: ActionFinal, wantEvent: engine.EventReachFinalGate,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertEvalCase(t, tc.input, tc.wantAction, tc.wantEvent, tc.wantStop, tc.wantNext, tc.wantErr)
		})
	}
}

// assertEvalCase runs one Evaluate case and checks every field of the decision.
// Lifted out of TestEvaluate so the table driver stays a flat loop and the
// per-case checks live in a function with no nesting-driven complexity.
func assertEvalCase(t *testing.T, input StageInput, wantAction Action, wantEvent engine.TaskEvent, wantStop, wantNext string, wantErr bool) {
	t.Helper()
	got, err := Evaluate(input)
	if (err != nil) != wantErr {
		t.Fatalf("Evaluate err = %v, wantErr = %v", err, wantErr)
	}
	if wantErr {
		return
	}
	if got.Action != wantAction {
		t.Errorf("Action = %v, want %v", got.Action, wantAction)
	}
	if got.FSMEvent != wantEvent {
		t.Errorf("FSMEvent = %q, want %q", got.FSMEvent, wantEvent)
	}
	if got.StopReason != wantStop {
		t.Errorf("StopReason = %q, want %q", got.StopReason, wantStop)
	}
	if got.NextStage != wantNext {
		t.Errorf("NextStage = %q, want %q", got.NextStage, wantNext)
	}
}

func TestAdvance_NoTransition(t *testing.T) {
	t.Parallel()
	// A non-terminal stage that somehow has zero transitions is a malformed pack
	// once the condition evaluator exists; at MVP advance() must fail loudly.
	_, err := advance(StageInput{StageID: "x", Stage: pack.Stage{Gate: pack.GateAuto}})
	if err == nil {
		t.Fatal("advance on a stage with no transitions must error")
	}
}

// TestSelectTransition covers the result-driven routing the fix-loop depends on.
// The review stage fans out to fix (changes_requested) and done (approved); the
// first matching condition wins, and a missing/unrecognized verdict falls back
// to the first transition so a reviewer that did not explicitly approve keeps
// the loop moving (bounded by the fix-cycle budget).
func TestSelectTransition(t *testing.T) {
	t.Parallel()
	reviewStage := pack.Stage{Gate: pack.GateAuto, Transitions: []pack.Transition{
		{To: "fix", Condition: `verdict == "changes_requested"`},
		{To: "done", Condition: `verdict == "approved"`},
	}}

	cases := []struct {
		name   string
		stage  pack.Stage
		result *agent.ResultJSON
		wantTo string
	}{
		{
			name:   "approved verdict routes to done (second edge)",
			stage:  reviewStage,
			result: &agent.ResultJSON{Status: agent.StatusComplete, Verdict: "approved"},
			wantTo: "done",
		},
		{
			name:   "changes_requested routes to fix (first edge)",
			stage:  reviewStage,
			result: &agent.ResultJSON{Status: agent.StatusComplete, Verdict: "changes_requested"},
			wantTo: "fix",
		},
		{
			name:   "missing verdict falls back to first edge (fix)",
			stage:  reviewStage,
			result: &agent.ResultJSON{Status: agent.StatusComplete},
			wantTo: "fix",
		},
		{
			name:   "unrecognized verdict falls back to first edge (fix)",
			stage:  reviewStage,
			result: &agent.ResultJSON{Status: agent.StatusComplete, Verdict: "needs_info"},
			wantTo: "fix",
		},
		{
			name:   "single unconditional transition always wins",
			stage:  pack.Stage{Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "review"}}},
			result: &agent.ResultJSON{Status: agent.StatusComplete, Verdict: "approved"},
			wantTo: "review",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := selectTransition(tc.stage, tc.result)
			if got.To != tc.wantTo {
				t.Fatalf("selectTransition.To = %q, want %q", got.To, tc.wantTo)
			}
		})
	}
}

// TestConditionMatches pins the minimal predicate grammar: empty matches
// always, `field == "value"` matches on equality, and anything else does not.
func TestConditionMatches(t *testing.T) {
	t.Parallel()
	complete := &agent.ResultJSON{Status: agent.StatusComplete, Verdict: "approved"}
	cases := []struct {
		name      string
		condition string
		result    *agent.ResultJSON
		want      bool
	}{
		{"empty matches", "", complete, true},
		{"verdict equal", `verdict == "approved"`, complete, true},
		{"verdict unequal", `verdict == "changes_requested"`, complete, false},
		{"status equal", `status == "complete"`, complete, true},
		{"status unequal", `status == "blocked"`, complete, false},
		{"single quotes", `verdict == 'approved'`, complete, true},
		{"no spaces", `verdict=="approved"`, complete, true},
		{"unknown field", `mood == "happy"`, complete, false},
		{"malformed", `verdict approved`, complete, false},
		{"nil result", `verdict == "approved"`, nil, false},
		{"empty condition nil result", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := conditionMatches(tc.condition, tc.result); got != tc.want {
				t.Fatalf("conditionMatches(%q) = %v, want %v", tc.condition, got, tc.want)
			}
		})
	}
}

func TestEvaluate_StopReasonsAreDistinct(t *testing.T) {
	t.Parallel()
	// The stop_reason enum is how the UI/API distinguishes pause causes; they must
	// not collide. Enumerate the values Evaluate can emit.
	want := map[string]bool{
		"open_questions": false, "gate": false,
		"adapter_error": false, "parse_error": false,
	}
	cases := []StageInput{
		{Result: &agent.ResultJSON{Status: agent.StatusBlocked, OpenQuestions: []string{"q"}}, Stage: nextStage("i")},
		{Result: &agent.ResultJSON{Status: agent.StatusComplete}, Stage: pack.Stage{Gate: pack.GateHumanApproval, Transitions: []pack.Transition{{To: "r"}}}},
		{Result: &agent.ResultJSON{Status: agent.StatusComplete}, Stage: nextStage("i"), AdapterError: true},
		{Result: &agent.ResultJSON{Status: agent.StatusComplete}, Stage: nextStage("i"), ParseError: true},
	}
	for _, caseInput := range cases {
		decision, _ := Evaluate(caseInput)
		if decision.StopReason != "" {
			want[decision.StopReason] = true
		}
	}
	for reason, seen := range want {
		if !seen {
			t.Errorf("stop_reason %q was never produced by the evaluator", reason)
		}
	}
}
