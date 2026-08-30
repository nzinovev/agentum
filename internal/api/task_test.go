package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
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
			_, _, err := parseTaskCreate([]byte(testCase.body()))
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
	writeTaskCreateError(secretRecorder, secretErr)
	if secretRecorder.Code != 422 {
		t.Errorf("secret status = %d, want 422", secretRecorder.Code)
	}

	badInputRecorder := httptest.NewRecorder()
	writeTaskCreateError(badInputRecorder, errors.New("description is required"))
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
	_, _, err := parseTaskCreate([]byte(body))
	if err == nil {
		t.Fatal("expected a parse error for the credential-shaped body")
	}
	return err
}

// TestToTaskResponse_CarriesRequestNotInput pins the response shape: the task
// echoes the description and the stored overrides, and the legacy `input` key
// is gone.
func TestToTaskResponse_CarriesRequestNotInput(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{
		ID: "T1", ProjectID: "P1", PipelinePack: "backend-development@0.1.0",
		Title: "Baseline run", Description: "Lower the log level.",
		Overrides: json.RawMessage(`{"checks":{"required":["verify"],"optional":[]}}`),
	}
	encoded, err := json.Marshal(toTaskResponse(task))
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
// task with no overrides: the fallback renders {} rather than null (the
// len==0 → "{}" fallback moved from the old input field).
func TestToTaskResponse_EmptyOverridesRenderAsObject(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(toTaskResponse(sqlc.Task{ID: "T2"}))
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
// rejected ordinary backend task descriptions under PolicyReject and the
// author had no override path: "Add Bearer authentication to /settings"
// matched bearer-token, and a sentence naming an env var next to the word
// "secret" matched labeled-secret. An auth task — the product's own documented
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
			if _, _, err := parseTaskCreate([]byte(body)); err != nil {
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
	_, _, err := parseTaskCreate([]byte(body))
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
