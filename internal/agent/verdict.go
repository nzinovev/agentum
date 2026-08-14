package agent

import (
	"encoding/json"
	"fmt"
)

// VerdictSchemaVersion is the verdict.json schema version this build parses.
const VerdictSchemaVersion = "1"

// VerdictFileName is the well-known filename a reviewer stage writes next to
// its result.json. The orchestrator constructs the path (no containment check
// needed) and reads it as the routing input a verdict condition matches on.
const VerdictFileName = "verdict.json"

// Verdict is the reviewer's routing verdict, parsed by the orchestrator (not
// the agent's prose). It is the only signal that can move a pipeline past a
// verdict-conditioned transition: result.json.summary cannot.
type Verdict string

const (
	VerdictApproved         Verdict = "approved"
	VerdictChangesRequested Verdict = "changes_requested"
)

// Severity is one finding's severity. The closed set matches docs/agent-
// contract.md; ParseVerdictJSON rejects anything else.
type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityMajor   Severity = "major"
	SeverityMinor   Severity = "minor"
)

// FindingCategory is the optional advisory taxonomy a reviewer may attach to a
// finding (ADR 0003 D9.5). It is recorded and rendered, never routed on: the
// "plan defect must not go to the fixer" rule is enforced by status: "blocked"
// + open_questions, which Evaluate handles before any transition. The category
// exists so the orchestrator can read back what kind of finding each one is
// without parsing prose.
type FindingCategory string

const (
	// CategoryImplementationDefect — code does not correctly satisfy an
	// acceptance criterion or repository invariant.
	CategoryImplementationDefect FindingCategory = "implementation_defect"
	// CategoryPlanDeviation — implementation materially departed from the
	// approved plan without a justified equivalent solution.
	CategoryPlanDeviation FindingCategory = "plan_deviation"
	// CategoryPlanDefect — the approved plan or acceptance criteria are
	// themselves inconsistent with the task or repository.
	CategoryPlanDefect FindingCategory = "plan_defect"
	// CategoryRequirementAmbiguity — newly discovered ambiguity requires a human
	// product or architectural decision.
	CategoryRequirementAmbiguity FindingCategory = "requirement_ambiguity"
)

// knownFindingCategories is the closed set ParseVerdictJSON validates against
// when category is present. Empty/absent category is always allowed.
var knownFindingCategories = map[FindingCategory]bool{
	CategoryImplementationDefect: true,
	CategoryPlanDeviation:        true,
	CategoryPlanDefect:           true,
	CategoryRequirementAmbiguity: true,
}

// Finding is one concrete change request the fixer should act on. Path and
// Detail give the fixer a target; ID lets the fixer reference it in its own
// result. Line is 1-based; 0 means "unspecified". Category is optional and
// advisory (ADR 0003 D9.5) — recorded and rendered, never routed on.
type Finding struct {
	ID       string          `json:"id"`
	Severity Severity        `json:"severity"`
	Path     string          `json:"path,omitempty"`
	Line     int             `json:"line,omitempty"`
	Detail   string          `json:"detail"`
	Category FindingCategory `json:"category,omitempty"`
}

// VerdictJSON is the file-derived reviewer verdict, parsed from
// <ArtifactDir>/verdict.json. See docs/agent-contract.md for the contract.
type VerdictJSON struct {
	SchemaVersion string    `json:"schema_version"`
	Verdict       Verdict   `json:"verdict"`
	Summary       string    `json:"summary,omitempty"`
	Findings      []Finding `json:"findings,omitempty"`
}

// rawVerdictJSON mirrors VerdictJSON but uses json.RawMessage / pointer fields
// for values we validate strictly, so a wrong type yields a precise error
// rather than a generic unmarshal failure. Mirrors rawResultJSON's discipline.
type rawVerdictJSON struct {
	SchemaVersion *string   `json:"schema_version"`
	Verdict       *string   `json:"verdict"`
	Summary       *string   `json:"summary"`
	Findings      []Finding `json:"findings"`
}

// ParseVerdictJSON strict-parses verdict.json bytes per docs/agent-contract.md:
//
//   - schema_version and verdict are required and must be valid.
//   - severity on each finding must be one of {blocker, major, minor}.
//   - Unknown fields are ignored (forward-compatible).
//   - changes_requested with zero findings is a contract violation: a fixer
//     with no findings has nothing to act on, exactly like a malformed
//     result.json.
//
// A returned error means the reviewer violated the contract; callers surface
// it as the verdict_unreadable retryable stop-point.
func ParseVerdictJSON(data []byte) (VerdictJSON, error) {
	var raw rawVerdictJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return VerdictJSON{}, fmt.Errorf("verdict.json: invalid JSON: %w", err)
	}
	if raw.SchemaVersion == nil {
		return VerdictJSON{}, fmt.Errorf("verdict.json: schema_version is required")
	}
	if *raw.SchemaVersion != VerdictSchemaVersion {
		return VerdictJSON{}, fmt.Errorf("verdict.json: schema_version %q unsupported (want %q)", *raw.SchemaVersion, VerdictSchemaVersion)
	}
	if raw.Verdict == nil {
		return VerdictJSON{}, fmt.Errorf("verdict.json: verdict is required")
	}
	switch Verdict(*raw.Verdict) {
	case VerdictApproved, VerdictChangesRequested:
	default:
		return VerdictJSON{}, fmt.Errorf("verdict.json: verdict %q is not one of {approved, changes_requested}", *raw.Verdict)
	}

	out := VerdictJSON{
		SchemaVersion: *raw.SchemaVersion,
		Verdict:       Verdict(*raw.Verdict),
		Findings:      raw.Findings,
	}
	if raw.Summary != nil {
		out.Summary = *raw.Summary
	}
	for index, finding := range raw.Findings {
		switch finding.Severity {
		case SeverityBlocker, SeverityMajor, SeverityMinor:
		default:
			return VerdictJSON{}, fmt.Errorf("verdict.json: findings[%d].severity %q is not one of {blocker, major, minor}", index, finding.Severity)
		}
		// category is optional (ADR 0003 D9.5); when present it must be a known
		// member of the advisory taxonomy. An unknown value is a contract
		// violation rather than a silent drop, so a typo does not masquerade as
		// "no category".
		if finding.Category != "" && !knownFindingCategories[finding.Category] {
			return VerdictJSON{}, fmt.Errorf("verdict.json: findings[%d].category %q is not one of {implementation_defect, plan_deviation, plan_defect, requirement_ambiguity}", index, finding.Category)
		}
	}
	// A changes_requested verdict with no findings is a contract violation: the
	// fixer would be pointed at an empty list and have nothing to act on. The
	// reviewer must either list concrete findings or return approved.
	if out.Verdict == VerdictChangesRequested && len(out.Findings) == 0 {
		return VerdictJSON{}, fmt.Errorf("verdict.json: verdict %q requires at least one finding", out.Verdict)
	}
	return out, nil
}
