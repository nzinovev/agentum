package policy

import (
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/pack"
)

// fullHostCaps is the opencode adapter's declared set (enforce.go): eight
// categories, secret included, mcp/skill excluded.
func fullHostCaps() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret",
	}
}

// floorPack builds a pack in the backend-development shape: an approval-gated
// plan, implement and fix source-writing stages, a verdict-conditional review
// loop. The builder variants below mutate one aspect per case.
func floorPack() *pack.Pack {
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "probe", Version: "1.0.0"},
		Capabilities: []string{"fs.read", "fs.write", "git.read", "exec.bash"},
		Budgets:      pack.Budgets{FixCycles: 2, AskToEdit: 3},
		Tiers:        pack.Tiers{Default: "fast"},
		Entry:        "plan",
		Approvals:    []pack.Approval{{Name: "plan", Stage: "plan", Artifact: "plan.md", Unlocks: "source_write"}},
		Stages: map[string]pack.Stage{
			"plan":      {Gate: pack.GateHumanApproval, Role: "analyst", Prompt: "plan.md", Transitions: []pack.Transition{{To: "implement"}}},
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md", Transitions: []pack.Transition{{To: "review"}}},
			"review": {Gate: pack.GateAuto, Role: "reviewer", Prompt: "rev.md", Transitions: []pack.Transition{
				{To: "fix", Condition: `verdict == "changes_requested"`},
				{To: "done", Condition: `verdict == "approved"`},
			}},
			"fix":  {Gate: pack.GateAuto, Role: "fixer", Prompt: "fix.md", Transitions: []pack.Transition{{To: "review"}}},
			"done": {},
		},
		PromptText: map[string]string{"plan": "p", "implement": "i", "review": "r", "fix": "f"},
	}
}

func ruleIDs(violations []Violation) []string {
	ids := make([]string, 0, len(violations))
	for _, violation := range violations {
		ids = append(ids, violation.Rule)
	}
	return ids
}

func hasViolation(violations []Violation, rule string, layer Layer) bool {
	for _, violation := range violations {
		if violation.Rule == rule && violation.Layer == layer {
			return true
		}
	}
	return false
}

// registryWith builds a parsed registry carrying one definition.
func registryWith(t *testing.T, name string, required bool) *checks.Registry {
	t.Helper()
	req := ""
	if required {
		req = "true"
	}
	raw := "api: agentum/v1\nchecks:\n  - name: " + name + "\n    command: [make, check]\n    required: " + req + "\n"
	registry, err := checks.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	return registry
}

func TestFloor_CleanPackPassesEveryRule(t *testing.T) {
	t.Parallel()
	floor := NewFloor(fullHostCaps())
	violations := floor.Check(Target{Pack: floorPack(), Origin: pack.OriginProject})
	if len(violations) != 0 {
		t.Fatalf("violations = %v, want none", violations)
	}
}

// Rule 1, missing approval: the pack source-writes with no human gate in
// front; the layer names the manifest that declared the graph.
func TestFloor_Rule1_NoApprovalBeforeSourceWrite(t *testing.T) {
	t.Parallel()
	noApproval := floorPack()
	noApproval.Approvals = nil
	floor := NewFloor(fullHostCaps())
	violations := floor.Check(Target{Pack: noApproval, Origin: pack.OriginProject})
	if !hasViolation(violations, RuleApprovalPrecedesSourceWrite, LayerProject) {
		t.Fatalf("violations = %v, want rule 1 with layer %q", violations, LayerProject)
	}
}

// Rule 1, leaked path: a prep stage before the approval fans out straight to
// the implementer, so a path from entry reaches source-writing work without
// traversing the approval gate.
func TestFloor_Rule1_SourceWriteStageLeaksAroundApproval(t *testing.T) {
	t.Parallel()
	leaky := floorPack()
	leaky.Entry = "prep"
	leaky.Stages["prep"] = pack.Stage{
		Gate: pack.GateAuto, Role: "analyst", Prompt: "prep.md",
		Transitions: []pack.Transition{{To: "implement"}, {To: "plan"}},
	}
	leaky.PromptText["prep"] = "prep"
	floor := NewFloor(fullHostCaps())
	violations := floor.Check(Target{Pack: leaky, Origin: pack.OriginProject})
	found := false
	for _, violation := range violations {
		if violation.Rule == RuleApprovalPrecedesSourceWrite && containsSubstring(violation.Message, `"implement"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("violations = %v, want rule 1 naming the leaked implement stage", violations)
	}
}

// Rule 1(c), the auto-gated approval stage: the approval is declared, every
// path passes it, and the pack is valid — but an auto gate lets the runner
// advance past it with no approval row, stranding the run at the implementer.
// The layer comes from the overrides provenance when the gate was patched.
func TestFloor_Rule1_AutoGatedApprovalStage(t *testing.T) {
	t.Parallel()
	autoGated := floorPack()
	autoGated.Stages["plan"] = pack.Stage{
		Gate: pack.GateAuto, Role: "analyst", Prompt: "plan.md",
		Transitions: []pack.Transition{{To: "implement"}},
	}
	floor := NewFloor(fullHostCaps())

	// A project manifest: the layer is the project.
	violations := floor.Check(Target{Pack: autoGated, Origin: pack.OriginProject})
	if !hasViolation(violations, RuleApprovalPrecedesSourceWrite, LayerProject) {
		t.Fatalf("violations = %v, want rule 1 naming the auto-gated approval stage", violations)
	}

	// The same gate arriving through an overrides patch: the provenance map
	// attributes it to the project layer even though the graph is builtin.
	violations = floor.Check(Target{
		Pack:         autoGated,
		Origin:       pack.OriginProjectOverBuiltin,
		FieldOrigins: map[string]string{"stages.plan.gate": "project"},
	})
	if !hasViolation(violations, RuleApprovalPrecedesSourceWrite, LayerProject) {
		t.Fatalf("violations = %v, want rule 1 attributing the patched gate to the project layer", violations)
	}

	// The same gate in the builtin base: the layer is builtin.
	violations = floor.Check(Target{Pack: autoGated, Origin: pack.OriginBuiltin})
	if !hasViolation(violations, RuleApprovalPrecedesSourceWrite, LayerBuiltin) {
		t.Fatalf("violations = %v, want rule 1 attributing the builtin gate to the builtin layer", violations)
	}
}

// Rule 2 restates an invariant pack validation already enforces (a reachable
// terminal exists; terminals carry no prompt), under the floor's name. The
// test pins both sides of that statement: the rule fires on a broken graph,
// and the shipped packs satisfy it.
func TestFloor_Rule2_InvariantHoldsForShapedPacks(t *testing.T) {
	t.Parallel()
	floor := NewFloor(fullHostCaps())
	if violations := floor.Check(Target{Pack: floorPack(), Origin: pack.OriginProject}); hasViolation(violations, RuleFinalReviewRequired, "") {
		t.Fatalf("violations = %v, a well-formed pack must satisfy rule 2", violations)
	}
	// A pack whose only reachable stages form a loop with no exit: rule 2's
	// own walk reports it even before the validator does.
	looped := floorPack()
	looped.Stages["review"] = pack.Stage{
		Gate: pack.GateAuto, Role: "reviewer", Prompt: "rev.md",
		Transitions: []pack.Transition{{To: "fix", Condition: `verdict == "changes_requested"`}, {To: "review", Condition: `verdict == "approved"`}},
	}
	delete(looped.Stages, "done")
	violations := floor.Check(Target{Pack: looped, Origin: pack.OriginProject})
	if !hasViolation(violations, RuleFinalReviewRequired, LayerProject) {
		t.Fatalf("violations = %v, want rule 2 naming the missing terminal", violations)
	}
}

// Rule 3: unknown names are refused early (the run-time refusal moved before
// the run starts), and a registry-required check in the pack's optional list
// is the named refusal of the one expressible downgrade attempt.
func TestFloor_Rule3_MandatoryChecks(t *testing.T) {
	t.Parallel()
	floor := NewFloor(fullHostCaps())

	withUnknown := floorPack()
	withUnknown.Checks.Required = []string{"no-such-check"}
	violations := floor.Check(Target{Pack: withUnknown, Origin: pack.OriginProject, Registry: registryWith(t, "go-test", false)})
	if !hasViolation(violations, RuleMandatoryChecksImmutable, LayerProject) {
		t.Fatalf("violations = %v, want rule 3 naming the unknown check", violations)
	}

	downgrade := floorPack()
	downgrade.Checks.Optional = []string{"go-test"}
	violations = floor.Check(Target{Pack: downgrade, Origin: pack.OriginProject, Registry: registryWith(t, "go-test", true)})
	if !hasViolation(violations, RuleMandatoryChecksImmutable, LayerProject) {
		t.Fatalf("violations = %v, want rule 3 refusing the mandatory-check downgrade", violations)
	}

	// An optional request for a check the registry does NOT hold required is
	// the legitimate "run but do not block" shape: no violation.
	legitimate := floorPack()
	legitimate.Checks.Optional = []string{"go-test"}
	violations = floor.Check(Target{Pack: legitimate, Origin: pack.OriginProject, Registry: registryWith(t, "go-test", false)})
	if hasViolation(violations, RuleMandatoryChecksImmutable, "") {
		t.Fatalf("violations = %v, a legitimate optional check must pass", violations)
	}

	// Nil registry (the boot check over builtin packs): rule 3 is skipped —
	// for builtin packs the monotonic checks.Resolve at run time holds.
	downgradeOriginBuiltin := floorPack()
	downgradeOriginBuiltin.Checks.Optional = []string{"go-test"}
	violations = floor.Check(Target{Pack: downgradeOriginBuiltin, Origin: pack.OriginBuiltin})
	if hasViolation(violations, RuleMandatoryChecksImmutable, "") {
		t.Fatalf("violations = %v, rule 3 must be skipped without a registry", violations)
	}
}

// Rule 4: a token outside the host set is a loud early refusal — including
// mcp.* and skill.*, which the adapter already refuses to enforce in a
// profile. Stage-level tokens are checked with the stage named.
func TestFloor_Rule4_CapabilitiesWithinHost(t *testing.T) {
	t.Parallel()
	floor := NewFloor(fullHostCaps())

	wide := floorPack()
	wide.Capabilities = append([]string{"net.fetch", "mcp.github", "skill.codereview"}, wide.Capabilities...)
	violations := floor.Check(Target{Pack: wide, Origin: pack.OriginProject})
	// net.fetch is within the host set; mcp.* and skill.* are not.
	for _, token := range []string{"mcp.github", "skill.codereview"} {
		found := false
		for _, violation := range violations {
			if violation.Rule == RuleCapabilitiesWithinHost && containsSubstring(violation.Message, token) {
				found = true
			}
		}
		if !found {
			t.Errorf("violations = %v, want rule 4 naming %q", violations, token)
		}
	}

	narrowed := floorPack()
	narrowedHost := []caps.Category{caps.CatFsRead, caps.CatGitRead}
	violations = floor.Check(Target{Pack: narrowed, Origin: pack.OriginBuiltin, HostCaps: narrowedHost})
	if !hasViolation(violations, RuleCapabilitiesWithinHost, LayerBuiltin) {
		t.Fatalf("violations = %v, want rule 4 refusing fs.write outside a narrowed host set", violations)
	}

	stageLevel := floorPack()
	stageLevel.Stages["implement"] = pack.Stage{
		Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md",
		Capabilities: []string{"git.delivery"},
		Transitions:  []pack.Transition{{To: "review"}},
	}
	violations = floor.Check(Target{Pack: stageLevel, Origin: pack.OriginProject})
	if !hasViolation(violations, RuleCapabilitiesWithinHost, LayerProject) {
		t.Fatalf("violations = %v, want rule 4 refusing the stage-level git.delivery token", violations)
	}
}

// The shipped packs pass every rule under the adapter's declared set — the
// same assertion the server boot check applies, pinned here for CI.
func TestFloor_ShippedPacksPass(t *testing.T) {
	t.Parallel()
	floor := NewFloor(fullHostCaps())
	for _, packDir := range []string{"../../packs/backend-development", "../../packs/small-change", "../../packs/research-first", "../../packs/minimal"} {
		loaded, err := pack.Load(packDir)
		if err != nil {
			t.Fatalf("load %s: %v", packDir, err)
		}
		if err := loaded.Validate(); err != nil {
			t.Fatalf("validate %s: %v", packDir, err)
		}
		violations := floor.Check(Target{Pack: loaded, Origin: pack.OriginBuiltin})
		if len(violations) != 0 {
			t.Fatalf("%s: violations = %v, want none", filepath.Base(packDir), violations)
		}
	}
}

// A structurally broken pack (the pre-validation floor path) skips what it
// cannot walk instead of panicking; rule 1 still fires on what it can see.
func TestFloor_ToleratesUnvalidatedPack(t *testing.T) {
	t.Parallel()
	broken := floorPack()
	broken.Entry = "nonexistent"
	broken.Approvals = nil
	floor := NewFloor(fullHostCaps())
	violations := floor.Check(Target{Pack: broken, Origin: pack.OriginProject})
	if !hasViolation(violations, RuleApprovalPrecedesSourceWrite, LayerProject) {
		t.Fatalf("violations = %v, want rule 1 on the unvalidated pack", violations)
	}
}

func containsSubstring(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
