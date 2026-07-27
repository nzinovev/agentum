package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchFor_PathFor_ArtifactDir(t *testing.T) {
	t.Parallel()
	if got := BranchFor("abc-123"); got != "agentum/abc-123" {
		t.Errorf("BranchFor = %q", got)
	}
	if got := PathFor("/repo", "abc-123"); got != filepath.Join("/repo", ".agentum", "worktrees", "abc-123") {
		t.Errorf("PathFor = %q", got)
	}
	got := ArtifactDir("/repo/.agentum/worktrees/abc-123", "abc-123", "spec")
	want := filepath.Join("/repo/.agentum/worktrees/abc-123", ".agentum", "abc-123", ".ag-artifacts", "spec")
	if got != want {
		t.Errorf("ArtifactDir = %q, want %q", got, want)
	}
}

func TestManager_Create_Idempotent_Remove(t *testing.T) {
	// Not parallel: each subtest builds on the same repo state, and Create/Remove
	// on the same repo must be observed in order.
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}

	manager := New()
	taskID := "task-001"

	// First create: makes the worktree. Empty baseCommit → HEAD (the
	// pre-F.6.1 path; production always passes a resolved SHA).
	worktree, err := manager.Create(t.Context(), repo, taskID, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if worktree.Branch != "agentum/task-001" {
		t.Errorf("Branch = %q", worktree.Branch)
	}
	if !isWorktree(worktree.Root) {
		t.Fatalf("worktree not created at %s", worktree.Root)
	}
	// The branch is checked out in the worktree.
	if out, err := git(t.Context(), worktree.Root, "rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		t.Fatalf("rev-parse HEAD: %v (%s)", err, out)
	} else if got := string(out); got[:len("agentum/task-001")] != "agentum/task-001" {
		t.Errorf("HEAD branch = %q", got)
	}

	// Second create: idempotent — returns the existing worktree, no error.
	secondWorktree, err := manager.Create(t.Context(), repo, taskID, "")
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if secondWorktree.Root != worktree.Root {
		t.Error("idempotent Create returned a different path")
	}

	// RemoveWorktree: tears down the working tree only. The branch must survive
	// (F.6.1 AC #3 — branch + commits remain resolvable after teardown).
	if err := manager.RemoveWorktree(t.Context(), repo, taskID); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if isWorktree(worktree.Root) {
		t.Error("worktree still present after RemoveWorktree")
	}
	if out, err := git(t.Context(), repo, "branch", "--list", "agentum/task-001"); err != nil {
		t.Fatalf("branch --list: %v", err)
	} else if len(out) == 0 {
		t.Errorf("branch deleted by RemoveWorktree; want it preserved: %q", out)
	}

	// DeleteBranch: explicit cleanup removes the branch now. Idempotent.
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if out, err := git(t.Context(), repo, "branch", "--list", "agentum/task-001"); err != nil {
		t.Fatalf("branch --list: %v", err)
	} else if len(out) != 0 {
		t.Errorf("branch still exists after DeleteBranch: %q", out)
	}

	// Both ops idempotent on a fully-cleaned task: no-op, no error.
	if err := manager.RemoveWorktree(t.Context(), repo, taskID); err != nil {
		t.Errorf("second RemoveWorktree should be a no-op, got: %v", err)
	}
	if err := manager.DeleteBranch(t.Context(), repo, taskID); err != nil {
		t.Errorf("second DeleteBranch should be a no-op, got: %v", err)
	}
}

func TestManager_EnsureIgnored(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup: %v", err)
	}
	manager := New()

	if _, err := manager.Create(t.Context(), repo, "task-002", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The repo's local excludes file must now ignore .agentum/.
	out, err := git(t.Context(), repo, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		t.Fatalf("locate excludes: %v", err)
	}
	excludePath := filepath.Join(repo, strings.TrimSpace(string(out)))
	content, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read excludes: %v", err)
	}
	if !strings.Contains(string(content), ".agentum/") {
		t.Errorf(".agentum/ not in excludes file; got:\n%s", content)
	}
	// .agentum/ must NOT show as untracked in the project repo.
	if out, err := git(t.Context(), repo, "status", "--porcelain"); err != nil {
		t.Fatalf("status: %v", err)
	} else if strings.Contains(string(out), ".agentum") {
		t.Errorf(".agentum shows as untracked; got:\n%s", out)
	}
}

// initRepoWithCommit turns dir into a git repo with one committed file. git
// worktree add requires at least one commit (it refuses an empty repo).
func initRepoWithCommit(dir string) error {
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@agentum"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &setupError{args: args, out: out, err: err}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"add", "README"},
		{"commit", "--quiet", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &setupError{args: args, out: out, err: err}
		}
	}
	return nil
}

type setupError struct {
	args []string
	out  []byte
	err  error
}

func (setupErr *setupError) Error() string {
	return "git " + setupErr.args[0] + ": " + setupErr.err.Error() + " (" + string(setupErr.out) + ")"
}
