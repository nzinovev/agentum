package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedChangeSurvivesTeardown is the F.6.1 AC #8 proof: a committed
// change on the agentum/<task-id> branch survives RemoveWorktree and remains
// diffable as base_commit..result_commit. The branch is the durable delivery
// output; the worktree is disposable.
func TestCommittedChangeSurvivesTeardown(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}

	manager := New()
	taskID := "task-survive"

	// Resolve the repo's HEAD as base_commit (mirrors what the runner does once
	// per task before creating the worktree).
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}

	wt, err := manager.Create(t.Context(), repo, taskID, baseCommit)
	if err != nil {
		t.Fatalf("create worktree off base_commit: %v", err)
	}

	// Simulate an agent making a committed change on the task branch.
	commitInWorktree(t, wt, "feature.txt", "new work", "implement feature")

	// Capture the result_commit (the branch tip) before teardown.
	resultCommit, err := manager.HeadCommit(t.Context(), wt.Root)
	if err != nil {
		t.Fatalf("head commit before teardown: %v", err)
	}
	if resultCommit == baseCommit {
		t.Fatalf("result_commit == base_commit; the committed change was not captured")
	}

	// Terminal teardown: working tree removed only.
	if err := manager.RemoveWorktree(t.Context(), repo, taskID); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if isWorktree(context.Background(), wt.Root) {
		t.Fatal("worktree dir still present after teardown")
	}

	// The branch and its commits remain resolvable; result_commit is the
	// diffable delivery anchor.
	assertDeliverySurvives(t, repo, taskID, baseCommit, resultCommit)

	// Explicit cleanup deletes the branch (and only the branch; worktree already
	// gone). Idempotent.
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if err := refExists(repo, "agentum/"+taskID); err == nil {
		t.Fatal("branch still resolvable after DeleteBranch")
	}
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Fatalf("DeleteBranch must be idempotent: %v", err)
	}
}

// commitInWorktree stages a single file as a commit on the worktree's checked-
// out branch — the shape of an agent's committed change. Lifted out of the
// teardown test so the add/commit loop does not inflate its complexity.
func commitInWorktree(t *testing.T, wt *Worktree, filename, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt.Root, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	for _, args := range [][]string{
		{"add", filename},
		{"commit", "--quiet", "-m", msg},
	} {
		if out, err := git(t.Context(), wt.Root, args...); err != nil {
			t.Fatalf("git %s: %v (%s)", args[0], err, out)
		}
	}
}

// assertDeliverySurvives checks the F.6.1 invariant that terminal teardown
// preserves: the agentum/<task-id> branch stays resolvable, the
// base_commit..result_commit diff is exactly the committed feature, and the
// branch tip equals the recorded result_commit.
func assertDeliverySurvives(t *testing.T, repo, taskID, baseCommit, resultCommit string) {
	t.Helper()
	if err := refExists(repo, "agentum/"+taskID); err != nil {
		t.Fatalf("branch not resolvable after teardown: %v", err)
	}
	diff := mustGit(t, repo, "diff", "--name-only", baseCommit+".."+resultCommit)
	if strings.TrimSpace(diff) != "feature.txt" {
		t.Fatalf("base..result diff = %q, want feature.txt", diff)
	}
	branchTip := strings.TrimSpace(mustGit(t, repo, "rev-parse", "agentum/"+taskID))
	if branchTip != resultCommit {
		t.Fatalf("branch tip = %s, result_commit = %s; they must match", branchTip, resultCommit)
	}
}

// TestResolveRef exercises base_ref → base_commit resolution: branch, tag, full
// SHA, and the default-to-HEAD behavior. An unknown ref surfaces as
// ErrUnknownRef so the caller rejects bad input before any work starts.
func TestResolveRef(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()

	// Resolve the repo's default branch dynamically — git's init default varies
	// (master vs main vs a configured init.defaultBranch), so we never hardcode
	// it. A second commit goes on a feature branch + tag so ref types differ.
	defaultBranch := strings.TrimSpace(mustGit(t, repo, "symbolic-ref", "--short", "HEAD"))
	headCommit := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))
	mustGit(t, repo, "checkout", "-b", "feature")
	mustWrite(repo, "second.txt", "x")
	mustGit(t, repo, "add", "second.txt")
	mustGit(t, repo, "commit", "--quiet", "-m", "second")
	mustGit(t, repo, "tag", "v1")

	cases := []struct {
		name string
		ref  string
	}{
		{"HEAD", "HEAD"},
		{"branch", defaultBranch},
		{"tag", "v1"},
		{"full SHA", headCommit},
		{"empty defaults to HEAD", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sha, err := manager.ResolveRef(t.Context(), repo, tc.ref)
			if err != nil {
				t.Fatalf("ResolveRef(%q): %v", tc.ref, err)
			}
			if len(sha) != 40 {
				t.Errorf("ResolveRef(%q) = %q; want a 40-char SHA", tc.ref, sha)
			}
		})
	}

	t.Run("unknown ref returns ErrUnknownRef", func(t *testing.T) {
		t.Parallel()
		_, err := manager.ResolveRef(t.Context(), repo, "no-such-ref")
		if err == nil {
			t.Fatal("ResolveRef(unknown) expected an error")
		}
		if !strings.Contains(err.Error(), ErrUnknownRef.Error()) {
			t.Errorf("error = %v, want it to wrap %q", err, ErrUnknownRef)
		}
	})
}

// TestReconcile_Classifications exercises all four reconciler verdicts (AC #5):
// clean, resumable, restorable, needs-attention. The runner trusts this to
// decide whether a side-effectful stage can replay.
func TestReconcile_Classifications(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, repo string, wt *Worktree, baseCommit string) string // returns lastCheckpoint (or "")
		want    Classification
	}{
		{
			name: "clean: HEAD at base, nothing happened",
			arrange: func(t *testing.T, _ string, _ *Worktree, _ string) string {
				return ""
			},
			want: ClassClean,
		},
		{
			name: "clean: HEAD at last checkpoint",
			arrange: func(t *testing.T, _ string, wt *Worktree, _ string) string {
				mustWrite(wt.Root, "c.txt", "x")
				mustGit(t, wt.Root, "add", "c.txt")
				mustGit(t, wt.Root, "commit", "--quiet", "-m", "checkpoint")
				checkpoint, err := New().HeadCommit(t.Context(), wt.Root)
				if err != nil {
					t.Fatalf("head: %v", err)
				}
				return checkpoint
			},
			want: ClassClean,
		},
		{
			name: "resumable: committed work beyond base, clean tree",
			arrange: func(t *testing.T, _ string, wt *Worktree, _ string) string {
				mustWrite(wt.Root, "work.txt", "x")
				mustGit(t, wt.Root, "add", "work.txt")
				mustGit(t, wt.Root, "commit", "--quiet", "-m", "did work")
				return ""
			},
			want: ClassResumable,
		},
		{
			name: "restorable: dirty working tree",
			arrange: func(t *testing.T, _ string, wt *Worktree, baseCommit string) string {
				// Uncommitted modification → restorable to base.
				mustWrite(wt.Root, "README", "dirty change")
				return ""
			},
			want: ClassRestorable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertReconcileClass(t, tc.arrange, tc.want)
		})
	}

	t.Run("needs attention: worktree missing", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		if err := initRepoWithCommit(repo); err != nil {
			t.Fatalf("setup: %v", err)
		}
		manager := New()
		// Never created a worktree for this task — Reconcile must not re-create
		// one silently; it surfaces for a human.
		state, err := manager.Reconcile(t.Context(), repo, "ghost-task", "", "")
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if state.Class != ClassNeedsAttention {
			t.Fatalf("class = %s, want needs_attention", state.Class)
		}
	})
}

// assertReconcileClass runs one reconciler case: build a repo + worktree off
// HEAD, run the case's arrange step, then assert Reconcile's verdict. Lifted
// out of the table driver so the setup/error branches do not inflate the
// test's complexity.
func assertReconcileClass(t *testing.T, arrange func(*testing.T, string, *Worktree, string) string, want Classification) {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	manager := New()
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	taskID := "task-" + sanitized(t.Name())
	wt, err := manager.Create(t.Context(), repo, taskID, baseCommit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lastCheckpoint := arrange(t, repo, wt, baseCommit)

	state, err := manager.Reconcile(t.Context(), repo, taskID, baseCommit, lastCheckpoint)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if state.Class != want {
		t.Fatalf("class = %s, want %s", state.Class, want)
	}
}

// TestReconcile_RestorableResetsToCheckpoint proves the restorable class
// carries the right restore target: the last checkpoint when present, else
// base_commit. The runner uses ResetHard with that target before retrying.
func TestReconcile_RestorableResetsToCheckpoint(t *testing.T) {
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	wt, err := manager.Create(t.Context(), repo, "task-restore", baseCommit)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Commit past base, capture as checkpoint, then dirty the tree.
	mustWrite(wt.Root, "safe.txt", "committed")
	mustGit(t, wt.Root, "add", "safe.txt")
	mustGit(t, wt.Root, "commit", "--quiet", "-m", "safe checkpoint")
	checkpoint, err := manager.HeadCommit(t.Context(), wt.Root)
	if err != nil {
		t.Fatalf("checkpoint head: %v", err)
	}
	mustWrite(wt.Root, "scratch.txt", "uncommitted")

	state, err := manager.Reconcile(t.Context(), repo, "task-restore", baseCommit, checkpoint)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if state.Class != ClassRestorable {
		t.Fatalf("class = %s, want restorable", state.Class)
	}
	if state.CheckpointCommit != checkpoint {
		t.Fatalf("CheckpointCommit = %s, want the last checkpoint %s", state.CheckpointCommit, checkpoint)
	}

	// Restore to the checkpoint clears the dirty tree AND untracked files; the
	// committed checkpoint survives, the scratch file is gone.
	if err := manager.Restore(t.Context(), wt.Root, state.CheckpointCommit); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Root, "scratch.txt")); err == nil {
		t.Error("scratch.txt survived Restore; working tree not restored")
	}
	if _, err := os.Stat(filepath.Join(wt.Root, "safe.txt")); err != nil {
		t.Error("safe.txt (committed at checkpoint) did not survive Restore")
	}
}

// --- helpers ---

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t.Context(), dir, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v (%s)", args[0], dir, err, out)
	}
	return string(out)
}

func mustWrite(dir, name, body string) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		panic(err)
	}
}

func refExists(repo, ref string) error {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref)
	if out, err := cmd.CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return errMissingRef(ref)
	}
	return nil
}

func errMissingRef(ref string) error {
	// A lightweight typed-ish error so the test reads cleanly.
	return &missingRefError{ref: ref}
}

type missingRefError struct{ ref string }

func (err *missingRefError) Error() string { return "ref not resolvable: " + err.ref }

// sanitized turns a subtest name into a branch-suffix-safe slug.
func sanitized(name string) string {
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == ':' || r == ',' || r == '-' || r == '.'
	})
	return strings.Join(fields, "_")
}
