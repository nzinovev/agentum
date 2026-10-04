package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/sqlc-dev/pqtype"
)

// validCreateBody is the body every accepted case starts from, as compact JSON.
const validCreateBody = `{
  "project_id": "P1",
  "pipeline_pack": "backend-development@0.1.0",
  "title": "Lower the log level of health endpoints",
  "description": "Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path.",
  "base_ref": "HEAD"
}`

func TestParseTaskCreate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      func() string
		wantErr   bool
		errSubstr string
		secret    bool
	}{
		{
			name: "well-formed body parses",
			body: func() string { return validCreateBody },
		},
		{
			name: "explicit remote-tracking base_ref parses",
			body: func() string {
				return strings.Replace(validCreateBody, `"base_ref": "HEAD"`,
					`"base_ref": "refs/remotes/origin/main"`, 1)
			},
		},
		{
			name: "absent base_ref rejected — no silent HEAD default",
			body: func() string {
				return strings.Replace(validCreateBody, ",\n  \"base_ref\": \"HEAD\"", "", 1)
			},
			wantErr:   true,
			errSubstr: "base_ref is required",
		},
		{
			name: "blank base_ref rejected",
			body: func() string {
				return strings.Replace(validCreateBody, `"base_ref": "HEAD"`, `"base_ref": "   "`, 1)
			},
			wantErr:   true,
			errSubstr: "base_ref is required",
		},
		{
			name: "overrides by name parse",
			body: func() string {
				return strings.Replace(validCreateBody, `"base_ref": "HEAD"`,
					`"base_ref": "HEAD", "overrides": {"checks": {"required": ["verify"]}}`, 1)
			},
		},
		{
			name: "absent description rejected",
			body: func() string {
				return strings.Replace(validCreateBody,
					`"description": "Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path.",`, "", 1)
			},
			wantErr:   true,
			errSubstr: "description is required",
		},
		{
			name: "blank description rejected",
			body: func() string {
				return strings.Replace(validCreateBody,
					`"Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path."`, `"   "`, 1)
			},
			wantErr:   true,
			errSubstr: "description is required",
		},
		{
			name: "unknown top-level field rejected",
			body: func() string {
				return strings.Replace(validCreateBody, `"base_ref": "HEAD"`,
					`"base_ref": "HEAD", "input": {}`, 1)
			},
			wantErr:   true,
			errSubstr: "input",
		},
		{
			name: "unknown field inside overrides rejected",
			body: func() string {
				return strings.Replace(validCreateBody, `"base_ref": "HEAD"`,
					`"base_ref": "HEAD", "overrides": {"checks": {"requred": ["verify"]}}`, 1)
			},
			wantErr:   true,
			errSubstr: `overrides.checks`,
		},
		{
			name: "over-budget description rejected",
			body: func() string {
				padded := strings.Repeat("x", taskinput.MaxDescriptionBytes+1)
				return strings.Replace(validCreateBody,
					`"Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path."`,
					`"`+padded+`"`, 1)
			},
			wantErr:   true,
			errSubstr: "description exceeds",
		},
		{
			name: "credential-shaped description refused",
			body: func() string {
				return strings.Replace(validCreateBody,
					`"Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path."`,
					`"Use token: ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789 for this."`, 1)
			},
			wantErr: true,
			secret:  true,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := parseRunCreate([]byte(testCase.body()))
			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if testCase.errSubstr != "" && !strings.Contains(err.Error(), testCase.errSubstr) {
				t.Errorf("error %q does not contain %q", err, testCase.errSubstr)
			}
			if testCase.secret != errors.Is(err, artifacts.ErrSecretDetected) {
				t.Errorf("errors.Is(err, ErrSecretDetected) = %v, want %v", errors.Is(err, artifacts.ErrSecretDetected), testCase.secret)
			}
		})
	}
}

func TestParseRunCreate_DefaultPack(t *testing.T) {
	t.Parallel()
	body := strings.Replace(validCreateBody, "  \"pipeline_pack\": \"backend-development@0.1.0\",\n", "", 1)
	request, _, err := parseRunCreate([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if request.PipelinePack != defaultRunPipelinePack {
		t.Errorf("pack = %q, want backend-development", request.PipelinePack)
	}
}

func TestParseRunCreate_TrimsBaseRef(t *testing.T) {
	t.Parallel()
	body := strings.Replace(validCreateBody, `"base_ref": "HEAD"`, `"base_ref": "  HEAD  "`, 1)
	request, _, err := parseRunCreate([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if request.BaseRef != "HEAD" {
		t.Errorf("base_ref = %q, want HEAD", request.BaseRef)
	}
}

func TestRequestErrorField_UsesStructuredField(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name  string
		err   error
		field string
	}{
		{name: "task field with unrelated copy", err: &taskinput.FieldError{Field: "title", Message: "please enter a task name"}, field: "title"},
		{name: "wrapped request field", err: fmt.Errorf("invalid input: %w", &requestFieldError{field: "base_ref", cause: errors.New("choose a revision")}), field: "base_ref"},
		{name: "unrelated error", err: errors.New("invalid JSON"), field: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := requestErrorField(testCase.err); got != testCase.field {
				t.Errorf("field = %q, want %q", got, testCase.field)
			}
		})
	}
}

func TestRunUIStateAndOrdering(t *testing.T) {
	harness := newRegistrationHarness(t)
	repositoryPath := t.TempDir()
	initRegistrationRepo(t, repositoryPath, "run ui state")
	project := harness.registerProject(repositoryPath)

	created := harness.seedRun(project.ID, "created", "")
	paused := harness.seedRun(project.ID, "created", "")
	if _, err := harness.queries.UpdateRunStage(t.Context(), sqlc.UpdateRunStageParams{
		ID: paused.ID, TenantID: testTenantID, CurrentStage: sql.NullString{String: "spec", Valid: true},
		State: "paused_open_questions", StopReason: "open_questions",
	}); err != nil {
		t.Fatal(err)
	}
	invocation, err := harness.queries.CreateStageInvocation(t.Context(), sqlc.CreateStageInvocationParams{
		TenantID: testTenantID, UserID: testUserID, RunID: paused.ID, Stage: "spec", Sequence: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.queries.FinishStageInvocation(t.Context(), sqlc.FinishStageInvocationParams{
		ID: invocation.ID, TenantID: testTenantID,
		Result: pqtype.NullRawMessage{RawMessage: json.RawMessage(`{"status":"blocked","open_questions":["Which database?"]}`), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	pausedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+paused.ID, nil)
	pausedRequest.SetPathValue("id", paused.ID)
	pausedRequest = pausedRequest.WithContext(authz.WithPrincipal(pausedRequest.Context(), harness.principal))
	pausedRecorder := httptest.NewRecorder()
	harness.api.handleGetRun(pausedRecorder, pausedRequest)
	if pausedRecorder.Code != http.StatusOK {
		t.Fatalf("paused GET status = %d: %s", pausedRecorder.Code, pausedRecorder.Body.String())
	}
	var pausedResponse runResponse
	if err := json.Unmarshal(pausedRecorder.Body.Bytes(), &pausedResponse); err != nil {
		t.Fatal(err)
	}
	if pausedResponse.CurrentStage != "spec" || pausedResponse.StopReason != "open_questions" || pausedResponse.OpenQuestions == nil || len(*pausedResponse.OpenQuestions) != 1 || (*pausedResponse.OpenQuestions)[0] != "Which database?" {
		t.Errorf("paused run response = %+v", pausedResponse)
	}
	failed := harness.seedRun(project.ID, "created", "")
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: failed.ID, TenantID: testTenantID, State: "failed", Error: "pack failed",
	}); err != nil {
		t.Fatal(err)
	}
	ordered, err := harness.queries.ListRunsByProject(t.Context(), sqlc.ListRunsByProjectParams{
		TenantID: testTenantID, ProjectID: project.ID, Limit: 2, Offset: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[0].ID != paused.ID || ordered[1].ID != failed.ID {
		t.Fatalf("first page = %+v; want paused before failed", ordered)
	}
	secondPage, err := harness.queries.ListRunsByProject(t.Context(), sqlc.ListRunsByProjectParams{
		TenantID: testTenantID, ProjectID: project.ID, Limit: 2, Offset: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage) != 1 || secondPage[0].ID != created.ID {
		t.Fatalf("second page = %+v; want created run", secondPage)
	}
	resumed, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: paused.ID, TenantID: testTenantID, State: "running", StopReason: "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.StopReason != "" || resumed.Error != "" || resumed.CancelReason != "" {
		t.Errorf("resumed diagnostics = %+v; want all empty", resumed)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+failed.ID, nil)
	request.SetPathValue("id", failed.ID)
	request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
	recorder := httptest.NewRecorder()
	harness.api.handleGetRun(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response runResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "pack failed" || response.CurrentStage != "" || response.StopReason != "" {
		t.Errorf("failed run response = %+v", response)
	}
	createBody := fmt.Sprintf(`{"project_id":%q,"title":"Default pack","description":"Check default pack.","base_ref":"HEAD"}`, project.ID)
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(createBody))
	createRequest = createRequest.WithContext(authz.WithPrincipal(createRequest.Context(), harness.principal))
	createRecorder := httptest.NewRecorder()
	harness.api.handleCreateRun(createRecorder, createRequest)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", createRecorder.Code, createRecorder.Body.String())
	}
	var createdResponse runResponse
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &createdResponse); err != nil {
		t.Fatal(err)
	}
	stored, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: createdResponse.ID, TenantID: testTenantID})
	if err != nil {
		t.Fatal(err)
	}
	if createdResponse.PipelinePack != "backend-development" || stored.PipelinePack != "backend-development" {
		t.Errorf("default pack response = %q, stored = %q", createdResponse.PipelinePack, stored.PipelinePack)
	}
}

func TestRunValidationErrorField(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	writeRequestBodyError(recorder, &taskinput.FieldError{Field: "description", Message: "description exceeds 10 bytes"})
	var response errorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Field != "description" || response.Error.Code != codeBadInput || response.Error.Message != "description exceeds 10 bytes" {
		t.Errorf("error = %+v", response.Error)
	}
}

// TestWriteTaskCreateError_SecretMapsTo422 pins the error mapping: a detected
// credential is a 422 bad_input — the same pairing artifact_edit.go uses — and
// every other parse failure is a 400 bad_input.
func TestWriteTaskCreateError_SecretMapsTo422(t *testing.T) {
	t.Parallel()
	// A credential SHAPE, not a labeled value: after the narrowing, the prose
	// scanner only fires on material that identifies itself.
	secretErr := parseErrorOf(t, strings.Replace(validCreateBody,
		`"Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path."`,
		`"Paste ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789 into the config."`, 1))

	secretRecorder := httptest.NewRecorder()
	writeRequestBodyError(secretRecorder, secretErr)
	if secretRecorder.Code != 422 {
		t.Errorf("secret status = %d, want 422", secretRecorder.Code)
	}

	badInputRecorder := httptest.NewRecorder()
	writeRequestBodyError(badInputRecorder, errors.New("description is required"))
	if badInputRecorder.Code != 400 {
		t.Errorf("plain validation status = %d, want 400", badInputRecorder.Code)
	}
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(secretRecorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if decoded.Error.Code != codeBadInput {
		t.Errorf("secret code = %q, want %q", decoded.Error.Code, codeBadInput)
	}
}

func parseErrorOf(t *testing.T, body string) error {
	t.Helper()
	_, _, err := parseRunCreate([]byte(body))
	if err == nil {
		t.Fatal("expected a parse error for the credential-shaped body")
	}
	return err
}

// TestToTaskResponse_CarriesRequestNotInput pins the response shape: the run
// echoes the description and the stored overrides, and the legacy `input` key
// is gone.
func TestToTaskResponse_CarriesRequestNotInput(t *testing.T) {
	t.Parallel()
	run := sqlc.Run{
		ID: "T1", ProjectID: "P1", PipelinePack: "backend-development@0.1.0",
		Title: "Baseline run", Description: "Lower the log level.",
		Overrides: json.RawMessage(`{"checks":{"required":["verify"],"optional":[]}}`),
	}
	encoded, err := json.Marshal(toRunResponse(run))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["description"] != "Lower the log level." {
		t.Errorf("description = %v, want the stored text", decoded["description"])
	}
	if decoded["overrides"] == nil {
		t.Error("overrides missing from the response")
	}
	if _, stillPresent := decoded["input"]; stillPresent {
		t.Error("the legacy input key must not appear in the response")
	}
}

// TestToTaskResponse_EmptyOverridesRenderAsObject keeps the JSON valid for a
// run with no overrides: the fallback renders {} rather than null (the
// len==0 → "{}" fallback moved from the old input field).
func TestToTaskResponse_EmptyOverridesRenderAsObject(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(toRunResponse(sqlc.Run{ID: "T2"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	overrides, isObject := decoded["overrides"].(map[string]any)
	if !isObject {
		t.Fatalf("overrides = %v, want a JSON object", decoded["overrides"])
	}
	if len(overrides) != 0 {
		t.Errorf("overrides = %v, want {}", overrides)
	}
}

// TestParseTaskCreate_ProseAboutCredentialsIsAccepted is the regression test
// for the scan narrowing. Before it, DefaultScanner's label-context rules
// rejected ordinary backend run descriptions under PolicyReject and the
// author had no override path: "Add Bearer authentication to /settings"
// matched bearer-token, and a sentence naming an env var next to the word
// "secret" matched labeled-secret. An auth run — the product's own documented
// example — could not be created at all. These must all be accepted.
func TestParseTaskCreate_ProseAboutCredentialsIsAccepted(t *testing.T) {
	t.Parallel()
	descriptions := []string{
		"Add Bearer authentication to the /settings endpoint.",
		"Rename the config key api_key = AGENTUM_WEBHOOK_SECRET_ENV",
		"secret: AGENTUM_WEBHOOK_SECRET_ENV should be read from env.",
		"Send the Authorization: Bearer header on every upstream call.",
		"Store the password hash with bcrypt instead of the current SHA-256.",
	}
	for _, description := range descriptions {
		t.Run(description, func(t *testing.T) {
			t.Parallel()
			body := strings.Replace(validCreateBody,
				`"Log /healthz and /readyz at Debug instead of Info; everything else stays at Info. Compare by exact path."`,
				`"`+description+`"`, 1)
			if _, _, err := parseRunCreate([]byte(body)); err != nil {
				t.Errorf("prose about credentials must be accepted, got: %v", err)
			}
		})
	}
}

// TestParseTaskCreate_CredentialInTitleRefused pins the other half of the
// scan: the title is delivered to the model and recorded in the manifest
// exactly like the description, so scanning only the description would leave
// the same leak one field to the left.
func TestParseTaskCreate_CredentialInTitleRefused(t *testing.T) {
	t.Parallel()
	body := strings.Replace(validCreateBody,
		`"Lower the log level of health endpoints"`,
		`"Rotate ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789"`, 1)
	_, _, err := parseRunCreate([]byte(body))
	if err == nil {
		t.Fatal("a credential in the title must be refused")
	}
	if !errors.Is(err, artifacts.ErrSecretDetected) {
		t.Errorf("err = %v, want ErrSecretDetected", err)
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error %q must name the offending field", err)
	}
}
