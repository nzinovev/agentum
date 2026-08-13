// Package pack defines the versioned pipeline-pack format (F.5).
//
// A pack is a directory containing a manifest.yaml plus a prompts/ tree of
// role-pure markdown system prompts. The manifest carries: pack identity
// (name, semver, persona), declared memory scopes, declared MCP capabilities,
// per-pack budgets (fix-loop and ask-to-edit recursion), a tier policy, and a
// named map of stages with explicit transitions.
//
// Stages are a named map (not an ordered list): each stage declares its own
// transitions, and the pack declares one entry stage. A transition carries a
// condition in the closed D1 grammar (see condition.go); a stage may fan out
// across several conditional edges with first-match-wins ordering. The
// validator enforces that a branching stage is total (a fallback or an
// exhaustive enum cover) and that any cycle is bounded by a fixer-role stage
// and a fix_cycles budget.
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
	Approvals    []Approval        `yaml:"approvals,omitempty"`
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

// Transition is a named edge to another stage. Condition is a term in the
// closed D1 grammar (see condition.go); empty means the unconditional fallback
// edge. A stage with several transitions fans out by condition with
// first-match-wins ordering.
type Transition struct {
	To        string `yaml:"to"`
	Condition string `yaml:"condition,omitempty"`
}

// EffectiveRole returns the capability role this stage runs under: the stage's
// explicit `role` when declared, otherwise the convention-derived role from the
// stage id. It mirrors caps.DeriveRole so the pack package can derive roles
// (for validation and for the runner's fixer list) without importing caps.
//
// The two derivations MUST agree on the fallback path; a drift test in
// internal/runner (the only package that imports both) asserts they do for the
// full convention-matching id space. Keep this switch in sync with
// caps.DeriveRole. The convention is a fallback, not a security boundary: a
// pack that needs a non-conventional role for an unusual stage declares it
// explicitly.
func EffectiveRole(stageID string, stage Stage) string {
	if stage.Role != "" {
		return stage.Role
	}
	return deriveRoleByID(stageID)
}

// deriveRoleByID is the convention fallback mirrored from caps.DeriveRole. See
// EffectiveRole for the sync requirement. Substring match (not tokenized) so
// "pre_review" and "review-v2" both match "review".
func deriveRoleByID(stageID string) string {
	switch {
	case stageID == "":
		return "analyst"
	case containsWord(stageID, "review"):
		return "reviewer"
	case containsWord(stageID, "implement"), containsWord(stageID, "build"):
		return "implementer"
	case containsWord(stageID, "fix"), containsWord(stageID, "patch"):
		return "fixer"
	default:
		return "analyst"
	}
}

// containsWord reports whether stageID contains word as a substring. Mirrors
// caps.containsWord; kept here so the role mirror is self-contained.
func containsWord(stageID, word string) bool {
	for index := 0; index+len(word) <= len(stageID); index++ {
		if stageID[index:index+len(word)] == word {
			return true
		}
	}
	return false
}

// Unlock is a closed-vocabulary name for what a human approval releases. v1 has
// exactly one member: source_write, the subtractive capability withholding that
// gates fs.write / git.write / exec.bash until a human approves the plan
// artifact. A pack with no approvals block behaves exactly as today — the
// withholding is never applied.
type Unlock string

const (
	// UnlockSourceWrite re-enables the source-writing capability categories for
	// every stage of the run. The runner records a durable task_approvals row
	// when a human advances past the approval stage, and removes the withholding
	// only when that row exists. See ADR 0003 D3.
	UnlockSourceWrite Unlock = "source_write"
)

// knownUnlocks is the closed set of approval unlock names. Inlined here for the
// same reason knownStageRoles is inlined in validate.go: the pack package stays
// a pure data format with no import of the runner's enforcement code.
var knownUnlocks = map[Unlock]bool{UnlockSourceWrite: true}

// Approval declares a human gate whose decision unlocks a capability subset for
// the rest of the run (ADR 0003 D3). The approval is recorded as a durable,
// orchestrator-owned task_approvals row keyed unique on (task, name), so a
// worker restart, a worktree restore, and a Restore to a checkpoint all leave
// the decision intact and a repeated approve is idempotent by construction.
type Approval struct {
	// Name is the approval's unique identifier and the key the durable
	// task_approvals row is keyed on. It is also the value the runner reads back
	// to decide whether the unlock applies.
	Name string `yaml:"name"`
	// Stage is the stage whose gate, when advanced past by a human, IS the
	// approval decision. Advancing past a stage that declares an approval writes
	// the approval row in the same transaction as the FSM transition (ADR 0003
	// D4) — there is no second verb for the same click.
	Stage string `yaml:"stage"`
	// Artifact names the file (relative to the stage's artifact dir) the
	// approval binds to. The runner captures it as an immutable revision and the
	// approval row stores the revision id, so "the implementer works within the
	// approved plan" is a property: a mismatch between the current revision and
	// the approved one is a controlled stop (plan_revision_drift). It must be a
	// bare file name — not a path — because it names a file inside the
	// orchestrator-constructed artifact dir.
	Artifact string `yaml:"artifact"`
	// Unlocks is the closed-vocabulary capability subset the approval releases.
	// v1 has one member, source_write; the field is a slice so a future approval
	// can release a narrower set without a model change.
	Unlocks string `yaml:"unlocks"`
}

// SourceWriteApproval returns the pack's source_write approval declaration, if
// any. A pack declares at most one such approval; the validator enforces
// uniqueness. The runner uses this to decide whether to apply the source-write
// withholding for the run and to resolve the approval row when a human advances
// past the approval stage.
func (p *Pack) SourceWriteApproval() (Approval, bool) {
	for _, approval := range p.Approvals {
		if Unlock(approval.Unlocks) == UnlockSourceWrite {
			return approval, true
		}
	}
	return Approval{}, false
}

// SourceWritingStages returns the ids of stages whose effective role is
// implementer or fixer — the roles that carry fs.write / git.write /
// exec.bash. The runner uses this to refuse entry to such stages when the
// source_write unlock is absent (layer 2 of the ADR 0003 D3 lock) and the
// validator uses it for the static reachability check (layer 3).
func (p *Pack) SourceWritingStages() []string {
	var sourceStages []string
	for stageID, stage := range p.Stages {
		role := EffectiveRole(stageID, stage)
		if role == "implementer" || role == "fixer" {
			sourceStages = append(sourceStages, stageID)
		}
	}
	return sourceStages
}

// FixerStages returns the ids of stages whose effective role is fixer. The
// runner uses this to compute the durable fix-cycle counter (MaxCycleForStages
// over the fixer set) and to guard fixer entry against budgets.fix_cycles.
func (p *Pack) FixerStages() []string {
	var fixerStages []string
	for stageID, stage := range p.Stages {
		if EffectiveRole(stageID, stage) == "fixer" {
			fixerStages = append(fixerStages, stageID)
		}
	}
	return fixerStages
}
