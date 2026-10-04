package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
)

// The continue endpoint's body contract. The pure tests pin every boundary
// rule of parseContinueBody without a database; the Postgres-backed tests pin
// the whole delivery: the accepted text lands in the queued job's payload, the
// run is resumed, and the human decision is on the record. A 200 alone proves
// nothing — before the fix the endpoint answered 200 while the text died in
// the job row the runner never read.

// TestParseContinueBody_AcceptsTheCanonicalShapes covers the acceptance table:
// a text-bearing body parses to that text, and every empty shape — no body,
// null, {}, a whitespace-only text — parses to the empty continuation.
func TestParseContinueBody_AcceptsTheCanonicalShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		wantText string
	}{
		{"text body", `{"text":"Используем PostgreSQL 17."}`, "Используем PostgreSQL 17."},
		{"empty body", "", ""},
		{"json null body", "null", ""},
		{"empty object", "{}", ""},
		{"whitespace-only text", `{"text":"   "}`, ""},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			continuation, err := parseContinueBody([]byte(testCase.body))
			if err != nil {
				t.Fatalf("parseContinueBody(%q): %v", testCase.body, err)
			}
			if continuation.Text != testCase.wantText {
				t.Errorf("text = %q, want %q", continuation.Text, testCase.wantText)
			}
		})
	}
}

// TestContinueBeforeFirstInvocation accepts a note at a pre-invocation
// user stop and queues it for the first stage through the run-level route.
func TestContinueBeforeFirstInvocation(t *testing.T) {
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, false)
	if _, err := harness.db.ExecContext(t.Context(),
		`DELETE FROM stage_invocations WHERE run_id = $1 AND tenant_id = $2`, runID, continueTestTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.UpdateRunStage(t.Context(), sqlc.UpdateRunStageParams{
		ID: runID, TenantID: continueTestTenant, CurrentStage: sql.NullString{String: "spec", Valid: true},
		State: "paused_user_stop", StopReason: "base_not_on_target",
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+runID+"/continue", strings.NewReader(`{"text":"base is now on the target"}`))
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{TenantID: continueTestTenant, UserID: continueTestUser}))
	response := httptest.NewRecorder()
	mux := http.NewServeMux()
	harness.api.Register(mux)
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("continue status = %d, body %s", response.Code, response.Body.String())
	}
	if state := harness.runStateOf(t, runID); state != "running" {
		t.Fatalf("state = %q, want running", state)
	}
	payload, found := harness.latestContinuePayload(t, runID)
	if !found || !strings.Contains(payload, "base is now on the target") {
		t.Fatalf("continue payload = %q, found = %t", payload, found)
	}
}

// TestParseContinueBody_RejectsMalformedInput is the refusal table: broken
// JSON, an unknown field, a non-string text, a second JSON object, invalid
// UTF-8, and an over-budget text are all errors, and a credential-shaped text
// is ErrSecretDetected specifically.
func TestParseContinueBody_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	overBudget := strings.Repeat("x", taskinput.MaxContinuationTextBytes+1)
	tests := []struct {
		name   string
		body   string
		secret bool
	}{
		{name: "broken json", body: `{"text":`},
		{name: "unknown field", body: `{"text":"ok","answers":[]}`},
		{name: "null text", body: `{"text":null}`},
		{name: "number text", body: `{"text":7}`},
		{name: "second json object", body: `{"text":"a"}{"text":"b"}`},
		{name: "invalid utf-8", body: "{\"text\":\"bad \xff byte\"}"},
		{name: "over-budget text", body: `{"text":"` + overBudget + `"}`},
		{name: "credential-shaped text", body: `{"text":"Paste ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789 into the config."}`, secret: true},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseContinueBody([]byte(testCase.body))
			if err == nil {
				t.Fatalf("parseContinueBody(%q) accepted malformed input", testCase.body)
			}
			if testCase.secret != errors.Is(err, artifacts.ErrSecretDetected) {
				t.Errorf("errors.Is(err, ErrSecretDetected) = %v, want %v; err: %v",
					errors.Is(err, artifacts.ErrSecretDetected), testCase.secret, err)
			}
		})
	}
}

// TestParseContinueBody_ProseAboutCredentialsIsAccepted mirrors the run-create
// narrowing: the prose scanner rejects credential SHAPES, not sentences about
// authentication, and an author answering a question must not be refused for
// mentioning the word token.
func TestParseContinueBody_ProseAboutCredentialsIsAccepted(t *testing.T) {
	t.Parallel()
	answers := []string{
		"Add Bearer authentication to the upstream call.",
		"Read the webhook secret from the environment.",
		"Store the password hash with bcrypt.",
	}
	for _, answer := range answers {
		t.Run(answer, func(t *testing.T) {
			t.Parallel()
			continuation, err := parseContinueBody([]byte(`{"text":"` + answer + `"}`))
			if err != nil {
				t.Fatalf("prose answer must be accepted, got: %v", err)
			}
			if continuation.Text != answer {
				t.Errorf("text = %q, want %q", continuation.Text, answer)
			}
		})
	}
}

// --- Postgres-backed delivery tests ---

const (
	continueTestTenant = "c7d0a3f1-2b4e-4c5a-9f3b-6d7e8f9a0b1c"
	continueTestUser   = "d8e1b4c2-3a5f-4d6b-8e4c-7f8a9b0c1d2e"
)

// continueHarness bundles one database, one API with a manifest service, and
// the direct-call helper the publication tests use.
type continueHarness struct {
	api     *API
	queries *sqlc.Queries
	db      *sql.DB
}

func newContinueHarness(t *testing.T) *continueHarness {
	t.Helper()
	handle := dbtest.Store(t)
	return &continueHarness{
		api: New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
			WithManifestService(manifest.New(manifest.Deps{DB: handle.Store.DB, Queries: handle.Queries}))),
		queries: handle.Queries,
		db:      handle.Store.DB,
	}
}

// insertPausedRun creates the project + run fixture paused at open questions,
// plus one finished spec invocation whose session id is present or absent as
// the test chooses, and initializes the manifest. Returns the run id.
func (harness *continueHarness) insertPausedRun(t *testing.T, withSession bool) string {
	t.Helper()
	const insertProjectAndRun = `
		WITH inserted_project AS (
		    INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits,
		                          repo_path, name, related_projects)
		    VALUES ($1, $2, $3, '{}', '/tmp/continue-fixture-' || $3, 'continue fixture ' || $3, '{}')
		    RETURNING id
		)
		INSERT INTO runs (tenant_id, user_id, project_id, pipeline_pack,
		                  title, description, overrides, base_ref, state, current_stage)
		SELECT $1, $2, inserted_project.id, 'backend-development',
		       'fixture', 'fixture', '{}', 'main', 'paused_open_questions', 'spec'
		FROM inserted_project
		RETURNING id`
	var runID string
	if err := harness.db.QueryRowContext(context.Background(), insertProjectAndRun,
		continueTestTenant, continueTestUser, sqlGenSuffix(t)).Scan(&runID); err != nil {
		t.Fatalf("insert run fixture: %v", err)
	}
	if err := harness.api.mfst.Init(t.Context(), continueTestTenant, continueTestUser, runID); err != nil {
		t.Fatal(err)
	}
	sessionID := sql.NullString{}
	if withSession {
		sessionID = sql.NullString{String: "sess-fixture-1", Valid: true}
	}
	if _, err := harness.queries.CreateStageInvocation(t.Context(), sqlc.CreateStageInvocationParams{
		TenantID: continueTestTenant, UserID: continueTestUser, RunID: runID,
		Stage: "spec", Sequence: 1, SessionID: sessionID, Cycle: 0,
	}); err != nil {
		t.Fatalf("insert stage invocation: %v", err)
	}
	return runID
}

// sqlGenSuffix makes the fixture's identity unique per test, so parallel
// subtests do not collide on the project's repo_identity.
func sqlGenSuffix(t *testing.T) string {
	t.Helper()
	return t.Name() + "-identity"
}

// callContinue dispatches the continue handler directly with the principal
// attached and both path values set, the same dispatch style the publication
// tests use.
func (harness *continueHarness) callContinue(t *testing.T, runID, body string, withPrincipal bool) *httptest.ResponseRecorder {
	t.Helper()
	var requestBody *strings.Reader
	if body != "" {
		requestBody = strings.NewReader(body)
	} else {
		requestBody = strings.NewReader("")
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/runs/"+runID+"/invocations/inv-fixture/continue", requestBody)
	request.SetPathValue("id", runID)
	request.SetPathValue("iid", "inv-fixture")
	if withPrincipal {
		request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
			TenantID: continueTestTenant, UserID: continueTestUser,
		}))
	}
	recorder := httptest.NewRecorder()
	harness.api.handleInvocationContinue(recorder, request)
	return recorder
}

// latestContinuePayload reads the payload of the newest continue job for the
// run. No sqlc read query exists for jobs (the queue owns that table), so the
// test reads the row directly.
func (harness *continueHarness) latestContinuePayload(t *testing.T, runID string) (string, bool) {
	t.Helper()
	var payload string
	err := harness.db.QueryRowContext(t.Context(),
		`SELECT payload::text FROM jobs WHERE run_id = $1 AND tenant_id = $2 AND kind = 'continue'
		 ORDER BY id DESC LIMIT 1`, runID, continueTestTenant).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read continue job payload: %v", err)
	}
	return payload, true
}

// runStateOf reads the run's current state.
func (harness *continueHarness) runStateOf(t *testing.T, runID string) string {
	t.Helper()
	run, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: runID, TenantID: continueTestTenant})
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return run.State
}

// TestContinueHandler_TextLandsInJobResumesRunAndRecordsDecision is the
// end-to-end half of the bug fix: POST with text → 200, the queued continue
// job carries the canonical text payload, the run left the paused state, and
// the manifest carries the continued gate decision.
func TestContinueHandler_TextLandsInJobResumesRunAndRecordsDecision(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, true)

	const userText = "Используем PostgreSQL 17. Новую таблицу создавать не нужно."
	recorder := harness.callContinue(t, runID, `{"text":"`+userText+`"}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
	}
	payload, found := harness.latestContinuePayload(t, runID)
	if !found {
		t.Fatal("no continue job was enqueued")
	}
	var decoded taskinput.Continuation
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("job payload %s does not decode as a continuation: %v", payload, err)
	}
	if decoded.Text != userText {
		t.Errorf("job payload text = %q, want %q", decoded.Text, userText)
	}
	if got := harness.runStateOf(t, runID); got != "running" {
		t.Errorf("run state = %q, want running", got)
	}
	body, _, _, err := harness.api.mfst.Get(t.Context(), continueTestTenant, runID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	foundContinued := false
	for _, decision := range body.GateDecisions {
		if decision.Gate == gateOpenQuestions && decision.Decision == decisionContinued {
			foundContinued = true
		}
	}
	if !foundContinued {
		t.Errorf("manifest carries no continued decision at the open_questions gate; got %+v", body.GateDecisions)
	}
}

// TestContinueHandler_EmptyBodiesKeepThePriorContract: empty, {}, and null
// bodies still resume the run and enqueue a continue job whose payload is the
// canonical empty object — the pre-text call stays byte-compatible.
func TestContinueHandler_EmptyBodiesKeepThePriorContract(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"empty object", "{}"},
		{"json null", "null"},
		{"whitespace text", `{"text":"   "}`},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			harness := newContinueHarness(t)
			runID := harness.insertPausedRun(t, true)

			recorder := harness.callContinue(t, runID, testCase.body, true)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", recorder.Code, recorder.Body.String())
			}
			payload, found := harness.latestContinuePayload(t, runID)
			if !found {
				t.Fatal("no continue job was enqueued")
			}
			if payload != "{}" {
				t.Errorf("job payload = %s, want the canonical empty object {}", payload)
			}
			if got := harness.runStateOf(t, runID); got != "running" {
				t.Errorf("run state = %q, want running", got)
			}
		})
	}
}

// TestContinueHandler_InvalidInputChangesNothing: every refused body leaves
// the run paused and the queue empty — no state change, no enqueue, the exact
// failure mode the bug report demands (a 200-shaped lie replaced by an honest
// refusal before the transition).
func TestContinueHandler_InvalidInputChangesNothing(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	overBudget := strings.Repeat("x", taskinput.MaxContinuationTextBytes+1)
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"broken json", `{"text":`, http.StatusBadRequest},
		{"unknown field", `{"text":"ok","answers":[]}`, http.StatusBadRequest},
		{"null text", `{"text":null}`, http.StatusBadRequest},
		{"number text", `{"text":7}`, http.StatusBadRequest},
		{"second json object", `{"text":"a"}{}`, http.StatusBadRequest},
		{"invalid utf-8", "{\"text\":\"bad \xff byte\"}", http.StatusBadRequest},
		{"over-budget text", `{"text":"` + overBudget + `"}`, http.StatusBadRequest},
		{"credential-shaped text", `{"text":"Paste ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789 into the config."}`, http.StatusUnprocessableEntity},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			harness := newContinueHarness(t)
			runID := harness.insertPausedRun(t, true)

			recorder := harness.callContinue(t, runID, testCase.body, true)
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if got := harness.runStateOf(t, runID); got != "paused_open_questions" {
				t.Errorf("a refused body changed the run state to %q", got)
			}
			if _, found := harness.latestContinuePayload(t, runID); found {
				t.Error("a refused body enqueued a continue job")
			}
		})
	}
}

// TestContinueHandler_TextWithoutCapturedSessionIsRefused: text rides the
// captured session; with no session on the latest invocation the request is a
// 409 before any enqueue, not a 200 onto a job that cannot deliver.
func TestContinueHandler_TextWithoutCapturedSessionIsRefused(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, false)

	recorder := harness.callContinue(t, runID, `{"text":"an answer with nowhere to go"}`, true)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", recorder.Code, recorder.Body.String())
	}
	if got := harness.runStateOf(t, runID); got != "paused_open_questions" {
		t.Errorf("run state = %q, want paused_open_questions (refused before the transition)", got)
	}
	if _, found := harness.latestContinuePayload(t, runID); found {
		t.Error("a sessionless text continue enqueued a job")
	}
	// Without text the same run continues: the 409 is about the text, not the
	// state.
	emptyRecorder := harness.callContinue(t, runID, "", true)
	if emptyRecorder.Code != http.StatusOK {
		t.Fatalf("empty continue status = %d, want 200; body: %s", emptyRecorder.Code, emptyRecorder.Body.String())
	}
}

// TestContinueHandler_SessionReadFailureIsInternalError: a failed invocation
// read is not an answer about the session — the session may exist while the
// read could not see it. The handler answers a logged 500, never a 409 that
// claims absence; a 409 would tell the author to give up on text that could
// have been delivered, and the run must stay paused with the queue untouched.
func TestContinueHandler_SessionReadFailureIsInternalError(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, true)
	// Break only the invocation read: the guard's GetRun still answers, the
	// session check's LatestStageForRun fails. Renaming the table in this
	// test's throwaway database leaves every other table intact.
	if _, err := harness.db.ExecContext(t.Context(),
		`ALTER TABLE stage_invocations RENAME TO stage_invocations_unreadable`); err != nil {
		t.Fatal(err)
	}
	recorder := harness.callContinue(t, runID, `{"text":"an answer the read never saw"}`, true)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != codeInternal {
		t.Errorf("code = %q, want %q", decoded.Error.Code, codeInternal)
	}
	if got := harness.runStateOf(t, runID); got != "paused_open_questions" {
		t.Errorf("run state = %q, want paused_open_questions (a failed read must not resume the run)", got)
	}
	if _, found := harness.latestContinuePayload(t, runID); found {
		t.Error("a failed session read enqueued a continue job")
	}
}

// TestContinueHandler_GuardAnswersStandardResponses: the handler now enters
// through the common guard — no principal is a 401, an unknown run is a 404
// with the shared message, and a run in the wrong state is the standard 409.
func TestContinueHandler_GuardAnswersStandardResponses(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("database test")
	}
	harness := newContinueHarness(t)
	runID := harness.insertPausedRun(t, true)

	unauthenticated := harness.callContinue(t, runID, `{}`, false)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Errorf("no principal: status = %d, want 401", unauthenticated.Code)
	}

	missing := harness.callContinue(t, "00000000-0000-0000-0000-000000000000", `{}`, true)
	if missing.Code != http.StatusNotFound {
		t.Errorf("unknown run: status = %d, want 404", missing.Code)
	}
	if !strings.Contains(missing.Body.String(), msgRunNotFound) {
		t.Errorf("unknown run body = %s, want the shared not-found message", missing.Body.String())
	}

	if _, err := harness.db.ExecContext(t.Context(),
		`UPDATE runs SET state = 'running' WHERE tenant_id = $1 AND id = $2`, continueTestTenant, runID); err != nil {
		t.Fatal(err)
	}
	running := harness.callContinue(t, runID, `{}`, true)
	if running.Code != http.StatusConflict {
		t.Errorf("running run: status = %d, want 409 illegal_transition", running.Code)
	}
}
