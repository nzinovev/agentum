package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The publication surface's read/write contract against a real Postgres:
// every form of absence answers 200 with an explicit state instead of a 404,
// and the write's three preconditions each produce their own 409 code. The
// provider is never contacted — the handlers read Postgres only, which is
// exactly what these tests pin.

const (
	publicationTestTenant = "7e6f2a9b-5c8d-4a3f-b7a9-0d1e2f3a4b5c"
	publicationTestUser   = "8f7a3b0c-6d9e-4b4a-c8b0-1e2f3a4b5c6d"
)

// publicationHarness bundles one database and one API with the publication
// configuration the test chooses.
type publicationHarness struct {
	api     *API
	queries *sqlc.Queries
	db      *sql.DB
}

func newPublicationHarness(t *testing.T, enabled bool) *publicationHarness {
	t.Helper()
	handle := dbtest.Store(t)
	return &publicationHarness{
		api: New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
			WithPublicationConfig(enabled, "noop")),
		queries: handle.Queries,
		db:      handle.Store.DB,
	}
}

// insertPublicationRun creates the project + run fixture and returns the run
// id. Raw inserts: the handler under test needs a run row, not the run
// lifecycle.
func (harness *publicationHarness) insertPublicationRun(t *testing.T, state string) string {
	t.Helper()
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/pub-fixture-' || $3, 'publication fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture', 'fixture', '{}', 'main', $4
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		publicationTestTenant, publicationTestUser, state, state).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}
	return runID
}

// callPublication issues the request against the matching handler with the
// principal attached and the {id} path value set — the handlers are
// dispatched directly, without the ServeMux that would populate it, and the
// boundary middleware is another test's subject.
func (harness *publicationHarness) callPublication(t *testing.T, method, path, runID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, nil)
	request.SetPathValue("id", runID)
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: publicationTestTenant, UserID: publicationTestUser,
	}))
	recorder := httptest.NewRecorder()
	switch method {
	case http.MethodGet:
		if strings.HasSuffix(path, "/publication") {
			harness.api.handleGetPublication(recorder, request)
		} else {
			harness.api.handleFinalReview(recorder, request)
		}
	case http.MethodPost:
		harness.api.handlePublishRun(recorder, request)
	}
	return recorder
}

// decodePublication decodes the response body.
func decodePublication(t *testing.T, recorder *httptest.ResponseRecorder) publicationResponse {
	t.Helper()
	var response publicationResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return response
}

// TestGetPublicationAnswersEveryAbsenceExplicitly: a run exists, so the GET
// never 404s — publication off answers "disabled", publication on without a
// row answers "not_attempted", and a row answers its own state with the
// reason of a refusal attached.
func TestGetPublicationAnswersEveryAbsenceExplicitly(t *testing.T) {
	t.Parallel()
	disabledHarness := newPublicationHarness(t, false)
	runID := disabledHarness.insertPublicationRun(t, "awaiting_final_review")
	recorder := disabledHarness.callPublication(t, http.MethodGet, "/api/v1/runs/"+runID+"/publication", runID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("disabled GET: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if response := decodePublication(t, recorder); response.State != "disabled" {
		t.Errorf("disabled GET state = %q, want disabled", response.State)
	}

	enabledHarness := newPublicationHarness(t, true)
	enabledRunID := enabledHarness.insertPublicationRun(t, "awaiting_final_review")
	recorder = enabledHarness.callPublication(t, http.MethodGet, "/api/v1/runs/"+enabledRunID+"/publication", enabledRunID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("not-attempted GET: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if response := decodePublication(t, recorder); response.State != "not_attempted" {
		t.Errorf("not-attempted GET state = %q, want not_attempted", response.State)
	}

	row, err := enabledHarness.queries.EnsurePublication(context.Background(), sqlc.EnsurePublicationParams{
		TenantID: publicationTestTenant, UserID: publicationTestUser, RunID: enabledRunID,
		Provider: "noop", RemoteBranch: "agentum/" + enabledRunID,
		PublishedCommit: "abc123",
	})
	if err != nil {
		t.Fatalf("ensure publication: %v", err)
	}
	if _, failErr := enabledHarness.queries.RecordPublicationFailure(context.Background(), sqlc.RecordPublicationFailureParams{
		TenantID: publicationTestTenant, RunID: enabledRunID, State: "failed",
		LastErrorCode: sql.NullString{String: "credentials_missing", Valid: true},
	}); failErr != nil {
		t.Fatalf("record failure: %v", failErr)
	}
	recorder = enabledHarness.callPublication(t, http.MethodGet, "/api/v1/runs/"+enabledRunID+"/publication", enabledRunID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("failed-row GET: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	response := decodePublication(t, recorder)
	if response.State != "failed" {
		t.Errorf("failed-row GET state = %q, want failed", response.State)
	}
	if response.LastError == nil || response.LastError.Code != "credentials_missing" {
		t.Errorf("last_error = %+v, want credentials_missing", response.LastError)
	}
	if response.Target == nil || response.Target.RemoteBranch != row.RemoteBranch {
		t.Errorf("target = %+v, want the row's remote branch %q", response.Target, row.RemoteBranch)
	}
}

// TestGetPublicationShowsAnExistingRowWhileDisabled: a delivered run keeps
// its pull request, target, and times after the operator switches publication
// off. The row is a fact about the run; the configuration governs what new
// runs do. Answering "disabled" here would erase a delivery that happened.
func TestGetPublicationShowsAnExistingRowWhileDisabled(t *testing.T) {
	t.Parallel()
	enabledHarness := newPublicationHarness(t, true)
	runID := enabledHarness.insertPublicationRun(t, "awaiting_final_review")
	if _, err := enabledHarness.queries.EnsurePublication(context.Background(), sqlc.EnsurePublicationParams{
		TenantID: publicationTestTenant, UserID: publicationTestUser, RunID: runID,
		Provider: "github", RemoteBranch: "agentum/" + runID, PublishedCommit: "abc123",
	}); err != nil {
		t.Fatalf("ensure publication: %v", err)
	}
	if _, err := enabledHarness.queries.RecordPublicationSuccess(context.Background(), sqlc.RecordPublicationSuccessParams{
		TenantID: publicationTestTenant, RunID: runID,
		TargetHost: "github.com", TargetOwner: "example", TargetRepository: "repo", BaseBranch: "main",
		PrNumber: sql.NullInt32{Int32: 12, Valid: true},
		PrUrl:    sql.NullString{String: "https://github.com/example/repo/pull/12", Valid: true},
		PrState:  sql.NullString{String: "open", Valid: true},
	}); err != nil {
		t.Fatalf("record success: %v", err)
	}

	// The same database read through an API whose publication switch is off.
	disabledView := New(enabledHarness.db, enabledHarness.queries, slog.New(slog.DiscardHandler), nil, WithPublicationConfig(false, "noop"))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/publication", nil)
	request.SetPathValue("id", runID)
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: publicationTestTenant, UserID: publicationTestUser,
	}))
	recorder := httptest.NewRecorder()
	disabledView.handleGetPublication(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET while disabled: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	response := decodePublication(t, recorder)
	if response.State != "published" {
		t.Errorf("state = %q, want published — the row wins over the switch", response.State)
	}
	if response.PullRequest == nil || response.PullRequest.Number != 12 ||
		response.PullRequest.URL != "https://github.com/example/repo/pull/12" {
		t.Errorf("pull request = %+v, want number 12 with its URL", response.PullRequest)
	}
	if response.PublishedAt == "" || response.BranchPushedAt == "" {
		t.Errorf("published_at = %q, branch_pushed_at = %q; a delivered run keeps its times", response.PublishedAt, response.BranchPushedAt)
	}
}

// TestGetPublicationSeparatesAReadFailureFromAnAbsence: a read that did not
// complete answers "unavailable", never "not_attempted" or "disabled". The
// reader must be able to tell an absent publication from an unread one, which
// is the distinction the coordinator already draws for a manifest it could not
// read. The status stays 200: the same view rides inside the final review, and
// a failed delivery-state read must not take the review down.
func TestGetPublicationSeparatesAReadFailureFromAnAbsence(t *testing.T) {
	t.Parallel()
	handle := dbtest.Store(t)
	apiInst := New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
		WithPublicationConfig(true, "noop"))

	// Closing the pool is the cheapest read failure that is not ErrNoRows;
	// the harness's own cleanup closes it again, which is a no-op.
	if err := handle.Store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+publicationTestUser+"/publication", nil)
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: publicationTestTenant, UserID: publicationTestUser,
	}))
	view := apiInst.publicationView(request, sqlc.Run{ID: publicationTestUser, TenantID: publicationTestTenant})
	if view.State != publicationStateUnavailable {
		t.Errorf("state = %q, want %q — an unread row is not an absent one",
			view.State, publicationStateUnavailable)
	}

	// The same failure with publication switched off must not read as
	// "disabled": the switch describes what new runs do, not what this read
	// found out.
	offInst := New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
		WithPublicationConfig(false, ""))
	if offView := offInst.publicationView(request, sqlc.Run{ID: publicationTestUser, TenantID: publicationTestTenant}); offView.State != publicationStateUnavailable {
		t.Errorf("state with publication off = %q, want %q", offView.State, publicationStateUnavailable)
	}
}

// TestPublishRunPreconditions pins the three 409s: publication off in
// configuration, a run not in a deliverable state, and an attempt already in
// flight under a live lease.
func TestPublishRunPreconditions(t *testing.T) {
	t.Parallel()
	disabledHarness := newPublicationHarness(t, false)
	disabledRunID := disabledHarness.insertPublicationRun(t, "awaiting_final_review")
	recorder := disabledHarness.callPublication(t, http.MethodPost, "/api/v1/runs/"+disabledRunID+"/publish", disabledRunID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("disabled POST: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeErrorCode(t, recorder); code != "publication_disabled" {
		t.Errorf("disabled POST code = %q, want publication_disabled", code)
	}

	enabledHarness := newPublicationHarness(t, true)
	unreadyRunID := enabledHarness.insertPublicationRun(t, "created")
	recorder = enabledHarness.callPublication(t, http.MethodPost, "/api/v1/runs/"+unreadyRunID+"/publish", unreadyRunID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("not-ready POST: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeErrorCode(t, recorder); code != "publication_not_ready" {
		t.Errorf("not-ready POST code = %q, want publication_not_ready", code)
	}

	inFlightRunID := enabledHarness.insertPublicationRun(t, "awaiting_final_review")
	if _, err := enabledHarness.queries.EnsurePublication(context.Background(), sqlc.EnsurePublicationParams{
		TenantID: publicationTestTenant, UserID: publicationTestUser, RunID: inFlightRunID,
		Provider: "noop", RemoteBranch: "agentum/" + inFlightRunID, PublishedCommit: "abc123",
	}); err != nil {
		t.Fatalf("ensure publication: %v", err)
	}
	if _, err := enabledHarness.db.ExecContext(context.Background(), `
		UPDATE run_publications SET state = 'publishing',
		    lease_expires_at = now() + interval '1 minute'
		WHERE run_id = $1`, inFlightRunID); err != nil {
		t.Fatalf("hold lease: %v", err)
	}
	recorder = enabledHarness.callPublication(t, http.MethodPost, "/api/v1/runs/"+inFlightRunID+"/publish", inFlightRunID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("in-flight POST: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if code := decodeErrorCode(t, recorder); code != "conflict" {
		t.Errorf("in-flight POST code = %q, want conflict", code)
	}
}

// TestPublishRunEnqueuesJobAndCreatesRow: an accepted retry creates the
// publication row (the gate may have run while publication was off) and
// enqueues the publish job in one transaction.
func TestPublishRunEnqueuesJobAndCreatesRow(t *testing.T) {
	t.Parallel()
	harness := newPublicationHarness(t, true)
	runID := harness.insertPublicationRun(t, "awaiting_final_review")

	recorder := harness.callPublication(t, http.MethodPost, "/api/v1/runs/"+runID+"/publish", runID)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("accepted POST: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	row, err := harness.queries.GetPublicationForRun(context.Background(), sqlc.GetPublicationForRunParams{
		TenantID: publicationTestTenant, RunID: runID,
	})
	if err != nil {
		t.Fatalf("publication row after POST: %v", err)
	}
	if row.State != "pending" || row.Provider != "noop" {
		t.Errorf("row = state %q provider %q; want pending noop", row.State, row.Provider)
	}
	var jobKinds []string
	jobRows, queryErr := harness.db.QueryContext(context.Background(),
		`SELECT kind FROM jobs WHERE run_id = $1 AND tenant_id = $2 ORDER BY id`, runID, publicationTestTenant)
	if queryErr != nil {
		t.Fatalf("query jobs: %v", queryErr)
	}
	defer jobRows.Close()
	for jobRows.Next() {
		var kind string
		if err := jobRows.Scan(&kind); err != nil {
			t.Fatalf("scan job kind: %v", err)
		}
		jobKinds = append(jobKinds, kind)
	}
	if len(jobKinds) != 1 || jobKinds[0] != "publish" {
		t.Errorf("enqueued kinds = %v, want [publish]", jobKinds)
	}

	// A second POST on the same run is accepted again — the retry is
	// idempotent at the row (unique index) and safe at the job (the lease
	// admits one attempt).
	recorder = harness.callPublication(t, http.MethodPost, "/api/v1/runs/"+runID+"/publish", runID)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("second POST: status %d, body %s", recorder.Code, recorder.Body.String())
	}
}

// TestFinalReviewCarriesPublicationBlock: the review payload embeds the same
// publication form the dedicated GET answers with, so a review works with a
// delivery, without one, and while one is failing — no provider is
// contacted on either path.
func TestFinalReviewCarriesPublicationBlock(t *testing.T) {
	t.Parallel()
	harness := newPublicationHarness(t, true)
	runID := harness.insertPublicationRun(t, "awaiting_final_review")

	recorder := harness.callPublication(t, http.MethodGet, "/api/v1/runs/"+runID+"/final-review", runID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("final-review: status %d, body %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"publication"`) {
		t.Errorf("final-review body has no publication block: %s", recorder.Body.String())
	}
	var payload struct {
		Publication *publicationResponse `json:"publication"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode final-review: %v", err)
	}
	if payload.Publication == nil || payload.Publication.State != "not_attempted" {
		t.Errorf("publication block = %+v, want not_attempted", payload.Publication)
	}
}

// decodeErrorCode pulls the error code out of a structured error body.
func decodeErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return body.Error.Code
}
