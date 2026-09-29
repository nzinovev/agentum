package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The base-ancestry contract (stage 5 of the run-recovery plan): a
// publishable run's base must belong to the publication target branch's
// history, verified against the remote-tracking ref before the worktree is
// created and re-verified on every continuation. A developer branch's
// unpushed commits must never ride into the run's pull request silently.

// defaultBranchOf names the repo's checked-out branch, so tests use whatever
// git's default is instead of assuming main.
func defaultBranchOf(t *testing.T, repo string) string {
	t.Helper()
	branch, err := execGit(repo, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatalf("read default branch: %v", err)
	}
	return branch
}

// baseAncestryFixture wires a publication-enabled runner over a repo whose
// base_ref is the checked-out branch; trackingSHA, when non-empty, seeds the
// remote-tracking comparison point at that commit.
func newBaseAncestryFixture(t *testing.T, remote, baseBranch, trackingSHA string) *recoveryFixture {
	t.Helper()
	fixture := newRecoveryFixture(t, "running")
	fixture.runner.publication = publicationHookForTest(remote, baseBranch)
	branch := defaultBranchOf(t, fixture.repo)
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: branch}
	if trackingSHA != "" {
		if _, err := execGit(fixture.repo, "update-ref", "refs/remotes/"+remote+"/"+baseBranch, trackingSHA); err != nil {
			t.Fatalf("seed tracking ref: %v", err)
		}
	}
	return fixture
}

// TestRunner_BaseOnTargetProceeds: the tracking ref sits at the run's base —
// the run proceeds normally.
func TestRunner_BaseOnTargetProceeds(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.runner.publication = publicationHookForTest("origin", "main")
	branch := defaultBranchOf(t, fixture.repo)
	head, headErr := execGit(fixture.repo, "rev-parse", "HEAD")
	if headErr != nil {
		t.Fatal(headErr)
	}
	if _, refErr := execGit(fixture.repo, "update-ref", "refs/remotes/origin/main", head); refErr != nil {
		t.Fatal(refErr)
	}
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: branch}
	fixture.driveToGate(t)
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate", state)
	}
	for _, event := range fixture.store.events {
		if event.Type == EvRunBaseOffTarget {
			t.Fatal("an on-target base produced an off-target event")
		}
	}
}

// TestRunner_DeveloperBranchBasePauses: the run's base sits one commit AHEAD
// of the target branch's comparison point — exactly the developer-branch
// scenario — so the run pauses instead of inheriting unpushed commits into
// its future pull request.
func TestRunner_DeveloperBranchBasePauses(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.runner.publication = publicationHookForTest("origin", "main")
	branch := defaultBranchOf(t, fixture.repo)
	// The "remote" stays at the current HEAD while the local branch gains an
	// unpushed commit: the run's base is then one commit beyond the target.
	remoteTip, tipErr := execGit(fixture.repo, "rev-parse", "HEAD")
	if tipErr != nil {
		t.Fatal(tipErr)
	}
	if writeErr := os.WriteFile(filepath.Join(fixture.repo, "local-wip.txt"), []byte("unpushed"), 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	if commitErr := gitCommitAll(fixture.repo, "local work ahead of the remote"); commitErr != nil {
		t.Fatal(commitErr)
	}
	if _, refErr := execGit(fixture.repo, "update-ref", "refs/remotes/origin/main", remoteTip); refErr != nil {
		t.Fatal(refErr)
	}
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: branch}

	if runErr := fixture.runner.HandleRun(context.Background(), job("run", fixture.runID, "tn", "us")); runErr != nil {
		t.Fatalf("run job: %v", runErr)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (base_not_on_target)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvRunBaseOffTarget)
	// No invocation ran over the off-target base: the run stopped before any
	// side effect.
	if count := len(fixture.store.invocations); count != 0 {
		t.Fatalf("invocations = %d, want none before the base check", count)
	}
}

// TestRunner_TrackingRefBaseWithoutConfiguredBranchProceeds: with no
// configured publication base branch, a base_ref spelled as the remote-tracking
// ref (the form the API docs and the error message recommend) names its target
// branch and proceeds; the same ref spelled `origin/main` does too.
func TestRunner_TrackingRefBaseWithoutConfiguredBranchProceeds(t *testing.T) {
	t.Parallel()
	for _, baseRef := range []string{"refs/remotes/origin/main", "origin/main"} {
		t.Run(baseRef, func(t *testing.T) {
			t.Parallel()
			fixture := newRecoveryFixture(t, "running")
			fixture.runner.publication = publicationHookForTest("origin", "")
			head, headErr := execGit(fixture.repo, "rev-parse", "HEAD")
			if headErr != nil {
				t.Fatal(headErr)
			}
			if _, refErr := execGit(fixture.repo, "update-ref", "refs/remotes/origin/main", head); refErr != nil {
				t.Fatal(refErr)
			}
			fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
				State: "running", PipelinePack: "test@0.1.0", BaseRef: baseRef}
			fixture.driveToGate(t)
			for _, event := range fixture.store.events {
				if event.Type == EvRunBaseOffTarget {
					t.Fatalf("a tracking-ref base on its target produced an off-target event: %s", event.Payload)
				}
			}
		})
	}
}

// TestRunner_MissingComparisonPointPauses: publication enabled, but the
// remote-tracking ref does not exist — the run stops asking for a fetch or a
// configured base, never silently continuing.
func TestRunner_MissingComparisonPointPauses(t *testing.T) {
	t.Parallel()
	fixture := newBaseAncestryFixture(t, "origin", "main", "")
	if runErr := fixture.runner.HandleRun(context.Background(), job("run", fixture.runID, "tn", "us")); runErr != nil {
		t.Fatalf("run job: %v", runErr)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (base_target_unverifiable)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvRunBaseOffTarget)
}

// TestRunner_HeadBaseRefWithoutConfiguredBranchPauses: with no configured
// publication base branch, a HEAD base_ref cannot name a comparison point —
// the run stops rather than defaulting to anything.
func TestRunner_HeadBaseRefWithoutConfiguredBranchPauses(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.runner.publication = publicationHookForTest("origin", "")
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: "HEAD"}
	if runErr := fixture.runner.HandleRun(context.Background(), job("run", fixture.runID, "tn", "us")); runErr != nil {
		t.Fatalf("run job: %v", runErr)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (base_target_unverifiable)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvRunBaseOffTarget)
}

// TestRunner_BaseCheckSkippedWhenPublicationDisabled: the ancestry gate is a
// publication concern; with publication off, a local-branch base runs.
func TestRunner_BaseCheckSkippedWhenPublicationDisabled(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: "HEAD"}
	fixture.driveToGate(t)
	if state := fixture.store.taskState(); state != "paused_gate" {
		t.Fatalf("state = %q, want paused_gate", state)
	}
}

// TestRunner_UnresolvableBaseRefPauses: a base_ref that names nothing is a
// liftable condition, not a run defect — the run pauses and continue
// re-attempts the resolution.
func TestRunner_UnresolvableBaseRefPauses(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t, "running")
	fixture.store.record = sqlc.Run{ID: fixture.runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseRef: "no-such-branch"}
	if runErr := fixture.runner.HandleRun(context.Background(), job("run", fixture.runID, "tn", "us")); runErr != nil {
		t.Fatalf("run job: %v", runErr)
	}
	if state := fixture.store.taskState(); state != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (base_ref_unresolvable)", state)
	}
	requireRecoveryEvent(t, fixture.store, EvRunBaseOffTarget)
}

// publicationHookForTest builds the hook shape server.New wires from config.
func publicationHookForTest(remote, baseBranch string) PublicationHook {
	return PublicationHook{Enabled: true, Provider: "github", Remote: remote, BaseBranch: baseBranch}
}
