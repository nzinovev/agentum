package api

import (
	"database/sql"
	"testing"
	"time"

	"github.com/sqlc-dev/pqtype"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestToInvocationResponse_Mapping pins the response mapping: cycle and
// sequence surface, nullable fields populate only when valid, and timestamps
// are RFC3339Nano UTC. The cycle column is the per-attempt record that makes
// "each attempt visible separately" checkable; this test pins that it is not
// dropped.
func TestToInvocationResponse_Mapping(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	finished := time.Date(2026, 8, 5, 10, 5, 0, 0, time.UTC)
	invocation := sqlc.StageInvocation{
		ID: "inv-1", Stage: "fix", Sequence: 3, Cycle: 1,
		StopReason: sql.NullString{String: "fix_budget_exhausted", Valid: true},
		SessionID:  sql.NullString{String: "sess-1", Valid: true},
		ResumeOf:   sql.NullString{String: "inv-0", Valid: true},
		StartedAt:  started,
		FinishedAt: sql.NullTime{Time: finished, Valid: true},
	}
	resp := toInvocationResponse(invocation)
	if resp.ID != "inv-1" || resp.Stage != "fix" || resp.Sequence != 3 || resp.Cycle != 1 {
		t.Errorf("identity/sequence/cycle wrong: %+v", resp)
	}
	if resp.StopReason != "fix_budget_exhausted" {
		t.Errorf("StopReason = %q", resp.StopReason)
	}
	if resp.SessionID != "sess-1" || resp.ResumeOf != "inv-0" {
		t.Errorf("SessionID/ResumeOf wrong: %+v", resp)
	}
	if resp.StartedAt != started.UTC().Format(time.RFC3339Nano) {
		t.Errorf("StartedAt = %q", resp.StartedAt)
	}
	if resp.FinishedAt != finished.UTC().Format(time.RFC3339Nano) {
		t.Errorf("FinishedAt = %q", resp.FinishedAt)
	}
}

// TestToInvocationResponse_NullFieldsOmitted confirms nullable fields render
// empty when invalid (a fresh invocation that has not finished, with no session
// and no stop reason) so the JSON shape is stable.
func TestToInvocationResponse_NullFieldsOmitted(t *testing.T) {
	t.Parallel()
	invocation := sqlc.StageInvocation{
		ID: "inv-2", Stage: "spec", Sequence: 1, Cycle: 0,
		StartedAt: time.Now().UTC(),
		Result:    pqtype.NullRawMessage{},
	}
	resp := toInvocationResponse(invocation)
	if resp.StopReason != "" || resp.SessionID != "" || resp.ResumeOf != "" || resp.FinishedAt != "" {
		t.Errorf("null fields must render empty: %+v", resp)
	}
	if resp.Cycle != 0 {
		t.Errorf("first-entry cycle must be 0, got %d", resp.Cycle)
	}
}
