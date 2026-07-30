package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadAndValidate_Minimal loads the committed testdata fixture and expects
// a clean validation. This is the happy path and the canary for the format.
func TestLoadAndValidate_Minimal(t *testing.T) {
	t.Parallel()
	p, err := Load("testdata/minimal")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if p.Pack.Name != "minimal" {
		t.Errorf("name = %q, want minimal", p.Pack.Name)
	}
	if p.Entry != "spec" {
		t.Errorf("entry = %q, want spec", p.Entry)
	}
	if got := p.Stages["spec"].PromptText(); got == "" {
		t.Error("spec prompt text not loaded")
	}
	if !p.Stages["done"].Terminal() {
		t.Error("done stage must be terminal")
	}
}

// writePack creates a temp pack directory from a manifest body and a map of
// prompt relative-path -> contents. Returns the dir path.
func writePack(t *testing.T, manifest string, prompts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for rel, body := range prompts {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// validManifest is a known-good manifest reused across negative cases; each
// case mutates one field to force exactly one validation problem.
const validManifest = `api: agentum/v1
pack:
  name: probe
  version: 1.0.0
  persona: engineering
memory:
  reads: [project]
  writes: true
capabilities: [fs.read]
budgets:
  fix_cycles: 2
  ask_to_edit: 1
tiers:
  default: fast
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: implement
  implement:
    gate: auto_if_clean
    prompt: prompts/implement.md
    transitions:
      - to: done
  done: {}
`

func validPrompts() map[string]string {
	return map[string]string{
		"prompts/spec.md":      "spec body",
		"prompts/implement.md": "implement body",
	}
}

func TestValidate_HappyPath_InMemory(t *testing.T) {
	t.Parallel()
	dir := writePack(t, validManifest, validPrompts())
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_NegativeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		manifest string
		prompts  map[string]string // optional; defaults to validPrompts()
		// wantSubstr must appear in the validation error; the case passes when
		// Validate returns an error containing it.
		wantSubstr string
	}{
		{
			name:       "bad api version",
			manifest:   replace(validManifest, "api: agentum/v1", "api: agentum/v9"),
			wantSubstr: "api must be",
		},
		{
			name:       "empty pack name",
			manifest:   replace(validManifest, "name: probe", "name: \"\""),
			wantSubstr: "pack.name is required",
		},
		{
			name:       "bad semver",
			manifest:   replace(validManifest, "version: 1.0.0", "version: 1.0"),
			wantSubstr: "pack.version must be semver",
		},
		{
			name:       "semver with leading zero",
			manifest:   replace(validManifest, "version: 1.0.0", "version: 01.0.0"),
			wantSubstr: "pack.version must be semver",
		},
		{
			name:       "unknown gate",
			manifest:   replace(validManifest, "gate: human_approval", "gate: auto_magic"),
			wantSubstr: "gate",
		},
		{
			name:       "dangling transition target",
			manifest:   replace(validManifest, "- to: implement", "- to: nowhere"),
			wantSubstr: "is not a defined stage",
		},
		{
			name:       "self-loop transition",
			manifest:   replace(validManifest, "- to: implement", "- to: spec"),
			wantSubstr: "self-loop",
		},
		{
			name:       "entry not defined",
			manifest:   replace(validManifest, "entry: spec", "entry: ghost"),
			wantSubstr: "entry",
		},
		{
			name: "orphan stage unreachable from entry",
			manifest: replace(validManifest,
				"  done: {}",
				"  done: {}\n  orphan:\n    gate: human_approval\n    prompt: prompts/spec.md\n    transitions:\n      - to: done"),
			wantSubstr: "not reachable from entry",
		},
		{
			name: "no reachable terminal (cycle only)",
			manifest: `api: agentum/v1
pack: {name: probe, version: 1.0.0, persona: engineering}
memory: {reads: [project], writes: true}
capabilities: [fs.read]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: fast}
entry: a
stages:
  a:
    gate: human_approval
    prompt: prompts/a.md
    transitions:
      - to: b
  b:
    gate: human_approval
    prompt: prompts/b.md
    transitions:
      - to: a
`,
			prompts:    map[string]string{"prompts/a.md": "a", "prompts/b.md": "b"},
			wantSubstr: "no terminal stage is reachable",
		},
		{
			name:       "bad memory scope",
			manifest:   replace(validManifest, "reads: [project]", "reads: [galactic]"),
			wantSubstr: "not one of {project, user, org}",
		},
		{
			name:       "negative fix budget",
			manifest:   replace(validManifest, "fix_cycles: 2", "fix_cycles: -1"),
			wantSubstr: "fix_cycles must be non-negative",
		},
		{
			name: "unknown stage role",
			manifest: replace(validManifest,
				"    prompt: prompts/spec.md\n    transitions:",
				"    prompt: prompts/spec.md\n    role: wizard\n    transitions:"),
			wantSubstr: "role",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prompts := tc.prompts
			if prompts == nil {
				prompts = validPrompts()
			}
			dir := writePack(t, tc.manifest, prompts)
			p, err := Load(dir)
			if err != nil {
				t.Fatalf("Load unexpectedly failed: %v", err)
			}
			err = p.Validate()
			if err == nil {
				t.Fatalf("Validate expected error containing %q, got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestLoad_MissingPromptForNonTerminal: a non-terminal stage without a prompt
// file is a load error, not just a validation error.
func TestLoad_MissingPromptForNonTerminal(t *testing.T) {
	t.Parallel()
	manifest := replace(validManifest, "prompt: prompts/spec.md", "")
	dir := writePack(t, manifest, validPrompts())
	if _, err := Load(dir); err == nil {
		t.Fatal("Load expected error for non-terminal stage without prompt, got nil")
	}
}

// TestLoad_PromptPathEscape: a prompt path that leaves the pack dir is refused.
func TestLoad_PromptPathEscape(t *testing.T) {
	t.Parallel()
	manifest := replace(validManifest, "prompt: prompts/spec.md", "prompt: ../../../etc/passwd")
	dir := writePack(t, manifest, validPrompts())
	if _, err := Load(dir); err == nil {
		t.Fatal("Load expected error for escaping prompt path, got nil")
	}
}

// TestTerminalWithPrompt: a terminal stage that also declares a prompt is
// contradictory and must fail validation.
func TestTerminalWithPrompt(t *testing.T) {
	t.Parallel()
	manifest := replace(validManifest, "  done: {}", "  done:\n    gate: human_final\n    prompt: prompts/spec.md")
	dir := writePack(t, manifest, validPrompts())
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := p.Validate(); err == nil {
		t.Fatal("Validate expected error for terminal stage with a prompt")
	}
}

// TestValidate_RoleAndCapabilitiesAccepted: a stage declaring a known role and
// a stage-level capability subset validates cleanly, and the fields round-trip
// through the loader.
func TestValidate_RoleAndCapabilitiesAccepted(t *testing.T) {
	t.Parallel()
	manifest := replace(validManifest,
		"    prompt: prompts/spec.md\n    transitions:",
		"    prompt: prompts/spec.md\n    role: analyst\n    capabilities: [fs.read, git.read]\n    transitions:")
	dir := writePack(t, manifest, validPrompts())
	packResult, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := packResult.Validate(); err != nil {
		t.Fatalf("Validate with role+capabilities failed: %v", err)
	}
	spec := packResult.Stages["spec"]
	if spec.Role != "analyst" {
		t.Errorf("spec.Role = %q, want analyst", spec.Role)
	}
	if len(spec.Capabilities) != 2 {
		t.Errorf("spec.Capabilities = %v, want [fs.read git.read]", spec.Capabilities)
	}
}

func replace(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("replace: old not found: " + old)
	}
	return strings.Replace(s, old, new, 1)
}

// TestShippedPacksLoadAndValidate loads and validates every pack shipped under
// the repository's packs/ directory. A pack committed with a structural mistake
// (bad gate, dangling transition, unreachable stage) would otherwise surface
// only at run time; this is the canary that catches it at test time. It also
// pins the backend-dev pack's process invariants (no stack-specific check names,
// two human gates via the plan gate + the terminal, a bounded fix budget).
func TestShippedPacksLoadAndValidate(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "packs")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read packs dir %s: %v", root, err)
	}
	if len(entries) == 0 {
		t.Fatalf("no packs found under %s", root)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		entry := entry
		t.Run(entry.Name(), func(t *testing.T) {
			t.Parallel()
			loaded, loadErr := Load(filepath.Join(root, entry.Name()))
			if loadErr != nil {
				t.Fatalf("Load %s: %v", entry.Name(), loadErr)
			}
			if err := loaded.Validate(); err != nil {
				t.Fatalf("Validate %s: %v", entry.Name(), err)
			}
		})
	}
}

// TestBackendDevPackInvariants pins the backend-dev pack's process contract: it
// carries no stack-specific check names, it bounds the fix loop, and its plan
// gate is the only human gate before the terminal (so no source-write stage runs
// before plan approval).
func TestBackendDevPackInvariants(t *testing.T) {
	t.Parallel()
	loaded, err := Load(filepath.Join("..", "..", "packs", "backend-dev"))
	if err != nil {
		t.Fatalf("Load backend-dev: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate backend-dev: %v", err)
	}
	if loaded.Pack.Name != "backend-dev" {
		t.Errorf("pack name = %q, want backend-dev", loaded.Pack.Name)
	}
	// No stack-specific check names: the pack relies on the project baseline so
	// the same pack runs unchanged across Go / Java / any backend stack.
	if len(loaded.Checks.Required) != 0 || len(loaded.Checks.Optional) != 0 {
		t.Errorf("backend-dev must not name stack-specific checks; got required=%v optional=%v",
			loaded.Checks.Required, loaded.Checks.Optional)
	}
	if loaded.Budgets.FixCycles <= 0 {
		t.Errorf("fix_cycles = %d, want > 0", loaded.Budgets.FixCycles)
	}
	plan, ok := loaded.Stages["plan"]
	if !ok {
		t.Fatal("plan stage missing")
	}
	if plan.Gate != GateHumanApproval {
		t.Errorf("plan gate = %q, want human_approval", plan.Gate)
	}
	// The plan stage is read-only (analyst); source-writing stages come after it.
	if plan.Role != "analyst" {
		t.Errorf("plan role = %q, want analyst", plan.Role)
	}
	// The terminal stage exists (the pipeline has an exit).
	if _, ok := loaded.Stages["done"]; !ok {
		t.Fatal("done terminal stage missing")
	}
	if !loaded.Stages["done"].Terminal() {
		t.Error("done stage must be terminal")
	}
}

func TestValidate_CheckPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		checks     string
		wantSubstr string
	}{
		{
			name: "valid required and optional",
			checks: `
checks:
  required: [build]
  optional: [lint]`,
		},
		{
			name: "empty required name",
			checks: `
checks:
  required: [""]`,
			wantSubstr: "checks.required",
		},
		{
			name: "duplicate optional name",
			checks: `
checks:
  optional: [lint, lint]`,
			wantSubstr: "lists \"lint\" more than once",
		},
		{
			name: "name in both required and optional",
			checks: `
checks:
  required: [build]
  optional: [build]`,
			wantSubstr: "both required and optional",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := validManifest + tc.checks
			dir := writePack(t, manifest, validPrompts())
			packResult, err := Load(dir)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertValidationError(t, packResult.Validate(), tc.wantSubstr)
		})
	}
}

// assertValidationError enforces the expected validation outcome: when wantSubstr
// is empty validation must pass; otherwise it must fail with an error containing
// the substring. Lifted so each table case reads flat.
func assertValidationError(t *testing.T, err error, wantSubstr string) {
	t.Helper()
	if wantSubstr == "" {
		if err != nil {
			t.Fatalf("expected clean validation, got: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("expected error containing %q, got %q", wantSubstr, err.Error())
	}
}
