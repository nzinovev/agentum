package artifacts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// openContainer opens dir as a Container and registers its Close.
func openContainer(t *testing.T, dir string) *Container {
	t.Helper()
	container, err := OpenContainer(dir)
	if err != nil {
		t.Fatalf("OpenContainer(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = container.Close() })
	return container
}

// TestContainer_ResolveAcceptsPathsInsideTheRoot is the baseline: the ordinary
// declarations an agent makes must keep working, in every shape the contract
// permits (relative, nested, slash-separated on Windows, absolute-but-inside).
func TestContainer_ResolveAcceptsPathsInsideTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "spec.md"), "spec")
	writeFile(t, filepath.Join(root, "docs", "adr", "0001.md"), "adr")
	container := openContainer(t, root)

	cases := []struct {
		declared string
		wantName string
	}{
		{declared: "spec.md", wantName: "spec.md"},
		{declared: "docs/adr/0001.md", wantName: "docs/adr/0001.md"},
		{declared: "./docs/adr/0001.md", wantName: "docs/adr/0001.md"},
		{declared: "docs/../spec.md", wantName: "spec.md"},
		{declared: filepath.Join(root, "spec.md"), wantName: "spec.md"},
	}
	for _, table := range cases {
		t.Run(table.declared, func(t *testing.T) {
			t.Parallel()
			resolved, err := container.Resolve(table.declared)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", table.declared, err)
			}
			if resolved.Name != table.wantName {
				t.Errorf("Name = %q, want %q", resolved.Name, table.wantName)
			}
			if !resolved.Exists {
				t.Error("Exists = false for a file that is on disk")
			}
		})
	}
}

// TestContainer_ResolveRejectsEscapes is the P0 this file exists for. An
// agent-declared path is untrusted input that the orchestrator then reads with
// its own privileges, so every shape that leaves the worktree has to be refused
// — not sanitized into something plausible.
func TestContainer_ResolveRejectsEscapes(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	writeFile(t, filepath.Join(parent, "secret.txt"), "host secret")
	container := openContainer(t, root)

	cases := []struct {
		name     string
		declared string
	}{
		{name: "parent traversal", declared: "../secret.txt"},
		{name: "deep traversal", declared: "docs/../../secret.txt"},
		{name: "absolute outside", declared: filepath.Join(parent, "secret.txt")},
		{name: "absolute host path", declared: hostRootPath()},
		{name: "sibling prefix", declared: "../worktree-other/file.txt"},
		{name: "the root itself", declared: "."},
	}
	for _, table := range cases {
		t.Run(table.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := container.Resolve(table.declared)
			if !errors.Is(err, ErrPathEscapesRoot) {
				t.Fatalf("Resolve(%q) = (%+v, %v), want ErrPathEscapesRoot", table.declared, resolved, err)
			}
		})
	}
}

// TestContainer_ResolveRejectsLinkEscape closes the escape the lexical checks
// cannot see. An agent has write access inside its own worktree, so it can
// plant a link and declare it — the resolved target is what the orchestrator
// would actually read.
//
// The link is a symlink where the platform allows one and a directory junction
// on Windows, where creating a symlink needs a privilege CI does not have.
// Junctions matter in their own right: filepath.EvalSymlinks does not follow
// them, so a containment check written by hand around EvalSymlinks reports a
// junction escape as contained. Only the OS-level check in os.Root catches it.
func TestContainer_ResolveRejectsLinkEscape(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "worktree")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	writeFile(t, filepath.Join(outside, "secret.txt"), "host secret")

	declared := linkOutOfTree(t, root, outside)
	container := openContainer(t, root)

	// Control: the file really is reachable through the link, so a rejection
	// below is the guard working rather than a missing file.
	if _, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(declared))); err != nil {
		t.Fatalf("control read through the link failed: %v", err)
	}

	resolved, err := container.Resolve(declared)
	if !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("Resolve(%q) through a link = (%+v, %v), want ErrPathEscapesRoot", declared, resolved, err)
	}
	if _, err := container.ReadFile(declared); !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("ReadFile(%q) through a link = %v, want ErrPathEscapesRoot", declared, err)
	}
}

// linkOutOfTree plants a link inside root pointing at outside and returns the
// declared path (relative to root) that reaches through it. Skips the test when
// the platform will not create either kind of link.
func linkOutOfTree(t *testing.T, root, outside string) string {
	t.Helper()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		return "escape/secret.txt"
	}
	if runtime.GOOS != "windows" {
		t.Skip("cannot create a symlink on this platform")
	}
	// Unprivileged symlink creation is off by default on Windows; a directory
	// junction needs no privilege and is the more interesting case anyway.
	link := filepath.Join(root, "escape")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction: %v (%s)", err, out)
	}
	return "escape/secret.txt"
}

// TestContainer_AllowsSymlinkInsideTheRoot: containment, not a blanket link
// ban. A symlink that stays in the tree is ordinary and must resolve.
//
// Symlinks only — Windows junctions are refused wherever they point, because
// os.Root will not traverse one at all. That is a deliberate asymmetry on the
// safe side: a junction inside a worktree is not something git produces, and
// refusing it costs an artifact where accepting it would need us to re-derive
// containment ourselves.
func TestContainer_AllowsSymlinkInsideTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "real", "content.md"), "content")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	container := openContainer(t, root)

	resolved, err := container.Resolve("alias/content.md")
	if err != nil {
		t.Fatalf("Resolve through an in-tree link: %v", err)
	}
	// The name is the declared location, not the link target: a later Sync has
	// to write back where the next stage expects to read.
	if resolved.Name != "alias/content.md" {
		t.Errorf("Name = %q, want alias/content.md", resolved.Name)
	}
	bytes, err := container.ReadFile(resolved.Name)
	if err != nil {
		t.Fatalf("ReadFile through an in-tree link: %v", err)
	}
	if string(bytes) != "content" {
		t.Errorf("content = %q, want %q", bytes, "content")
	}
}

// TestContainer_MissingFileIsNotABreach: an agent that declares a file it never
// wrote has a contract gap, which the caller records and moves past. It must
// not be conflated with an escape, which fails the stage.
func TestContainer_MissingFileIsNotABreach(t *testing.T) {
	t.Parallel()
	container := openContainer(t, t.TempDir())

	resolved, err := container.Resolve("never-written.md")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Exists {
		t.Error("Exists = true for a file that was never written")
	}
	if resolved.Name != "never-written.md" {
		t.Errorf("Name = %q, want never-written.md", resolved.Name)
	}
	if _, err := container.ReadFile(resolved.Name); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadFile of a missing file = %v, want os.ErrNotExist", err)
	}
}

// TestContainer_RejectsMissingFileOutsideTheRoot: the traversal check runs
// before the existence check, so a path that does not exist yet still cannot
// name a location outside the tree.
func TestContainer_RejectsMissingFileOutsideTheRoot(t *testing.T) {
	t.Parallel()
	container := openContainer(t, t.TempDir())
	if _, err := container.Resolve("../not-written-either.md"); !errors.Is(err, ErrPathEscapesRoot) {
		t.Fatalf("error = %v, want ErrPathEscapesRoot", err)
	}
}

func TestContainer_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	container := openContainer(t, t.TempDir())
	if _, err := container.Resolve(""); err == nil {
		t.Error("empty declared path accepted")
	}
	if _, err := container.Resolve("   "); err == nil {
		t.Error("blank declared path accepted")
	}
	if _, err := OpenContainer(""); err == nil {
		t.Error("empty container directory accepted")
	}
}

// TestContainer_WriteFileStaysInside covers the write side: the Syncer
// materializes revisions back into a worktree the agent had write access to, so
// a name that escapes must be refused there too.
func TestContainer_WriteFileStaysInside(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "worktree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	container := openContainer(t, root)

	if err := container.WriteFile("docs/nested/spec.md", []byte("body"), 0o644); err != nil {
		t.Fatalf("WriteFile inside the root: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, "docs", "nested", "spec.md"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(written) != "body" {
		t.Errorf("content = %q, want %q", written, "body")
	}

	if err := container.WriteFile("../escaped.md", []byte("nope"), 0o644); err == nil {
		t.Error("WriteFile accepted a path outside the root")
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped.md")); err == nil {
		t.Error("WriteFile wrote outside the root")
	}
}

// TestEnsureInside_DotDotPrefixIsNotTraversal: a file whose name merely starts
// with ".." is a legal file, and rejecting it would be a false denial. The
// check is on the path segment, not the string prefix.
func TestEnsureInside_DotDotPrefixIsNotTraversal(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "wt")
	if err := ensureInside(root, filepath.Join(root, "..hidden")); err != nil {
		t.Errorf("ensureInside rejected a file named %q: %v", "..hidden", err)
	}
	if err := ensureInside(root, filepath.Join(root, "..")); !errors.Is(err, ErrPathEscapesRoot) {
		t.Errorf("ensureInside(..) = %v, want ErrPathEscapesRoot", err)
	}
}

// TestEnsureInside_CaseFoldingOnWindows: NTFS is case-insensitive, so a
// declared path that differs only in case names the same file. Rejecting it
// would be fail-closed but wrong.
func TestEnsureInside_CaseFoldingOnWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("case folding only applies on Windows")
	}
	root := `D:\repos\agentum\.agentum\worktrees\task-1`
	if err := ensureInside(root, strings.ToUpper(root)+`\SPEC.MD`); err != nil {
		t.Errorf("ensureInside rejected a case variant of the same path: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// hostRootPath returns an absolute path that is never inside a temp worktree —
// the shape an agent uses when it declares a host file outright.
func hostRootPath() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\drivers\etc\hosts`
	}
	return "/etc/passwd"
}
