// Package references owns the reference-file channel: project files that are
// not in the repository, declared by path in .agentum.yaml and delivered to a
// run as a snapshot.
//
// A run executes in a worktree created from its base commit, so a file the
// project keeps out of git never reaches an agent. This package copies the
// declared paths out of the local checkout once per run into a pristine store
// outside the worktree (Snapshot), and before every stage syncs that store
// into a directory inside the worktree (Materialize). The agent reads the copy
// with plain fs.read. The source directory and the pristine store stay outside
// the worktree, where the agent's file tools are denied.
//
// The package reads and writes the filesystem only through os.Root handles, so
// a symlink inside a declared directory cannot move a read out of the checkout
// or a write out of the worktree.
package references

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/instructions"
)

// Budgets for one run's snapshot. A file over MaxFileBytes is skipped. The
// snapshot stops at the first file that would exceed MaxFiles or
// MaxTotalBytes. A declared directory can hold build output or a dependency
// tree, and copying it once per run would fill the disk.
const (
	MaxFileBytes  = 1 << 20  // 1 MiB per file
	MaxTotalBytes = 16 << 20 // 16 MiB across the whole snapshot
	MaxFiles      = 1000

	maxDeclaredPaths = 8
	// maxRecordedSkips caps the per-path skip list kept in the index.
	// SkippedTotal carries the real count.
	maxRecordedSkips = 200

	indexFile = "index.json"
	// filesDir holds the copied bytes inside the pristine store. The copies
	// live one level below the index so a declared path named "index.json"
	// cannot overwrite it.
	filesDir = "files"
)

// reservedSegments are directory and file names Snapshot never copies, at any
// depth: git metadata and Agentum's own runtime state.
var reservedSegments = []string{".git", ".agentum"}

// SkipReason says why Snapshot did not copy a path.
type SkipReason string

const (
	// SkipTracked marks a file that exists in the run's base commit. The
	// worktree already holds the committed version, and copying the checkout's
	// would hand the run uncommitted edits of a tracked file.
	SkipTracked SkipReason = "tracked"
	// SkipNotRegular marks a symlink, device, socket, or pipe. Symlinks are
	// never followed.
	SkipNotRegular SkipReason = "not_regular"
	// SkipReserved marks a path named after one of reservedSegments.
	SkipReserved SkipReason = "reserved"
	// SkipSecretName marks a file whose name is credential-shaped (".env",
	// "id_rsa", "*.pem").
	SkipSecretName SkipReason = "secret_name"
	// SkipSecretContent marks a file whose bytes match a credential pattern.
	SkipSecretContent SkipReason = "secret_content"
	// SkipTooLarge marks a file over MaxFileBytes.
	SkipTooLarge SkipReason = "too_large"
	// SkipBudget marks the file at which the snapshot reached MaxFiles or
	// MaxTotalBytes. Nothing after it is copied.
	SkipBudget SkipReason = "budget"
	// SkipUnreadable marks a path the checkout would not let Snapshot read.
	SkipUnreadable SkipReason = "unreadable"
)

// Skip is one path Snapshot left out, with the reason.
type Skip struct {
	Path   string     `json:"path"`
	Reason SkipReason `json:"reason"`
}

// File is one copied file: its repo-relative forward-slash path, the sha256 of
// its bytes, and its size.
type File struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}

// Entry is the snapshot of one declared path.
type Entry struct {
	// Path is the declared path in cleaned form ("design/" → "design").
	Path string `json:"path"`
	// TreeHash is the sha256 over the copied files' sorted "path\0hash\n"
	// lines. Two runs received the same bytes under this path when their tree
	// hashes are equal. Empty when the path is missing.
	TreeHash string `json:"tree_hash,omitempty"`
	Files    []File `json:"files,omitempty"`
	Bytes    int64  `json:"bytes"`
	// Missing reports that the declared path did not exist in the checkout.
	Missing bool `json:"missing,omitempty"`
	// BudgetExhausted reports that the snapshot stopped inside or before this
	// path, so the copy is incomplete.
	BudgetExhausted bool `json:"budget_exhausted,omitempty"`
	// Skipped lists up to maxRecordedSkips skipped paths. SkippedTotal is the
	// number of paths skipped.
	Skipped      []Skip `json:"skipped,omitempty"`
	SkippedTotal int    `json:"skipped_total,omitempty"`
}

// skip records one skipped path, keeping the list within maxRecordedSkips.
func (entry *Entry) skip(skippedPath string, reason SkipReason) {
	entry.SkippedTotal++
	if len(entry.Skipped) < maxRecordedSkips {
		entry.Skipped = append(entry.Skipped, Skip{Path: skippedPath, Reason: reason})
	}
}

// Pin is one run's snapshot: an Entry per declared path, in declaration order.
type Pin struct {
	Entries []Entry `json:"entries"`
}

// Empty reports whether the project declared no reference paths.
func (pin Pin) Empty() bool { return len(pin.Entries) == 0 }

// TrackedLister reports which paths under dir exist in the run's base commit,
// keyed by repo-relative forward-slash path. The runner supplies it from the
// commit's tree. An interface keeps this package free of a git dependency.
type TrackedLister interface {
	TrackedPaths(ctx context.Context, dir string) (map[string]bool, error)
}

// Scanner decides whether a file may be copied. Each method returns a non-nil
// error to refuse the file. The runner supplies the artifact store's
// credential rules.
type Scanner interface {
	ScanName(name string) error
	ScanContent(name string, content []byte) error
}

// Clean returns a declared path in the form the snapshot is keyed by: forward
// slashes, no trailing slash, no "." segments.
func Clean(declared string) string {
	return path.Clean(filepath.ToSlash(strings.TrimSpace(declared)))
}

// ValidateDeclaredSet validates the references list of .agentum.yaml and
// returns every problem found. Each path follows the repo-relative rules
// shared with the instructions list. A path is also refused when it names the
// repository root, lies inside a reserved directory, or repeats or contains
// another declared path. The list holds at most maxDeclaredPaths entries.
func ValidateDeclaredSet(paths []string) []string {
	var problems []string
	if len(paths) > maxDeclaredPaths {
		problems = append(problems, fmt.Sprintf("references: at most %d entries, got %d", maxDeclaredPaths, len(paths)))
	}
	accepted := make([]string, 0, len(paths))
	for index, declared := range paths {
		if err := instructions.ValidateRepoRelativePath("reference path", declared); err != nil {
			problems = append(problems, fmt.Sprintf("references[%d]: %v", index, err))
			continue
		}
		cleaned := Clean(declared)
		if cleaned == "." {
			problems = append(problems, fmt.Sprintf("references[%d]: path %q names the repository root; name a directory or file inside it", index, declared))
			continue
		}
		if segment, reserved := reservedSegmentOf(cleaned); reserved {
			problems = append(problems, fmt.Sprintf("references[%d]: path %q is inside %s, which is never copied", index, declared, segment))
			continue
		}
		if overlapping, found := overlapOf(cleaned, accepted); found {
			problems = append(problems, fmt.Sprintf("references[%d]: path %q repeats or overlaps %q", index, declared, overlapping))
			continue
		}
		accepted = append(accepted, cleaned)
	}
	return problems
}

// reservedSegmentOf returns the reserved segment a cleaned path contains.
func reservedSegmentOf(cleaned string) (string, bool) {
	for _, segment := range strings.Split(cleaned, "/") {
		if isReserved(segment) {
			return segment, true
		}
	}
	return "", false
}

func isReserved(name string) bool {
	for _, reserved := range reservedSegments {
		if strings.EqualFold(name, reserved) {
			return true
		}
	}
	return false
}

// overlapOf returns the accepted path that equals candidate, contains it, or
// is contained by it. Overlapping paths would copy the same file twice.
func overlapOf(candidate string, accepted []string) (string, bool) {
	for _, existing := range accepted {
		if candidate == existing ||
			strings.HasPrefix(candidate, existing+"/") ||
			strings.HasPrefix(existing, candidate+"/") {
			return existing, true
		}
	}
	return "", false
}

// Snapshot copies the declared paths from checkoutRoot into pristineDir and
// returns the resulting Pin. It replaces whatever pristineDir held. The index
// is written last, so Load reports an interrupted snapshot as absent.
//
// A path that cannot be copied is recorded on its Entry as a Skip and the
// snapshot continues. Snapshot returns an error only when the store cannot be
// written, the tracked listing fails, or ctx ends. On error pristineDir is
// removed.
func Snapshot(
	ctx context.Context,
	checkoutRoot, pristineDir string,
	declared []string,
	tracked TrackedLister,
	scanner Scanner,
) (Pin, error) {
	pin, err := snapshotInto(ctx, checkoutRoot, pristineDir, declared, tracked, scanner)
	if err != nil {
		_ = os.RemoveAll(pristineDir)
		return Pin{}, err
	}
	return pin, nil
}

func snapshotInto(
	ctx context.Context,
	checkoutRoot, pristineDir string,
	declared []string,
	tracked TrackedLister,
	scanner Scanner,
) (Pin, error) {
	if err := os.RemoveAll(pristineDir); err != nil {
		return Pin{}, fmt.Errorf("references: clear store %q: %w", pristineDir, err)
	}
	storeDir := filepath.Join(pristineDir, filesDir)
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return Pin{}, fmt.Errorf("references: create store %q: %w", storeDir, err)
	}
	source, err := os.OpenRoot(checkoutRoot)
	if err != nil {
		return Pin{}, fmt.Errorf("references: open checkout %q: %w", checkoutRoot, err)
	}
	defer source.Close()
	store, err := os.OpenRoot(storeDir)
	if err != nil {
		return Pin{}, fmt.Errorf("references: open store %q: %w", storeDir, err)
	}
	defer store.Close()

	snapshot := &snapshotRun{ctx: ctx, source: source, store: store, scanner: scanner}
	pin := Pin{Entries: make([]Entry, 0, len(declared))}
	for _, declaredPath := range declared {
		entry, captureErr := snapshot.declaredPath(Clean(declaredPath), tracked)
		if captureErr != nil {
			return Pin{}, captureErr
		}
		pin.Entries = append(pin.Entries, entry)
	}

	encoded, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		return Pin{}, fmt.Errorf("references: encode index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pristineDir, indexFile), encoded, 0o644); err != nil {
		return Pin{}, fmt.Errorf("references: write index: %w", err)
	}
	return pin, nil
}

// snapshotRun is the state of one Snapshot call: the two roots and the budget
// spent so far across every declared path.
type snapshotRun struct {
	ctx     context.Context
	source  *os.Root
	store   *os.Root
	scanner Scanner

	copiedFiles int
	copiedBytes int64
	exhausted   bool
}

// declaredPath snapshots one declared path into an Entry.
func (snapshot *snapshotRun) declaredPath(declared string, tracked TrackedLister) (Entry, error) {
	entry := Entry{Path: declared}
	if snapshot.exhausted {
		entry.BudgetExhausted = true
		return entry, nil
	}
	info, statErr := snapshot.source.Lstat(filepath.FromSlash(declared))
	if errors.Is(statErr, fs.ErrNotExist) {
		entry.Missing = true
		return entry, nil
	}
	if statErr != nil {
		// os.Root refuses a path that leaves the checkout through a symlinked
		// parent. That is a path Snapshot must not read, not a broken snapshot.
		entry.skip(declared, SkipUnreadable)
		entry.TreeHash = treeHash(nil)
		return entry, nil
	}
	trackedPaths, trackedErr := tracked.TrackedPaths(snapshot.ctx, declared)
	if trackedErr != nil {
		return Entry{}, fmt.Errorf("references: list tracked paths under %q: %w", declared, trackedErr)
	}
	switch {
	case info.Mode().IsRegular():
		if err := snapshot.file(&entry, declared, trackedPaths); err != nil {
			return Entry{}, err
		}
	case info.IsDir():
		if err := snapshot.directory(&entry, declared, trackedPaths); err != nil {
			return Entry{}, err
		}
	default:
		entry.skip(declared, SkipNotRegular)
	}
	entry.TreeHash = treeHash(entry.Files)
	return entry, nil
}

// directory walks dir depth-first in name order, so the file at which the
// budget runs out is the same on every run over the same tree.
func (snapshot *snapshotRun) directory(entry *Entry, dir string, trackedPaths map[string]bool) error {
	if err := snapshot.ctx.Err(); err != nil {
		return err
	}
	children, readErr := readDirSorted(snapshot.source, dir)
	if readErr != nil {
		entry.skip(dir, SkipUnreadable)
		return nil
	}
	for _, child := range children {
		if snapshot.exhausted {
			return nil
		}
		childPath := dir + "/" + child.Name()
		switch {
		case isReserved(child.Name()):
			entry.skip(childPath, SkipReserved)
		case child.IsDir():
			if err := snapshot.directory(entry, childPath, trackedPaths); err != nil {
				return err
			}
		case child.Type().IsRegular():
			if err := snapshot.file(entry, childPath, trackedPaths); err != nil {
				return err
			}
		default:
			entry.skip(childPath, SkipNotRegular)
		}
	}
	return nil
}

// file copies one regular file into the store, or records why it did not. It
// returns an error only for a failed store write.
func (snapshot *snapshotRun) file(entry *Entry, filePath string, trackedPaths map[string]bool) error {
	if trackedPaths[filePath] {
		entry.skip(filePath, SkipTracked)
		return nil
	}
	if snapshot.scanner.ScanName(filePath) != nil {
		entry.skip(filePath, SkipSecretName)
		return nil
	}
	osPath := filepath.FromSlash(filePath)
	info, statErr := snapshot.source.Lstat(osPath)
	if statErr != nil {
		entry.skip(filePath, SkipUnreadable)
		return nil
	}
	if !info.Mode().IsRegular() {
		entry.skip(filePath, SkipNotRegular)
		return nil
	}
	if info.Size() > MaxFileBytes {
		entry.skip(filePath, SkipTooLarge)
		return nil
	}
	if snapshot.copiedFiles >= MaxFiles || snapshot.copiedBytes+info.Size() > MaxTotalBytes {
		entry.skip(filePath, SkipBudget)
		entry.BudgetExhausted = true
		snapshot.exhausted = true
		return nil
	}
	content, readErr := snapshot.source.ReadFile(osPath)
	if readErr != nil {
		entry.skip(filePath, SkipUnreadable)
		return nil
	}
	if len(content) > MaxFileBytes {
		// The file grew between the stat and the read.
		entry.skip(filePath, SkipTooLarge)
		return nil
	}
	if snapshot.scanner.ScanContent(filePath, content) != nil {
		entry.skip(filePath, SkipSecretContent)
		return nil
	}
	if err := writeFile(snapshot.store, filePath, content); err != nil {
		return fmt.Errorf("references: store %q: %w", filePath, err)
	}
	size := int64(len(content))
	entry.Files = append(entry.Files, File{Path: filePath, Hash: hashBytes(content), Bytes: size})
	entry.Bytes += size
	snapshot.copiedFiles++
	snapshot.copiedBytes += size
	return nil
}

// Load reads the Pin a previous Snapshot wrote to pristineDir. found is false
// when the store holds no index: no snapshot was taken, or one was
// interrupted.
func Load(pristineDir string) (pin Pin, found bool, err error) {
	raw, readErr := os.ReadFile(filepath.Join(pristineDir, indexFile))
	if errors.Is(readErr, fs.ErrNotExist) {
		return Pin{}, false, nil
	}
	if readErr != nil {
		return Pin{}, false, fmt.Errorf("references: read index: %w", readErr)
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		return Pin{}, false, fmt.Errorf("references: parse index: %w", err)
	}
	return pin, true, nil
}

// ChangeAction says what Materialize did to bring one path back to the pin.
type ChangeAction string

const (
	// ChangeRestored marks a pinned file that was missing or held other bytes
	// and was rewritten from the pristine store.
	ChangeRestored ChangeAction = "restored"
	// ChangeRemoved marks a path the pin does not contain and that was deleted.
	ChangeRemoved ChangeAction = "removed"
)

// Change is one correction Materialize made to an existing copy.
type Change struct {
	Path   string       `json:"path"`
	Action ChangeAction `json:"action"`
}

// Materialize makes relativeDir under worktreeRoot hold exactly the files of
// pin, with the bytes kept in pristineDir. A file that differs from its pinned
// hash is rewritten, and a path the pin does not contain is removed. An agent
// with write access to the worktree can edit its copy during a stage, and this
// sync is what keeps the next stage from reading that edit.
//
// The returned changes describe corrections to a copy that already existed.
// The first materialization into an absent directory returns none.
//
// Materialize returns an error when a pristine file no longer matches its
// recorded hash. Writing it would deliver bytes the run's evidence does not
// describe.
func Materialize(pin Pin, pristineDir, worktreeRoot, relativeDir string) ([]Change, error) {
	target, err := os.OpenRoot(worktreeRoot)
	if err != nil {
		return nil, fmt.Errorf("references: open worktree %q: %w", worktreeRoot, err)
	}
	defer target.Close()

	relativeDir = Clean(relativeDir)
	fresh, err := ensureRealDir(target, relativeDir)
	if err != nil {
		return nil, err
	}

	expectedFiles := make(map[string]File)
	expectedDirs := make(map[string]bool)
	for _, entry := range pin.Entries {
		for _, file := range entry.Files {
			expectedFiles[file.Path] = file
			for parent := path.Dir(file.Path); parent != "." && parent != "/"; parent = path.Dir(parent) {
				expectedDirs[parent] = true
			}
		}
	}

	var changes []Change
	if !fresh {
		changes, err = pruneUnexpected(target, relativeDir, "", expectedFiles, expectedDirs)
		if err != nil {
			return nil, err
		}
	}
	if len(expectedFiles) == 0 {
		return changes, nil
	}

	storeDir := filepath.Join(pristineDir, filesDir)
	store, err := os.OpenRoot(storeDir)
	if err != nil {
		return nil, fmt.Errorf("references: open store %q: %w", storeDir, err)
	}
	defer store.Close()

	filePaths := make([]string, 0, len(expectedFiles))
	for filePath := range expectedFiles {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	for _, filePath := range filePaths {
		pinned := expectedFiles[filePath]
		targetPath := relativeDir + "/" + filePath
		current, readErr := target.ReadFile(filepath.FromSlash(targetPath))
		if readErr == nil && hashBytes(current) == pinned.Hash {
			continue
		}
		content, storeErr := store.ReadFile(filepath.FromSlash(filePath))
		if storeErr != nil {
			return nil, fmt.Errorf("references: read pristine %q: %w", filePath, storeErr)
		}
		if hashBytes(content) != pinned.Hash {
			return nil, fmt.Errorf("references: pristine copy of %q does not match its recorded hash", filePath)
		}
		if writeErr := writeFile(target, targetPath, content); writeErr != nil {
			return nil, fmt.Errorf("references: write %q: %w", targetPath, writeErr)
		}
		if !fresh {
			changes = append(changes, Change{Path: filePath, Action: ChangeRestored})
		}
	}
	return changes, nil
}

// ensureRealDir makes relativeDir a real directory under root and reports
// whether its last component was created by this call. A symlink or a file at
// the last component is replaced. An os.Root follows a symlink that stays
// inside the root, so a link planted at the copy's location would otherwise
// point the sync, and its removals, at another part of the worktree.
func ensureRealDir(root *os.Root, relativeDir string) (created bool, err error) {
	segments := strings.Split(relativeDir, "/")
	current := ""
	for index, segment := range segments {
		current = path.Join(current, segment)
		osPath := filepath.FromSlash(current)
		last := index == len(segments)-1
		info, statErr := root.Lstat(osPath)
		switch {
		case statErr == nil && info.IsDir():
			continue
		case statErr == nil && !last:
			return false, fmt.Errorf("references: %q in the worktree is not a directory", current)
		case statErr == nil:
			if removeErr := root.Remove(osPath); removeErr != nil {
				return false, fmt.Errorf("references: replace %q: %w", current, removeErr)
			}
		case !errors.Is(statErr, fs.ErrNotExist):
			return false, fmt.Errorf("references: stat %q: %w", current, statErr)
		}
		if mkdirErr := root.Mkdir(osPath, 0o755); mkdirErr != nil {
			return false, fmt.Errorf("references: create %q: %w", current, mkdirErr)
		}
		created = last
	}
	return created, nil
}

// pruneUnexpected removes every path under relativeDir that the pin does not
// contain: an extra file, an extra directory, a symlink, or a path whose type
// differs from the pinned one. subPath is the position inside relativeDir.
func pruneUnexpected(
	root *os.Root,
	relativeDir, subPath string,
	expectedFiles map[string]File,
	expectedDirs map[string]bool,
) ([]Change, error) {
	children, err := readDirSorted(root, path.Join(relativeDir, subPath))
	if err != nil {
		return nil, fmt.Errorf("references: list %q: %w", path.Join(relativeDir, subPath), err)
	}
	var changes []Change
	for _, child := range children {
		childPath := path.Join(subPath, child.Name())
		_, isPinnedFile := expectedFiles[childPath]
		switch {
		case child.IsDir() && expectedDirs[childPath]:
			nested, nestedErr := pruneUnexpected(root, relativeDir, childPath, expectedFiles, expectedDirs)
			if nestedErr != nil {
				return nil, nestedErr
			}
			changes = append(changes, nested...)
		case child.Type().IsRegular() && isPinnedFile:
			// Content is compared by the caller.
		default:
			fullPath := path.Join(relativeDir, childPath)
			if removeErr := root.RemoveAll(filepath.FromSlash(fullPath)); removeErr != nil {
				return nil, fmt.Errorf("references: remove %q: %w", fullPath, removeErr)
			}
			changes = append(changes, Change{Path: childPath, Action: ChangeRemoved})
		}
	}
	return changes, nil
}

// readDirSorted lists dir under root in name order. Entry types come from the
// directory itself, so a symlink is reported as a symlink and never as the
// directory or file it points at.
func readDirSorted(root *os.Root, dir string) ([]fs.DirEntry, error) {
	handle, err := root.Open(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	children, err := handle.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(children, func(left, right int) bool { return children[left].Name() < children[right].Name() })
	return children, nil
}

// writeFile writes content at a forward-slash path under root, creating the
// parent directories inside the root.
func writeFile(root *os.Root, filePath string, content []byte) error {
	osPath := filepath.FromSlash(filePath)
	if parent := filepath.Dir(osPath); parent != "." && parent != "" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	return root.WriteFile(osPath, content, 0o644)
}

// treeHash returns the sha256 over the files' "path\0hash\n" lines in path
// order, so the result does not depend on the order the walk found them in.
func treeHash(files []File) string {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Path < sorted[right].Path })
	digest := sha256.New()
	for _, file := range sorted {
		digest.Write([]byte(file.Path))
		digest.Write([]byte{0})
		digest.Write([]byte(file.Hash))
		digest.Write([]byte{'\n'})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func hashBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
