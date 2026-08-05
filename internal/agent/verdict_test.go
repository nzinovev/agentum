package agent

import (
	"strings"
	"testing"
)

func TestParseVerdictJSON_ValidFull(t *testing.T) {
	t.Parallel()
	in := `{
  "schema_version": "1",
  "verdict": "changes_requested",
  "summary": "two blockers in auth flow",
  "findings": [
    {"id": "F1", "severity": "blocker", "path": "internal/auth/login.go", "line": 42, "detail": "nil deref on empty password"},
    {"id": "F2", "severity": "major", "path": "internal/auth/login.go", "detail": "missing rate limit"}
  ],
  "future_field": "ignored"
}`
	verdict, err := ParseVerdictJSON([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict.SchemaVersion != "1" || verdict.Verdict != VerdictChangesRequested {
		t.Errorf("schema/verdict = %q/%q, want 1/changes_requested", verdict.SchemaVersion, verdict.Verdict)
	}
	if verdict.Summary != "two blockers in auth flow" {
		t.Errorf("summary = %q", verdict.Summary)
	}
	if len(verdict.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2", len(verdict.Findings))
	}
	if verdict.Findings[0].ID != "F1" || verdict.Findings[0].Severity != SeverityBlocker || verdict.Findings[0].Line != 42 {
		t.Errorf("finding[0] = %+v", verdict.Findings[0])
	}
}

func TestParseVerdictJSON_MinimalApproved(t *testing.T) {
	t.Parallel()
	verdict, err := ParseVerdictJSON([]byte(`{"schema_version":"1","verdict":"approved"}`))
	if err != nil {
		t.Fatalf("minimal approved: %v", err)
	}
	if verdict.Verdict != VerdictApproved || len(verdict.Findings) != 0 {
		t.Errorf("minimal parse wrong: %+v", verdict)
	}
}

func TestParseVerdictJSON_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "missing schema_version",
			in:   `{"verdict":"approved"}`,
			want: "schema_version is required",
		},
		{
			name: "missing verdict",
			in:   `{"schema_version":"1"}`,
			want: "verdict is required",
		},
		{
			name: "unsupported schema_version",
			in:   `{"schema_version":"2","verdict":"approved"}`,
			want: "unsupported",
		},
		{
			name: "bad verdict enum",
			in:   `{"schema_version":"1","verdict":"maybe"}`,
			want: "not one of",
		},
		{
			name: "changes_requested no findings",
			in:   `{"schema_version":"1","verdict":"changes_requested"}`,
			want: "requires at least one finding",
		},
		{
			name: "bad severity",
			in:   `{"schema_version":"1","verdict":"changes_requested","findings":[{"id":"F1","severity":"critical","detail":"x"}]}`,
			want: "findings[0].severity",
		},
		{
			name: "malformed json",
			in:   `{not json`,
			want: "invalid JSON",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseVerdictJSON([]byte(testCase.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestParseVerdictJSON_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()
	// Future/unknown fields must not break parsing.
	verdict, err := ParseVerdictJSON([]byte(`{"schema_version":"1","verdict":"approved","future":{"x":1},"_meta":"z"}`))
	if err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if verdict.Verdict != VerdictApproved {
		t.Errorf("verdict = %q, want approved", verdict.Verdict)
	}
}

func TestParseVerdictJSON_AllSeverities(t *testing.T) {
	t.Parallel()
	// Every member of the closed severity set must parse on a changes_requested
	// verdict (which requires findings).
	in := `{
  "schema_version": "1",
  "verdict": "changes_requested",
  "findings": [
    {"id": "F1", "severity": "blocker", "detail": "a"},
    {"id": "F2", "severity": "major", "detail": "b"},
    {"id": "F3", "severity": "minor", "detail": "c"}
  ]
}`
	verdict, err := ParseVerdictJSON([]byte(in))
	if err != nil {
		t.Fatalf("all severities: %v", err)
	}
	if len(verdict.Findings) != 3 {
		t.Fatalf("findings len = %d, want 3", len(verdict.Findings))
	}
}
