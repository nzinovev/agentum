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
	"strings"
	"sync/atomic"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// The worktree reconcile and discard endpoints: the human actions over a tree
// the runner refused to decide on itself. The pure tests pin the discard body
// contract; the Postgres-backed tests pin the delivery — the reconcile
// decision lands in the queued job's payload with the run resumed, and the
// discard endpoint refuses a live run before anything is enqueued.

func TestParseDiscardWorktreeBody_Contract(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("c", 40)
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid body", `{"expected_head":"` + head + `","discard_uncommitted":true}`, false},
		{"valid without flag", `{"expected_head":"` + head + `"}`, false},
		{"valid unreadable tree", `{"discard_unreadable":true,"discard_uncommitted":true}`, false},
		{"unreadable without loss confirmation", `{"discard_unreadable":true}`, true},
		{"unreadable with expected head", `{"discard_unreadable":true,"discard_uncommitted":true,"expected_head":"` + head + `"}`, true},
		{"empty body", "", true},
		{"json null", "null", true},
		{"broken json", `{"expected_head":`, true},
		{"unknown field", `{"expected_head":"` + head + `","oops":true}`, true},
		{"short head", `{"expected_head":"abc"}`, true},
		{"non-string head", `{"expected_head":12345}`, true},
		{"second json object", `{"expected_head":"` + head + `"}{}`, true},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			request, err := parseDiscardWorktreeBody([]byte(testCase.body))
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("parseDiscardWorktreeBody(%q) accepted a malformed body", testCase.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDiscardWorktreeBody(%q): %v", testCase.body, err)
			}
			if !request.DiscardUnreadable && request.ExpectedHead != strings.Repeat("c", 40) {
				t.Errorf("expected_head = %q", request.ExpectedHead)
			}
		})
	}
}

// TestWorktreeDiscardEndpoint_UnreadableTree: a terminal run can enqueue a
// separately confirmed removal when its worktree HEAD cannot be read.
func TestWorktreeDiscardEndpoint_UnreadableTree(t *testing.T) {
	harness := newWorktreeHarness(t)
	runID := harness.insertWorktreeFixtureRun(t, "done")
	record, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	project, err := harness.queries.GetProject(t.Context(), sqlc.GetProjectParams{ID: record.ProjectID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	baseCommit, err := worktree.New().ResolveRef(t.Context(), project.RepoPath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.SetBaseCommit(t.Context(), sqlc.SetBaseCommitParams{
		ID: runID, TenantID: continueTestTenant, BaseCommit: sql.NullString{String: baseCommit, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	gitLink := filepath.Join(worktree.PathFor(project.RepoPath, runID), ".git")
	if err := os.WriteFile(gitLink, []byte("gitdir: /missing/metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	response := harness.callWorktreeAction(t, "discard", runID,
		`{"discard_unreadable":true,"discard_uncommitted":true}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("unreadable discard status = %d, body %s", response.Code, response.Body.String())
	}
	readRequest := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID, nil)
	readRequest.SetPathValue("id", runID)
	readRequest = readRequest.WithContext(authz.WithPrincipal(readRequest.Context(), authz.Principal{
		TenantID: continueTestTenant, UserID: continueTestUser,
	}))
	readResponse := httptest.NewRecorder()
	harness.api.handleGetRun(readResponse, readRequest)
	if readResponse.Code != http.StatusOK {
		t.Fatalf("GET run during removal: %d %s", readResponse.Code, readResponse.Body.String())
	}
	var readState runResponse
	if err := json.Unmarshal(readResponse.Body.Bytes(), &readState); err != nil {
		t.Fatal(err)
	}
	if readState.Worktree == nil || readState.Worktree.State != "removing" || readState.Worktree.LastError != nil {
		t.Fatalf("unreadable removal state = %+v", readState.Worktree)
	}
}

// worktreeHarness bundles one database and one API for the endpoint tests.
type worktreeHarness struct {
	api     *API
	queries *sqlc.Queries
	db      *sql.DB
}

func newWorktreeHarness(t *testing.T) *worktreeHarness {
	t.Helper()
	handle := dbtest.Store(t)
	return &worktreeHarness{
		api: New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
			WithManifestService(manifest.New(manifest.Deps{DB: handle.Store.DB, Queries: handle.Queries}))),
		queries: handle.Queries,
		db:      handle.Store.DB,
	}
}

// worktreeFixtureSeq makes each fixture's repo identity unique per call, so
// one test inserting several runs never collides on idx_projects_identity.
var worktreeFixtureSeq atomic.Int64

// insertWorktreeFixtureRun creates a project + run in the given state, with
// one finished spec invocation, and initializes the manifest.
func (harness *worktreeHarness) insertWorktreeFixtureRun(t *testing.T, state string) string {
	t.Helper()
	identity := fmt.Sprintf("%s-%d", t.Name(), worktreeFixtureSeq.Add(1))
	repoPath := t.TempDir()
	initRegistrationRepo(t, repoPath, identity)
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', $5, 'worktree fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state, current_stage, stop_reason)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture', 'fixture', '{}', 'main', $4, 'spec',
		       CASE WHEN $4 = 'paused_user_stop' THEN 'worktree_uncommitted_changes' ELSE '' END
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		continueTestTenant, continueTestUser, identity, state, repoPath).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}
	if _, err := worktree.New().Create(t.Context(), repoPath, runID, ""); err != nil {
		t.Fatalf("create worktree fixture: %v", err)
	}
	if err := harness.api.mfst.Init(t.Context(), continueTestTenant, continueTestUser, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.CreateStageInvocation(t.Context(), sqlc.CreateStageInvocationParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Stage: "spec", Sequence: 1, SessionID: sql.NullString{String: "sess-fixture-1", Valid: true}, Cycle: 0,
	}); err != nil {
		t.Fatalf("insert stage invocation: %v", err)
	}
	return runID
}

func (harness *worktreeHarness) headFor(t *testing.T, runID string) string {
	t.Helper()
	record, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	project, err := harness.queries.GetProject(t.Context(), sqlc.GetProjectParams{ID: record.ProjectID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	head, err := worktree.New().HeadCommit(t.Context(), worktree.PathFor(project.RepoPath, runID))
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// callWorktreeAction dispatches one of the worktree handlers directly with
// the principal attached and the {id} path value set.
func (harness *worktreeHarness) callWorktreeAction(t *testing.T, action, runID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/worktree/"+action, strings.NewReader(body))
	request.SetPathValue("id", runID)
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: continueTestTenant, UserID: continueTestUser,
	}))
	recorder := httptest.NewRecorder()
	switch action {
	case "reconcile":
		harness.api.handleWorktreeReconcile(recorder, request)
	case "discard":
		harness.api.handleWorktreeDiscard(recorder, request)
	default:
		t.Fatalf("unknown worktree action %q", action)
	}
	return recorder
}

// TestWorktreeReconcileEndpoint_DeliversDecisionToJob pins the delivery: a
// valid decision on a paused_user_stop run transitions to running, stores the
// canonical payload on the reconcile job, and records the human decision.
func TestWorktreeReconcileEndpoint_DeliversDecisionToJob(t *testing.T) {
	t.Parallel()
	harness := newWorktreeHarness(t)
	runID := harness.insertWorktreeFixtureRun(t, "paused_user_stop")

	head := harness.headFor(t, runID)
	stale := harness.callWorktreeAction(t, "reconcile", runID,
		`{"mode":"keep_as_checkpoint","expected_head":"`+strings.Repeat("d", 40)+`"}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale HEAD status = %d, body %s", stale.Code, stale.Body.String())
	}
	recorder := harness.callWorktreeAction(t, "reconcile", runID,
		`{"mode":"keep_as_checkpoint","expected_head":"`+head+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconcile status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	updated, runErr := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if updated.State != "running" {
		t.Fatalf("run state = %q, want running", updated.State)
	}
	const readJob = `
		SELECT payload FROM jobs
		WHERE run_id = $1 AND tenant_id = $2 AND kind = 'reconcile'
		ORDER BY id DESC LIMIT 1`
	var payload []byte
	if err := harness.db.QueryRowContext(t.Context(), readJob, runID, continueTestTenant).Scan(&payload); err != nil {
		t.Fatalf("no reconcile job enqueued: %v", err)
	}
	var decoded struct {
		Mode         string `json:"mode"`
		ExpectedHead string `json:"expected_head"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("reconcile payload is not the canonical decision: %v", err)
	}
	if decoded.Mode != "keep_as_checkpoint" || decoded.ExpectedHead != head {
		t.Fatalf("payload = %+v, want the decision the human made", decoded)
	}
}

// TestWorktreeReconcileEndpoint_RefusesWrongStateAndBody pins the refusals:
// a run that is not at the user-stop pause is a 409, and a malformed or
// unconfirmed decision is a 400 — both without a job.
func TestWorktreeReconcileEndpoint_RefusesWrongStateAndBody(t *testing.T) {
	t.Parallel()
	harness := newWorktreeHarness(t)
	head := strings.Repeat("e", 40)

	runningRun := harness.insertWorktreeFixtureRun(t, "running")
	recorder := harness.callWorktreeAction(t, "reconcile", runningRun,
		`{"mode":"resume_session","expected_head":"`+head+`"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("state refusal status = %d, want 409", recorder.Code)
	}

	pausedRun := harness.insertWorktreeFixtureRun(t, "paused_user_stop")
	recorder = harness.callWorktreeAction(t, "reconcile", pausedRun, `{"mode":"nope","expected_head":"`+head+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mode refusal status = %d, want 400", recorder.Code)
	}
	recorder = harness.callWorktreeAction(t, "reconcile", pausedRun,
		`{"mode":"discard_to_checkpoint","expected_head":"`+head+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed discard status = %d, want 400", recorder.Code)
	}
	const countJobs = `SELECT count(*) FROM jobs WHERE run_id = $1 AND tenant_id = $2`
	var jobCount int
	if err := harness.db.QueryRowContext(t.Context(), countJobs, pausedRun, continueTestTenant).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 {
		t.Fatalf("refused requests enqueued %d job(s)", jobCount)
	}
}

// TestWorktreeDiscardEndpoint_Guards pins the eligibility contract: a running
// run is refused with 409 and no job; a paused run with no jobs is accepted
// (202) with the confirmed payload enqueued.
func TestWorktreeDiscardEndpoint_Guards(t *testing.T) {
	t.Parallel()
	harness := newWorktreeHarness(t)
	head := strings.Repeat("f", 40)

	runningRun := harness.insertWorktreeFixtureRun(t, "running")
	recorder := harness.callWorktreeAction(t, "discard", runningRun,
		`{"expected_head":"`+head+`","discard_uncommitted":true}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("live-run refusal status = %d, want 409", recorder.Code)
	}

	pausedRun := harness.insertWorktreeFixtureRun(t, "paused_user_stop")
	head = harness.headFor(t, pausedRun)
	recorder = harness.callWorktreeAction(t, "discard", pausedRun,
		`{"expected_head":"`+head+`","discard_uncommitted":true}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("discard status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	const readJob = `
		SELECT payload FROM jobs
		WHERE run_id = $1 AND tenant_id = $2 AND kind = 'discard_worktree'
		ORDER BY id DESC LIMIT 1`
	var payload []byte
	if err := harness.db.QueryRowContext(t.Context(), readJob, pausedRun, continueTestTenant).Scan(&payload); err != nil {
		t.Fatalf("no discard job enqueued: %v", err)
	}
	var decoded struct {
		ExpectedHead       string `json:"expected_head"`
		DiscardUncommitted bool   `json:"discard_uncommitted"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("discard payload malformed: %v", err)
	}
	if decoded.ExpectedHead != head || !decoded.DiscardUncommitted {
		t.Fatalf("payload = %+v, want the confirmed request", decoded)
	}
}
