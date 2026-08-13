package pack

import (
	"strings"
	"testing"
)

// backendManifest is a backend-development-shaped pack used to exercise the
// approvals block. It mirrors the real packs/backend-development/manifest.yaml
// shape: plan (human_approval, analyst) → implement (implementer) → review
// (reviewer) ⇄ fix (fixer) → done, with a source_write approval on plan.
const backendManifest = `api: agentum/v1
pack:
  name: probe
  version: 1.0.0
  persona: engineering
memory: {reads: [project], writes: false}
capabilities: [fs.read, fs.write, git.read, git.write, exec.bash]
budgets: {fix_cycles: 2, ask_to_edit: 0}
tiers: {default: strong}
entry: plan
approvals:
  - {name: plan, stage: plan, artifact: plan.md, unlocks: source_write}
stages:
  plan:
    gate: human_approval
    role: analyst
    prompt: prompts/plan.md
    capabilities: [fs.read, git.read]
    transitions:
      - to: implement
  implement:
    gate: auto
    role: implementer
    prompt: prompts/implement.md
    transitions:
      - to: review
  review:
    gate: auto
    role: reviewer
    prompt: prompts/review.md
    capabilities: [fs.read, git.read]
    transitions:
      - {to: fix, condition: 'verdict == "changes_requested"'}
      - {to: done, condition: 'verdict == "approved"'}
  fix:
    gate: auto
    role: fixer
    prompt: prompts/fix.md
    transitions:
      - to: review
  done: {}
`

func backendPrompts() map[string]string {
	return map[string]string{
		"prompts/plan.md":      "Plan the change.",
		"prompts/implement.md": "Implement the plan.",
		"prompts/review.md":    "Review the change.",
		"prompts/fix.md":       "Fix the findings.",
	}
}

// TestValidate_Approvals_BackendShape: the backend-development pack shape with a
// well-formed source_write approval validates cleanly, and the helpers resolve
// the approval and the protected stages.
func TestValidate_Approvals_BackendShape(t *testing.T) {
	t.Parallel()
	dir := writePack(t, backendManifest, backendPrompts())
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate backend-shaped pack: %v", err)
	}

	approval, ok := loaded.SourceWriteApproval()
	if !ok {
		t.Fatal("SourceWriteApproval returned false on a pack that declares source_write")
	}
	if approval.Name != "plan" || approval.Stage != "plan" || approval.Artifact != "plan.md" {
		t.Errorf("SourceWriteApproval = %+v, want {plan plan plan.md source_write}", approval)
	}

	sourceStages := loaded.SourceWritingStages()
	if !contains(sourceStages, "implement") || !contains(sourceStages, "fix") {
		t.Errorf("SourceWritingStages = %v, want implement and fix", sourceStages)
	}
	if contains(sourceStages, "plan") || contains(sourceStages, "review") {
		t.Errorf("SourceWritingStages = %v, must not include plan/review", sourceStages)
	}
}

// TestValidate_Approvals_NegativeCases: each malformed approval declaration is
// rejected with a precise message.
func TestValidate_Approvals_NegativeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		transform  func(string) string
		wantSubstr string
	}{
		{
			name: "duplicate approval name",
			transform: func(manifest string) string {
				return strings.Replace(manifest,
					"  - {name: plan, stage: plan, artifact: plan.md, unlocks: source_write}",
					"  - {name: plan, stage: plan, artifact: plan.md, unlocks: source_write}\n  - {name: plan, stage: plan, artifact: other.md, unlocks: source_write}",
					1)
			},
			wantSubstr: "lists name \"plan\" more than once",
		},
		{
			name: "unknown unlock",
			transform: func(manifest string) string {
				return strings.Replace(manifest, "unlocks: source_write}", "unlocks: deploy}", 1)
			},
			wantSubstr: "not one of the known unlock names",
		},
		{
			name: "approval stage not defined",
			transform: func(manifest string) string {
				return strings.Replace(manifest, "stage: plan, artifact: plan.md", "stage: ghost, artifact: plan.md", 1)
			},
			wantSubstr: "is not a defined stage",
		},
		{
			name: "approval stage terminal",
			transform: func(manifest string) string {
				return strings.Replace(manifest, "stage: plan, artifact: plan.md", "stage: done, artifact: plan.md", 1)
			},
			wantSubstr: "is terminal and cannot host an approval gate",
		},
		{
			name: "artifact with path separator",
			transform: func(manifest string) string {
				return strings.Replace(manifest, "artifact: plan.md", "artifact: sub/plan.md", 1)
			},
			wantSubstr: "must be a bare file name",
		},
		{
			name: "empty approval name",
			transform: func(manifest string) string {
				return strings.Replace(manifest, "name: plan, stage: plan", "name: \"\", stage: plan", 1)
			},
			wantSubstr: "name is empty",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			manifest := testCase.transform(backendManifest)
			dir := writePack(t, manifest, backendPrompts())
			loaded, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertValidationError(t, loaded.Validate(), testCase.wantSubstr)
		})
	}
}

// TestValidate_Approvals_SourceWriterReachableWithoutApproval: an implementer
// reachable from entry without passing the approval stage is a layer-3 authoring
// mistake the validator catches.
func TestValidate_Approvals_SourceWriterReachableWithoutApproval(t *testing.T) {
	t.Parallel()
	// plan (approval) → review → done; but a second edge plan → implement → review
	// lets the run reach implement without ever recording the approval at the
	// human gate. The validator must reject this at load time.
	manifest := strings.Replace(backendManifest,
		"      - to: implement",
		"      - to: review\n  bypass:\n    gate: auto\n    role: implementer\n    prompt: prompts/implement.md\n    transitions:\n      - to: review",
		1)
	dir := writePack(t, manifest, backendPrompts())
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertValidationError(t, loaded.Validate(), "reachable from entry")
}

// TestValidate_Approvals_NoApprovalsBlockUnaffected: packs/minimal and every
// test fixture without an approvals block must validate exactly as before.
func TestValidate_Approvals_NoApprovalsBlockUnaffected(t *testing.T) {
	t.Parallel()
	dir := writePack(t, validManifest, validPrompts())
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate pack without approvals must pass: %v", err)
	}
	if _, ok := loaded.SourceWriteApproval(); ok {
		t.Error("SourceWriteApproval returned true on a pack with no approvals block")
	}
	if stages := loaded.SourceWritingStages(); len(stages) != 1 || stages[0] != "implement" {
		// validManifest has one implementer stage (implement); it should be the
		// only source-writing stage even though no approval protects it — the
		// helper reports the set, the validator does not require an approval.
		t.Errorf("SourceWritingStages = %v, want [implement]", stages)
	}
}

func contains(slice []string, want string) bool {
	for _, entry := range slice {
		if entry == want {
			return true
		}
	}
	return false
}
