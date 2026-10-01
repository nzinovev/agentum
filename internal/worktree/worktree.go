// Package worktree manages the per-run git worktrees off a project's repo
// (C5). The runner creates one worktree per run at <repo>/.agentum/worktrees/
// <run-id>/ on branch agentum/<run-id>, reuses it across stages and resumes,
// and tears it down when the run reaches a human-terminal state (done /
// cancelled). A failed run keeps its tree: the uncommitted work may be the
// only copy, and disposal is a separate audited human action.
//
// F.6.1 splits teardown into two distinct actions:
//   - RemoveWorktree disposes of the per-run working tree at terminal state.
//     The branch agentum/<run-id> and its commits survive — they are the
//     durable delivery output a human reviews and Epic 8 hands off.
//   - DeleteBranch is the explicit, audited cleanup that removes the branch once
//     the delivery is no longer needed. It is never auto-run at teardown.
//
// Per-stage artifacts live under the worktree at <root>/.agentum/<run-id>/
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
	"strconv"
	"strings"
)

// Worktree is a created per-run working tree.
type Worktree struct {
	Root     string // absolute path to the worktree's working directory
	Branch   string // agentum/<run-id>
	RepoPath string // absolute path to the project repo it was created from
}

// revParseCmd is the git subcommand several methods use to resolve refs and
// HEAD to a commit SHA. A const so the calls share one source of truth.
const revParseCmd = "rev-parse"

// BranchFor returns the canonical branch name for a run.
func BranchFor(runID string) string {
	return "agentum/" + runID
}

// PathFor returns the canonical worktree path under a project repo:
// <repo>/.agentum/worktrees/<run-id>.
func PathFor(repoPath, runID string) string {
	return filepath.Join(repoPath, ".agentum", "worktrees", runID)
}

// ArtifactDir returns the per-stage artifact directory inside a worktree. The
// caller (runner) is responsible for creating it; the adapter writes
// result.json there.
func ArtifactDir(wtRoot, runID, stage string) string {
	return filepath.Join(wtRoot, ".agentum", runID, ".ag-artifacts", stage)
}

// Manager creates, inspects, reconciles, and removes per-run worktrees. It
// carries no mutable state; methods are safe to call concurrently for different
// run ids (git serializes worktree operations internally).
type Manager struct{}

// New returns a Manager.
func New() *Manager { return &Manager{} }

// Create makes (or, if it already exists, returns) the worktree for runID off
// repoPath on branch agentum/<run-id>, rooted at baseCommit. A non-empty
// baseCommit (a resolved full SHA) is used as the branch start-point so the
// run's lineage is pinned to exactly what base_ref pointed at when the runner
// resolved it; an empty baseCommit falls back to the repo's current HEAD (used
// by tests and the pre-F.6.1 path). When the branch already exists without a
// worktree (a discarded tree of a resumable run), the branch is checked out at
// its tip and baseCommit is not used. It maintains the repo's info/exclude
// rules for .agentum/ (worktrees and artifacts stay out of the user's commits,
// .agentum/packs/ stays committable) so worktrees and artifacts do not pollute
// the user's working tree as untracked files.
func (manager *Manager) Create(ctx context.Context, repoPath, runID, baseCommit string) (*Worktree, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, runID)
	branch := BranchFor(runID)

	// Idempotent: a worktree already at this path is returned as-is. This keeps
	// resume/retry (which re-enters Create) from failing on the second pass —
	// and preserves the lineage of an in-flight run (we never rebuild it from
	// a different base mid-run). The check requires the worktree to be LIVE
	// (git can enter it): a directory left over from a repository move holds a
	// .git file with stale absolute pointers and is not a worktree until
	// Repair rewires it — returning it as-is would hand the run a directory
	// every git command refuses to touch.
	if isWorktree(ctx, wtPath) {
		return &Worktree{Root: wtPath, Branch: branch, RepoPath: repoAbs}, nil
	}

	if err := manager.EnsureExcludes(ctx, repoAbs); err != nil {
		// Non-fatal: a missing exclude entry only means the user sees untracked
		// .agentum files. Log-worthy at the caller, not a creation blocker.
		_ = err
	}

	// Parent must exist before `git worktree add` in some git versions.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent dir: %w", err)
	}

	// A surviving branch with no worktree is a run whose tree was discarded
	// while the run stayed resumable (RemoveWorktree keeps the branch). Check
	// the branch out again at its own tip: the run's committed work is the
	// lineage now, and re-creating it off baseCommit would drop it — while
	// `-b` on an existing branch fails outright and strands the run.
	args := []string{"worktree", "add", wtPath, branch}
	if _, branchErr := git(ctx, repoAbs, revParseCmd, "--verify", "--quiet", "refs/heads/"+branch); branchErr != nil {
		// Create the worktree on a new branch off baseCommit (or HEAD). -b
		// names the branch; the branch is created off the start-point and
		// checked out in the new working tree. Pinning to baseCommit is what
		// makes base_commit an immutable lineage anchor — a later move of
		// base_ref cannot change it after the fact.
		args = []string{"worktree", "add", "-b", branch, wtPath}
		if baseCommit != "" {
			args = append(args, baseCommit)
		}
	}
	if out, err := git(ctx, repoAbs, args...); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return &Worktree{Root: wtPath, Branch: branch, RepoPath: repoAbs}, nil
}

// ResolveRef resolves a ref (branch / tag / SHA / "HEAD") to its full commit SHA
// in the project repo. Used once per run, before the worktree is created, so
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

// Commit stages everything in the worktree and commits it on the run branch,
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
		// A boundary that produced no change is reported as the
		// unchanged HEAD, not as a new empty commit. An empty commit per stage
		// would corrupt the lineage a reviewer reads (a flat line of no-op
		// checkpoints obscuring where real work landed).
		head, headErr := manager.HeadCommit(ctx, wtRoot)
		if headErr != nil {
			return "", false, headErr
		}
		return head, false, nil
	}
	// Stage everything tracked under the worktree. .agentum/'s children are
	// excluded by EnsureExcludes (except .agentum/packs/, the project's own
	// tracked packs), so artifact-dir churn does not enter the commit — this
	// keeps the checkpoint a snapshot of the work, not of orchestrator
	// bookkeeping.
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
// registry — from the run's lineage anchor (base_commit), so an agent that
// edits the file inside its worktree cannot weaken the checks that gate its own
// delivery. A path that does not exist at the commit is reported as
// os.ErrNotExist; callers treat that as "the project defines no registry."
//
// Existence is decided by the COMMIT'S TREE, not by git's error text. The old
// form ran `git show commit:path` and matched the English "does not exist"
// message to detect absence — which broke the moment git chose a different
// wording ("exists on disk, but not in ...", a localized message) and let a
// file present only in the working copy masquerade as a hard git failure (or
// the reverse). Here the commit is verified first (an invalid or unknown SHA is
// its own error, never an absence), then `git ls-tree commit -- path` decides
// presence: a valid commit plus empty listing means the path is absent from
// the tree — independent of locale and of the working copy. Only then does
// `git show` read the bytes; a corrupt object fails there, as a real error.
func (manager *Manager) FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(commit) == "" {
		return nil, errors.New("worktree: FileAtCommit requires a non-empty commit")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("worktree: FileAtCommit requires a non-empty path")
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	if out, resolveErr := git(ctx, repoAbs, revParseCmd, "--verify", "--quiet", commit+"^{commit}"); resolveErr != nil {
		return nil, fmt.Errorf("worktree: commit %s does not resolve: %w (%s)", commit, resolveErr, strings.TrimSpace(string(out)))
	}
	listing, listErr := git(ctx, repoAbs, "ls-tree", commit, "--", path)
	if listErr != nil {
		return nil, fmt.Errorf("git ls-tree %s -- %s: %w (%s)", commit, path, listErr, strings.TrimSpace(string(listing)))
	}
	if len(strings.TrimSpace(string(listing))) == 0 {
		return nil, fmt.Errorf("%w: %s at %s", os.ErrNotExist, path, commit)
	}
	out, err := git(ctx, repoAbs, "show", commit+":"+path)
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w (%s)", commit, path, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// TreeEntry is one entry of a commit's recursive tree listing. Mode is the raw
// git mode string ("100644", "100755", "120000" for a symlink, "160000" for a
// submodule) and Size is the blob size in bytes (0 for non-blobs).
type TreeEntry struct {
	Path string
	Mode string
	Type string
	Size int64
}

// ListTreeAtCommit lists every entry under dir exactly as it existed at commit,
// recursively. A directory absent from the commit's tree is an empty listing,
// not an error — callers treat that as "the project defines no pack there",
// mirroring FileAtCommit's absence-by-tree contract. The commit itself is
// verified first (rev-parse --verify), so an unknown SHA is its own error and
// never masquerades as absence.
//
// The listing is the read side of the agent-immutability seam for project
// packs: mode and size arrive from the commit's tree, so symlink entries and
// oversized blobs are rejected before any byte is read. Paths come back raw
// (-z: no quoting) even when they carry spaces or non-ASCII bytes.
func (manager *Manager) ListTreeAtCommit(ctx context.Context, repoPath, commit, dir string) ([]TreeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(commit) == "" {
		return nil, errors.New("worktree: ListTreeAtCommit requires a non-empty commit")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("worktree: ListTreeAtCommit requires a non-empty dir")
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	if out, resolveErr := git(ctx, repoAbs, revParseCmd, "--verify", "--quiet", commit+"^{commit}"); resolveErr != nil {
		return nil, fmt.Errorf("worktree: commit %s does not resolve: %w (%s)", commit, resolveErr, strings.TrimSpace(string(out)))
	}
	out, err := git(ctx, repoAbs, "ls-tree", "-r", "-l", "-z", commit, "--", dir)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s -- %s: %w (%s)", commit, dir, err, strings.TrimSpace(string(out)))
	}
	var entries []TreeEntry
	for _, record := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if record == "" {
			continue
		}
		metadata, path, found := strings.Cut(record, "\t")
		if !found {
			return nil, fmt.Errorf("git ls-tree %s: unparseable record %q", commit, record)
		}
		fields := strings.Fields(metadata)
		if len(fields) != 4 {
			return nil, fmt.Errorf("git ls-tree %s: unparseable metadata %q", commit, metadata)
		}
		size := int64(0)
		if fields[3] != "-" {
			parsedSize, parseErr := strconv.ParseInt(fields[3], 10, 64)
			if parseErr != nil {
				return nil, fmt.Errorf("git ls-tree %s: unparseable size %q: %w", commit, fields[3], parseErr)
			}
			size = parsedSize
		}
		entries = append(entries, TreeEntry{Path: path, Mode: fields[0], Type: fields[1], Size: size})
	}
	return entries, nil
}

// UncommittedChange is one entry of the porcelain status listing restricted to
// a path. Code is the two-character XY status ("??" untracked, "!!" ignored,
// " M" worktree-modified, "M " staged, " D"/"D " deleted, "R " renamed, and
// combinations). RenamedFrom carries a rename's source path, which porcelain
// -z emits as a separate NUL-terminated field after the destination.
type UncommittedChange struct {
	Code        string
	Path        string
	RenamedFrom string
}

// defaultUncommittedLimit caps the entries UncommittedChanges returns when the
// caller passes no limit: enough to describe a drifted pack directory many
// times over, small enough that an event payload stays readable.
const defaultUncommittedLimit = 200

// UncommittedChanges lists the uncommitted changes under path — untracked
// files (--untracked-files=all) and ignored files (--ignored=traditional:
// enumerated individually inside ignored directories, unlike `matching` which
// would print a whole ignored ancestor such as "!! .agentum/" outside the
// pathspec) alongside modifications, staged changes, deletions and renames of
// tracked files. The caller decides which codes pause a run; this method only
// reports. Paths are raw (-z: no quoting) even with spaces or non-ASCII bytes,
// and both sides of a rename are kept so a rename out of the directory is not
// lost. Output is filtered strictly to entries whose path (or rename source)
// lies under the pathspec directory.
//
// A pathspec matching nothing on disk makes git print a "could not open
// directory" warning and exit 0; that is an empty result, not an error — only
// a non-zero exit is.
func (manager *Manager) UncommittedChanges(ctx context.Context, repoPath, path string, limit int) ([]UncommittedChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("worktree: UncommittedChanges requires a non-empty path")
	}
	if limit <= 0 {
		limit = defaultUncommittedLimit
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	out, err := git(ctx, repoAbs, "status", "--porcelain", "-z", "--untracked-files=all", "--ignored=traditional", "--", path)
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain -- %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	prefix := strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(path)), "/") + "/"
	tokens := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	var changes []UncommittedChange
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if token == "" {
			continue
		}
		if len(token) < 4 {
			return nil, fmt.Errorf("git status --porcelain: unparseable record %q", token)
		}
		code := token[:2]
		entryPath := token[3:]
		renameSource := ""
		if strings.ContainsAny(code, "RC") {
			// Porcelain -z emits a rename/copy source as the next NUL-separated
			// field; today git reports pathspec-limited renames as a plain
			// deletion instead, but a config or version that does emit R must
			// not desynchronize this parser — the extra field is consumed and
			// itself checked against the prefix.
			if index+1 < len(tokens) {
				index++
				renameSource = tokens[index]
			}
		}
		underPrefix := strings.HasPrefix(entryPath, prefix) || strings.HasPrefix(filepath.ToSlash(renameSource), prefix)
		if !underPrefix {
			continue
		}
		changes = append(changes, UncommittedChange{Code: code, Path: entryPath, RenamedFrom: renameSource})
		if len(changes) == limit {
			break
		}
	}
	return changes, nil
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

// DirtySummary lists the uncommitted paths (each porcelain entry's full
// "XY path" line, renames as "old -> new"), capped at limit entries. It feeds
// the worktree-uncommitted-changes diagnostic: the human choosing a recovery
// mode reads which files are at stake before deciding. A read failure returns
// nil — the diagnosis degrades to "dirty, paths unreadable" rather than
// blocking the pause that carries it.
func (manager *Manager) DirtySummary(ctx context.Context, wtRoot string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	out, err := git(ctx, wtRoot, "status", "--porcelain")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	entries := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "" {
			continue
		}
		entries = append(entries, trimmed)
		if len(entries) == limit {
			break
		}
	}
	return entries
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

// Reconcile classifies the worktree for runID after a crash, before a retry or
// resume. baseCommit is the run's resolved base_commit (the lineage anchor);
// lastCheckpoint is the most recent orchestrator-recorded checkpoint SHA, or ""
// if none exists yet (the reconciler then falls back to baseCommit).
//
// The classification is conservative by design: when in doubt, surface for
// human attention rather than risk replaying a side-effectful stage.
func (manager *Manager) Reconcile(ctx context.Context, repoPath, runID, baseCommit, lastCheckpoint string) (ReconcileState, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileState{}, err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return ReconcileState{}, fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, runID)
	if !isWorktree(ctx, wtPath) {
		// Worktree gone but run wants to run: a human removed it (or teardown
		// ran early). Re-creating would rebuild from base and replay
		// side effects with no record — surface instead.
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
// checkpoint when one was recorded, else the run's base_commit.
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

// IsAncestor reports whether ancestor is reachable from descendant in the
// repo at repoPath. Used by the base-ancestry checks (run start and
// publication): the run's base must lie in the publication target branch's
// history, so the eventual pull request carries the run's commits and no
// one else's. Exit-status based — no error text is matched.
func (manager *Manager) IsAncestor(ctx context.Context, repoPath, ancestor, descendant string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if strings.TrimSpace(ancestor) == "" || strings.TrimSpace(descendant) == "" {
		return false, errors.New("worktree: IsAncestor requires non-empty commits")
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return false, fmt.Errorf("resolve repo path: %w", err)
	}
	// --is-ancestor exits 0 when true, 1 when false; any other failure (an
	// unknown object among other causes) surfaces as its own error.
	cmd := exec.CommandContext(ctx, "git", "-C", repoAbs, "merge-base", "--is-ancestor", ancestor, descendant)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w (%s)",
		ancestor, descendant, err, strings.TrimSpace(string(out)))
}

// CountAhead runs `git rev-list --count from..to` in the repo at repoPath:
// how many commits to has beyond from. Used for the base-ancestry diagnosis
// (how many unpublished commits a local base rides on top of the target
// branch's comparison point).
func (manager *Manager) CountAhead(ctx context.Context, repoPath, from, to string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return 0, fmt.Errorf("resolve repo path: %w", err)
	}
	out, err := git(ctx, repoAbs, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s..%s: %w (%s)", from, to, err, strings.TrimSpace(string(out)))
	}
	count, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if parseErr != nil {
		return 0, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(string(out)), parseErr)
	}
	return count, nil
}

// RemoveWorktree removes only the per-run working tree. The agentum/<run-id>
// branch and its commits remain resolvable — they are the durable delivery
// output that survives teardown (F.6.1 AC #3). Idempotent: a missing worktree
// is a no-op. Used at terminal state (done/cancelled/failed).
func (manager *Manager) RemoveWorktree(ctx context.Context, repoPath, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	wtPath := PathFor(repoAbs, runID)
	if !isWorktree(ctx, wtPath) {
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

// DeleteBranch removes the agentum/<run-id> branch. This is the explicit,
// audited cleanup action — distinct from terminal teardown. Idempotent: a
// missing branch is a no-op. -D forces removal even if not merged: a delivered
// run's commits are reviewed via result_commit / the branch ref; deletion is
// the operator saying "I am done with this delivery."
func (manager *Manager) DeleteBranch(ctx context.Context, repoPath, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	branch := BranchFor(runID)
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

// excludeLines are the lines Agentum maintains in the repo's info/exclude
// (.git/info/exclude, resolved via git so worktree-shared repos are correct).
// The pair ignores every child of .agentum/ (per-run worktrees and the
// <root>/.agentum/<run-id>/.ag-artifacts/ trees inside run worktrees, which
// git add -A in checkpoint commits and git clean -fd in Restore must not see)
// while re-including .agentum/packs/ so a project can commit its own packs
// without -f. A bare ".agentum/" line cannot do both: it hides the packs too,
// and because an excluded directory cannot be re-included by a child rule, the
// negative pattern only works when the parent is matched child-by-child.
var excludeLines = []string{"/.agentum/*", "!/.agentum/packs/"}

// EnsureExcludes maintains Agentum's exclude lines in the repo's info/exclude:
// idempotent, and it rewrites a bare ".agentum/" line — whether Agentum's own
// older form or one added by an operator, since either hides .agentum/packs/
// and makes project packs uncommittable without -f. Lines Agentum does not own
// are left untouched. Operators who genuinely want .agentum/ ignored wholesale
// must move the rule into the repo's .gitignore (which Agentum never edits);
// the trade-off — packs needing git add -f — is theirs.
//
// Non-fatal by design at every call site: a failed write only means the user
// sees untracked .agentum files (or, for the drift check, the legacy line
// keeps hiding packs, which the check surfaces as ignored entries).
func (manager *Manager) EnsureExcludes(ctx context.Context, repoAbs string) error {
	out, err := git(ctx, repoAbs, revParseCmd, "--git-path", "info/exclude")
	if err != nil {
		return fmt.Errorf("locate excludes file: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(repoAbs, excludePath)
	}
	content, _ := os.ReadFile(excludePath)
	lines := strings.Split(string(content), "\n")
	managed := map[string]bool{}
	for _, line := range excludeLines {
		managed[line] = true
	}
	managed[".agentum/"] = true // the legacy form this call replaces
	var kept []string
	for _, line := range lines {
		if !managed[strings.TrimSpace(line)] {
			kept = append(kept, line)
		}
	}
	// Re-append our pair on its own trailing block. Trim a trailing empty
	// element the Split always produces so the file does not grow a blank
	// line per call.
	if len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	desired := strings.Join(kept, "\n")
	if len(desired) > 0 {
		desired += "\n"
	}
	desired += strings.Join(excludeLines, "\n") + "\n"
	if desired == string(content) {
		return nil // nothing to do; the file already carries exactly our pair
	}
	if writeErr := os.WriteFile(excludePath, []byte(desired), 0o644); writeErr != nil {
		return fmt.Errorf("write excludes file: %w", writeErr)
	}
	return nil
}

// isWorktree reports whether path is a worktree git can actually enter: the
// directory with its .git entry exists AND git resolves it. The second half is
// what separates a live worktree from a moved one — git stores absolute paths
// in both .git/worktrees/<id>/gitdir and the worktree's .git file, so after
// the repository moves on disk the directory and its .git file are all still
// there while every git command inside them fails on the stale pointer.
// Presence alone used to pass this check, and a broken worktree sailed
// through as healthy.
func isWorktree(ctx context.Context, path string) bool {
	if !DirPresent(path) {
		return false
	}
	out, err := git(ctx, path, revParseCmd, "--git-dir")
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// DirPresent reports whether a worktree directory with its .git entry exists
// at path, whether or not the link is live. The runner uses it to decide
// whether a repair is worth running before reconciling: after a repository
// move, presence plus a dead link is exactly the state repair exists to fix.
func DirPresent(path string) bool {
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

// Repair rewires a per-run worktree whose absolute pointers went stale after
// the repository moved on disk. `git worktree repair <path>` rewrites both
// sides of the linkage (.git/worktrees/<id>/gitdir and the worktree's .git
// file); the bare form without a path does not recover a moved repository, so
// the worktree's own path is always passed. Idempotent and cheap — repairing
// a healthy worktree is a no-op — which is why the runner calls it whenever
// the worktree directory is present before creating and reconciling, instead
// of trying to detect staleness itself.
func (manager *Manager) Repair(ctx context.Context, repoPath, runID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	out, err := git(ctx, repoAbs, "worktree", "repair", PathFor(repoAbs, runID))
	if err != nil {
		return fmt.Errorf("git worktree repair: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
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
