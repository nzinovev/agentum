// Package worktree manages the per-task git worktrees off a project's repo
// (C5). The runner creates one worktree per task at <repo>/.agentum/worktrees/
// <task-id>/ on branch agentum/<task-id>, reuses it across stages and resumes,
// and tears it down when the task reaches a terminal state.
//
// Per-stage artifacts live under the worktree at <root>/.agentum/<task-id>/
// .ag-artifacts/<stage>/ (the §6.4 path convention; filesystem-as-bus, C1/C4).
// The runner computes these paths via ArtifactDir and creates the directories
// before invoking the adapter.
//
// All git operations shell out to the git binary found on PATH; there is no
// libgit2 dependency. The project repo must be a real work tree (validated at
// project registration).
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree is a created per-task working tree.
type Worktree struct {
	Root     string // absolute path to the worktree's working directory
	Branch   string // agentum/<task-id>
	RepoPath string // absolute path to the project repo it was created from
}

// BranchFor returns the canonical branch name for a task.
func BranchFor(taskID string) string {
	return "agentum/" + taskID
}

// PathFor returns the canonical worktree path under a project repo:
// <repo>/.agentum/worktrees/<task-id>.
func PathFor(repoPath, taskID string) string {
	return filepath.Join(repoPath, ".agentum", "worktrees", taskID)
}

// ArtifactDir returns the per-stage artifact directory inside a worktree. The
// caller (runner) is responsible for creating it; the adapter writes
// result.json there.
func ArtifactDir(wtRoot, taskID, stage string) string {
	return filepath.Join(wtRoot, ".agentum", taskID, ".ag-artifacts", stage)
}

// Manager creates and removes per-task worktrees. It carries no mutable state;
// methods are safe to call concurrently for different task ids (git serializes
// worktree operations internally).
type Manager struct{}

// New returns a Manager.
func New() *Manager { return &Manager{} }

// Create makes (or, if it already exists, returns) the worktree for taskID off
// repoPath on branch agentum/<task-id>, rooted at the repo's current HEAD. It
// ensures the repo ignores its own .agentum/ dir so worktrees and artifacts do
// not pollute the user's working tree as untracked files.
func (manager *Manager) Create(ctx context.Context, repoPath, taskID string) (*Worktree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, taskID)
	branch := BranchFor(taskID)

	// Idempotent: a worktree already at this path is returned as-is. This keeps
	// resume/retry (which re-enters Create) from failing on the second pass.
	if isWorktree(wtPath) {
		return &Worktree{Root: wtPath, Branch: branch, RepoPath: repoAbs}, nil
	}

	if err := manager.ensureIgnored(ctx, repoAbs); err != nil {
		// Non-fatal: a missing exclude entry only means the user sees untracked
		// .agentum files. Log-worthy at the caller, not a creation blocker.
		_ = err
	}

	// Parent must exist before `git worktree add` in some git versions.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent dir: %w", err)
	}

	// Create the worktree on a new branch from HEAD. -b names the branch; the
	// branch is created off the repo's current HEAD and checked out in the new
	// working tree. -f would overwrite a stale path; we avoid it and rely on
	// idempotency above plus explicit Remove for teardown.
	if out, err := git(ctx, repoAbs, "worktree", "add", "-b", branch, wtPath); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return &Worktree{Root: wtPath, Branch: branch, RepoPath: repoAbs}, nil
}

// Remove deletes the worktree for taskID and prunes its branch. Safe to call on
// an already-removed task (no-op). Used at terminal state (done/cancelled/
// failed) per §7.1.3.
func (manager *Manager) Remove(ctx context.Context, repoPath, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, taskID)
	branch := BranchFor(taskID)

	if isWorktree(wtPath) {
		// --force: the worktree may contain uncommitted agent work; teardown at
		// terminal state discards it (artifacts were already captured).
		if out, err := git(ctx, repoAbs, "worktree", "remove", "--force", wtPath); err != nil {
			return fmt.Errorf("git worktree remove: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	// Delete the task branch. -D forces removal even if not merged: a cancelled
	// or failed task's branch is discarded; a done task's commits were merged
	// elsewhere (F.8) or are abandoned by design at MVP. Errors for a missing
	// branch are ignored (already gone / never created).
	if out, err := git(ctx, repoAbs, "branch", "-D", branch); err != nil {
		if !strings.Contains(string(out), "not found") {
			return fmt.Errorf("git branch delete: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// ensureIgnored appends ".agentum/" to the repo's local excludes file
// (.git/info/exclude, resolved via git so worktree-shared repos are correct) so
// the worktrees dir and in-worktree artifact dirs never appear as untracked.
// Idempotent. This is local-only: it does not touch any tracked .gitignore.
func (manager *Manager) ensureIgnored(ctx context.Context, repoAbs string) error {
	out, err := git(ctx, repoAbs, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("locate excludes file: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	excludePath := filepath.Join(repoAbs, strings.TrimSpace(string(out)))
	content, _ := os.ReadFile(excludePath)
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == ".agentum/" {
			return nil // already ignored
		}
	}
	excludeEntry := ".agentum/\n"
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		excludeEntry = "\n" + excludeEntry
	}
	file, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open excludes file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(excludeEntry); err != nil {
		return fmt.Errorf("write excludes file: %w", err)
	}
	return nil
}

// isWorktree reports whether path looks like an existing worktree (a non-empty
// dir containing a .git file — worktrees use a .git file, not a .git dir).
func isWorktree(path string) bool {
	fileInfo, err := os.Stat(path)
	if err != nil || !fileInfo.IsDir() {
		return false
	}
	// A git worktree's working dir holds a `.git` file pointing at the common
	// dir. `gitfile` presence is the reliable signal; an empty dir is not one.
	gitEntry := filepath.Join(path, ".git")
	if _, err := os.Stat(gitEntry); err != nil {
		return false
	}
	return true
}

// git runs a git command in dir and returns combined output. Combined (not just
// stderr) so callers see git's full diagnostic; trimmed at the call sites.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	return cmd.CombinedOutput()
}

// ErrNotExist is returned when a worktree is expected but absent. Kept for
// callers that want to distinguish from a git failure.
var ErrNotExist = errors.New("worktree does not exist")
