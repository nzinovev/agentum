package pack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadBase loads the committed minimal fixture, used as the base for override
// cases unless a case needs a custom base.
func loadBase(t *testing.T) *Pack {
	t.Helper()
	p, err := Load("testdata/minimal")
	if err != nil {
		t.Fatalf("Load base: %v", err)
	}
	return p
}

func ptrGate(g Gate) *Gate       { return &g }
func ptrString(s string) *string { return &s }
func ptrInt(n int) *int          { return &n }

// TestResolve_PromptSwapAndParamPatch is the happy path: swap one prompt,
// patch one stage's gate + tier, patch a budget. The resolved pack validates,
// carries the swapped text, the patched gate, and the base is untouched.
func TestResolve_PromptSwapAndParamPatch(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	origPrompt := base.Stages["spec"].PromptText()
	origGate := base.Stages["spec"].Gate

	ov := &Overrides{
		Base:       "minimal",
		Prompts:    map[string]string{"spec": "my-spec.md"},
		promptText: map[string]string{"spec": "swapped spec body"},
		Stages: map[string]StagePatch{
			"spec": {Gate: ptrGate(GateAutoOnApproval), Tier: ptrString("strong")},
		},
		Budgets: &BudgetsPatch{FixCycles: ptrInt(9)},
	}

	resolved, err := Resolve(base, ov)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolved.Stages["spec"].PromptText(); got != "swapped spec body" {
		t.Errorf("spec prompt text = %q, want swapped", got)
	}
	if resolved.Stages["spec"].Gate != GateAutoOnApproval {
		t.Errorf("spec gate = %q, want auto_on_approval", resolved.Stages["spec"].Gate)
	}
	if resolved.Stages["spec"].Tier != "strong" {
		t.Errorf("spec tier = %q, want strong", resolved.Stages["spec"].Tier)
	}
	if resolved.Budgets.FixCycles != 9 {
		t.Errorf("fix_cycles = %d, want 9", resolved.Budgets.FixCycles)
	}

	// base untouched
	if base.Stages["spec"].PromptText() != origPrompt {
		t.Error("Resolve mutated base prompt text")
	}
	if base.Stages["spec"].Gate != origGate {
		t.Error("Resolve mutated base gate")
	}
	if base.Budgets.FixCycles == 9 {
		t.Error("Resolve mutated base budget")
	}
}

// TestResolve_ForkMetadata: a fork with no mutations still validates and
// records Forked + BaseRef.
func TestResolve_ForkMetadata(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	ov := &Overrides{Base: "minimal@^0", Fork: true}
	resolved, err := Resolve(base, ov)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolved.Forked {
		t.Error("resolved.Forked = false, want true")
	}
	if resolved.BaseRef != "minimal@^0" {
		t.Errorf("BaseRef = %q, want minimal@^0", resolved.BaseRef)
	}
	if ov.HasMutation() {
		t.Error("fork-only override reported as having mutations")
	}
}

// TestResolve_InvalidGateRejected: a layer-4 patch to an invalid gate is caught
// by re-validation.
func TestResolve_InvalidGateRejected(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	bad := Gate("auto_magic")
	ov := &Overrides{
		Base:   "minimal",
		Stages: map[string]StagePatch{"spec": {Gate: &bad}},
	}
	if _, err := Resolve(base, ov); err == nil {
		t.Fatal("Resolve expected error for invalid gate override, got nil")
	} else if !strings.Contains(err.Error(), "gate") {
		t.Fatalf("error = %q, want substring 'gate'", err.Error())
	}
}

// TestResolve_UnknownStagePrompt: overriding a prompt for a stage that doesn't
// exist is a consumer error.
func TestResolve_UnknownStagePrompt(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	ov := &Overrides{
		Base:       "minimal",
		Prompts:    map[string]string{"ghost": "x.md"},
		promptText: map[string]string{"ghost": "x"},
	}
	if _, err := Resolve(base, ov); err == nil {
		t.Fatal("Resolve expected error for unknown stage, got nil")
	}
}

// TestResolve_UnknownStagePatch: patching a stage that doesn't exist is an error.
func TestResolve_UnknownStagePatch(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	ov := &Overrides{
		Base:   "minimal",
		Stages: map[string]StagePatch{"ghost": {Tier: ptrString("strong")}},
	}
	if _, err := Resolve(base, ov); err == nil {
		t.Fatal("Resolve expected error for unknown stage patch, got nil")
	}
}

// TestResolve_PromptOnTerminalStage: overriding a prompt on a terminal stage
// makes no sense (terminal stages have no prompt) and must fail.
func TestResolve_PromptOnTerminalStage(t *testing.T) {
	t.Parallel()
	base := loadBase(t)
	ov := &Overrides{
		Base:       "minimal",
		Prompts:    map[string]string{"done": "x.md"},
		promptText: map[string]string{"done": "x"},
	}
	if _, err := Resolve(base, ov); err == nil {
		t.Fatal("Resolve expected error for prompt override on terminal stage")
	}
}

// TestLoadOverrides_EndToEnd writes an override document with a real prompt
// file and confirms LoadOverrides + Resolve produce the swapped text.
func TestLoadOverrides_EndToEnd(t *testing.T) {
	t.Parallel()
	base := loadBase(t)

	dir := t.TempDir()
	overridesYAML := `base: minimal
prompts:
  spec: prompts/my-spec.md
stages:
  spec:
    tier: strong
budgets:
  fix_cycles: 7
`
	if err := os.WriteFile(filepath.Join(dir, "overrides.yaml"), []byte(overridesYAML), 0o644); err != nil {
		t.Fatalf("write overrides.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "my-spec.md"), []byte("from file body"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	resolved, err := Resolve(base, ov)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolved.Stages["spec"].PromptText(); got != "from file body" {
		t.Errorf("spec prompt = %q, want 'from file body'", got)
	}
	if resolved.Stages["spec"].Tier != "strong" {
		t.Errorf("spec tier = %q, want strong", resolved.Stages["spec"].Tier)
	}
	if resolved.Budgets.FixCycles != 7 {
		t.Errorf("fix_cycles = %d, want 7", resolved.Budgets.FixCycles)
	}
}

// --- Source (layer 1) ---

// writeSourcePack writes a pack into root/<name>/ so DirSource can find it.
func writeSourcePack(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, name)
	manifest := strings.ReplaceAll(`api: agentum/v1
pack: {name: NAME, version: VER, persona: engineering}
memory: {reads: [project], writes: true}
capabilities: [fs.read]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: fast}
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions: [{to: done}]
  done: {}
`, "NAME", name)
	manifest = strings.ReplaceAll(manifest, "VER", version)
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "spec.md"), []byte("spec"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
}

func TestDirSource_Resolve(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSourcePack(t, root, "probe", "1.2.3")
	src := NewDirSource(root)

	cases := []struct {
		ref     string
		wantErr bool
	}{
		{"probe", false},       // any version
		{"probe@^1", false},    // major matches
		{"probe@1.2.3", false}, // exact
		{"probe@^2", true},     // major mismatch
		{"probe@1.0.0", true},  // exact mismatch
		{"missing", true},      // not found
		{"probe@bad", true},    // malformed constraint
		{"@^1", true},          // empty name
		{"", true},             // empty ref
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			p, err := src.Resolve(context.Background(), tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) expected error, got nil", tc.ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.ref, err)
			}
			if p.Pack.Name != "probe" {
				t.Errorf("name = %q, want probe", p.Pack.Name)
			}
			if p.BaseRef != tc.ref {
				t.Errorf("BaseRef = %q, want %q", p.BaseRef, tc.ref)
			}
		})
	}
}

// TestDirSource_NameMismatch: a manifest declaring a different name than its
// directory is rejected, so a misplaced pack can't shadow another.
func TestDirSource_NameMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSourcePack(t, root, "probe", "1.0.0")
	// Corrupt: rename dir but not the manifest name field.
	if err := os.Rename(filepath.Join(root, "probe"), filepath.Join(root, "renamed")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	src := NewDirSource(root)
	if _, err := src.Resolve(context.Background(), "renamed"); err == nil {
		t.Fatal("expected name-mismatch error, got nil")
	}
}

// TestResolve_FromSource composes the two: resolve a base by ref, then apply
// overrides — the shape the runner uses.
func TestResolve_FromSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSourcePack(t, root, "probe", "1.0.0")
	src := NewDirSource(root)

	base, err := src.Resolve(context.Background(), "probe@^1")
	if err != nil {
		t.Fatalf("source resolve: %v", err)
	}
	ov := &Overrides{
		Base:   "probe@^1",
		Stages: map[string]StagePatch{"spec": {Tier: ptrString("strong")}},
	}
	resolved, err := Resolve(base, ov)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Stages["spec"].Tier != "strong" {
		t.Errorf("tier = %q, want strong", resolved.Stages["spec"].Tier)
	}
	if resolved.BaseRef != "probe@^1" {
		t.Errorf("BaseRef = %q, want probe@^1", resolved.BaseRef)
	}
}
