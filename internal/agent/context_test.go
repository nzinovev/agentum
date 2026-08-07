package agent

import (
	"context"
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
