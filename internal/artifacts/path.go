package artifacts

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrPathEscapesRoot is returned when a path resolves outside the root it was
// declared against. Callers branch on it to distinguish a containment breach
// (the agent named a file it has no business naming) from an ordinary IO error.
var ErrPathEscapesRoot = errors.New("artifacts: path escapes its root")

// DeclaredPath is a caller-declared artifact location that survived containment
// checking against a Container.
type DeclaredPath struct {
	// Name is the root-relative, slash-separated path. It is both the
	// (task, name) key in the revisions index and the handle every subsequent
	// read or write goes through, so it must never carry ".." or a drive
	// letter.
	Name string
	// Exists reports whether the path was present when it was resolved. A
	// declared-but-unwritten artifact is a contract gap, not a breach — the
	// caller records the gap and moves on.
	Exists bool
}

// Container confines file access to one directory tree.
//
// Agents declare artifact paths in result.json, which makes those paths
// untrusted input that the orchestrator then reads with its own privileges.
// Three escapes have to be closed:
//
//	absolute      "/etc/passwd"          → named outright
//	traversal     "../../.ssh/id_rsa"    → walked out of the tree
//	reparse       "link" → /etc/passwd   → a link the agent planted itself
//
// A lexical check closes the first two but cannot see the third, and resolving
// the path by hand does not close it either: filepath.EvalSymlinks follows
// POSIX symlinks but silently returns Windows junctions unresolved, so a
// junction inside the worktree would read as contained. Every operation
// therefore goes through an os.Root handle, where the containment check is the
// OS's and is performed as part of the open itself — which also closes the gap
// between checking a path and using it.
//
// One asymmetry follows from that choice: os.Root traverses a symlink that
// stays inside the root, but refuses a Windows junction wherever it points.
// Erring that way costs an artifact in a case git does not produce, where the
// alternative would be re-deriving containment in code that has already been
// shown not to work.
type Container struct {
	root *os.Root
	dir  string
}

// OpenContainer opens dir as a containment root. The caller owns Close.
func OpenContainer(dir string) (*Container, error) {
	if dir == "" {
		return nil, errors.New("artifacts: cannot open a container without a directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: open container %q: %w", dir, err)
	}
	return &Container{root: root, dir: dir}, nil
}

// Close releases the root handle.
func (container *Container) Close() error { return container.root.Close() }

// Dir returns the container's directory. For diagnostics — reading a file by
// joining it with a name would bypass the containment the Container exists for.
func (container *Container) Dir() string { return container.dir }

// Resolve checks a declared path against the container and returns the name it
// is indexed under. Returns an ErrPathEscapesRoot-wrapping error when the path
// leaves the tree by any route.
//
// A path that does not exist is not an error: an agent that declares a file it
// never wrote has a contract gap, which is the caller's to record. It cannot be
// a reparse escape either, since there is nothing there to follow.
func (container *Container) Resolve(declared string) (DeclaredPath, error) {
	name, err := container.relativize(declared)
	if err != nil {
		return DeclaredPath{}, err
	}
	if _, statErr := container.root.Stat(name); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return DeclaredPath{Name: name}, nil
		}
		// Anything else — an escape the lexical pass could not see, a
		// permission failure — is a refusal. The orchestrator does not get to
		// guess at a path it could not resolve.
		return DeclaredPath{}, fmt.Errorf("%w: %q: %w", ErrPathEscapesRoot, declared, statErr)
	}
	return DeclaredPath{Name: name, Exists: true}, nil
}

// ReadFile reads a contained file by the name Resolve returned.
func (container *Container) ReadFile(name string) ([]byte, error) {
	bytes, err := container.root.ReadFile(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %q: %w", ErrPathEscapesRoot, name, err)
	}
	return bytes, nil
}

// WriteFile writes bytes to a contained path, creating parent directories
// inside the container as needed. Used by the Syncer to materialize a revision
// back into a worktree: the write side needs the same guarantee as the read
// side, or a link planted by an earlier stage turns a sync into an overwrite of
// a host file.
func (container *Container) WriteFile(name string, bytes []byte, perm fs.FileMode) error {
	if parent := filepath.Dir(filepath.FromSlash(name)); parent != "." && parent != "" {
		if err := container.root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("artifacts: mkdir %q in %q: %w", parent, container.dir, err)
		}
	}
	if err := container.root.WriteFile(name, bytes, perm); err != nil {
		return fmt.Errorf("artifacts: write %q in %q: %w", name, container.dir, err)
	}
	return nil
}

// relativize turns a declared path into a clean, slash-separated name relative
// to the container. Absolute paths are accepted only when they point inside it.
// This runs before the os.Root call so an obvious escape is reported as one,
// rather than as the OS's generic "path escapes from parent".
func (container *Container) relativize(declared string) (string, error) {
	if strings.TrimSpace(declared) == "" {
		return "", errors.New("artifacts: empty declared path")
	}
	absRoot, err := filepath.Abs(container.dir)
	if err != nil {
		return "", fmt.Errorf("artifacts: resolve container %q: %w", container.dir, err)
	}
	candidate := filepath.FromSlash(declared)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(absRoot, candidate)
	}
	candidate = filepath.Clean(candidate)
	if err := ensureInside(absRoot, candidate); err != nil {
		return "", fmt.Errorf("%w: %w", ErrPathEscapesRoot, err)
	}
	rel, err := filepath.Rel(absRoot, candidate)
	if err != nil {
		return "", fmt.Errorf("artifacts: relativize %q under %q: %w", declared, absRoot, err)
	}
	if rel == "." {
		return "", fmt.Errorf("%w: %q names the container itself", ErrPathEscapesRoot, declared)
	}
	return filepath.ToSlash(rel), nil
}

// ensureInside verifies absPath is lexically contained by rootDir. Both must be
// absolute. It catches traversal and absolute-outside paths; it cannot catch a
// symlink or junction, which is why it is only ever the first of two checks —
// see Container.
//
// On Windows the comparison folds case, because the filesystem does: rejecting
// "d:\wt\main.go" against root "D:\wt" would be a false denial of the same file.
func ensureInside(rootDir, absPath string) error {
	rel, err := filepath.Rel(foldPath(rootDir), foldPath(absPath))
	if err != nil {
		return fmt.Errorf("resolve %q under %q: %w", absPath, rootDir, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is not under %q", ErrPathEscapesRoot, absPath, rootDir)
	}
	return nil
}

// foldPath normalizes a path for comparison on case-insensitive filesystems.
// Identity everywhere except Windows.
func foldPath(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
