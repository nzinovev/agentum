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
		name     string
		in       StageInput
		wantAct  Action
		wantEvt  engine.TaskEvent // empty for ActionAdvance
		wantRS   string           // stop_reason
		wantNext string           // next stage (ActionAdvance)
		wantErr  bool
	}{
		{
			name: "blocked with open questions → pause for answers (resume)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusBlocked, OpenQuestions: []string{"which DB?"}},
				Stage:  nextStage("impl"),
			},
			wantAct: ActionPause, wantEvt: engine.EventStopOpenQ, wantRS: "open_questions",
		},
		{
			name: "blocked without open questions → gate review (strict default)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusBlocked},
				Stage:  nextStage("impl"),
			},
			wantAct: ActionPause, wantEvt: engine.EventStopGate, wantRS: "gate",
		},
		{
			name: "complete + auto gate → advance to next stage",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  nextStage("impl"),
			},
			wantAct: ActionAdvance, wantNext: "impl",
		},
		{
			name: "complete + auto_if_clean + clean → advance",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoIfClean, Transitions: []pack.Transition{{To: "review"}}},
				Clean:  true,
			},
			wantAct: ActionAdvance, wantNext: "review",
		},
		{
			name: "complete + auto_if_clean + dirty → gate review",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoIfClean, Transitions: []pack.Transition{{To: "review"}}},
				Clean:  false,
			},
			wantAct: ActionPause, wantEvt: engine.EventStopGate, wantRS: "gate",
		},
		{
			name: "complete + human_approval → gate review",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateHumanApproval, Transitions: []pack.Transition{{To: "review"}}},
			},
			wantAct: ActionPause, wantEvt: engine.EventStopGate, wantRS: "gate",
		},
		{
			name: "complete + auto_on_approval → gate review (advances on explicit continue)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAutoOnApproval, Transitions: []pack.Transition{{To: "review"}}},
			},
			wantAct: ActionPause, wantEvt: engine.EventStopGate, wantRS: "gate",
		},
		{
			name: "complete + terminal stage → reach final gate",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  terminalStage(pack.GateHumanFinal),
			},
			wantAct: ActionFinal, wantEvt: engine.EventReachFinalGate,
		},
		{
			name: "partial + terminal stage → reach final gate (partial still finishes)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusPartial},
				Stage:  terminalStage(pack.GateHumanFinal),
			},
			wantAct: ActionFinal, wantEvt: engine.EventReachFinalGate,
		},
		{
			name: "partial + auto gate → advance (operator chose auto; respect it)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusPartial},
				Stage:  nextStage("review"),
			},
			wantAct: ActionAdvance, wantNext: "review",
		},
		{
			name: "adapter error → retryable pause (paused_user_stop shape, §5.3)",
			in: StageInput{
				Result:       &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:        nextStage("impl"),
				AdapterError: true,
			},
			wantAct: ActionPause, wantEvt: engine.EventStopUser, wantRS: "adapter_error",
		},
		{
			name: "parse error → retryable pause",
			in: StageInput{
				Result:     &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:      nextStage("impl"),
				ParseError: true,
			},
			wantAct: ActionPause, wantEvt: engine.EventStopUser, wantRS: "parse_error",
		},
		{
			name: "adapter error takes precedence over a complete result",
			in: StageInput{
				Result:       &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:        nextStage("impl"),
				AdapterError: true, ParseError: true,
			},
			wantAct: ActionPause, wantEvt: engine.EventStopUser, wantRS: "adapter_error",
		},
		{
			name:    "no result and no error → programming error",
			in:      StageInput{Stage: nextStage("impl")},
			wantErr: true,
		},
		{
			name: "advance on a stage with no transitions → error (guard against bad pack)",
			in: StageInput{
				Result: &agent.ResultJSON{Status: agent.StatusComplete},
				Stage:  pack.Stage{Gate: pack.GateAuto}, // terminal, but auto gate
			},
			// Terminal + complete returns ActionFinal first, so this shouldn't
			// reach advance(). Construct the case directly via a non-terminal
			// marker: covered by the explicit advance() unit test below.
			wantAct: ActionFinal, wantEvt: engine.EventReachFinalGate,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Evaluate(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Evaluate err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Action != tc.wantAct {
				t.Errorf("Action = %v, want %v", got.Action, tc.wantAct)
			}
			if got.FSMEvent != tc.wantEvt {
				t.Errorf("FSMEvent = %q, want %q", got.FSMEvent, tc.wantEvt)
			}
			if got.StopReason != tc.wantRS {
				t.Errorf("StopReason = %q, want %q", got.StopReason, tc.wantRS)
			}
			if got.NextStage != tc.wantNext {
				t.Errorf("NextStage = %q, want %q", got.NextStage, tc.wantNext)
			}
		})
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
	for _, c := range cases {
		d, _ := Evaluate(c)
		if d.StopReason != "" {
			want[d.StopReason] = true
		}
	}
	for rs, seen := range want {
		if !seen {
			t.Errorf("stop_reason %q was never produced by the evaluator", rs)
		}
	}
}
