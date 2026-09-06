package api

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
)

// TestHumanDecisionPatch_MapsEachActionToItsGateAndDecision pins the
// humanDecisionPatch mapping the four lifecycle actions rely on. The patch is
// a pure function over (stage, gate, decision, user, timestamp), so the
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
		user     string
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
			patch := humanDecisionPatch(testCase.stage, testCase.gate, testCase.decision, testCase.user, at)
			if len(patch.GateDecisions) != 1 {
				t.Fatalf("patch has %d decisions, want 1", len(patch.GateDecisions))
			}
			decision := patch.GateDecisions[0]
			if decision.Stage != testCase.stage {
				t.Errorf("Stage = %q, want %q", decision.Stage, testCase.stage)
			}
			if decision.Gate != testCase.gate {
				t.Errorf("Gate = %q, want %q", decision.Gate, testCase.gate)
			}
			if decision.Decision != testCase.decision {
				t.Errorf("Decision = %q, want %q", decision.Decision, testCase.decision)
			}
			// Every lifecycle handler writes a HUMAN decision: actor is the
			// vocabulary value, and the user id lands in its own field — a
			// human's name must never masquerade as the actor kind, nor an
			// orchestrator action as the human's.
			if decision.Actor != "human" {
				t.Errorf("Actor = %q, want the vocabulary value \"human\"", decision.Actor)
			}
			if decision.UserID != testCase.user {
				t.Errorf("UserID = %q, want %q", decision.UserID, testCase.user)
			}
			if !decision.Timestamp.Equal(at) {
				t.Errorf("Timestamp = %v, want %v", decision.Timestamp, at)
			}
		})
	}
}

// TestHumanDecisionPatch_OnlyCarriesGateDecisions guards that the patch does
// not accidentally set other manifest sections — the caller merges it into a
// body that already holds stage evidence, and a stray section would be
// clobbering.
func TestHumanDecisionPatch_OnlyCarriesGateDecisions(t *testing.T) {
	t.Parallel()
	patch := humanDecisionPatch("impl", gateFinal, decisionApproved, "alice", time.Now().UTC())
	if patch.Input != nil || patch.Prompts != nil || patch.Artifacts != nil || patch.Model != nil {
		t.Errorf("patch carries sections beyond GateDecisions: %+v", patch)
	}
	if patch.GateDecisions == nil {
		t.Error("GateDecisions is nil")
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

// TestRecordHumanDecisionTx_NilManifestServiceIsNoOp keeps the cancel-path
// tolerance honest at the boundary: a nil manifest service (unit tests, or a
// server that did not wire one) records no decision and returns nil under both
// policies, so the lifecycle action still proceeds. The sealed/missing-manifest
// absorption under recordLenient is the same idea applied to a service that
// exists but refuses; exercising it requires a DB, so the policy logic itself
// (errors.Is ErrSealed/ErrNoManifest under lenient) is review-only here.
func TestRecordHumanDecisionTx_NilManifestServiceIsNoOp(t *testing.T) {
	t.Parallel()
	patch := humanDecisionPatch("impl", gateCancel, decisionRejected, "alice", time.Now().UTC())
	apiInst := newEditAPI(nil) // mfst is nil
	for _, policy := range []recordPolicy{recordLenient, recordStrict} {
		if err := apiInst.recordHumanDecisionTx(context.Background(), nil, authzPrincipal(), "task-1", patch, policy); err != nil {
			t.Errorf("nil manifest service under policy %v returned %v; it must be a no-op", policy, err)
		}
	}
}

func authzPrincipal() authz.Principal {
	return authz.Principal{TenantID: "tenant-1", UserID: "user-1"}
}
