package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

const retryFixtureError = "initialize triage evidence: manifest: init: boom"

// insertRunInState creates the project + run fixture in the given state with
// the given error, at the spec stage, and initializes its manifest. A failed
// fixture also seals the manifest the way failRun does, so the retry path
// meets the record it meets in production.
func (harness *continueHarness) insertRunInState(t *testing.T, state, runError string) string {
	t.Helper()
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/retry-fixture-' || $3, 'retry fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state, current_stage, error)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture', 'fixture', '{}', 'main', $4, 'spec', $5
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		continueTestTenant, continueTestUser, sqlGenSuffix(t), state, runError).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}
	if err := harness.api.mfst.Init(t.Context(), continueTestTenant, continueTestUser, runID); err != nil {
		t.Fatal(err)
	}
	if state == "failed" {
		if err := harness.api.mfst.Seal(t.Context(), continueTestTenant, continueTestUser, runID, manifest.SealFailed); err != nil {
			t.Fatal(err)
		}
	}
	return runID
}

// callRunAction dispatches a run-level lifecycle handler directly with the
// principal attached, the dispatch style the continue tests use.
func (harness *continueHarness) callRunAction(t *testing.T, runID, action string, handler http.HandlerFunc, withPrincipal bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/"+action, nil)
	request.SetPathValue("id", runID)
	if withPrincipal {
		request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
			TenantID: continueTestTenant, UserID: continueTestUser,
		}))
	}
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

// enqueueFixtureJob inserts a job of the given kind and status for the run.
func (harness *continueHarness) enqueueFixtureJob(t *testing.T, runID, kind, status string) {
	t.Helper()
	if _, err := harness.db.ExecContext(t.Context(),
		`INSERT INTO jobs (tenant_id, user_id, run_id, kind, status) VALUES ($1, $2, $3, $4, $5)`,
		continueTestTenant, continueTestUser, runID, kind, status); err != nil {
		t.Fatalf("insert %s job: %v", kind, err)
	}
}

// countJobsOfKind counts the run's jobs of one kind.
func (harness *continueHarness) countJobsOfKind(t *testing.T, runID, kind string) int {
	t.Helper()
	count, err := harness.queries.CountJobsOfKindForRun(t.Context(), sqlc.CountJobsOfKindForRunParams{
		RunID: runID, TenantID: continueTestTenant, Kind: kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	return int(count)
}

// TestRetryHandler_FailedRunResumesOnReopenedManifest is the whole retry
// contract in one pass: the run is running again with its error cleared, one
// retry job carries the failure it retried past, and the failure seal is
// lifted so the human decision — and every evidence write the resumed run
// makes after it — lands on the same manifest.
func TestRetryHandler_FailedRunResumesOnReopenedManifest(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertRunInState(t, "failed", retryFixtureError)

	recorder := harness.callRunAction(t, runID, "retry", harness.api.handleRetryRun, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "running" || run.Error != "" {
		t.Errorf("run state = %q, error = %q; want running with the error cleared", run.State, run.Error)
	}
	var payload string
	if err := harness.db.QueryRowContext(t.Context(),
		`SELECT payload::text FROM jobs WHERE run_id = $1 AND kind = 'retry'`, runID).Scan(&payload); err != nil {
		t.Fatalf("read retry job: %v", err)
	}
	var decoded retryJobPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.PreviousError != retryFixtureError {
		t.Errorf("retry payload = %s (err %v), want previous_error %q", payload, err, retryFixtureError)
	}
	body, seal, _, err := harness.api.mfst.Get(t.Context(), continueTestTenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	if seal.Sealed {
		t.Errorf("manifest still sealed (%s); a retried run must keep recording", seal.Reason)
	}
	if body.EvidenceComplete != nil || len(body.Missing) != 0 {
		t.Errorf("seal-derived fields survived the reopen: complete=%v missing=%v", body.EvidenceComplete, body.Missing)
	}
	recorded := false
	for _, decision := range body.GateDecisions {
		if decision.Gate == gateRetry && decision.Decision == decisionRetried && decision.Actor == string(authz.ActorHuman) {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("gate decisions = %+v, want a human retry/retried entry", body.GateDecisions)
	}
}

// TestRetryHandler_RefusesWhatCannotResume: every refusal is a 409 that leaves
// the run where it was and enqueues nothing. A run that is not failed has
// nothing to retry; an unfinished job would act on the tree the resumed run
// takes over; a deleted branch took the checkpoints the retry resumes from;
// a manifest sealed for another reason, or already corrected, cannot be
// reopened without falsifying the record.
func TestRetryHandler_RefusesWhatCannotResume(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	cases := []struct {
		name     string
		state    string
		prepare  func(t *testing.T, harness *continueHarness, runID string)
		wantCode string
	}{
		{name: "done", state: "done", wantCode: codeIllegalTransition},
		{name: "cancelled", state: "cancelled", wantCode: codeIllegalTransition},
		{name: "running", state: "running", wantCode: codeIllegalTransition},
		{
			name: "unfinished job", state: "failed", wantCode: codeIllegalTransition,
			prepare: func(t *testing.T, harness *continueHarness, runID string) {
				harness.enqueueFixtureJob(t, runID, "discard_worktree", "pending")
			},
		},
		{
			name: "branch deleted", state: "failed", wantCode: codeIllegalTransition,
			prepare: func(t *testing.T, harness *continueHarness, runID string) {
				harness.enqueueFixtureJob(t, runID, "cleanup", "done")
			},
		},
		{
			name: "manifest corrected", state: "failed", wantCode: codeConflict,
			prepare: func(t *testing.T, harness *continueHarness, runID string) {
				if err := harness.api.mfst.Correct(t.Context(), continueTestTenant, continueTestUser, runID,
					"fixture correction", manifest.Body{}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			harness := newContinueHarness(t)
			runID := harness.insertRunInState(t, tc.state, retryFixtureError)
			if tc.prepare != nil {
				tc.prepare(t, harness, runID)
			}
			recorder := harness.callRunAction(t, runID, "retry", harness.api.handleRetryRun, true)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body: %s", recorder.Code, recorder.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body.Error.Code != tc.wantCode {
				t.Errorf("body = %s, want code %q", recorder.Body.String(), tc.wantCode)
			}
			if state := harness.runStateOf(t, runID); state != tc.state {
				t.Errorf("run state = %q after a refused retry, want %q", state, tc.state)
			}
			if count := harness.countJobsOfKind(t, runID, jobKindRetry); count != 0 {
				t.Errorf("refused retry enqueued %d retry jobs", count)
			}
		})
	}
}

// TestRetryHandler_RefusedCleanupKeepsRetryOpen: a cleanup the runner refused
// left the branch in place, so the checkpoints are still there to resume from.
func TestRetryHandler_RefusedCleanupKeepsRetryOpen(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertRunInState(t, "failed", retryFixtureError)
	harness.enqueueFixtureJob(t, runID, "cleanup", "failed")
	recorder := harness.callRunAction(t, runID, "retry", harness.api.handleRetryRun, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
}

// TestRetryHandler_GuardAnswersStandardResponses: retry enters through the
// common guard, so a missing principal and an unknown run answer the way
// every other run action does.
func TestRetryHandler_GuardAnswersStandardResponses(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertRunInState(t, "failed", retryFixtureError)
	if recorder := harness.callRunAction(t, runID, "retry", harness.api.handleRetryRun, false); recorder.Code != http.StatusUnauthorized {
		t.Errorf("no principal: status = %d, want 401", recorder.Code)
	}
	missing := "00000000-0000-0000-0000-000000000000"
	if recorder := harness.callRunAction(t, missing, "retry", harness.api.handleRetryRun, true); recorder.Code != http.StatusNotFound {
		t.Errorf("unknown run: status = %d, want 404", recorder.Code)
	}
}

// TestCancelHandler_ClosesFailedRunOnly: a failed run is the one terminal run
// cancel still accepts — it lands in cancelled with a teardown job, and the
// failure seal stays the manifest's record of how the run ended. A done run
// stays refused.
func TestCancelHandler_ClosesFailedRunOnly(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	cases := []struct {
		state      string
		wantStatus int
		wantState  string
	}{
		{state: "failed", wantStatus: http.StatusOK, wantState: "cancelled"},
		{state: "done", wantStatus: http.StatusConflict, wantState: "done"},
		{state: "cancelled", wantStatus: http.StatusConflict, wantState: "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			t.Parallel()
			harness := newContinueHarness(t)
			runID := harness.insertRunInState(t, tc.state, "")
			recorder := harness.callRunAction(t, runID, "cancel", harness.api.handleCancelRun, true)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if state := harness.runStateOf(t, runID); state != tc.wantState {
				t.Errorf("run state = %q, want %q", state, tc.wantState)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			if count := harness.countJobsOfKind(t, runID, jobKindTeardown); count != 1 {
				t.Errorf("teardown jobs = %d, want 1", count)
			}
			_, seal, _, err := harness.api.mfst.Get(t.Context(), continueTestTenant, runID)
			if err != nil {
				t.Fatal(err)
			}
			if seal.Reason != string(manifest.SealFailed) {
				t.Errorf("seal reason = %q after closing a failed run, want %q", seal.Reason, manifest.SealFailed)
			}
		})
	}
}
