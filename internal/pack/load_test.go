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

func replace(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("replace: old not found: " + old)
	}
	return strings.Replace(s, old, new, 1)
}
