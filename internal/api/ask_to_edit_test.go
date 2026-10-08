package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"log/slog"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The ask-to-edit endpoint: the plan-gate Request changes. The Postgres-backed
// tests pin the whole delivery — the accepted remarks land in the driving
// job's payload with the run resumed, the human decision is on the record,
// and every refusal (wrong state, wrong stage, spent budget, post-approval,
// malformed body) leaves the gate untouched with no job.

// askToEditHarness is one database plus an API wired with a minimal
// backend-development pack (plan approval stage, ask_to_edit budget) read
// from a throwaway packs directory.
type askToEditHarness struct {
	api     *API
	queries *sqlc.Queries
	db      *sql.DB
	art     artifacts.Store
}

func newAskToEditHarness(t *testing.T, askToEditBudget int) *askToEditHarness {
	t.Helper()
	packsRoot := t.TempDir()
	packDir := filepath.Join(packsRoot, "backend-development")
	if err := os.MkdirAll(filepath.Join(packDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestYAML := "api: agentum/v1\n" +
		"pack:\n  name: backend-development\n  version: 0.1.0\n" +
		"budgets: {fix_cycles: 2, ask_to_edit: " + strconv.Itoa(askToEditBudget) + "}\n" +
		"tiers: {default: strong}\n" +
		"entry: plan\n" +
		"approvals:\n  - {name: plan, stage: plan, artifact: plan.md, unlocks: source_write}\n" +
		"stages:\n" +
		"  plan:\n    gate: human_approval\n    role: analyst\n    prompt: prompts/plan.md\n    transitions: [{to: implement}]\n" +
		"  implement:\n    gate: auto\n    role: implementer\n    prompt: prompts/implement.md\n    transitions: [{to: done}]\n" +
		"  done: {}\n"
	if err := os.WriteFile(filepath.Join(packDir, "manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "prompts", "plan.md"), []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "prompts", "implement.md"), []byte("implement"), 0o644); err != nil {
		t.Fatal(err)
	}
	handle := dbtest.Store(t)
	artifactStore := artifacts.NewSQLStore(artifacts.SQLStoreDeps{
		DB: handle.Store.DB, Queries: handle.Queries, Blobs: artifacts.NewBlobStore(t.TempDir()),
	})
	return &askToEditHarness{
		api: New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
			WithManifestService(manifest.New(manifest.Deps{DB: handle.Store.DB, Queries: handle.Queries})),
			WithPackCatalog(pack.NewProjectSource(pack.NewDirSource(packsRoot), nil)),
			WithArtifactStore(artifactStore)),
		queries: handle.Queries,
		db:      handle.Store.DB,
		art:     artifactStore,
	}
}

// askToEditFixtureSeq makes each fixture's repo identity unique per call, so
// one test inserting several runs never collides on idx_projects_identity.
var askToEditFixtureSeq atomic.Int64

// insertPlanGateRun creates the project + run paused at the plan gate on the
// approval stage, with one finished plan invocation, and initializes the
// manifest.
func (harness *askToEditHarness) insertPlanGateRun(t *testing.T, state string) string {
	t.Helper()
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/ask-to-edit-fixture-' || $3, 'ask to edit fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state, current_stage)
		SELECT $1, $2, inserted_project.id, 'backend-development@0.1.0',
		       'fixture', 'fixture', '{}', 'main', $4, 'plan'
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		continueTestTenant, continueTestUser, fmt.Sprintf("%s-%d", t.Name(), askToEditFixtureSeq.Add(1)), state).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}
	if err := harness.api.mfst.Init(t.Context(), continueTestTenant, continueTestUser, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.CreateStageInvocation(t.Context(), sqlc.CreateStageInvocationParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Stage: "plan", Sequence: 1, SessionID: sql.NullString{String: "sess-plan-1", Valid: true}, Cycle: 0,
	}); err != nil {
		t.Fatalf("insert stage invocation: %v", err)
	}
	return runID
}

// callAskToEdit dispatches the handler directly, the same dispatch style the
// continue tests use.
func (harness *askToEditHarness) callAskToEdit(t *testing.T, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/runs/"+runID+"/invocations/inv-fixture/ask-to-edit", strings.NewReader(body))
	request.SetPathValue("id", runID)
	request.SetPathValue("iid", "inv-fixture")
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: continueTestTenant, UserID: continueTestUser,
	}))
	recorder := httptest.NewRecorder()
	harness.api.handleInvocationAskToEdit(recorder, request)
	return recorder
}

// countAskToEditJobs reads how many ask_to_edit jobs the run was given.
func (harness *askToEditHarness) countAskToEditJobs(t *testing.T, runID string) int {
	t.Helper()
	count, err := harness.queries.CountJobsOfKindForRun(t.Context(), sqlc.CountJobsOfKindForRunParams{
		RunID: runID, TenantID: continueTestTenant, Kind: "ask_to_edit",
	})
	if err != nil {
		t.Fatal(err)
	}
	return int(count)
}

// TestAskToEditEndpoint_DeliversRemarksToJob pins the accepted path: 200, the
// run resumes, the job carries the canonical remarks payload, and the human
// decision is recorded.
func TestAskToEditEndpoint_DeliversRemarksToJob(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")

	recorder := harness.callAskToEdit(t, runID, `{"text":"Cover the rollback path and name the files you will touch."}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ask-to-edit status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	updated, runErr := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if updated.State != "running" {
		t.Fatalf("run state = %q, want running", updated.State)
	}
	if count := harness.countAskToEditJobs(t, runID); count != 1 {
		t.Fatalf("ask_to_edit jobs = %d, want 1", count)
	}
	const readJob = `
		SELECT payload FROM jobs
		WHERE run_id = $1 AND tenant_id = $2 AND kind = 'ask_to_edit'
		ORDER BY id DESC LIMIT 1`
	var payload []byte
	if err := harness.db.QueryRowContext(t.Context(), readJob, runID, continueTestTenant).Scan(&payload); err != nil {
		t.Fatalf("read job payload: %v", err)
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not the canonical continuation: %v", err)
	}
	if decoded.Text != "Cover the rollback path and name the files you will touch." {
		t.Fatalf("payload text = %q", decoded.Text)
	}
}

// TestAskToEditEndpoint_Refusals pins the refusal table: wrong state, empty
// text, a secret-shaped text, and a spent budget — each leaves no job.
func TestAskToEditEndpoint_Refusals(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 1)
	runID := harness.insertPlanGateRun(t, "paused_gate")

	// Wrong state.
	runningRun := harness.insertPlanGateRun(t, "running")
	if recorder := harness.callAskToEdit(t, runningRun, `{"text":"x"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("wrong-state status = %d, want 409", recorder.Code)
	}
	// Empty text.
	if recorder := harness.callAskToEdit(t, runID, `{"text":"   "}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty-text status = %d, want 400", recorder.Code)
	}
	// Credential-shaped text.
	if recorder := harness.callAskToEdit(t, runID,
		`{"text":"use ghp_16C7e42f292c6912E7710c838347Ae178B4a"}`); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("secret-text status = %d, want 422", recorder.Code)
	}
	if count := harness.countAskToEditJobs(t, runID); count != 0 {
		t.Fatalf("refused requests enqueued %d job(s)", count)
	}

	// Spent budget: one accepted request exhausts a budget of 1.
	if recorder := harness.callAskToEdit(t, runID, `{"text":"first remark"}`); recorder.Code != http.StatusOK {
		t.Fatalf("first request status = %d", recorder.Code)
	}
	// Return the run to the gate shape the second request sees.
	if _, stateErr := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: runID, TenantID: continueTestTenant, State: "paused_gate",
	}); stateErr != nil {
		t.Fatal(stateErr)
	}
	recorder := harness.callAskToEdit(t, runID, `{"text":"second remark"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("budget-exhausted status = %d, want 409", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), codeEditBudgetExhausted) {
		t.Fatalf("budget refusal body = %s", recorder.Body.String())
	}
	if count := harness.countAskToEditJobs(t, runID); count != 1 {
		t.Fatalf("jobs after exhaustion = %d, want the single accepted one", count)
	}
}

// TestAskToEditEndpoint_RefusesAfterApproval pins the boundary: once the plan
// approval is granted and source-write unlocked, remarks are a rework
// decision this action does not make.
func TestAskToEditEndpoint_RefusesAfterApproval(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	if _, err := harness.queries.CreateApproval(t.Context(), sqlc.CreateApprovalParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan", Decision: "approved", Actor: "human",
	}); err != nil {
		t.Fatal(err)
	}
	recorder := harness.callAskToEdit(t, runID, `{"text":"too late"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("post-approval status = %d, want 409", recorder.Code)
	}
	if count := harness.countAskToEditJobs(t, runID); count != 0 {
		t.Fatalf("post-approval request enqueued %d job(s)", count)
	}
}

// TestAskToEditEndpoint_ZeroBudgetDisablesRevision pins the pack switch: a
// pack declaring ask_to_edit: 0 refuses every revision request.
func TestAskToEditEndpoint_ZeroBudgetDisablesRevision(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 0)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	recorder := harness.callAskToEdit(t, runID, `{"text":"any remark"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("zero-budget status = %d, want 409", recorder.Code)
	}
	if count := harness.countAskToEditJobs(t, runID); count != 0 {
		t.Fatalf("zero-budget request enqueued %d job(s)", count)
	}
}

// putPlanRevision stores a plan revision under the approval's artifact name
// (plan/plan.md), as the planner's stage does, and returns its id.
func (harness *askToEditHarness) putPlanRevision(t *testing.T, runID, content, expectedCurrent string) string {
	t.Helper()
	revision, err := harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "plan/plan.md", Kind: "file", Bytes: []byte(content), Actor: artifacts.ActorSystem,
		ExpectedCurrentRevision: expectedCurrent,
	})
	if err != nil {
		t.Fatalf("put plan revision: %v", err)
	}
	return revision.ID
}

// callAdvance dispatches the advance handler directly.
func (harness *askToEditHarness) callAdvance(t *testing.T, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/runs/"+runID+"/invocations/inv-fixture/advance", strings.NewReader(body))
	request.SetPathValue("id", runID)
	request.SetPathValue("iid", "inv-fixture")
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: continueTestTenant, UserID: continueTestUser,
	}))
	recorder := httptest.NewRecorder()
	harness.api.handleInvocationAdvance(recorder, request)
	return recorder
}

// countJobsOfKind reads how many jobs of kind the run was given.
func (harness *askToEditHarness) countJobsOfKind(t *testing.T, runID, kind string) int {
	t.Helper()
	count, err := harness.queries.CountJobsOfKindForRun(t.Context(), sqlc.CountJobsOfKindForRunParams{
		RunID: runID, TenantID: continueTestTenant, Kind: kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	return int(count)
}

// requireRunState asserts the run's persisted state.
func (harness *askToEditHarness) requireRunState(t *testing.T, runID, want string) {
	t.Helper()
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != want {
		t.Fatalf("run state = %q, want %q", run.State, want)
	}
}

// TestPlanGate_AnswersBindToTheirRevision pins that every plan-gate answer
// names the revision it was given to. After Request changes produced a new
// revision, a client still showing the old one can neither approve the new
// plan unseen nor send it remarks: omitting the revision is 428, naming a
// superseded one is 409, and neither enqueues a job or records an approval.
// Only an advance naming the current revision approves — and binds to it.
func TestPlanGate_AnswersBindToTheirRevision(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	firstRevision := harness.putPlanRevision(t, runID, "plan v1", "")

	for _, refusal := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "no revision", body: `{"text":"cover rollback"}`, wantStatus: http.StatusPreconditionRequired},
		{name: "unknown revision", body: `{"text":"cover rollback","target_revision_id":"00000000-0000-0000-0000-000000000000"}`, wantStatus: http.StatusConflict},
	} {
		if recorder := harness.callAskToEdit(t, runID, refusal.body); recorder.Code != refusal.wantStatus {
			t.Fatalf("ask-to-edit with %s: status = %d, want %d (body %s)", refusal.name, recorder.Code, refusal.wantStatus, recorder.Body.String())
		}
	}
	if count := harness.countJobsOfKind(t, runID, "ask_to_edit"); count != 0 {
		t.Fatalf("refused remarks enqueued %d ask_to_edit job(s)", count)
	}
	harness.requireRunState(t, runID, "paused_gate")

	accepted := harness.callAskToEdit(t, runID, `{"text":"cover rollback","target_revision_id":"`+firstRevision+`"}`)
	if accepted.Code != http.StatusOK {
		t.Fatalf("ask-to-edit on the current revision: status = %d, body %s", accepted.Code, accepted.Body.String())
	}

	// The planner answers with a second revision and the run is back at the gate.
	secondRevision := harness.putPlanRevision(t, runID, "plan v2", firstRevision)
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: runID, TenantID: continueTestTenant, State: "paused_gate",
	}); err != nil {
		t.Fatal(err)
	}

	for _, refusal := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "no body", body: ``, wantStatus: http.StatusPreconditionRequired},
		{name: "the superseded revision", body: `{"expected_revision_id":"` + firstRevision + `"}`, wantStatus: http.StatusConflict},
		{name: "an unknown field", body: `{"revision":"` + secondRevision + `"}`, wantStatus: http.StatusBadRequest},
	} {
		if recorder := harness.callAdvance(t, runID, refusal.body); recorder.Code != refusal.wantStatus {
			t.Fatalf("advance with %s: status = %d, want %d (body %s)", refusal.name, recorder.Code, refusal.wantStatus, recorder.Body.String())
		}
	}
	if count := harness.countJobsOfKind(t, runID, "advance"); count != 0 {
		t.Fatalf("refused advances enqueued %d advance job(s)", count)
	}
	if _, err := harness.queries.GetApproval(t.Context(), sqlc.GetApprovalParams{
		TenantID: continueTestTenant, RunID: runID, Name: "plan",
	}); err == nil {
		t.Fatal("a refused advance recorded an approval")
	}
	harness.requireRunState(t, runID, "paused_gate")

	approved := harness.callAdvance(t, runID, `{"expected_revision_id":"`+secondRevision+`"}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("advance on the current revision: status = %d, body %s", approved.Code, approved.Body.String())
	}
	approval, err := harness.queries.GetApproval(t.Context(), sqlc.GetApprovalParams{
		TenantID: continueTestTenant, RunID: runID, Name: "plan",
	})
	if err != nil {
		t.Fatalf("read approval: %v", err)
	}
	if approval.ArtifactRevisionID.String != secondRevision {
		t.Fatalf("approval bound to %q, want the revision the human named %q", approval.ArtifactRevisionID.String, secondRevision)
	}
}

// TestWithinStageAdvanceRequiresRevision returns 428 for a missing
// expected_revision_id when the short plan already has a revision.
func TestWithinStageAdvanceRequiresRevision(t *testing.T) {
	harness := newAskToEditHarness(t, 3)
	packRoot := t.TempDir()
	packDir := filepath.Join(packRoot, "backend-development")
	if err := os.MkdirAll(filepath.Join(packDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestBody := `api: agentum/v1
pack:
  name: backend-development
  version: 0.1.0
memory: {reads: [project], writes: false}
capabilities: [fs.read, fs.write, git.read, git.write, exec.bash]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: strong}
entry: implement
approvals:
  - {name: short_plan, stage: implement, artifact: plan.md, unlocks: source_write, within_stage: true}
stages:
  implement:
    gate: human_approval
    role: implementer
    prompt: prompts/implement.md
    transitions: [{to: done}]
  done: {}
`
	if err := os.WriteFile(filepath.Join(packDir, "manifest.yaml"), []byte(manifestBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "prompts", "implement.md"), []byte("implement"), 0o644); err != nil {
		t.Fatal(err)
	}
	harness.api.packs = pack.NewProjectSource(pack.NewDirSource(packRoot), nil)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	if _, err := harness.queries.UpdateRunStage(t.Context(), sqlc.UpdateRunStageParams{
		ID: runID, TenantID: continueTestTenant, CurrentStage: sql.NullString{String: "implement", Valid: true}, State: "paused_gate",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.art.Put(t.Context(), artifacts.PutParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Name: "implement/plan.md", Kind: "plan_md", Bytes: []byte("short plan"), Actor: artifacts.ActorSystem,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := harness.callAdvance(t, runID, "")
	if recorder.Code != http.StatusPreconditionRequired || !strings.Contains(recorder.Body.String(), codePreconditionMissing) {
		t.Errorf("missing short-plan revision: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if count := harness.countJobsOfKind(t, runID, "advance"); count != 0 {
		t.Errorf("refused advance queued %d jobs", count)
	}
}

// TestPlanGate_RevisionWriteWaitsForGateAnswer pins the serialization a gate
// answer relies on: while a transaction holds the run row — as the answer's
// transition update does before it reads the plan revision — a plan write
// waits, even a first create that has no revision row to lock. It lands only
// after that transaction ends, never between the answer's read and commit.
func TestPlanGate_RevisionWriteWaitsForGateAnswer(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")

	gateTx, err := harness.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gateTx.Rollback() }()
	if _, err := gateTx.ExecContext(t.Context(), `UPDATE runs SET state = state WHERE id = $1`, runID); err != nil {
		t.Fatalf("hold the run row: %v", err)
	}

	written := make(chan error, 1)
	go func() {
		_, putErr := harness.art.Put(context.Background(), artifacts.PutParams{
			TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
			Name: "plan/plan.md", Kind: "file", Bytes: []byte("plan v1"), Actor: artifacts.ActorSystem,
		})
		written <- putErr
	}()
	select {
	case putErr := <-written:
		t.Fatalf("the plan write landed while the run row was held (err=%v)", putErr)
	case <-time.After(300 * time.Millisecond):
	}
	if err := gateTx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case putErr := <-written:
		if putErr != nil {
			t.Fatalf("plan write after the gate transaction: %v", putErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the plan write never completed after the gate transaction ended")
	}
}

// TestPlanGate_InTransactionPreconditionRefuses pins the in-transaction half of
// the 428: a plan revision that appeared after the handler's own read (which
// found none, so it let a request without a revision id through) is caught
// inside the answer's transaction — no job, no approval, the gate unchanged.
func TestPlanGate_InTransactionPreconditionRefuses(t *testing.T) {
	t.Parallel()
	harness := newAskToEditHarness(t, 3)
	runID := harness.insertPlanGateRun(t, "paused_gate")
	harness.putPlanRevision(t, runID, "plan written after the handler's read", "")

	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	principal := authz.Principal{TenantID: continueTestTenant, UserID: continueTestUser}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/invocations/inv-fixture/advance", nil)
	request = request.WithContext(authz.WithPrincipal(request.Context(), principal))
	approval := planApproval{name: "plan", stage: "plan", artifact: "plan.md"}
	_, resumeErr := harness.api.applyResume(request, run, engine.EventAdvance, "advance", nil,
		gateDecisionPatch(run, principal, gateAdvance, decisionApproved), approval, principal)
	var missing revisionPreconditionMissingError
	if !errors.As(resumeErr, &missing) {
		t.Fatalf("applyResume err = %v, want the in-transaction precondition refusal", resumeErr)
	}
	recorder := httptest.NewRecorder()
	statusForTransition(recorder, resumeErr)
	if recorder.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", recorder.Code)
	}
	if count := harness.countJobsOfKind(t, runID, "advance"); count != 0 {
		t.Fatalf("a refused answer enqueued %d advance job(s)", count)
	}
	if _, approvalErr := harness.queries.GetApproval(t.Context(), sqlc.GetApprovalParams{
		TenantID: continueTestTenant, RunID: runID, Name: "plan",
	}); approvalErr == nil {
		t.Fatal("a refused answer recorded an approval")
	}
	harness.requireRunState(t, runID, "paused_gate")
}
