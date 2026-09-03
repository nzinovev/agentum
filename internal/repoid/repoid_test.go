package repoid

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs git in dir, failing the test on error, and returns trimmed
// stdout. GIT_CONFIG_GLOBAL is pointed at /dev/null so a developer's global
// git config cannot change how the fixture behaves.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initCommittedRepo turns dir into a git work tree with one commit and returns
// the commit SHA. The marker lands in both the file content and the message so
// two fixtures never share a root commit SHA (identical author, tree, and
// second-granular timestamp would otherwise hash to the same commit).
func initCommittedRepo(t *testing.T, dir, marker string) string {
	t.Helper()
	runGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runGit(t, dir, "config", "user.email", "test@agentum")
	runGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+marker+"\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	return runGit(t, dir, "commit", "--quiet", "-m", "init "+marker)
}

// TestResolve_IdentityStableAcrossMoveAndClone pins the property the identity
// exists for: moving the directory and cloning the repository change the path,
// never the identity; an unrelated repository produces a different one.
func TestResolve_IdentityStableAcrossMoveAndClone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	original := t.TempDir()
	initCommittedRepo(t, original, "original")

	before, err := Resolve(ctx, original)
	if err != nil {
		t.Fatalf("resolve original: %v", err)
	}
	if !strings.HasPrefix(before.Value, "git-roots:v1:") {
		t.Fatalf("identity %q lacks the scheme prefix", before.Value)
	}

	moved := filepath.Join(t.TempDir(), "moved-repo")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move repo: %v", err)
	}
	afterMove, err := Resolve(ctx, moved)
	if err != nil {
		t.Fatalf("resolve moved: %v", err)
	}
	if afterMove.Value != before.Value {
		t.Fatalf("moving the directory changed identity: %q -> %q", before.Value, afterMove.Value)
	}

	cloneDir := t.TempDir()
	runGit(t, cloneDir, "clone", "--quiet", moved, filepath.Join(cloneDir, "repo"))
	fromClone, err := Resolve(ctx, filepath.Join(cloneDir, "repo"))
	if err != nil {
		t.Fatalf("resolve clone: %v", err)
	}
	if fromClone.Value != before.Value {
		t.Fatalf("clone of the same history gave a different identity: %q != %q", fromClone.Value, before.Value)
	}

	independent := t.TempDir()
	initCommittedRepo(t, independent, "independent")
	other, err := Resolve(ctx, independent)
	if err != nil {
		t.Fatalf("resolve independent: %v", err)
	}
	if other.Value == before.Value {
		t.Fatal("unrelated repositories must not share an identity")
	}
}

// TestResolve_SubdirectoryResolvesToRoot: registering from inside the tree
// must yield the same identity and the tree's root, so /repo, /repo/, and
// /repo/internal/api are one project, not three.
func TestResolve_SubdirectoryResolvesToRoot(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	initCommittedRepo(t, repo, "repo")
	inside := filepath.Join(repo, "internal", "api")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("make subdir: %v", err)
	}

	fromRoot, err := Resolve(context.Background(), repo)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	fromInside, err := Resolve(context.Background(), inside)
	if err != nil {
		t.Fatalf("resolve subdir: %v", err)
	}
	if fromInside.Value != fromRoot.Value {
		t.Fatalf("subdir identity %q != root identity %q", fromInside.Value, fromRoot.Value)
	}
	if fromInside.TopLevel != repo {
		t.Fatalf("subdir TopLevel = %q, want the root %q", fromInside.TopLevel, repo)
	}
}

// TestResolve_TypedRefusals: each unusable working copy is refused with its
// own error, and each message names the way to fix the state.
func TestResolve_TypedRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("not a work tree", func(t *testing.T) {
		t.Parallel()
		plain := t.TempDir()
		if _, err := Resolve(ctx, plain); !errors.Is(err, ErrNotWorkTree) {
			t.Fatalf("plain dir: err = %v, want ErrNotWorkTree", err)
		}
		missing := filepath.Join(t.TempDir(), "gone")
		if _, err := Resolve(ctx, missing); !errors.Is(err, ErrNotWorkTree) {
			t.Fatalf("missing path: err = %v, want ErrNotWorkTree", err)
		}
	})

	t.Run("no commits", func(t *testing.T) {
		t.Parallel()
		empty := t.TempDir()
		runGit(t, empty, "init", "--quiet")
		_, err := Resolve(ctx, empty)
		if !errors.Is(err, ErrNoCommits) {
			t.Fatalf("empty repo: err = %v, want ErrNoCommits", err)
		}
		if !strings.Contains(err.Error(), "commit") {
			t.Fatalf("refusal %q does not name the fix", err)
		}
	})

	t.Run("shallow clone", func(t *testing.T) {
		t.Parallel()
		source := t.TempDir()
		initCommittedRepo(t, source, "source")
		// A second commit so --depth=1 really cuts history off.
		if err := os.WriteFile(filepath.Join(source, "second.md"), []byte("2\n"), 0o644); err != nil {
			t.Fatalf("write second: %v", err)
		}
		runGit(t, source, "add", "second.md")
		runGit(t, source, "commit", "--quiet", "-m", "second")

		shallowParent := t.TempDir()
		shallow := filepath.Join(shallowParent, "shallow")
		// file:// defeats git's local-clone optimization, without which
		// --depth is ignored.
		runGit(t, shallowParent, "clone", "--quiet", "--depth=1", "file://"+source, shallow)
		_, err := Resolve(ctx, shallow)
		if !errors.Is(err, ErrShallow) {
			t.Fatalf("shallow clone: err = %v, want ErrShallow", err)
		}
		if !strings.Contains(err.Error(), "unshallow") {
			t.Fatalf("refusal %q does not name the fix", err)
		}
	})

	t.Run("linked worktree names the main tree", func(t *testing.T) {
		t.Parallel()
		main := t.TempDir()
		initCommittedRepo(t, main, "main")
		linkedParent := t.TempDir()
		linked := filepath.Join(linkedParent, "linked")
		runGit(t, main, "worktree", "add", "--quiet", linked)

		_, err := Resolve(ctx, linked)
		var linkedErr *LinkedWorktreeError
		if !errors.As(err, &linkedErr) {
			t.Fatalf("linked worktree: err = %v, want LinkedWorktreeError", err)
		}
		if !errors.Is(err, ErrLinkedWorktree) {
			t.Fatalf("linked worktree: err = %v, want ErrLinkedWorktree", err)
		}
		if linkedErr.MainWorkTree != main {
			t.Fatalf("error names main work tree %q, want %q", linkedErr.MainWorkTree, main)
		}
		if !strings.Contains(err.Error(), "register the main work tree") {
			t.Fatalf("refusal %q does not say what to register instead", err)
		}
	})
}
