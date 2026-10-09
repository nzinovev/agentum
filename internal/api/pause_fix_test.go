package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

func actionRequest(method, path, runID, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.SetPathValue("id", runID)
	return request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: continueTestTenant, UserID: continueTestUser,
	}))
}

// TestPauseRunPersistsPendingRequest: repeated requests keep one timestamp and
// a completed stop clears the flag before the run is continued.
func TestPauseRunPersistsPendingRequest(t *testing.T) {
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, true)
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: runID, TenantID: continueTestTenant, State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	request := actionRequest(http.MethodPost, "/api/v1/runs/"+runID+"/pause", runID, "")
	first := httptest.NewRecorder()
	harness.api.handlePauseRun(first, request)
	if first.Code != http.StatusOK {
		t.Fatalf("pause status = %d: %s", first.Code, first.Body.String())
	}
	stopped, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || !stopped.PauseRequestedAt.Valid {
		t.Fatalf("pause request missing: %v", err)
	}
	second := httptest.NewRecorder()
	harness.api.handlePauseRun(second, request)
	if second.Code != http.StatusOK {
		t.Fatalf("repeated pause status = %d", second.Code)
	}
	repeated, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || !repeated.PauseRequestedAt.Time.Equal(stopped.PauseRequestedAt.Time) {
		t.Fatalf("request timestamp changed: %v", err)
	}
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: runID, TenantID: continueTestTenant, State: "paused_user_stop", StopReason: "user_pause",
	}); err != nil {
		t.Fatal(err)
	}
	paused, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || paused.PauseRequestedAt.Valid {
		t.Fatalf("pending pause survived stop: %v", err)
	}
	refused := httptest.NewRecorder()
	harness.api.handlePauseRun(refused, request)
	if refused.Code != http.StatusConflict {
		t.Fatalf("pause after stop status = %d", refused.Code)
	}
}

// TestEditedApprovedPlanCanBeReapproved: an edit at a user stop rewinds to the
// plan gate and advance replaces the approval revision before work resumes.
func TestEditedApprovedPlanCanBeReapproved(t *testing.T) {
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	original, err := harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan/plan.md", Kind: "plan_md", Bytes: []byte("Original plan"), Actor: artifacts.ActorAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.CreateApproval(t.Context(), sqlc.CreateApprovalParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan", Decision: "approved", ArtifactRevisionID: sql.NullString{String: original.ID, Valid: true}, Actor: string(authz.ActorHuman),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.UpdateRunStage(t.Context(), sqlc.UpdateRunStageParams{
		ID: runID, TenantID: continueTestTenant, CurrentStage: sql.NullString{String: "implement", Valid: true},
		State: "paused_user_stop", StopReason: "user_pause",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.db.ExecContext(t.Context(),
		`UPDATE runs SET active_fix_request_revision_id = $1 WHERE id = $2 AND tenant_id = $3`,
		original.ID, runID, continueTestTenant); err != nil {
		t.Fatal(err)
	}
	request := actionRequest(http.MethodPut, "/api/v1/runs/"+runID+"/invocations/inv/artifacts/plan/plan.md", runID,
		`{"content":"Revised plan","expected_revision_id":"`+original.ID+`"}`)
	request.SetPathValue("name", "plan/plan.md")
	response := httptest.NewRecorder()
	harness.api.handleArtifactPut(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", response.Code, response.Body.String())
	}
	current, err := harness.art.Current(t.Context(), continueTestTenant, runID, "plan/plan.md")
	if err != nil {
		t.Fatal(err)
	}
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || run.State != "paused_gate" || run.StopReason != "plan_revision_drift" || run.CurrentStage.String != "plan" || run.ActiveFixRequestRevisionID.Valid {
		t.Fatalf("reopened run = %+v, err = %v", run, err)
	}
	advance := actionRequest(http.MethodPost, "/api/v1/runs/"+runID+"/invocations/inv/advance", runID,
		`{"expected_revision_id":"`+current.ID+`"}`)
	advance.SetPathValue("iid", "inv")
	approved := httptest.NewRecorder()
	harness.api.handleInvocationAdvance(approved, advance)
	if approved.Code != http.StatusOK {
		t.Fatalf("reapprove status = %d: %s", approved.Code, approved.Body.String())
	}
	approval, err := harness.queries.GetApproval(t.Context(), sqlc.GetApprovalParams{TenantID: continueTestTenant, RunID: runID, Name: "plan"})
	if err != nil || approval.ArtifactRevisionID.String != current.ID {
		t.Fatalf("approval revision = %q, err = %v", approval.ArtifactRevisionID.String, err)
	}
}

// TestEditedApprovedPlanCanBeRejected protects the decision path after a
// revised plan replaces a previously approved revision.
func TestEditedApprovedPlanCanBeRejected(t *testing.T) {
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	original, err := harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan/plan.md", Kind: "plan_md", Bytes: []byte("Original plan"), Actor: artifacts.ActorAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.CreateApproval(t.Context(), sqlc.CreateApprovalParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan", Decision: "approved", ArtifactRevisionID: sql.NullString{String: original.ID, Valid: true}, Actor: string(authz.ActorHuman),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.UpdateRunStage(t.Context(), sqlc.UpdateRunStageParams{
		ID: runID, TenantID: continueTestTenant, CurrentStage: sql.NullString{String: "implement", Valid: true},
		State: "paused_user_stop", StopReason: "user_pause",
	}); err != nil {
		t.Fatal(err)
	}
	request := actionRequest(http.MethodPut, "/api/v1/runs/"+runID+"/invocations/inv/artifacts/plan/plan.md", runID,
		`{"content":"Revised plan","expected_revision_id":"`+original.ID+`"}`)
	request.SetPathValue("name", "plan/plan.md")
	response := httptest.NewRecorder()
	harness.api.handleArtifactPut(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("edit status = %d: %s", response.Code, response.Body.String())
	}
	current, err := harness.art.Current(t.Context(), continueTestTenant, runID, "plan/plan.md")
	if err != nil {
		t.Fatal(err)
	}
	reject := actionRequest(http.MethodPost, "/api/v1/runs/"+runID+"/reject", runID, "")
	rejected := httptest.NewRecorder()
	harness.api.handleRejectRun(rejected, reject)
	if rejected.Code != http.StatusOK {
		t.Fatalf("reject status = %d: %s", rejected.Code, rejected.Body.String())
	}
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || run.State != "cancelled" {
		t.Fatalf("rejected run = %+v, err = %v", run, err)
	}
	approval, err := harness.queries.GetApproval(t.Context(), sqlc.GetApprovalParams{TenantID: continueTestTenant, RunID: runID, Name: "plan"})
	if err != nil || approval.Decision != "rejected" || approval.ArtifactRevisionID.String != current.ID {
		t.Fatalf("approval after reject = %+v, err = %v", approval, err)
	}
}

// TestPlanRevisionGateRollback keeps the earlier revision current when the
// state change in the same transaction fails.
func TestPlanRevisionGateRollback(t *testing.T) {
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	original, err := harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan/plan.md", Kind: "plan_md", Bytes: []byte("Original plan"), Actor: artifacts.ActorAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan/plan.md", Kind: "plan_md", Bytes: []byte("Uncommitted plan"), Actor: artifacts.ActorHuman,
		ExpectedCurrentRevision: original.ID,
		AfterRevision: func(_ context.Context, _ *sqlc.Queries, _ artifacts.Revision) error {
			return errors.New("gate write failed")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "gate write failed") {
		t.Fatalf("gate write error = %v", err)
	}
	current, err := harness.art.Current(t.Context(), continueTestTenant, runID, "plan/plan.md")
	if err != nil || current.ID != original.ID {
		t.Fatalf("current revision after rollback = %+v, err = %v", current, err)
	}
}

// TestFinalFixRequestQueuesFixer: a human note is stored as an artifact and
// the run keeps the prior result while a new fixer job starts.
func TestFinalFixRequestQueuesFixer(t *testing.T) {
	harness := newContinueHarness(t)
	harness.api.packs = pack.NewProjectSource(pack.NewDirSource("../../packs"), nil)
	harness.api.art = artifacts.NewSQLStore(artifacts.SQLStoreDeps{
		DB: harness.db, Queries: harness.queries, Blobs: artifacts.NewBlobStore(t.TempDir()),
	})
	runID := harness.insertPausedRun(t, true)
	const oldCommit = "0123456789abcdef0123456789abcdef01234567"
	if _, err := harness.queries.SetResultCommit(t.Context(), sqlc.SetResultCommitParams{
		ID: runID, TenantID: continueTestTenant, ResultCommit: sql.NullString{String: oldCommit, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: runID, TenantID: continueTestTenant, State: "awaiting_final_review",
	}); err != nil {
		t.Fatal(err)
	}
	request := actionRequest(http.MethodPost, "/api/v1/runs/"+runID+"/fix-request", runID,
		`{"text":"Cover the failed retry path","result_commit":"`+oldCommit+`"}`)
	response := httptest.NewRecorder()
	harness.api.handleFixRequest(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("fix status = %d: %s", response.Code, response.Body.String())
	}
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil || run.State != "running" || run.CurrentStage.String != "fix" || run.PreviousResultCommit.String != oldCommit || run.ResultCommit.Valid || !run.ActiveFixRequestRevisionID.Valid {
		t.Fatalf("fix run = %+v, err = %v", run, err)
	}
	revision, err := harness.api.art.Current(t.Context(), continueTestTenant, runID, "final/fix-request.md")
	if err != nil || revision.Actor != artifacts.ActorHuman {
		t.Fatalf("fix artifact = %+v, err = %v", revision, err)
	}
	if run.ActiveFixRequestRevisionID.String != revision.ID {
		t.Fatalf("active fix revision = %q, artifact = %q", run.ActiveFixRequestRevisionID.String, revision.ID)
	}
	_, err = harness.api.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "final/fix-request.md", Kind: "human_fix_request", Bytes: []byte("Late competing note"),
		Actor: artifacts.ActorHuman, ExpectedCurrentRevision: revision.ID,
		RequiredRunState: "awaiting_final_review",
	})
	if !errors.Is(err, artifacts.ErrRevisionConflict) {
		t.Fatalf("late fix request write error = %v, want revision conflict", err)
	}
	current, err := harness.api.art.Current(t.Context(), continueTestTenant, runID, "final/fix-request.md")
	if err != nil || current.ID != revision.ID {
		t.Fatalf("accepted fix note was replaced: %+v, err = %v", current, err)
	}
	count, err := harness.queries.CountJobsOfKindForRun(t.Context(), sqlc.CountJobsOfKindForRunParams{RunID: runID, TenantID: continueTestTenant, Kind: "fix_request"})
	if err != nil || count != 1 {
		t.Fatalf("fix jobs = %d, err = %v", count, err)
	}
}
