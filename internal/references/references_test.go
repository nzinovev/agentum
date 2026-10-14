package references

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTracked answers TrackedPaths from a fixed set, or with a fixed error.
type fakeTracked struct {
	paths map[string]bool
	err   error
}

func (tracked fakeTracked) TrackedPaths(context.Context, string) (map[string]bool, error) {
	return tracked.paths, tracked.err
}

// fakeScanner refuses the names and the content markers it is given.
type fakeScanner struct {
	refusedNames   map[string]bool
	refusedContent string
}

func (scanner fakeScanner) ScanName(name string) error {
	if scanner.refusedNames[filepath.Base(name)] {
		return errors.New("credential-shaped name")
	}
	return nil
}

func (scanner fakeScanner) ScanContent(_ string, content []byte) error {
	if scanner.refusedContent != "" && strings.Contains(string(content), scanner.refusedContent) {
		return errors.New("credential-shaped content")
	}
	return nil
}

// writeTree creates the given files (forward-slash relative path → content)
// under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for relativePath, content := range files {
		absolutePath := filepath.Join(root, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", relativePath, err)
		}
		if err := os.WriteFile(absolutePath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relativePath, err)
		}
	}
}

func skipReasons(entry Entry) map[string]SkipReason {
	reasons := make(map[string]SkipReason, len(entry.Skipped))
	for _, skipped := range entry.Skipped {
		reasons[skipped.Path] = skipped.Reason
	}
	return reasons
}

func filePaths(entry Entry) []string {
	paths := make([]string, 0, len(entry.Files))
	for _, file := range entry.Files {
		paths = append(paths, file.Path)
	}
	return paths
}

// TestValidateDeclaredSet: each rule of the references list refuses its own
// violation and names the offending index, and a well-formed list passes.
func TestValidateDeclaredSet(t *testing.T) {
	cases := []struct {
		name    string
		paths   []string
		problem string // substring of the single expected problem; empty = valid
	}{
		{name: "directory and file", paths: []string{"design", "notes/todo.md"}},
		{name: "trailing slash", paths: []string{"design/"}},
		{name: "empty", paths: []string{""}, problem: "references[0]: reference path is empty"},
		{name: "absolute", paths: []string{"/etc"}, problem: "must be repo-relative"},
		{name: "escape", paths: []string{"../sibling"}, problem: "escapes the repo root"},
		{name: "backslash", paths: []string{`design\system`}, problem: "must use forward slashes"},
		{name: "repository root", paths: []string{"."}, problem: "names the repository root"},
		{name: "git metadata", paths: []string{".git/hooks"}, problem: "is inside .git"},
		{name: "agentum state", paths: []string{"design/.agentum"}, problem: "is inside .agentum"},
		{name: "duplicate", paths: []string{"design", "design/"}, problem: `references[1]: path "design/" repeats or overlaps "design"`},
		{name: "nested", paths: []string{"design", "design/system"}, problem: "repeats or overlaps"},
		{name: "parent after child", paths: []string{"design/system", "design"}, problem: "repeats or overlaps"},
		{
			name:    "too many",
			paths:   []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"},
			problem: "at most 8 entries, got 9",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			problems := ValidateDeclaredSet(testCase.paths)
			if testCase.problem == "" {
				if len(problems) != 0 {
					t.Fatalf("expected no problems, got %v", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], testCase.problem) {
				t.Fatalf("expected one problem containing %q, got %v", testCase.problem, problems)
			}
		})
	}
}

// TestSnapshot_SkipReasons: every file Snapshot leaves out is recorded with
// the reason that applies to it, and the files it copies land in the store
// with their bytes and hashes.
func TestSnapshot_SkipReasons(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	writeTree(t, checkout, map[string]string{
		"design/README.md":           "readme",
		"design/system/tokens.css":   "tokens",
		"design/tracked.md":          "uncommitted edit of a tracked file",
		"design/.env":                "KEY=value",
		"design/leak.md":             "contains SECRET-MARKER here",
		"design/big.bin":             strings.Repeat("x", MaxFileBytes+1),
		"design/.git/config":         "nested repository metadata",
		"design/.agentum/state.json": "{}",
	})
	writeTree(t, outside, map[string]string{"private.md": "outside the checkout"})
	if err := os.Symlink(filepath.Join(outside, "private.md"), filepath.Join(checkout, "design", "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "design", "linked-dir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	pristine := filepath.Join(t.TempDir(), "store")
	pin, err := Snapshot(context.Background(), checkout, pristine, []string{"design/"},
		fakeTracked{paths: map[string]bool{"design/tracked.md": true}},
		fakeScanner{refusedNames: map[string]bool{".env": true}, refusedContent: "SECRET-MARKER"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(pin.Entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(pin.Entries))
	}
	entry := pin.Entries[0]
	if entry.Path != "design" {
		t.Errorf("entry path: got %q, want the cleaned form %q", entry.Path, "design")
	}

	wantFiles := []string{"design/README.md", "design/system/tokens.css"}
	if got := filePaths(entry); strings.Join(got, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("copied files: got %v, want %v", got, wantFiles)
	}
	wantSkips := map[string]SkipReason{
		"design/tracked.md": SkipTracked,
		"design/.env":       SkipSecretName,
		"design/leak.md":    SkipSecretContent,
		"design/big.bin":    SkipTooLarge,
		"design/.git":       SkipReserved,
		"design/.agentum":   SkipReserved,
		"design/link.md":    SkipNotRegular,
		"design/linked-dir": SkipNotRegular,
	}
	gotSkips := skipReasons(entry)
	for skippedPath, wantReason := range wantSkips {
		if gotSkips[skippedPath] != wantReason {
			t.Errorf("skip %s: got %q, want %q", skippedPath, gotSkips[skippedPath], wantReason)
		}
	}
	if entry.SkippedTotal != len(wantSkips) {
		t.Errorf("skipped total: got %d, want %d (%v)", entry.SkippedTotal, len(wantSkips), gotSkips)
	}

	stored, readErr := os.ReadFile(filepath.Join(pristine, filesDir, "design", "README.md"))
	if readErr != nil || string(stored) != "readme" {
		t.Errorf("stored copy: got %q, %v", stored, readErr)
	}
	if entry.Files[0].Hash != hashBytes([]byte("readme")) || entry.Bytes != int64(len("readme")+len("tokens")) {
		t.Errorf("file hash or entry bytes not recorded: %+v", entry)
	}
	if _, statErr := os.Stat(filepath.Join(pristine, filesDir, "design", "linked-dir")); !os.IsNotExist(statErr) {
		t.Errorf("a symlinked directory was followed into the store: %v", statErr)
	}
}

// TestSnapshot_DeclaredPathShapes: a declared path that is absent is recorded
// as missing, a declared regular file is copied, and a declared symlink is
// skipped without being followed.
func TestSnapshot_DeclaredPathShapes(t *testing.T) {
	checkout := t.TempDir()
	outside := t.TempDir()
	writeTree(t, checkout, map[string]string{"notes/todo.md": "todo"})
	if err := os.Symlink(outside, filepath.Join(checkout, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	pin, err := Snapshot(context.Background(), checkout, filepath.Join(t.TempDir(), "store"),
		[]string{"absent", "notes/todo.md", "linked"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !pin.Entries[0].Missing || pin.Entries[0].TreeHash != "" {
		t.Errorf("absent path: got %+v, want Missing with no tree hash", pin.Entries[0])
	}
	if got := filePaths(pin.Entries[1]); len(got) != 1 || got[0] != "notes/todo.md" {
		t.Errorf("declared file: got %v", got)
	}
	if got := skipReasons(pin.Entries[2]); got["linked"] != SkipNotRegular || len(pin.Entries[2].Files) != 0 {
		t.Errorf("declared symlink: got %+v", pin.Entries[2])
	}
}

// TestSnapshot_BudgetStopsTheWalk: the snapshot stops at the first file over
// the file-count budget, marks that path and every later declared path as
// incomplete, and stops at the same file on a second run over the same tree.
func TestSnapshot_BudgetStopsTheWalk(t *testing.T) {
	checkout := t.TempDir()
	files := make(map[string]string, MaxFiles+2)
	for index := 0; index < MaxFiles+2; index++ {
		files[fmt.Sprintf("bulk/f/%04d.txt", index)] = "x"
	}
	files["later/note.md"] = "never reached"
	writeTree(t, checkout, files)

	var stoppedAt []string
	for attempt := 0; attempt < 2; attempt++ {
		pin, err := Snapshot(context.Background(), checkout, filepath.Join(t.TempDir(), "store"),
			[]string{"bulk", "later"}, fakeTracked{}, fakeScanner{})
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		bulk, later := pin.Entries[0], pin.Entries[1]
		if len(bulk.Files) != MaxFiles || !bulk.BudgetExhausted {
			t.Fatalf("bulk: got %d files, exhausted=%v; want %d and true", len(bulk.Files), bulk.BudgetExhausted, MaxFiles)
		}
		if bulk.SkippedTotal != 1 || bulk.Skipped[0].Reason != SkipBudget {
			t.Fatalf("bulk skips: got %+v, want the single budget skip", bulk.Skipped)
		}
		if !later.BudgetExhausted || len(later.Files) != 0 || later.Missing {
			t.Errorf("later path: got %+v, want incomplete with no files", later)
		}
		stoppedAt = append(stoppedAt, bulk.Skipped[0].Path)
	}
	if stoppedAt[0] != stoppedAt[1] {
		t.Errorf("budget cut-off differs between runs: %q vs %q", stoppedAt[0], stoppedAt[1])
	}
}

// TestSnapshot_TrackedListingFailure: a failed tracked listing fails the
// snapshot and leaves no store behind, so Load reports it as absent.
func TestSnapshot_TrackedListingFailure(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{"design/README.md": "readme"})
	pristine := filepath.Join(t.TempDir(), "store")

	_, err := Snapshot(context.Background(), checkout, pristine, []string{"design"},
		fakeTracked{err: errors.New("git failed")}, fakeScanner{})
	if err == nil || !strings.Contains(err.Error(), "git failed") {
		t.Fatalf("expected the listing error, got %v", err)
	}
	if _, found, loadErr := Load(pristine); found || loadErr != nil {
		t.Errorf("Load after a failed snapshot: found=%v err=%v, want absent", found, loadErr)
	}
}

// TestLoad_ReturnsTheSnapshotPin: Load returns the Pin Snapshot wrote, so a
// later job of the same run works from the first job's snapshot. The tree hash
// does not change when the same bytes are snapshotted again.
func TestLoad_ReturnsTheSnapshotPin(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{"design/a.md": "a", "design/sub/b.md": "b"})
	pristine := filepath.Join(t.TempDir(), "store")

	first, err := Snapshot(context.Background(), checkout, pristine, []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	loaded, found, err := Load(pristine)
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if loaded.Entries[0].TreeHash != first.Entries[0].TreeHash || len(loaded.Entries[0].Files) != 2 {
		t.Errorf("loaded pin differs from the snapshot: %+v vs %+v", loaded, first)
	}

	second, err := Snapshot(context.Background(), checkout, filepath.Join(t.TempDir(), "other"), []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if second.Entries[0].TreeHash != first.Entries[0].TreeHash {
		t.Errorf("tree hash changed for identical bytes: %s vs %s", second.Entries[0].TreeHash, first.Entries[0].TreeHash)
	}

	writeTree(t, checkout, map[string]string{"design/a.md": "changed"})
	third, err := Snapshot(context.Background(), checkout, filepath.Join(t.TempDir(), "third"), []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("third Snapshot: %v", err)
	}
	if third.Entries[0].TreeHash == first.Entries[0].TreeHash {
		t.Error("tree hash did not change when a file's bytes changed")
	}
}

// TestMaterialize_RestoresThePin: the worktree copy holds exactly the pinned
// files after every Materialize. An edited file is rewritten, a deleted file
// returns, and an added file, directory, or symlink is removed. The first
// materialization reports no changes.
func TestMaterialize_RestoresThePin(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{
		"design/README.md":         "readme",
		"design/system/tokens.css": "tokens",
		"design/system/bundle.css": "bundle",
	})
	pristine := filepath.Join(t.TempDir(), "store")
	pin, err := Snapshot(context.Background(), checkout, pristine, []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	worktreeRoot := t.TempDir()
	const relativeDir = ".agentum/run-1/refs"
	if err := os.MkdirAll(filepath.Join(worktreeRoot, ".agentum", "run-1"), 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	copyRoot := filepath.Join(worktreeRoot, filepath.FromSlash(relativeDir))

	changes, err := Materialize(pin, pristine, worktreeRoot, relativeDir)
	if err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("first materialization reported changes: %v", changes)
	}
	if content, readErr := os.ReadFile(filepath.Join(copyRoot, "design", "system", "tokens.css")); readErr != nil || string(content) != "tokens" {
		t.Fatalf("materialized copy: got %q, %v", content, readErr)
	}

	// Editing the source after the snapshot must not reach the copy.
	writeTree(t, checkout, map[string]string{"design/README.md": "edited after the snapshot"})

	writeTree(t, copyRoot, map[string]string{
		"design/README.md":       "tampered",
		"design/extra.md":        "added by a stage",
		"design/scratch/note.md": "added directory",
	})
	if err := os.Remove(filepath.Join(copyRoot, "design", "system", "bundle.css")); err != nil {
		t.Fatalf("remove pinned file: %v", err)
	}
	victim := filepath.Join(worktreeRoot, "source.go")
	if err := os.WriteFile(victim, []byte("package main"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	symlinks := os.Symlink(victim, filepath.Join(copyRoot, "design", "link.go")) == nil

	changes, err = Materialize(pin, pristine, worktreeRoot, relativeDir)
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	got := make(map[string]ChangeAction, len(changes))
	for _, change := range changes {
		got[change.Path] = change.Action
	}
	want := map[string]ChangeAction{
		"design/README.md":         ChangeRestored,
		"design/system/bundle.css": ChangeRestored,
		"design/extra.md":          ChangeRemoved,
		"design/scratch":           ChangeRemoved,
	}
	if symlinks {
		want["design/link.go"] = ChangeRemoved
	}
	if len(got) != len(want) {
		t.Errorf("changes: got %v, want %v", got, want)
	}
	for changedPath, action := range want {
		if got[changedPath] != action {
			t.Errorf("change %s: got %q, want %q", changedPath, got[changedPath], action)
		}
	}
	if content, _ := os.ReadFile(filepath.Join(copyRoot, "design", "README.md")); string(content) != "readme" {
		t.Errorf("README after restore: got %q, want the snapshot bytes", content)
	}
	if _, statErr := os.Stat(filepath.Join(copyRoot, "design", "extra.md")); !os.IsNotExist(statErr) {
		t.Errorf("extra file survived: %v", statErr)
	}
	if content, _ := os.ReadFile(victim); string(content) != "package main" {
		t.Errorf("removing a symlink touched its target: %q", content)
	}

	changes, err = Materialize(pin, pristine, worktreeRoot, relativeDir)
	if err != nil || len(changes) != 0 {
		t.Errorf("Materialize over a clean copy: changes=%v err=%v, want none", changes, err)
	}
}

// TestMaterialize_ReplacesASymlinkedCopyDir: a symlink planted where the copy
// directory belongs is replaced by a real directory, and the directory it
// pointed at keeps its files. Without the replacement the sync would remove
// every path there that the pin does not contain.
func TestMaterialize_ReplacesASymlinkedCopyDir(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{"design/README.md": "readme"})
	pristine := filepath.Join(t.TempDir(), "store")
	pin, err := Snapshot(context.Background(), checkout, pristine, []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	worktreeRoot := t.TempDir()
	writeTree(t, worktreeRoot, map[string]string{"src/main.go": "package main"})
	runDir := filepath.Join(worktreeRoot, ".agentum", "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.Symlink(filepath.Join(worktreeRoot, "src"), filepath.Join(runDir, "refs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Materialize(pin, pristine, worktreeRoot, ".agentum/run-1/refs"); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if content, readErr := os.ReadFile(filepath.Join(worktreeRoot, "src", "main.go")); readErr != nil || string(content) != "package main" {
		t.Errorf("source behind the planted link changed: %q, %v", content, readErr)
	}
	info, statErr := os.Lstat(filepath.Join(runDir, "refs"))
	if statErr != nil || !info.IsDir() {
		t.Fatalf("copy dir is not a real directory: %v, %v", info, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, "refs", "design", "README.md")); statErr != nil {
		t.Errorf("pinned file missing from the copy: %v", statErr)
	}
}

// TestMaterialize_RefusesACorruptedStore: Materialize returns an error and
// writes nothing when a pristine file no longer matches its recorded hash.
func TestMaterialize_RefusesACorruptedStore(t *testing.T) {
	checkout := t.TempDir()
	writeTree(t, checkout, map[string]string{"design/README.md": "readme"})
	pristine := filepath.Join(t.TempDir(), "store")
	pin, err := Snapshot(context.Background(), checkout, pristine, []string{"design"}, fakeTracked{}, fakeScanner{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	writeTree(t, filepath.Join(pristine, filesDir), map[string]string{"design/README.md": "rewritten in the store"})

	worktreeRoot := t.TempDir()
	_, err = Materialize(pin, pristine, worktreeRoot, "refs")
	if err == nil || !strings.Contains(err.Error(), "does not match its recorded hash") {
		t.Fatalf("expected a hash mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(worktreeRoot, "refs", "design", "README.md")); !os.IsNotExist(statErr) {
		t.Errorf("a file was written from a corrupted store: %v", statErr)
	}
}
