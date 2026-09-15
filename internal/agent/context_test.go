package agent

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeProbeInvocation wires the adapter to the test binary as its agent for a
// debug-skill probe, and returns the adapter plus the invocation to probe. The
// mode selects the fake's behaviour (wellformed, malformed, exit, hang).
func fakeProbeInvocation(t *testing.T, mode string) (*OpencodeAdapter, Invocation) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	workdir := t.TempDir()
	artifactDir := filepath.Join(workdir, ".agentum", "artifacts", "spec")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	t.Setenv(fakeDebugSkillEnv, mode)
	return NewOpencodeAdapter(self), Invocation{Workdir: workdir, ArtifactDir: artifactDir}
}

// shrinkProbeTimeout shortens the probe deadline for one test so a hang is
// caught in milliseconds rather than the production 10s.
func shrinkProbeTimeout(t *testing.T) {
	t.Helper()
	original := probeTimeout
	probeTimeout = 400 * time.Millisecond
	t.Cleanup(func() { probeTimeout = original })
}

// TestProbeContext_WellformedParsesAndDiscardsBody (ADR 0002 D6): the prober
// parses the JSON array of skills, hashes each body, and NEVER returns the body
// itself. The built-in skill (location "<built-in>") and a user skill both land
// in the report with name, location, description, hash, and bytes.
func TestProbeContext_WellformedParsesAndDiscardsBody(t *testing.T) {
	adapter, inv := fakeProbeInvocation(t, fakeDebugWellformed)
	report, err := adapter.ProbeContext(context.Background(), inv)
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if report.SkillsProbe != ContextProbeOK {
		t.Errorf("SkillsProbe = %q, want %q", report.SkillsProbe, ContextProbeOK)
	}
	if report.SkillsError != "" {
		t.Errorf("SkillsError = %q, want empty", report.SkillsError)
	}
	if !strings.Contains(strings.Join(report.AutoInstructions, ","), "AGENTS.md") {
		t.Errorf("AutoInstructions = %v, want to contain AGENTS.md", report.AutoInstructions)
	}
	if len(report.Skills) != 2 {
		t.Fatalf("Skills = %d entries, want 2", len(report.Skills))
	}
	// The built-in skill is reported with its location.
	builtin := report.Skills[0]
	if builtin.Name != "customize-opencode" || builtin.Location != "<built-in>" {
		t.Errorf("built-in skill = %+v, want name=customize-opencode location=<built-in>", builtin)
	}
	if builtin.Hash == "" || builtin.Bytes == 0 {
		t.Errorf("built-in skill hash/bytes empty: %+v", builtin)
	}
	// The body must never appear in the returned struct — hashes and sizes only.
	for _, skill := range report.Skills {
		if strings.Contains(skill.Hash, "MARKER") {
			t.Errorf("skill %q hash leaked body content: %q", skill.Name, skill.Hash)
		}
		if strings.Contains(skill.Description, "MARKER") {
			t.Errorf("skill %q description leaked body content: %q", skill.Name, skill.Description)
		}
	}
}

// TestProbeContext_MalformedJSONIsAFailureNotAPanic: malformed output yields a
// SkillsError and a failed probe label, with AutoInstructions still populated.
// The decode path must not panic on bad input.
func TestProbeContext_MalformedJSONIsAFailureNotAPanic(t *testing.T) {
	adapter, inv := fakeProbeInvocation(t, fakeDebugMalformed)
	report, err := adapter.ProbeContext(context.Background(), inv)
	if err != nil {
		t.Fatalf("ProbeContext returned error (expected a report with SkillsError): %v", err)
	}
	if report.SkillsError == "" {
		t.Error("SkillsError empty for malformed JSON")
	}
	if report.SkillsProbe != ContextProbeFailedPrefix+"json" {
		t.Errorf("SkillsProbe = %q, want %q", report.SkillsProbe, ContextProbeFailedPrefix+"json")
	}
	if len(report.AutoInstructions) == 0 {
		t.Error("AutoInstructions empty though probe failed — baseline must survive")
	}
}

// TestProbeContext_NonZeroExitIsAFailure: a failing subprocess yields a failed
// probe with the exit label, AutoInstructions still populated.
func TestProbeContext_NonZeroExitIsAFailure(t *testing.T) {
	adapter, inv := fakeProbeInvocation(t, fakeDebugExit)
	report, err := adapter.ProbeContext(context.Background(), inv)
	if err != nil {
		t.Fatalf("ProbeContext returned error: %v", err)
	}
	if report.SkillsError == "" {
		t.Error("SkillsError empty for non-zero exit")
	}
	if report.SkillsProbe != ContextProbeFailedPrefix+"exit" {
		t.Errorf("SkillsProbe = %q, want %q", report.SkillsProbe, ContextProbeFailedPrefix+"exit")
	}
	if len(report.AutoInstructions) == 0 {
		t.Error("AutoInstructions empty though probe failed")
	}
}

// TestProbeContext_HangingProbeIsKilledAtTimeout: a probe that produces nothing
// and never exits is killed at the probe timeout and yields a timeout failure
// label, with AutoInstructions still populated. This is the known opencode
// no-output defect (ADR 0002 §1) — the probe must not hang the run.
func TestProbeContext_HangingProbeIsKilledAtTimeout(t *testing.T) {
	// Cannot use t.Parallel: it shrinks probeTimeout and killGrace, which are
	// package-global.
	shrinkProbeTimeout(t)
	shrinkKillGrace(t)
	adapter, inv := fakeProbeInvocation(t, fakeDebugHang)
	report, err := adapter.ProbeContext(context.Background(), inv)
	if err != nil {
		t.Fatalf("ProbeContext returned error: %v", err)
	}
	if report.SkillsProbe != ContextProbeFailedPrefix+"timeout" {
		t.Errorf("SkillsProbe = %q, want %q", report.SkillsProbe, ContextProbeFailedPrefix+"timeout")
	}
	if len(report.AutoInstructions) == 0 {
		t.Error("AutoInstructions empty though probe timed out — baseline must survive")
	}
}

// TestAutoInstructionBaselineIsAGENTS: the opencode adapter's static baseline is
// AGENTS.md at the project root (measured 2026-08-05: the file reaches the
// system prompt with zero tool calls and no configuration).
func TestAutoInstructionBaselineIsAGENTS(t *testing.T) {
	t.Parallel()
	if len(autoInstructionBaseline) != 1 || autoInstructionBaseline[0] != "AGENTS.md" {
		t.Errorf("autoInstructionBaseline = %v, want [AGENTS.md]", autoInstructionBaseline)
	}
}

// TestNoRuntimeInstructionFilenameOutsideThisPackage: which file a runtime
// loads by itself is a fact about that runtime, and the executor guard does not
// catch it — "AGENTS.md" does not contain the executor's name. A caller that
// hardcodes it (a fallback for a failed probe is how it gets in) pins the wrong
// file for every other runtime, silently, and only on the path where the probe
// could not answer. Callers take the baseline from the descriptor instead.
func TestNoRuntimeInstructionFilenameOutsideThisPackage(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	thisPackage := filepath.Join(repoRoot, "internal", "agent")

	walkErr := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "tmp", "packs", "docs", "node_modules":
				return fs.SkipDir
			}
			if path == thisPackage {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		// Mode 0: comments are not attached, so prose naming the file as an
		// example stays legal — only real string literals are examined.
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			for _, baseline := range autoInstructionBaseline {
				if strings.Contains(literal.Value, baseline) {
					t.Errorf("%s:%d: string literal %s names a runtime-injected instruction file; "+
						"take it from Describe().AutoInstructions instead",
						path, fileSet.Position(literal.Pos()).Line, literal.Value)
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}

// TestDescribe_AutoInstructionsAreDeclaredAndCopied: the descriptor publishes
// the runtime-injected baseline for callers that cannot run a probe, and hands
// out a copy — a caller mutating the slice must not rewrite what every later
// run pins.
func TestDescribe_AutoInstructionsAreDeclaredAndCopied(t *testing.T) {
	t.Parallel()
	descriptor := NewOpencodeAdapter("opencode").Describe()
	if len(descriptor.AutoInstructions) == 0 {
		t.Fatal("descriptor declares no auto-instruction baseline; callers have nothing to fall back to")
	}
	descriptor.AutoInstructions[0] = "MUTATED.md"
	if again := NewOpencodeAdapter("opencode").Describe(); again.AutoInstructions[0] == "MUTATED.md" {
		t.Error("Describe must return a fresh baseline slice; the mutation leaked into the descriptor")
	}
}
