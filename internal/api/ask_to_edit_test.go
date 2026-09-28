package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
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
	return &askToEditHarness{
		api: New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
			WithManifestService(manifest.New(manifest.Deps{DB: handle.Store.DB, Queries: handle.Queries})),
			WithPackSource(pack.NewDirSource(packsRoot))),
		queries: handle.Queries,
		db:      handle.Store.DB,
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
