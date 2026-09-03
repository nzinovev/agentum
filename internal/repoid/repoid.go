// Package repoid derives a repository's identity and the working-copy facts
// registration needs, by asking git rather than trusting the client's path.
//
// A local path is the most changeable property a repository has: moving the
// directory or cloning it elsewhere changes the path and changes nothing about
// the repository itself. Identity here is therefore a fingerprint of the
// repository's own history — the sorted list of root commits reachable from
// HEAD — carried with a versioned prefix so the value names the rule that
// produced it. Moving the directory changes the path, never the identity.
//
// The same entry point is the single git gate of project registration: not a
// work tree, no commits, a shallow clone, or a linked work tree are each a
// typed refusal whose text names the fix, because each of those states either
// has no identity to take or cannot serve as the working copy the runner
// builds worktrees from.
package repoid

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// schemePrefix names the identity computation rule. A future rule (a marker in
// .git/, a normalized remote URL, a server-issued identity) introduces a new
// prefix, not a type migration: the value says how it was derived, and two
// values under different prefixes are simply different identities.
const schemePrefix = "git-roots:v1"

// Identity is what Resolve learned about one working copy.
type Identity struct {
	// Value is the repository identity, "git-roots:v1:<sha256 hex>". Stable
	// across moving the directory and across clones of the same history.
	Value string
	// TopLevel is the absolute root of the working tree (git rev-parse
	// --show-toplevel). Registration stores this as the checkout path rather
	// than whatever the client sent, so a subdirectory or a trailing slash
	// resolves to the same project row.
	TopLevel string
	// Roots are the root commits Value was folded from. Registration stores
	// them so the hot paths can confirm identity by object existence (Verify)
	// instead of walking the whole history again: deriving the fingerprint
	// requires reaching the beginning of history, confirming it does not.
	Roots []string
}

// Typed refusals. Each carries (via fmt.Errorf wrapping) a message that names
// the way to fix the state, because a refusal the operator cannot act on is a
// dead end, not a gate.
var (
	// ErrNotWorkTree: the path is not inside a git work tree at all (a plain
	// directory, a bare repository, or the .git directory itself).
	ErrNotWorkTree = errors.New("path is not inside a git work tree")
	// ErrNoCommits: HEAD does not resolve — there is no history to fingerprint,
	// and the runner could not resolve base_ref or create a worktree either.
	ErrNoCommits = errors.New("repository has no commits — make at least one commit before registering")
	// ErrShallow: a shallow clone's fingerprint would sit at the cut boundary,
	// and `git fetch --unshallow` would silently change the identity — the exact
	// failure this package exists to remove.
	ErrShallow = errors.New("repository is a shallow clone — run `git fetch --unshallow` and register again")
	// ErrLinkedWorktree: a linked work tree of some main repository. Agentum
	// creates its own worktrees under the repo it is given, so registering a
	// linked work tree would nest worktrees and point the project at a
	// disposable tree.
	ErrLinkedWorktree = errors.New("path is a linked worktree")
	// ErrForeignRepository: the path is a usable working copy of a repository
	// other than the one whose roots were recorded against it.
	ErrForeignRepository = errors.New("path holds a different repository")
)

// LinkedWorktreeError names the main work tree the linked work tree belongs
// to, so the fix is copying a path, not diagnosing git.
type LinkedWorktreeError struct {
	MainWorkTree string
}

func (linkedErr *LinkedWorktreeError) Error() string {
	return fmt.Sprintf("%s of %s; register the main work tree instead",
		ErrLinkedWorktree.Error(), linkedErr.MainWorkTree)
}

func (linkedErr *LinkedWorktreeError) Unwrap() error { return ErrLinkedWorktree }

// Resolve asks git about path and returns the repository identity and the
// working-tree root, or a typed refusal. Every fact is probed, none inferred
// from the shape of the path.
//
// Resolve walks the entire reachable history to find the root commits — the
// one full traversal any history-derived fingerprint must pay. It belongs to
// registration, which runs once; callers that only need to know whether a
// path still holds the recorded repository use Verify, which never walks.
func Resolve(ctx context.Context, path string) (Identity, error) {
	topLevel, err := probeWorkTree(ctx, path)
	if err != nil {
		return Identity{}, err
	}

	// Root commits reachable from HEAD, hashed in sorted order. A repository
	// with several independent root lines (merging unrelated histories) gets a
	// stable fingerprint for as long as the reachable root set does not change.
	rootsOutput, err := gitOutput(ctx, path, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		// HEAD not resolvable means there is nothing to fingerprint.
		return Identity{}, ErrNoCommits
	}
	roots := strings.Fields(rootsOutput)
	if len(roots) == 0 {
		return Identity{}, ErrNoCommits
	}
	sort.Strings(roots)

	digest := sha256.Sum256([]byte(strings.Join(roots, "\n")))
	return Identity{
		Value:    schemePrefix + ":" + hex.EncodeToString(digest[:]),
		TopLevel: topLevel,
		Roots:    roots,
	}, nil
}

// Verify answers the hot-path question — does path still hold the repository
// these root commits belong to — without re-deriving the identity. The roots'
// existence is checked by object lookup, which does not depend on the size of
// the history; the full walk is only needed to PRODUCE an identity, and that
// happens once, at registration.
//
// The cheap gates are Resolve's (work tree, shallow, linked work tree), so
// both entry points refuse the same unusable copies the same way.
//
// A deliberate difference from re-deriving: Verify confirms the recorded
// roots are present, not that the root SET has not grown. A repository that
// pulled in an unrelated history (a subtree merge) after registration gains a
// new root — re-derivation would change its identity and pause every
// in-flight run, Verify lets them pass. That is the kinder outcome (an added
// subtree should not orphan a running task), and it is why the recorded roots
// are the contract, not the current fingerprint.
func Verify(ctx context.Context, path string, roots []string) (topLevel string, err error) {
	if len(roots) == 0 {
		return "", errors.New("repoid: Verify requires at least one recorded root commit")
	}
	topLevel, err = probeWorkTree(ctx, path)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		// cat-file -e is an object lookup: exit 0 says the commit exists in
		// this repository, anything else says this is not its history.
		if _, lookupErr := gitOutput(ctx, path, "cat-file", "-e", root+"^{commit}"); lookupErr != nil {
			return "", fmt.Errorf("%w: root commit %s is absent", ErrForeignRepository, root)
		}
	}
	return topLevel, nil
}

// probeWorkTree runs the cheap gates both Resolve and Verify share: a real
// work tree, its root, not shallow, not a linked work tree. Each refusal is
// typed with its fix.
func probeWorkTree(ctx context.Context, path string) (string, error) {
	insideWorkTree, err := gitOutput(ctx, path, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotWorkTree, err)
	}
	if insideWorkTree != "true" {
		// Bare repos and the .git dir itself report "false"; a project repo
		// must be a work tree the runner can operate in.
		return "", ErrNotWorkTree
	}

	topLevel, err := gitOutput(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve working tree root: %w: %s", ErrNotWorkTree, err)
	}

	shallow, err := gitOutput(ctx, path, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return "", fmt.Errorf("probe shallow state: %w", err)
	}
	if shallow == "true" {
		return "", ErrShallow
	}

	// A linked work tree keeps its metadata in <main>/.git/worktrees/<id>,
	// so --git-dir differs from --git-common-dir there. The common dir's
	// parent is the main work tree — named in the error so the fix is a
	// copy-paste.
	//
	// rev-parse answers may be relative; git prints them relative to the
	// invocation directory (the -C dir), NOT to the working tree root — from
	// a subdirectory --git-dir comes back absolute while --git-common-dir
	// comes back as "../../.git". Anchoring both at the invocation directory
	// resolves them to comparable absolute paths.
	invocationDir := path
	gitDir, err := gitOutput(ctx, invocationDir, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git dir: %w", err)
	}
	gitCommonDir, err := gitOutput(ctx, invocationDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	if !sameGitDir(gitDir, gitCommonDir, invocationDir) {
		return "", &LinkedWorktreeError{MainWorkTree: mainWorkTreeOf(gitCommonDir, invocationDir)}
	}
	return topLevel, nil
}

// sameGitDir compares the two rev-parse answers, accepting relative answers
// (git prints them relative to the invocation directory) by anchoring them
// there first.
func sameGitDir(gitDir, gitCommonDir, invocationDir string) bool {
	return absoluteGitPath(gitDir, invocationDir) == absoluteGitPath(gitCommonDir, invocationDir)
}

// absoluteGitPath anchors a possibly-relative rev-parse answer at the
// directory git ran in. An absolute answer is cleaned only.
func absoluteGitPath(revParseValue, invocationDir string) string {
	if filepath.IsAbs(revParseValue) {
		return filepath.Clean(revParseValue)
	}
	return filepath.Clean(filepath.Join(invocationDir, revParseValue))
}

// mainWorkTreeOf derives the main work tree from the common dir: the common
// dir is <main>/.git, so its parent is <main>. Falls back to the common dir
// itself when it is not shaped that way — the error still names where the
// real working copy lives.
func mainWorkTreeOf(gitCommonDir, invocationDir string) string {
	absolute := absoluteGitPath(gitCommonDir, invocationDir)
	if base := filepath.Base(absolute); base == ".git" {
		return filepath.Dir(absolute)
	}
	return absolute
}

// gitOutput runs one git command in path and returns its trimmed stdout. On
// failure the error carries git's stderr, which usually names the real
// problem (no such path, not a repository) — stdout alone is kept for the
// value because it feeds the identity: git writes warnings to stderr, and a
// stream glued together would put them inside the fingerprint, making it
// unrepeatable.
func gitOutput(ctx context.Context, path string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", path}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if diagnostic := strings.TrimSpace(stderr.String()); diagnostic != "" {
			return "", fmt.Errorf("%s (%s)", err, diagnostic)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
