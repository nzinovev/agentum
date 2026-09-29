package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/manifest"
)

// The project-config comparison contract: when a run's worktree is first
// created, .agentum.yaml in the source checkout is compared with the pinned
// base_commit version and the result is recorded in the manifest's
// context.project_config. A difference is a warning event, never a stop — the
// run always applies the base_commit version — and a checkout edited later
// changes nothing for a run already under way.

const driftTestConfig = "api: agentum/v1\nchecks: []\n"

// newConfigFixture builds a recovery fixture whose runner records manifest
// evidence into a fake, so the tests can read context.project_config.
func newConfigFixture(t *testing.T) (*recoveryFixture, *fakeManifestService) {
	t.Helper()
	fixture := newRecoveryFixture(t, "running")
	manifestFake := &fakeManifestService{}
	fixture.runner.mfst = manifestFake
	return fixture, manifestFake
}

// recordedProjectConfig returns the project-config evidence the runner
// recorded, or nil.
func recordedProjectConfig(service *fakeManifestService) *manifest.ProjectConfigEvidence {
	service.mu.Lock()
	defer service.mu.Unlock()
	for _, patch := range service.addEvidence {
		if patch.Context != nil && patch.Context.ProjectConfig != nil {
			return patch.Context.ProjectConfig
		}
	}
	return nil
}

func countEvents(fixture *recoveryFixture, eventType string) int {
	count := 0
	for _, event := range fixture.store.events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

// TestRunner_ProjectConfigComparedAtStart covers every checkout shape: the
// run proceeds to its gate in each, the evidence names the change kind and
// whether base_commit carries a config, and only a difference emits the
// warning event. An untracked config on disk — the reproduced defect — reads
// as "added", not as a git read failure.
func TestRunner_ProjectConfigComparedAtStart(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name string
		// committed is the config committed before the run pins its base;
		// empty commits none.
		committed string
		// onDisk rewrites the checkout copy after the commit; removeOnDisk
		// deletes it; neither leaves the checkout as committed.
		onDisk        string
		removeOnDisk  bool
		unrelatedDirt bool
		wantPresent   bool
		wantChange    string
	}{
		{name: "no config anywhere", wantPresent: false, wantChange: ""},
		{name: "untracked config on disk", onDisk: driftTestConfig, wantPresent: false, wantChange: "added"},
		{name: "committed config edited on disk", committed: driftTestConfig, onDisk: driftTestConfig + "# edited\n", wantPresent: true, wantChange: "modified"},
		{name: "committed config removed on disk", committed: driftTestConfig, removeOnDisk: true, wantPresent: true, wantChange: "removed"},
		{name: "committed config unchanged", committed: driftTestConfig, wantPresent: true, wantChange: ""},
		{name: "unrelated dirty file", unrelatedDirt: true, wantPresent: false, wantChange: ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			fixture, manifestFake := newConfigFixture(t)
			configPath := filepath.Join(fixture.repo, checks.ConfigFile)
			if scenario.committed != "" {
				if err := os.WriteFile(configPath, []byte(scenario.committed), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := gitCommitAll(fixture.repo, "add config"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.onDisk != "" {
				if err := os.WriteFile(configPath, []byte(scenario.onDisk), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.removeOnDisk {
				if err := os.Remove(configPath); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.unrelatedDirt {
				if err := os.WriteFile(filepath.Join(fixture.repo, "unrelated-wip.txt"), []byte("operator's local work"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			fixture.driveToGate(t)

			evidence := recordedProjectConfig(manifestFake)
			if evidence == nil {
				t.Fatal("no context.project_config evidence recorded")
			}
			if evidence.PresentAtBase != scenario.wantPresent || evidence.CheckoutChange != scenario.wantChange {
				t.Fatalf("evidence = present_at_base %v, checkout_change %q; want %v, %q",
					evidence.PresentAtBase, evidence.CheckoutChange, scenario.wantPresent, scenario.wantChange)
			}
			wantEvents := 0
			if scenario.wantChange != "" {
				wantEvents = 1
			}
			if got := countEvents(fixture, EvProjectConfigDrift); got != wantEvents {
				t.Fatalf("%s events = %d, want %d", EvProjectConfigDrift, got, wantEvents)
			}
			if gaps := manifestFake.gapSections(); len(gaps) != 0 {
				t.Fatalf("evidence gaps = %v; a config comparison is a fact, never a gap", gaps)
			}
			if scenario.unrelatedDirt {
				// Worktrees build from base_commit, not the working copy.
				if _, statErr := os.Stat(filepath.Join(fixture.worktreeRoot(), "unrelated-wip.txt")); statErr == nil {
					t.Fatal("untracked checkout file leaked into the run worktree")
				}
			}
		})
	}
}

// TestRunner_ProjectConfigEditLaterDoesNotStopTheRun pins the continuation
// side: once the run's worktree exists, a config the operator creates in the
// checkout neither stops a continue nor emits a second comparison — the run
// pinned its config when it began.
func TestRunner_ProjectConfigEditLaterDoesNotStopTheRun(t *testing.T) {
	t.Parallel()
	fixture, _ := newConfigFixture(t)
	fixture.driveToGate(t)
	if err := os.WriteFile(filepath.Join(fixture.repo, checks.ConfigFile), []byte(driftTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.resumeAsRunning(t)
	if err := fixture.runner.HandleAdvance(context.Background(), job("advance", fixture.runID, "tn", "us")); err != nil {
		t.Fatalf("advance after a checkout config edit: %v", err)
	}
	if state := fixture.store.taskState(); state == "paused_user_stop" || state == "failed" {
		t.Fatalf("state = %q; a later checkout edit must not stop the run", state)
	}
	if got := countEvents(fixture, EvProjectConfigDrift); got != 0 {
		t.Fatalf("%s events = %d, want 0 after the worktree exists", EvProjectConfigDrift, got)
	}
}
