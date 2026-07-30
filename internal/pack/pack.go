// Package pack defines the versioned pipeline-pack format (F.5).
//
// A pack is a directory containing a manifest.yaml plus a prompts/ tree of
// role-pure markdown system prompts. The manifest carries: pack identity
// (name, semver, persona), declared memory scopes, declared MCP capabilities,
// per-pack budgets (fix-loop and ask-to-edit recursion), a tier policy, and a
// named map of stages with explicit transitions.
//
// Stages are a named map (not an ordered list): each stage declares its own
// transitions, and the pack declares one entry stage. This is the substrate for
// conditional-linear routing (Epic 4): a transition grows an opaque condition
// field and a stage may fan out. We do not build a DAG.
//
// This is PR 1 of F.5: types, loader, validator. Override-layer resolution
// (lock-major / fork / override-prompts / override-params) lands in PR 2.
package pack

// APIVersion is the manifest api: value this build understands.
const APIVersion = "agentum/v1"

// Gate is the per-stage control vocabulary (carryover C11). The engine and the
// gate surface agree on these six values.
type Gate string

const (
	GateAuto           Gate = "auto"
	GateAutoIfClean    Gate = "auto_if_clean"
	GateAutoOnApproval Gate = "auto_on_approval"
	GateHumanApproval  Gate = "human_approval"
	GateHumanFinal     Gate = "human_final"
	GateHumanEdit      Gate = "human_edit"
)

// MemoryScope is one of the three memory scopes. Only project is wired into
// retrieval at the dogfooding MVP; user is light, org is inert.
type MemoryScope string

const (
	ScopeProject MemoryScope = "project"
	ScopeUser    MemoryScope = "user"
	ScopeOrg     MemoryScope = "org"
)

// Pack is the in-memory, loaded-and-validated representation of a pack
// directory. PromptText holds the file contents read by the loader.
type Pack struct {
	API          string            `yaml:"api"`
	Pack         Meta              `yaml:"pack"`
	Memory       Memory            `yaml:"memory"`
	Capabilities []string          `yaml:"capabilities"`
	Budgets      Budgets           `yaml:"budgets"`
	Tiers        Tiers             `yaml:"tiers"`
	Checks       CheckPolicy       `yaml:"checks"`
	Entry        string            `yaml:"entry"`
	Stages       map[string]Stage  `yaml:"stages"`
	PromptText   map[string]string `yaml:"-"` // keyed by stage id; populated by Load

	// Dir is the absolute path the pack was loaded from. Empty for packs built
	// in memory by the override resolver.
	Dir string `yaml:"-"`

	// BaseRef records the base reference this pack was resolved from, when
	// produced by Resolve (e.g. "java-spring@^1"). Empty for a directly-loaded
	// pack.
	BaseRef string `yaml:"-"`

	// Forked records layer 2 (detach from upstream). It is metadata only at
	// resolve time — a forked pack is a detached copy, not a different shape.
	Forked bool `yaml:"-"`
}

// Meta is the pack identity block.
type Meta struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"` // semver MAJOR.MINOR.PATCH
	Persona     string `yaml:"persona"`
	Description string `yaml:"description,omitempty"`
}

// Memory declares which scopes the pack reads and whether it writes.
type Memory struct {
	Reads  []MemoryScope `yaml:"reads"`
	Writes bool          `yaml:"writes"`
}

// Budgets carries the per-pack recursion caps. fix_cycles replaces the
// hardcoded MaxFixCycles = 3 (L6); ask_to_edit is the scoped-edit recursion
// budget (design §3.7). Unit choices (cycles vs tokens vs cost) are Epic 3.4.
type Budgets struct {
	FixCycles int `yaml:"fix_cycles"`
	AskToEdit int `yaml:"ask_to_edit"`
}

// Tiers is the model-tier policy. Default is the fallback tier name; a stage
// may override it via Stage.Tier. Tier names are opaque here — concrete
// model ids resolve via the BYO-models config (F.4) in Epic 3.
type Tiers struct {
	Default string `yaml:"default"`
}

// CheckPolicy is the pack's contribution to the project-check set. A pack may
// add checks to the effective set *by name only* — the names must exist in the
// project's versioned registry (.agentum.yaml); the runner's checks.Resolve
// rejects unknown names before any execution. Required names are mandatory (a
// failure blocks delivery); Optional names run but do not block. Neither can
// supply a command, remove a project baseline check, or weaken an already-
// mandatory check — mandatory is monotonic across the project baseline, the
// pack, and the task.
type CheckPolicy struct {
	Required []string `yaml:"required,omitempty"`
	Optional []string `yaml:"optional,omitempty"`
}

// Stage is one agent-invocation step in the pipeline. Non-terminal stages have
// a prompt; terminal stages (no transitions) are engine states and omit it.
type Stage struct {
	Gate        Gate         `yaml:"gate"`
	Prompt      string       `yaml:"prompt,omitempty"` // file path relative to the pack dir
	Tier        string       `yaml:"tier,omitempty"`   // optional; overrides Tiers.Default
	Transitions []Transition `yaml:"transitions,omitempty"`

	// Role is the optional capability-profile selector for this stage. One of
	// analyst | reviewer | implementer | fixer. When absent, the runner derives
	// it from the stage id by convention (spec/analyze→analyst, review→
	// reviewer, implement→implementer, fix→fixer; default analyst). The role
	// selects the baseline capability template; caps.Effective still intersects
	// it with pack ∩ stage ∩ host. See internal/caps.
	Role string `yaml:"role,omitempty"`

	// Capabilities is the optional stage-level subset that narrows the pack's
	// declared capabilities for this stage. Absent means "inherit the pack
	// set"; present means "this stage allows only these." Each entry is a
	// scope-less category (e.g. "fs.read", "fs.write", "git.read") or a named
	// entity ("secret.github_token", "mcp.github").
	Capabilities []string `yaml:"capabilities,omitempty"`

	// promptText is the loaded prompt file contents. Unexported so the override
	// resolver can only set it via the loader's discipline.
	promptText string
}

// PromptText returns the loaded prompt contents for this stage.
func (s Stage) PromptText() string { return s.promptText }

// setPromptText is used by the loader (and, later, the override resolver) to
// attach file contents after reading.
func (s *Stage) setPromptText(t string) { s.promptText = t }

// Terminal reports whether this stage has no outgoing transitions.
func (s Stage) Terminal() bool { return len(s.Transitions) == 0 }

// Transition is a named edge to another stage. Condition is opaque until the
// Epic 4 evaluator lands; a stage with multiple transitions fans out by
// condition.
type Transition struct {
	To        string `yaml:"to"`
	Condition string `yaml:"condition,omitempty"`
}
