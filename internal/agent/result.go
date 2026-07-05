package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResultSchemaVersion is the result.json schema version this build parses.
const ResultSchemaVersion = "1"

// Status is the stage outcome the agent reports in result.json.
type Status string

const (
	StatusComplete Status = "complete"
	StatusPartial  Status = "partial"
	StatusBlocked  Status = "blocked"
)

// MemoryKind is the kind of a proposed memory entry. Must match the
// memory_entries.kind column values.
type MemoryKind string

const (
	MemDecision   MemoryKind = "decision"
	MemConvention MemoryKind = "convention"
	MemSpecRef    MemoryKind = "spec_ref"
	MemFix        MemoryKind = "fix"
	MemNote       MemoryKind = "note"
)

// ResultJSON is the file-derived portion of Result, parsed from
// <ArtifactDir>/result.json. See docs/agent-contract.md for the contract.
type ResultJSON struct {
	SchemaVersion string        `json:"schema_version"`
	Status        Status        `json:"status"`
	Summary       string        `json:"summary,omitempty"`
	OpenQuestions []string      `json:"open_questions,omitempty"`
	Artifacts     []Artifact    `json:"artifacts,omitempty"`
	MemoryWrites  []MemoryWrite `json:"memory_writes,omitempty"`
	EditTargets   []string      `json:"edit_targets,omitempty"`
	Notes         string        `json:"notes,omitempty"`
}

// Artifact is one produced file/reference.
type Artifact struct {
	Path string `json:"path"`
	Kind string `json:"kind,omitempty"`
}

// MemoryWrite is a proposed memory entry. Committed only at final task
// approval; until then it is staged.
type MemoryWrite struct {
	Kind     MemoryKind `json:"kind"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Keywords []string   `json:"keywords,omitempty"`
}

// rawResultJSON mirrors ResultJSON but uses json.RawMessage for fields we
// validate strictly, so a wrong type yields a precise error rather than a
// generic unmarshal failure.
type rawResultJSON struct {
	SchemaVersion *string       `json:"schema_version"`
	Status        *string       `json:"status"`
	Summary       *string       `json:"summary"`
	OpenQuestions []string      `json:"open_questions"`
	Artifacts     []Artifact    `json:"artifacts"`
	MemoryWrites  []MemoryWrite `json:"memory_writes"`
	EditTargets   []string      `json:"edit_targets"`
	Notes         *string       `json:"notes"`
}

// ParseResultJSON strict-parses result.json bytes per docs/agent-contract.md:
//
//   - schema_version and status are required and must be valid.
//   - Known fields are strictly typed when present (status ∈ the enum;
//     memory_writes[*].kind ∈ the enum).
//   - Unknown fields are ignored (forward-compatible).
//
// A returned error means the agent violated the contract; callers surface it
// as a retryable stop-point.
func ParseResultJSON(data []byte) (ResultJSON, error) {
	// Reject unknown-on-top-level? No — the contract says unknown fields are
	// ignored. json.Unmarshal already ignores unknown keys by default.
	var raw rawResultJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return ResultJSON{}, fmt.Errorf("result.json: invalid JSON: %w", err)
	}
	if raw.SchemaVersion == nil {
		return ResultJSON{}, fmt.Errorf("result.json: schema_version is required")
	}
	if *raw.SchemaVersion != ResultSchemaVersion {
		return ResultJSON{}, fmt.Errorf("result.json: schema_version %q unsupported (want %q)", *raw.SchemaVersion, ResultSchemaVersion)
	}
	if raw.Status == nil {
		return ResultJSON{}, fmt.Errorf("result.json: status is required")
	}
	switch Status(*raw.Status) {
	case StatusComplete, StatusPartial, StatusBlocked:
	default:
		return ResultJSON{}, fmt.Errorf("result.json: status %q is not one of {complete, partial, blocked}", *raw.Status)
	}

	out := ResultJSON{
		SchemaVersion: *raw.SchemaVersion,
		Status:        Status(*raw.Status),
		OpenQuestions: raw.OpenQuestions,
		Artifacts:     raw.Artifacts,
		EditTargets:   raw.EditTargets,
	}
	if raw.Summary != nil {
		out.Summary = *raw.Summary
	}
	if raw.Notes != nil {
		out.Notes = *raw.Notes
	}
	for i, mw := range raw.MemoryWrites {
		switch mw.Kind {
		case MemDecision, MemConvention, MemSpecRef, MemFix, MemNote:
		default:
			return ResultJSON{}, fmt.Errorf("result.json: memory_writes[%d].kind %q is not one of {decision, convention, spec_ref, fix, note}", i, mw.Kind)
		}
		out.MemoryWrites = append(out.MemoryWrites, mw)
	}
	return out, nil
}

// HasOpenQuestions reports whether the result asks a human anything. This is
// the gate's primary stop signal.
func (r ResultJSON) HasOpenQuestions() bool { return len(r.OpenQuestions) > 0 }

// HasMemoryWrites reports whether the result staged any decision-trail entries.
func (r ResultJSON) HasMemoryWrites() bool { return len(r.MemoryWrites) > 0 }

// ResultContractPreamble is the routing-block fragment that tells the agent
// where to write result.json and what schema to follow. The runner includes
// this in the rendered routing block; it lives here so the contract has one
// source of truth.
func ResultContractPreamble(absArtifactDir string) string {
	var b strings.Builder
	b.WriteString("\n\n## Structured result\n\n")
	b.WriteString("When you finish, write your structured result as JSON to:\n  ")
	b.WriteString(absArtifactDir)
	b.WriteString("/result.json\n\n")
	b.WriteString("Schema (schema_version and status are required):\n")
	b.WriteString("```json\n")
	b.WriteString(`{
  "schema_version": "1",
  "status": "complete | partial | blocked",
  "summary": "short summary of what you did",
  "open_questions": ["..."],
  "artifacts": [{"path": "...", "kind": "spec|code|adr|..."}],
  "memory_writes": [{"kind": "decision", "title": "...", "body": "...", "keywords": ["..."]}],
  "edit_targets": ["..."],
  "notes": "..."
}`)
	b.WriteString("\n```\n")
	b.WriteString("- status must be one of: complete, partial, blocked.\n")
	b.WriteString("- memory_writes.kind must be one of: decision, convention, spec_ref, fix, note.\n")
	b.WriteString("- Unknown fields are ignored. Absent optionals default empty.\n")
	return b.String()
}
