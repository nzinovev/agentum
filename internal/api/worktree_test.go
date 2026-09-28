package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
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
			if request.ExpectedHead != strings.Repeat("c", 40) {
				t.Errorf("expected_head = %q", request.ExpectedHead)
			}
		})
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
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/worktree-fixture-' || $3, 'worktree fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state, current_stage)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture', 'fixture', '{}', 'main', $4, 'spec'
		FROM inserted_project
		RETURNING id`
	var runID string
	identity := fmt.Sprintf("%s-%d", t.Name(), worktreeFixtureSeq.Add(1))
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		continueTestTenant, continueTestUser, identity, state).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
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

	head := strings.Repeat("d", 40)
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
