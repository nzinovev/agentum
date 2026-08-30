// Package worktree manages the per-task git worktrees off a project's repo
// (C5). The runner creates one worktree per task at <repo>/.agentum/worktrees/
// <task-id>/ on branch agentum/<task-id>, reuses it across stages and resumes,
// and tears it down when the task reaches a terminal state.
//
// F.6.1 splits teardown into two distinct actions:
//   - RemoveWorktree disposes of the per-task working tree at terminal state.
//     The branch agentum/<task-id> and its commits survive — they are the
//     durable delivery output a human reviews and Epic 8 hands off.
//   - DeleteBranch is the explicit, audited cleanup that removes the branch once
//     the delivery is no longer needed. It is never auto-run at teardown.
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

// revParseCmd is the git subcommand several methods use to resolve refs and
// HEAD to a commit SHA. A const so the calls share one source of truth.
const revParseCmd = "rev-parse"

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

// Manager creates, inspects, reconciles, and removes per-task worktrees. It
// carries no mutable state; methods are safe to call concurrently for different
// task ids (git serializes worktree operations internally).
type Manager struct{}

// New returns a Manager.
func New() *Manager { return &Manager{} }

// Create makes (or, if it already exists, returns) the worktree for taskID off
// repoPath on branch agentum/<task-id>, rooted at baseCommit. A non-empty
// baseCommit (a resolved full SHA) is used as the branch start-point so the
// task's lineage is pinned to exactly what base_ref pointed at when the runner
// resolved it; an empty baseCommit falls back to the repo's current HEAD (used
// by tests and the pre-F.6.1 path). It ensures the repo ignores its own
// .agentum/ dir so worktrees and artifacts do not pollute the user's working
// tree as untracked files.
func (manager *Manager) Create(ctx context.Context, repoPath, taskID, baseCommit string) (*Worktree, error) {
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
	// resume/retry (which re-enters Create) from failing on the second pass —
	// and preserves the lineage of an in-flight task (we never rebuild it from
	// a different base mid-run).
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

	// Create the worktree on a new branch off baseCommit (or HEAD). -b names the
	// branch; the branch is created off the start-point and checked out in the
	// new working tree. Pinning to baseCommit is what makes base_commit an
	// immutable lineage anchor — a later move of base_ref cannot retcon it.
	args := []string{"worktree", "add", "-b", branch, wtPath}
	if baseCommit != "" {
		args = append(args, baseCommit)
	}
	if out, err := git(ctx, repoAbs, args...); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return &Worktree{Root: wtPath, Branch: branch, RepoPath: repoAbs}, nil
}

// ResolveRef resolves a ref (branch / tag / SHA / "HEAD") to its full commit SHA
// in the project repo. Used once per task, before the worktree is created, so
// base_commit is an immutable anchor. Returns ErrUnknownRef when the ref cannot
// be resolved — the caller surfaces this as a bad-input error before any work
// starts.
func (manager *Manager) ResolveRef(ctx context.Context, repoPath, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(ref) == "" {
		ref = "HEAD"
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	// --verify ensures a single SHA; ^{commit} peels tags to the commit so a
	// tag base_ref lands on the commit it points at. --quiet keeps stderr clean
	// on a miss; we synthesize a typed error for the caller.
	out, err := git(ctx, repoAbs, revParseCmd, "--verify", "--quiet", ref+"^{commit}")
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return "", fmt.Errorf("%w: %q", ErrUnknownRef, ref)
	}
	return strings.TrimSpace(string(out)), nil
}

// orchestratorIdentity is the git identity the orchestrator uses for the
// checkpoint commits it authors. It is passed inline via `git -c` rather than
// read from ambient config, because user.name / user.email are unset in CI
// containers and on a fresh operator server, and `git commit` refuses without
// them. A named constant also makes the audit trail unambiguous: a checkpoint
// commit's author is Agentum, not a human and not the agent's own git.write
// commits (which carry whatever identity the adapter configured). This is the
// git.delivery privilege the capability model reserves for the orchestrator
// (internal/caps: CatGitDelivery) being exercised — no agent role carries it.
const (
	orchestratorIdentityName  = "agentum"
	orchestratorIdentityEmail = "agentum@orchestrator"
)

// Commit stages everything in the worktree and commits it on the task branch,
// returning the new commit SHA. Returns (head, false, nil) when the tree is
// already clean — a boundary that produced no change is a real outcome, not an
// error, and an empty commit would pollute the lineage a reviewer reads.
//
// The orchestrator authors the commit itself rather than reading whatever the
// agent left at HEAD because the agent is not granted git.delivery: nothing in
// the orchestrator ran `git commit` before this, so without Commit a checkpoint
// is whatever the agent happened to commit (often nothing), and the work lives
// only in the working tree until teardown discards it. The checkpoint SHA this
// returns is therefore a real snapshot the orchestrator owns — Restore can
// return to it, and a reviewer can check it out and reproduce what was verified.
func (manager *Manager) Commit(ctx context.Context, wtRoot, message string) (commit string, created bool, err error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	clean, err := manager.IsClean(ctx, wtRoot)
	if err != nil {
		return "", false, err
	}
	if clean {
		// A boundary that produced no change is reported honestly as the
		// unchanged HEAD, not as a new empty commit. An empty commit per stage
		// would corrupt the lineage a reviewer reads (a flat line of no-op
		// checkpoints obscuring where real work landed).
		head, headErr := manager.HeadCommit(ctx, wtRoot)
		if headErr != nil {
			return "", false, headErr
		}
		return head, false, nil
	}
	// Stage everything tracked under the worktree. .agentum/ is excluded by
	// ensureIgnored (worktree.go:430), so artifact-dir churn does not enter the
	// commit — this keeps the checkpoint a snapshot of the work, not of
	// orchestrator bookkeeping.
	if out, stageErr := git(ctx, wtRoot, "add", "-A"); stageErr != nil {
		return "", false, fmt.Errorf("git add -A: %w (%s)", stageErr, strings.TrimSpace(string(out)))
	}
	// Identity is passed inline via -c so the commit succeeds without ambient
	// git config (CI containers, fresh servers) and is authored by Agentum in
	// the audit trail.
	if out, commitErr := git(ctx, wtRoot,
		"-c", "user.name="+orchestratorIdentityName,
		"-c", "user.email="+orchestratorIdentityEmail,
		"commit", "-m", message,
	); commitErr != nil {
		return "", false, fmt.Errorf("git commit: %w (%s)", commitErr, strings.TrimSpace(string(out)))
	}
	head, headErr := manager.HeadCommit(ctx, wtRoot)
	if headErr != nil {
		return "", false, headErr
	}
	return head, true, nil
}

// HeadCommit returns the full commit SHA the worktree's HEAD currently points
// at. The runner records this at stage boundaries as a checkpoint SHA. Operates
// on the worktree dir (the checked-out branch HEAD), not the project repo.
func (manager *Manager) HeadCommit(ctx context.Context, wtRoot string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	out, err := git(ctx, wtRoot, revParseCmd, "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// FileAtCommit reads path exactly as it existed at commit in repoPath, returning
// the raw bytes. Used to load agent-immutable project config — the checks
// registry — from the task's lineage anchor (base_commit), so an agent that
// edits the file inside its worktree cannot weaken the checks that gate its own
// delivery. A path that does not exist at the commit is reported as
// os.ErrNotExist; callers treat that as "the project defines no registry."
func (manager *Manager) FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(commit) == "" {
		return nil, errors.New("worktree: FileAtCommit requires a non-empty commit")
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	out, err := git(ctx, repoAbs, "show", commit+":"+path)
	if err != nil {
		message := strings.TrimSpace(string(out))
		// git exits non-zero with a "does not exist" message when the path is
		// absent at the commit; surface that as a typed os.ErrNotExist so the
		// caller can distinguish "no registry" from a real git failure.
		if strings.Contains(message, "does not exist") {
			return nil, fmt.Errorf("%w: %s at %s", os.ErrNotExist, path, commit)
		}
		return nil, fmt.Errorf("git show %s:%s: %w (%s)", commit, path, err, message)
	}
	return out, nil
}

// IsClean reports whether the worktree has no uncommitted changes. Exposed so
// the runner's evaluator (auto_if_clean gate) and the reconciler share one
// definition of "clean". Any porcelain entry ⇒ not clean.
func (manager *Manager) IsClean(ctx context.Context, wtRoot string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	out, err := git(ctx, wtRoot, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return len(strings.TrimSpace(string(out))) == 0, nil
}

// Diff runs `git diff [--stat] from..to` in the worktree and returns the raw
// output. Used by the orchestrator-produced diff (ADR 0003 D5): the reviewer
// reads the real change set without needing exec.bash to run git itself. stat
// selects the --stat form (a compact summary, never truncated by the caller);
// the default is the full patch (which the caller caps on a hunk boundary).
// Both commits must be resolvable in the worktree's repo.
func (manager *Manager) Diff(ctx context.Context, wtRoot, from, to string, stat bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return nil, errors.New("worktree: Diff requires non-empty from and to commits")
	}
	args := []string{"diff"}
	if stat {
		args = append(args, "--stat")
	}
	args = append(args, from+".."+to)
	out, err := git(ctx, wtRoot, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff %s..%s: %w (%s)", from, to, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Restore moves the worktree's checked-out branch, its index, its working tree,
// AND removes untracked files to the given commit. Used by the reconciler when a
// crashed worktree is classified as restorable: it restores the last checkpoint
// (or base_commit) so a side-effectful stage is never blindly replayed against a
// half-modified tree. `git reset --hard` alone does not remove untracked files
// the agent may have written mid-stage, so Restore pairs it with `git clean
// -fd` — the post-restore tree is byte-identical to the checkpoint. The commit
// must be a resolvable full SHA from a prior checkpoint or base_commit.
func (manager *Manager) Restore(ctx context.Context, wtRoot, commit string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(commit) == "" {
		return errors.New("worktree: Restore requires a non-empty commit")
	}
	out, err := git(ctx, wtRoot, "reset", "--hard", commit)
	if err != nil {
		return fmt.Errorf("git reset --hard %s: %w (%s)", commit, err, strings.TrimSpace(string(out)))
	}
	// Remove untracked files AND empty dirs the agent left behind mid-stage.
	// -d lets clean remove directories; -f makes non-empty untracked removal
	// non-interactive. The .agentum/ worktree-local artifact tree is excluded
	// (gitignored), so this never touches result.json or stage artifacts.
	cleanOut, err := git(ctx, wtRoot, "clean", "-fd")
	if err != nil {
		return fmt.Errorf("git clean -fd: %w (%s)", err, strings.TrimSpace(string(cleanOut)))
	}
	return nil
}

// Classification is the reconciler's verdict on a worktree's state after a
// crash, before a retry/resume. It drives the runner's recovery policy: a
// side-effectful stage is replayed only against a clean or safely-resumable
// tree; a restorable tree is reset to CheckpointCommit first; anything else
// surfaces for a human.
type Classification int

const (
	// ClassUnknown is the zero value and is never returned by Reconcile; it
	// exists so an unset Classification reads as "not yet classified".
	ClassUnknown Classification = iota
	// ClassClean means no committed work beyond the base and no uncommitted
	// changes. The stage can start fresh as if it had never run.
	ClassClean
	// ClassResumable means committed work exists beyond the base, and the
	// working tree is clean. The next stage resumes from the recorded HEAD.
	ClassResumable
	// ClassRestorable means the working tree has uncommitted changes. The
	// runner resets to CheckpointCommit (the last checkpoint, or base_commit if
	// none) before retrying — a side-effectful stage is never replayed against
	// a partially-modified tree.
	ClassRestorable
	// ClassNeedsAttention means the worktree is missing or in a state the
	// reconciler cannot safely classify (detached HEAD, HEAD behind base). The
	// runner surfaces this for a human rather than guessing.
	ClassNeedsAttention
)

// String gives the audit-log / event payload representation of a class.
func (classification Classification) String() string {
	switch classification {
	case ClassClean:
		return "clean"
	case ClassResumable:
		return "resumable"
	case ClassRestorable:
		return "restorable"
	case ClassNeedsAttention:
		return "needs_attention"
	default:
		return "unknown"
	}
}

// ReconcileState is the reconciler's verdict: the class plus the commit the
// runner should act on. For ClassRestorable it is the restore target (last
// checkpoint SHA, falling back to baseCommit); for the clean/resumable classes
// it is the current HEAD; for needs-attention it is empty.
type ReconcileState struct {
	Class            Classification
	HeadCommit       string // current worktree HEAD (empty if worktree missing)
	CheckpointCommit string // restore target for ClassRestorable (last checkpoint or base)
}

// Reconcile classifies the worktree for taskID after a crash, before a retry or
// resume. baseCommit is the task's resolved base_commit (the lineage anchor);
// lastCheckpoint is the most recent orchestrator-recorded checkpoint SHA, or ""
// if none exists yet (the reconciler then falls back to baseCommit).
//
// The classification is conservative by design: when in doubt, surface for
// human attention rather than risk replaying a side-effectful stage.
func (manager *Manager) Reconcile(ctx context.Context, repoPath, taskID, baseCommit, lastCheckpoint string) (ReconcileState, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileState{}, err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return ReconcileState{}, fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, taskID)
	if !isWorktree(wtPath) {
		// Worktree gone but task wants to run: a human removed it (or teardown
		// ran early). Re-creating would silently rebuild from base and replay
		// side effects — surface instead.
		return ReconcileState{Class: ClassNeedsAttention}, nil
	}

	head, err := manager.HeadCommit(ctx, wtPath)
	if err != nil {
		return ReconcileState{}, err
	}
	clean, err := manager.IsClean(ctx, wtPath)
	if err != nil {
		return ReconcileState{}, err
	}

	// Dirty working tree ⇒ restorable. The restore target is the last
	// checkpoint (an orchestrator-owned boundary) if one exists; otherwise
	// base_commit.
	if !clean {
		return ReconcileState{
			Class: ClassRestorable, HeadCommit: head,
			CheckpointCommit: restoreTarget(lastCheckpoint, baseCommit),
		}, nil
	}
	return classifyCleanTree(ctx, repoAbs, head, baseCommit, lastCheckpoint)
}

// restoreTarget picks the commit a restorable worktree resets to: the last
// checkpoint when one was recorded, else the task's base_commit.
func restoreTarget(lastCheckpoint, baseCommit string) string {
	if strings.TrimSpace(lastCheckpoint) != "" {
		return lastCheckpoint
	}
	return baseCommit
}

// classifyCleanTree verdicts a clean working tree. If HEAD is exactly the base
// or the last checkpoint, nothing has happened since that boundary — clean.
// Otherwise committed work exists and the next stage resumes from HEAD, unless
// HEAD is not descended from base (a divergent lineage), which surfaces for a
// human rather than guessing.
func classifyCleanTree(ctx context.Context, repoAbs, head, baseCommit, lastCheckpoint string) (ReconcileState, error) {
	if head == baseCommit {
		return ReconcileState{Class: ClassClean, HeadCommit: head, CheckpointCommit: baseCommit}, nil
	}
	if lastCheckpoint != "" && head == lastCheckpoint {
		return ReconcileState{Class: ClassClean, HeadCommit: head, CheckpointCommit: lastCheckpoint}, nil
	}
	if lineageDiverged(ctx, repoAbs, baseCommit, head) {
		return ReconcileState{Class: ClassNeedsAttention, HeadCommit: head}, nil
	}
	return ReconcileState{Class: ClassResumable, HeadCommit: head, CheckpointCommit: lastCheckpoint}, nil
}

// lineageDiverged reports whether head is cleanly NOT descended from
// baseCommit — an unexpected lineage (force-pushed behind, detached) the
// reconciler must not guess about. `merge-base --is-ancestor` exits non-zero
// when base is not an ancestor; an empty output on that non-zero exit is the
// clean verdict, while a real git failure usually carries a message.
func lineageDiverged(ctx context.Context, repoAbs, baseCommit, head string) bool {
	if baseCommit == "" || baseCommit == head {
		return false
	}
	out, err := git(ctx, repoAbs, "merge-base", "--is-ancestor", baseCommit, head)
	return err != nil && strings.TrimSpace(string(out)) == ""
}

// RemoveWorktree removes only the per-task working tree. The agentum/<task-id>
// branch and its commits remain resolvable — they are the durable delivery
// output that survives teardown (F.6.1 AC #3). Idempotent: a missing worktree
// is a no-op. Used at terminal state (done/cancelled/failed).
func (manager *Manager) RemoveWorktree(ctx context.Context, repoPath, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, taskID)
	if !isWorktree(wtPath) {
		return nil
	}
	// --force: the worktree may contain uncommitted agent work; teardown at
	// terminal state discards the *working tree* but the branch tip (committed
	// delivery) is preserved by virtue of not deleting the branch here.
	out, err := git(ctx, repoAbs, "worktree", "remove", "--force", wtPath)
	if err != nil {
		return fmt.Errorf("git worktree remove: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteBranch removes the agentum/<task-id> branch. This is the explicit,
// audited cleanup action — distinct from terminal teardown. Idempotent: a
// missing branch is a no-op. -D forces removal even if not merged: a delivered
// task's commits are reviewed via result_commit / the branch ref; deletion is
// the operator saying "I am done with this delivery."
func (manager *Manager) DeleteBranch(ctx context.Context, repoPath, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	branch := BranchFor(taskID)
	out, err := git(ctx, repoAbs, "branch", "-D", branch)
	if err != nil {
		// A missing branch is a successful no-op; anything else is real.
		if strings.Contains(string(out), "not found") {
			return nil
		}
		return fmt.Errorf("git branch delete: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureIgnored appends ".agentum/" to the repo's local excludes file
// (.git/info/exclude, resolved via git so worktree-shared repos are correct) so
// the worktrees dir and in-worktree artifact dirs never appear as untracked.
// Idempotent. This is local-only: it does not touch any tracked .gitignore.
func (manager *Manager) ensureIgnored(ctx context.Context, repoAbs string) error {
	out, err := git(ctx, repoAbs, revParseCmd, "--git-path", "info/exclude")
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
	if _, writeErr := file.WriteString(excludeEntry); writeErr != nil {
		_ = file.Close()
		return fmt.Errorf("write excludes file: %w", writeErr)
	}
	// Close is checked, not deferred: this handle is open for output, where a
	// write can still be buffered, so a full disk or an I/O fault surfaces
	// here and nowhere else. Discarding it would report a successful append
	// that never reached the excludes file.
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close excludes file: %w", closeErr)
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

// Typed errors the callers branch on.
var (
	// ErrNotExist is returned when a worktree is expected but absent. Kept for
	// callers that want to distinguish from a git failure.
	ErrNotExist = errors.New("worktree does not exist")
	// ErrUnknownRef is returned by ResolveRef when the ref cannot be resolved
	// to a commit in the repo. Callers surface it as a bad-input error before
	// any work starts.
	ErrUnknownRef = errors.New("worktree: unknown ref")
)
