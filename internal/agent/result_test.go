package agent

import (
	"strings"
	"testing"
)

func TestParseResultJSON_ValidFull(t *testing.T) {
	t.Parallel()
	in := `{
  "schema_version": "1",
  "status": "complete",
  "summary": "wrote the spec",
  "open_questions": ["pick a db?"],
  "artifacts": [{"path": "specs/a.md", "kind": "spec"}],
  "memory_writes": [{"kind": "decision", "title": "use postgres", "body": "...", "keywords": ["db"]}],
  "edit_targets": ["src/a.ts:s"],
  "notes": "nit: rename",
  "future_field": "ignored"
}`
	r, err := ParseResultJSON([]byte(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.SchemaVersion != "1" || r.Status != StatusComplete {
		t.Errorf("schema/status = %q/%q, want 1/complete", r.SchemaVersion, r.Status)
	}
	if r.Summary != "wrote the spec" {
		t.Errorf("summary = %q", r.Summary)
	}
	if len(r.OpenQuestions) != 1 || len(r.MemoryWrites) != 1 || len(r.Artifacts) != 1 {
		t.Errorf("slice lengths = oq=%d mw=%d art=%d", len(r.OpenQuestions), len(r.MemoryWrites), len(r.Artifacts))
	}
	if !r.HasOpenQuestions() || !r.HasMemoryWrites() {
		t.Error("HasOpenQuestions/HasMemoryWrites misreported")
	}
}

func TestParseResultJSON_Minimal(t *testing.T) {
	t.Parallel()
	r, err := ParseResultJSON([]byte(`{"schema_version":"1","status":"blocked"}`))
	if err != nil {
		t.Fatalf("minimal valid: %v", err)
	}
	if r.Status != StatusBlocked || r.HasOpenQuestions() || r.HasMemoryWrites() {
		t.Errorf("minimal parse wrong: %+v", r)
	}
}

func TestParseResultJSON_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"missing schema_version", `{"status":"complete"}`, "schema_version is required"},
		{"missing status", `{"schema_version":"1"}`, "status is required"},
		{"unsupported schema_version", `{"schema_version":"2","status":"complete"}`, "unsupported"},
		{"bad status enum", `{"schema_version":"1","status":"magic"}`, "not one of"},
		{"bad memory kind", `{"schema_version":"1","status":"complete","memory_writes":[{"kind":"wish","title":"x","body":"y"}]}`, "memory_writes[0].kind"},
		{"malformed json", `{not json`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseResultJSON([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseResultJSON_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()
	// Future/unknown fields must not break parsing.
	r, err := ParseResultJSON([]byte(`{"schema_version":"1","status":"partial","future":{"x":1},"_meta":"z"}`))
	if err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
	if r.Status != StatusPartial {
		t.Errorf("status = %q, want partial", r.Status)
	}
}

func TestResultContractPreamble_ContainsPath(t *testing.T) {
	t.Parallel()
	got := ResultContractPreamble("/wt/.agentum/wt1/.ag-artifacts/spec")
	if !strings.Contains(got, "/wt/.agentum/wt1/.ag-artifacts/spec/result.json") {
		t.Errorf("preamble missing the artifact path; got:\n%s", got)
	}
	if !strings.Contains(got, "schema_version") || !strings.Contains(got, "complete | partial | blocked") {
		t.Errorf("preamble missing schema; got:\n%s", got)
	}
}
