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
package routing

import (
	"fmt"
	"strings"
)

// Block is the input to Render: the per-invocation context the runner assembles.
type Block struct {
	TaskID      string // the task this invocation belongs to
	ProjectName string // human-readable project name
	Stage       string // the stage id from the pack (e.g. "spec", "implement")
	Gate        string // the stage's gate value (one of the six C11 values)
	ArtifactDir string // absolute path; the agent writes result.json here

	// PriorStages are earlier stages' result.json paths, for cross-stage
	// reference (filesystem-as-bus, C1). Empty for the first stage.
	PriorStages []PriorStage

	// Memory is the rendered "project decisions, most recent first" block.
	// Empty string renders an inert stub; Epic 1 fills it.
	Memory string

	// Capabilities are the pack∩stage capability subset granted to this
	// invocation. Empty renders an inert stub; Epic 6 enforces them.
	Capabilities []string
}

// PriorStage is one earlier stage whose artifacts are referenceable.
type PriorStage struct {
	Stage string
	Path  string // absolute path to that stage's result.json
}

// Render produces the markdown routing block. It is deterministic and pure;
// the runner prepends it to the role-pure prompt from the pack.
func Render(b Block) string {
	var sb strings.Builder
	fmt.Fprintln(&sb, "# Agentum routing block")
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "You are running as stage **%s** (gate: %s) in task %s on project %s.\n",
		b.Stage, b.Gate, b.TaskID, b.ProjectName)
	fmt.Fprintln(&sb)

	// Orchestrator-owned result.json contract — identical for every pack/agent.
	fmt.Fprintln(&sb, "## Your output contract (REQUIRED)")
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "Write your structured result to:\n  %s/result.json\n", b.ArtifactDir)
	fmt.Fprintln(&sb, "This file is the orchestrator's signal to advance, pause, or gate. It MUST be")
	fmt.Fprintln(&sb, "valid JSON with at minimum:")
	fmt.Fprintln(&sb, "- `schema_version`: \"1\"")
	fmt.Fprintln(&sb, "- `status`: \"complete\" | \"partial\" | \"blocked\"")
	fmt.Fprintln(&sb, "Optional fields (default empty): `summary`, `open_questions[]`, `artifacts[]`,")
	fmt.Fprintln(&sb, "`memory_writes[]`, `edit_targets[]`, `notes`.")
	fmt.Fprintln(&sb, "If you cannot complete, set `status: \"blocked\"` and list what you need in")
	fmt.Fprintln(&sb, "`open_questions`. Unknown fields are ignored (forward-compatible).")
	fmt.Fprintln(&sb)

	// Memory section — inert stub until Epic 1.
	fmt.Fprintln(&sb, "## Memory (project decisions, most recent first)")
	fmt.Fprintln(&sb)
	if strings.TrimSpace(b.Memory) != "" {
		fmt.Fprintln(&sb, b.Memory)
	} else {
		fmt.Fprintln(&sb, "_No project decisions injected yet._")
	}
	fmt.Fprintln(&sb)

	// Capabilities section — inert stub until Epic 6.
	fmt.Fprintln(&sb, "## Capabilities available")
	fmt.Fprintln(&sb)
	if len(b.Capabilities) > 0 {
		fmt.Fprintf(&sb, "Granted: %s\n", strings.Join(b.Capabilities, ", "))
	} else {
		fmt.Fprintln(&sb, "_No capabilities declared (agent uses its native defaults)._")
	}
	fmt.Fprintln(&sb)

	// Prior stage artifacts — filesystem-as-bus cross-references.
	if len(b.PriorStages) > 0 {
		fmt.Fprintln(&sb, "## Prior stage artifacts")
		fmt.Fprintln(&sb)
		for _, ps := range b.PriorStages {
			fmt.Fprintf(&sb, "- **%s**: %s\n", ps.Stage, ps.Path)
		}
		fmt.Fprintln(&sb)
	}

	return sb.String()
}
