package api

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/manifest"
)

// TestHumanDecisionPatch_MapsEachActionToItsGateAndDecision pins the
// humanDecisionPatch mapping the four lifecycle actions rely on. The patch is
// a pure function over (stage, gate, decision, actor, timestamp), so the
// gate/decision contract each action records is unit-testable without a
// database or HTTP harness — the transactional wiring around it (runInTx,
// AddEvidenceTx) is review-only, not covered here, because the important
// invariant is that the right decision string and gate label land on the
// manifest for each user intent.
func TestHumanDecisionPatch_MapsEachActionToItsGateAndDecision(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		stage    string
		gate     string
		decision string
		actor    string
	}{
		{"advance passes a gate as approved", "impl", gateAdvance, decisionApproved, "alice"},
		{"approve passes final as approved", "impl", gateFinal, decisionApproved, "alice"},
		{"continue resumes open_questions as continued", "spec", gateOpenQuestions, decisionContinued, "bob"},
		{"continue resumes user_stop as continued", "spec", gateUserStop, decisionContinued, "bob"},
		{"cancel rejects via cancel gate", "impl", gateCancel, decisionRejected, "carol"},
		{"human edit records edited at human_edit gate", "spec", gateHumanEdit, decisionEdited, "alice"},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			patch := humanDecisionPatch(testCase.stage, testCase.gate, testCase.decision, testCase.actor, at)
			if len(patch.HumanGates) != 1 {
				t.Fatalf("patch has %d decisions, want 1", len(patch.HumanGates))
			}
			decision := patch.HumanGates[0]
			if decision.Stage != testCase.stage {
				t.Errorf("Stage = %q, want %q", decision.Stage, testCase.stage)
			}
			if decision.Gate != testCase.gate {
				t.Errorf("Gate = %q, want %q", decision.Gate, testCase.gate)
			}
			if decision.Decision != testCase.decision {
				t.Errorf("Decision = %q, want %q", decision.Decision, testCase.decision)
			}
			if decision.Actor != testCase.actor {
				t.Errorf("Actor = %q, want %q", decision.Actor, testCase.actor)
			}
			if !decision.Timestamp.Equal(at) {
				t.Errorf("Timestamp = %v, want %v", decision.Timestamp, at)
			}
		})
	}
}

// TestHumanDecisionPatch_OnlyCarriesHumanGates guards that the patch does not
// accidentally set other manifest sections — the caller merges it into a body
// that already holds stage evidence, and a stray section would be clobbering.
func TestHumanDecisionPatch_OnlyCarriesHumanGates(t *testing.T) {
	t.Parallel()
	patch := humanDecisionPatch("impl", gateFinal, decisionApproved, "alice", time.Now().UTC())
	if patch.Input != nil || patch.Prompts != nil || patch.Artifacts != nil || patch.Model != nil {
		t.Errorf("patch carries sections beyond HumanGates: %+v", patch)
	}
	if patch.HumanGates == nil {
		t.Error("HumanGates is nil")
	}
}

// TestCurrentStageOr_FallsBackWhenUnset keeps the Stage field's fallback honest:
// a task that has not entered a stage yet must not record an empty stage that a
// reviewer cannot place, and a set stage must pass through verbatim.
func TestCurrentStageOr_FallsBackWhenUnset(t *testing.T) {
	t.Parallel()
	if got := currentStageOr(sql.NullString{}, "pre-stage"); got != "pre-stage" {
		t.Errorf("unset stage: got %q, want fallback", got)
	}
	if got := currentStageOr(sql.NullString{String: "impl", Valid: true}, "pre-stage"); got != "impl" {
		t.Errorf("set stage: got %q, want impl", got)
	}
	// A valid-but-empty NullString is treated as unset.
	if got := currentStageOr(sql.NullString{String: "", Valid: true}, "pre-stage"); got != "pre-stage" {
		t.Errorf("empty-but-valid stage: got %q, want fallback", got)
	}
}

// TestIsHumanDecisionRecordFailure_DistinguishesRecordErrors keeps the gate
// action's fail-closed behaviour honest: a record failure must surface (the
// approval is not on the record), while a plain error must not be mistaken for
// one. The transactional wiring that produces these errors is review-only.
func TestIsHumanDecisionRecordFailure_DistinguishesRecordErrors(t *testing.T) {
	t.Parallel()
	if !isHumanDecisionRecordFailure(humanDecisionRecordError{cause: manifest.ErrSealed}) {
		t.Error("a record failure was not detected")
	}
	plainErr := errors.New("some other failure")
	if isHumanDecisionRecordFailure(plainErr) {
		t.Error("a plain error was mistaken for a record failure")
	}
}
