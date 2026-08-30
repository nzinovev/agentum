// Package routing renders the orchestrator-owned routing block prepended to a
// stage's role-pure prompt (C2). The block tells the agent its role without
// baking orchestration concerns into the prompt itself: which stage and gate it
// is running, where to write its structured result, what prior stages produced,
// and (later) injected memory and granted capabilities.
//
// The result.json contract preamble is orchestrator-owned, not pack-owned (C6
// vendor neutrality): every adapter enforces the same schema, and Render emits
// the preamble regardless of pack or agent. Memory and capabilities sections
// are inert stubs here — their renderers slot in when Epic 1 / Epic 6 land,
// with no change to the runner.
//
// The block text lives in template.md next to this file (embedded at build)
// so the prompt can be edited without touching Go; the contract stays in code
// only via the Block fields the runner fills in.
package routing

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
)

// Block is the input to Render: the per-invocation context the runner assembles.
type Block struct {
	TaskID      string // the task this invocation belongs to
	ProjectName string // human-readable project name
	Stage       string // the stage id from the pack (e.g. "spec", "implement")
	Gate        string // the stage's gate value (one of the six C11 values)
	ArtifactDir string // absolute path; the agent writes result.json here

	// Title and Description are the task request: the requested behaviour, source
	// of truth #1 for every stage. Rendered as the block's first section so the
	// run's purpose precedes its contract. Deliberately no Overrides field here —
	// the overrides are orchestrator-only by construction and anything in them
	// must never reach the model; the resolved ## Project checks section already
	// renders the effective set.
	Title       string
	Description string

	// PriorStages are earlier stages' result.json paths, for cross-stage
	// reference (filesystem-as-bus, C1). Empty for the first stage.
	PriorStages []PriorStage

	// Memory is the rendered "project decisions, most recent first" block.
	// Empty string renders an inert stub; Epic 1 fills it.
	Memory string

	// Capabilities are the pack∩stage capability subset granted to this
	// invocation. Empty renders an inert stub; Epic 6 enforces them.
	Capabilities []string

	// Checks are the resolved project checks the orchestrator will run itself at
	// the delivery boundary (ADR 0002 D8). They are rendered so the agent knows
	// the build/test commands and can run them to check its own work — hiding
	// them is unenforceable (.agentum.yaml is fs.read-able at the repo root) and
	// harmful (an implementer that knows the commands saves a review/fix cycle).
	// The agent learns WHAT the checks are; it cannot change WHICH checks gate
	// delivery. Empty renders nothing. The runner maps checks.Item onto this
	// plain struct so routing has no checks-package import.
	Checks []CheckRef

	// VerdictPath is the absolute path to the verdict.json a verdict-sourcing
	// stage must write. Empty for a stage that does not branch on verdict; when
	// set, the template renders the verdict contract (the schema + the path),
	// so every adapter enforces the same shape and packs do not restate it.
	// Detected via pack.Stage.SourcesVerdict(), never by substring-scanning.
	VerdictPath string

	// PlanPath is the absolute path to the plan.md a plan-sourcing stage must
	// write (ADR 0003 D2). Empty unless this stage is the pack's approval
	// stage; when set, the template renders the plan contract so the agent
	// writes its Planning Bundle to the exact path the orchestrator captures as
	// an immutable revision and the human approves.
	PlanPath string

	// ApprovedPlan points every stage after the approval at the approved plan
	// revision, so the implementer and the reviewer read the exact revision the
	// human approved (ADR 0003 D2). Nil before the approval or for a pack with
	// no approval block.
	ApprovedPlan *PlanRef

	// Diff points a reviewer-role stage (and the final gate) at the
	// orchestrator-produced diff against the task's base commit (ADR 0003 D5).
	// Nil for non-reviewer stages; the reviewer reads the real change set with
	// fs.read alone, since RoleReviewer grants no exec.bash to run git itself.
	Diff *DiffRef

	// ReviewFindings, when set, points a stage entered through a verdict-
	// conditioned transition at the predecessor's findings artifact, so the
	// fixer reads structured findings rather than a log. Nil for a stage not
	// entered via a verdict edge.
	ReviewFindings *ReviewRef
}

// PlanRef points a post-approval stage at the approved plan. Path is the
// absolute worktree location of the materialized revision; RevisionID lets an
// agent or a human verify the exact revision without re-reading the file.
type PlanRef struct {
	Stage       string // the approval stage id whose plan.md was approved
	Path        string // absolute path to the materialized plan.md
	RevisionID  string // the immutable revision id the human approved
	ContentHash string // sha256 hex of the approved content, for quick comparison
}

// DiffRef points a reviewer at the orchestrator-produced change set (ADR 0003
// D5). The patch is capped (1 MiB) with an explicit truncation marker on a hunk
// boundary; the stat is never truncated. Both are immutable revisions outside
// any worktree, so they survive teardown and stay reviewable.
type DiffRef struct {
	PatchPath       string // absolute path to the materialized diff.patch
	StatPath        string // absolute path to the materialized diff.stat
	PatchRevisionID string
	StatRevisionID  string
	BaseCommit      string // the base_commit the diff is against
	HeadCommit      string // the checkpoint the diff runs to
	Truncated       bool   // true when the patch was capped at the 1 MiB limit
}

// ReviewRef points a fixer stage at the reviewer's findings. The count lets the
// routing block say how many findings to expect without the agent re-reading
// the whole artifact to count them.
type ReviewRef struct {
	Stage string // the reviewer stage id whose verdict produced the findings
	Path  string // absolute path to the reviewer's verdict.json
	Count int    // number of findings in the verdict
}

// CheckRef is one resolved project check rendered into the routing block (ADR
// 0002 D8). A plain struct on purpose — routing must not import the checks
// package, so the runner maps checks.Item onto this shape at render time.
type CheckRef struct {
	Name        string   // the check name (resolved, not raw)
	Command     []string // the arg vector (no shell)
	Description string   // human-readable purpose, when set
	Required    bool     // true for a mandatory (baseline) check
}

// PriorStage is one earlier stage whose artifacts are referenceable.
type PriorStage struct {
	Stage string
	Path  string // absolute path to that stage's result.json
}

// templateText is the routing-block markdown. Embedded at build so the binary
// stays self-contained; editing the file requires a recompile, which keeps the
// orchestrator-owned result.json contract versioned with the code that depends
// on it.
//
//go:embed template.md
var templateText string

// blockTemplate is parsed once at package init. Must panics if the template is
// malformed — that is a compile-time-ish bug we want to surface immediately,
// not a runtime failure path for Render.
var blockTemplate = template.Must(template.New("routing").
	Funcs(template.FuncMap{"join": strings.Join}).
	Parse(templateText))

// Render produces the markdown routing block. It is deterministic and pure;
// the runner prepends it to the role-pure prompt from the pack.
//
// Execute against a strings.Builder cannot fail on the write side (Builder's
// Writer contract never errors), and a Block-shape mismatch would have been
// caught at parse time; the panic surfaces any template-walk bug rather than
// silently emitting a partial block.
func Render(block Block) string {
	var builder strings.Builder
	if err := blockTemplate.Execute(&builder, block); err != nil {
		panic(fmt.Sprintf("routing: execute template: %v", err))
	}
	return builder.String()
}
