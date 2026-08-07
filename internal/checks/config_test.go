package checks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValidate(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid registry",
			yaml: `
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
    workdir: .
    timeout_seconds: 60
    max_output_bytes: 4096
    required: true
  - name: lint
    command: ["golangci-lint", "run"]
`,
		},
		{
			name: "empty api defaults to v1",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
`,
		},
		{
			name: "wrong api rejected",
			yaml: `
api: agentum/v2
checks:
  - name: build
    command: ["go", "build", "./..."]
`,
			wantErr: "api must be",
		},
		{
			name: "duplicate name rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
  - name: build
    command: ["go", "test", "./..."]
`,
			wantErr: "duplicated",
		},
		{
			name: "empty command rejected",
			yaml: `
checks:
  - name: build
    command: []
`,
			wantErr: "non-empty arg vector",
		},
		{
			name: "workdir escaping rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
    workdir: ../outside
`,
			wantErr: "escapes the repo root",
		},
		{
			name: "negative timeout rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
    timeout_seconds: -1
`,
			wantErr: "non-negative",
		},
		{
			name: "valid instructions list",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - AGENTS.md
  - docs/conventions.md
`,
		},
		{
			name: "instructions without checks",
			yaml: `
api: agentum/v1
instructions:
  - AGENTS.md
`,
		},
		{
			name: "instruction absolute path rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - /etc/passwd
`,
			wantErr: "absolute",
		},
		{
			name: "instruction parent escape rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - ../outside.md
`,
			wantErr: "escapes",
		},
		{
			name: "duplicate instruction path rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - AGENTS.md
  - AGENTS.md
`,
			wantErr: "duplicated",
		},
		{
			name: "too many instruction paths rejected",
			yaml: `
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - docs/a.md
  - docs/b.md
  - docs/c.md
  - docs/d.md
  - docs/e.md
  - docs/f.md
  - docs/g.md
  - docs/h.md
  - docs/i.md
`,
			wantErr: "at most",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := Parse([]byte(tc.yaml))
			if !assertParseError(t, err, tc.wantErr) {
				if registry.Revision == "" {
					t.Fatal("Revision is empty for a valid registry")
				}
			}
		})
	}
}

// assertParseError enforces the expected parse outcome: when wantErr is set the
// error must contain it; otherwise there must be no error. Returns whether an
// error was expected (so the caller knows to skip the success checks).
func assertParseError(t *testing.T, err error, wantErr string) bool {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return false
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("expected error containing %q, got %q", wantErr, err.Error())
	}
	return true
}

func TestParseCollectsAllProblems(t *testing.T) {
	// Two distinct problems in one config should both surface.
	yaml := `
api: agentum/v2
checks:
  - name: build
    command: []
  - name: build
    command: ["go", "build"]
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "api must be") {
		t.Errorf("error should mention api problem, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "non-empty arg vector") {
		t.Errorf("error should mention command problem, got %q", err.Error())
	}
}

func TestInstructionPaths(t *testing.T) {
	t.Run("returns declared paths in order", func(t *testing.T) {
		yaml := `
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
instructions:
  - docs/conventions.md
  - docs/style.md
`
		registry, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		paths := registry.InstructionPaths()
		if len(paths) != 2 || paths[0] != "docs/conventions.md" || paths[1] != "docs/style.md" {
			t.Fatalf("InstructionPaths = %v, want [docs/conventions.md docs/style.md]", paths)
		}
	})
	t.Run("nil when not declared", func(t *testing.T) {
		yaml := `
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
`
		registry, err := Parse([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if paths := registry.InstructionPaths(); paths != nil {
			t.Fatalf("InstructionPaths = %v, want nil", paths)
		}
	})
	t.Run("nil registry is safe", func(t *testing.T) {
		var registry *Registry
		if paths := registry.InstructionPaths(); paths != nil {
			t.Fatalf("nil registry InstructionPaths = %v, want nil", paths)
		}
	})
}

func TestRevisionStableAcrossFormatting(t *testing.T) {
	// Same definitions, different YAML whitespace/order, must hash equal.
	one := `
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
    required: true
  - name: test
    command: ["go", "test", "./..."]
`
	two := `
api: agentum/v1
checks:
  - name: test
    command: ["go", "test", "./..."]
  - name: build
    command: ["go", "build", "./..."]
    required: true
`
	first, err := Parse([]byte(one))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte(two))
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != second.Revision {
		t.Fatalf("registry revision must be order/format independent; got %q and %q", first.Revision, second.Revision)
	}

	// A changed contract must change the revision.
	changed, err := Parse([]byte(strings.ReplaceAll(one, `"go", "build", "./..."`, `"go","build","."`)))
	if err != nil {
		t.Fatal(err)
	}
	if changed.Revision == first.Revision {
		t.Fatal("registry revision must change when a command changes")
	}
}

func TestLoadFromRepo(t *testing.T) {
	t.Run("missing file is empty registry", func(t *testing.T) {
		dir := t.TempDir()
		registry, err := LoadFromRepo(dir)
		if err != nil {
			t.Fatalf("missing config must not error: %v", err)
		}
		if registry != nil {
			t.Fatalf("missing config must return nil registry, got %+v", registry)
		}
	})
	t.Run("present file loads", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
`), 0o644); err != nil {
			t.Fatal(err)
		}
		registry, err := LoadFromRepo(dir)
		if err != nil {
			t.Fatal(err)
		}
		def, ok := registry.Get("build")
		if !ok {
			t.Fatal("expected build check in registry")
		}
		if def.Command[0] != "go" {
			t.Fatalf("unexpected command: %v", def.Command)
		}
	})
}

func TestGetAndNames(t *testing.T) {
	registry, err := Parse([]byte(`
checks:
  - name: vet
    command: ["go", "vet", "./..."]
  - name: build
    command: ["go", "build", "./..."]
`))
	if err != nil {
		t.Fatal(err)
	}
	names := registry.Names()
	if len(names) != 2 || names[0] != "build" || names[1] != "vet" {
		t.Fatalf("Names must be sorted: %v", names)
	}
	if _, ok := registry.Get("missing"); ok {
		t.Fatal("Get(missing) should be false")
	}
}
