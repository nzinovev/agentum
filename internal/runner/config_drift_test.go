package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/checks"
)

// The project-config drift contract (stage 2 of the run-recovery plan): a
// run compares .agentum.yaml in its source checkout against the pinned
// base_commit on every start and continuation, and stops in
// paused_user_stop (project_config_drift) BEFORE any invocation when they
// differ. Absence is a value, untracked-on-disk counts as added, unrelated
// dirty files never block the run, and a continue after the checkout matches
// the anchor again proceeds.

// driftFixture drives one run of the spec(human_approval)→impl→done pack and
// leaves it at paused_gate, with the repo's HEAD as the run's base_commit.
func driveDriftFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	fixture := newRecoveryFixture(t, "running")
	fixture.driveToGate(t)
	return fixture
}

// TestRunner_ConfigDriftUntrackedConfigPauses pins the reproduced defect: a
// .agentum.yaml created after the run's base_commit (present on disk,
// absent from the commit) must stop the run with project_config_drift —
// not fail it with a git read error, and not silently run the new config.
func TestRunner_ConfigDriftUntrackedConfigPauses(t *testing.T) {
	t.Parallel()
	fixture := driveDriftFixture(t)
	// A NEW config appears in the source checkout, untracked.
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile),
		[]byte("api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n    required: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)

	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("continue with config drift: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (project_config_drift)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvProjectConfigDrift)
	// No spec invocation started over the drifted config.
	if count := len(fixture.store.invocations); count != 1 {
		t.Fatalf("invocations = %d, want the single pre-drift invocation", count)
	}
}

// TestRunner_ConfigDriftModifiedConfigPauses covers the tracked-but-edited
// shape: the file exists on both sides with different bytes.
func TestRunner_ConfigDriftModifiedConfigPauses(t *testing.T) {
	t.Parallel()
	fixture := driveDriftFixture(t)
	// Seed a committed config at a SECOND commit, pin the run's base at the
	// first, then edit the file on disk: both sides present, bytes differ.
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile), []byte("api: agentum/v1\nchecks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitAll(fixture.repo, "add config"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile), []byte("api: agentum/v1\nchecks: []\n# edited on disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("continue with modified config: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (project_config_drift)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvProjectConfigDrift)
}

// TestRunner_ConfigDriftRemovedConfigPauses covers the removed shape: the
// config was committed at the base but deleted from the working copy.
func TestRunner_ConfigDriftRemovedConfigPauses(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	// Commit a config BEFORE the run, so the run's base_commit carries it.
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile), []byte("api: agentum/v1\nchecks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitAll(fixture.repo, "add config"); err != nil {
		t.Fatal(err)
	}
	fixture.driveToGate(t)
	if err := os.Remove(filepath.Join(fixture.repo, checks.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("continue with removed config: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (project_config_drift)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvProjectConfigDrift)
}

// TestRunner_ConfigDriftUnrelatedDirtyFileProceeds pins the boundary: a
// dirty checkout that differs from base_commit in any OTHER file does not
// block the run — only the pinned config participates in the check.
func TestRunner_ConfigDriftUnrelatedDirtyFileProceeds(t *testing.T) {
	t.Parallel()
	fixture := driveDriftFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.repo, "unrelated-wip.txt"), []byte("operator's local work"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("continue with unrelated dirt: %v", err)
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate (the spec gate, not a drift pause)", state)
	}
	for _, event := range fixture.store.events {
		if event.Type == EvProjectConfigDrift {
			t.Fatal("unrelated dirty file produced a config drift event")
		}
	}
	// The unrelated file must not leak into the run's worktree: worktrees
	// build from the base commit, not the working copy.
	if _, statErr := os.Stat(filepath.Join(fixture.worktreeRoot(), "unrelated-wip.txt")); statErr == nil {
		t.Fatal("untracked checkout file leaked into the run worktree")
	}
}

// TestRunner_ConfigDriftContinueRechecksPrecondition pins the recovery
// shape: after the drift pause, a continue re-runs the check — while the
// checkout still differs it pauses again, and once the checkout matches the
// anchor the run proceeds.
func TestRunner_ConfigDriftContinueRechecksPrecondition(t *testing.T) {
	t.Parallel()
	fixture := driveDriftFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile), []byte("api: agentum/v1\nchecks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want the drift pause", state)
	}
	// The human reverts the checkout to match the anchor; the next continue
	// proceeds to the spec gate.
	if err := os.Remove(filepath.Join(fixture.repo, checks.ConfigFile)); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate after the drift cleared", state)
	}
}

// TestRunner_NoConfigAnywhereProceeds pins the empty-registry semantics: a
// project with no .agentum.yaml on disk AND none at base_commit runs with an
// empty registry — the recorded evidence gap says so, and no drift fires.
func TestRunner_NoConfigAnywhereProceeds(t *testing.T) {
	t.Parallel()
	fixture := driveDriftFixture(t)
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleContinue(context.Background(), job("continue", fixture.runID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate (no drift without a config)", state)
	}
	for _, event := range fixture.store.events {
		if event.Type == EvProjectConfigDrift {
			t.Fatal("absent-on-both-sides config produced a drift event")
		}
	}
}
