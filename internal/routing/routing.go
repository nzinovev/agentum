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
