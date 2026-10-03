package worktree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file cover the project-pack read surface: ListTreeAtCommit
// (the commit-pinned pack-directory listing behind the agent-immutability
// seam), UncommittedChanges (the drift signal for a project pack's directory),
// and EnsureExcludes (the info/exclude maintenance that keeps .agentum/packs/
// committable while worktrees and artifacts stay out of commits).

// commitPack turns dir into a repo whose HEAD carries .agentum/packs/x/ with a
// manifest and one prompt, returning the commit SHA. The drifted-copy cases
// below mutate this state on top.
func commitPack(t *testing.T, dir string) string {
	t.Helper()
	if err := initRepoWithCommit(dir); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	packDir := filepath.Join(dir, ".agentum", "packs", "x")
	if err := os.MkdirAll(filepath.Join(packDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(dir, ".agentum/packs/x/manifest.yaml", "api: agentum/v1\n")
	mustWrite(dir, ".agentum/packs/x/prompts/p.md", "prompt body\n")
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "--quiet", "-m", "add pack"},
	} {
		if err := gitInRepo(dir, args...); err != nil {
			t.Fatalf("git %s: %v", args[0], err)
		}
	}
	anchor, err := headOf(dir)
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	return anchor
}

func TestManager_ListTreeAtCommit(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	anchor := commitPack(t, repo)
	// A committed symlink: git stores it as mode 120000, the entry class the
	// project-pack reader must reject.
	if err := os.Symlink("p.md", filepath.Join(repo, ".agentum", "packs", "x", "prompts", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := gitInRepo(repo, "commit", "--quiet", "-m", "symlink"); err != nil {
		t.Fatal(err)
	}
	withLink, err := headOf(repo)
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	manager := New()
	entries, err := manager.ListTreeAtCommit(t.Context(), repo, anchor, ".agentum/packs/x")
	if err != nil {
		t.Fatalf("ListTreeAtCommit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (manifest + prompt): %+v", len(entries), entries)
	}
	byPath := map[string]TreeEntry{}
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	manifest, ok := byPath[".agentum/packs/x/manifest.yaml"]
	if !ok {
		t.Fatalf("manifest.yaml missing from listing: %+v", entries)
	}
	if manifest.Mode != "100644" || manifest.Type != "blob" {
		t.Errorf("manifest entry = {mode %s type %s}, want {100644 blob}", manifest.Mode, manifest.Type)
	}
	if manifest.Size != int64(len("api: agentum/v1\n")) {
		t.Errorf("manifest size = %d, want %d", manifest.Size, len("api: agentum/v1\n"))
	}

	// The symlink commit lists the link as mode 120000 — the signal that a
	// pack directory references outside itself.
	linked, err := manager.ListTreeAtCommit(t.Context(), repo, withLink, ".agentum/packs/x")
	if err != nil {
		t.Fatalf("ListTreeAtCommit (with symlink): %v", err)
	}
	foundSymlink := false
	for _, entry := range linked {
		if strings.HasSuffix(entry.Path, "link.md") && entry.Mode == "120000" {
			foundSymlink = true
		}
	}
	if !foundSymlink {
		t.Errorf("symlink entry not listed with mode 120000: %+v", linked)
	}

	// A directory absent at the commit is an empty listing, not an error.
	absent, err := manager.ListTreeAtCommit(t.Context(), repo, anchor, ".agentum/packs/none")
	if err != nil {
		t.Fatalf("absent dir must be an empty listing, got error: %v", err)
	}
	if len(absent) != 0 {
		t.Fatalf("absent dir entries = %+v, want empty", absent)
	}

	// An unresolvable commit is its own error, never an absence.
	bogus := strings.Repeat("0", 40)
	if _, err := manager.ListTreeAtCommit(t.Context(), repo, bogus, ".agentum/packs/x"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unresolvable commit must be its own error, got %v", err)
	}
}

func TestManager_UncommittedChanges(t *testing.T) {
	t.Parallel()

	// assertCodes folds the listing into "XY path" strings for comparison.
	assertCodes := func(t *testing.T, changes []UncommittedChange) []string {
		t.Helper()
		out := make([]string, 0, len(changes))
		for _, change := range changes {
			record := change.Code + " " + change.Path
			if change.RenamedFrom != "" {
				record += " <- " + change.RenamedFrom
			}
			out = append(out, record)
		}
		return out
	}
	containsRecord := func(records []string, want string) bool {
		for _, record := range records {
			if record == want {
				return true
			}
		}
		return false
	}

	t.Run("clean committed pack", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/x")
		if err != nil {
			t.Fatalf("UncommittedChanges: %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("changes = %v, want none", assertCodes(t, changes))
		}
	})

	t.Run("untracked and modified and deleted tracked files", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		mustWrite(repo, ".agentum/packs/x/prompts/extra.md", "new prompt\n")
		mustWrite(repo, ".agentum/packs/x/manifest.yaml", "api: agentum/v1\npack: edited\n")
		if err := os.Remove(filepath.Join(repo, ".agentum", "packs", "x", "prompts", "p.md")); err != nil {
			t.Fatal(err)
		}
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/x")
		if err != nil {
			t.Fatalf("UncommittedChanges: %v", err)
		}
		records := assertCodes(t, changes)
		if !containsRecord(records, "?? .agentum/packs/x/prompts/extra.md") {
			t.Errorf("untracked prompt missing: %v", records)
		}
		if !containsRecord(records, " M .agentum/packs/x/manifest.yaml") {
			t.Errorf("worktree-modified manifest missing: %v", records)
		}
		if !containsRecord(records, " D .agentum/packs/x/prompts/p.md") {
			t.Errorf("worktree-deleted prompt missing: %v", records)
		}
		if len(records) != 3 {
			t.Errorf("changes = %v, want exactly the three entries", records)
		}
	})

	t.Run("staged modification and rename out of the pack dir", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		mustWrite(repo, ".agentum/packs/x/manifest.yaml", "api: agentum/v1\npack: staged\n")
		if err := gitInRepo(repo, "add", ".agentum/packs/x/manifest.yaml"); err != nil {
			t.Fatal(err)
		}
		// Renaming a prompt OUT of the pack directory is a deletion from it:
		// under a pathspec git reports the old side as a staged deletion (its
		// rename detection does not run pathspec-limited), and the record for
		// the new location stays outside the pack dir and is filtered.
		if err := gitInRepo(repo, "mv", ".agentum/packs/x/prompts/p.md", "moved-prompt.md"); err != nil {
			t.Fatal(err)
		}
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/x")
		if err != nil {
			t.Fatalf("UncommittedChanges: %v", err)
		}
		records := assertCodes(t, changes)
		if !containsRecord(records, "M  .agentum/packs/x/manifest.yaml") {
			t.Errorf("staged modification missing: %v", records)
		}
		if !containsRecord(records, "D  .agentum/packs/x/prompts/p.md") {
			t.Errorf("rename out of the pack dir must surface as a staged deletion: %v", records)
		}
		for _, change := range changes {
			if change.Path == "moved-prompt.md" {
				t.Errorf("record outside the pack dir must be filtered: %v", records)
			}
		}
	})

	t.Run("ignored files enumerated individually", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		// A project .gitignore that excludes .agentum/ wholesale — the case
		// where --ignored=matching would print a single "!! .agentum/" line
		// outside the pathspec and both false-pause and hide real drift.
		mustWrite(repo, ".gitignore", ".agentum/\n")
		if err := os.MkdirAll(filepath.Join(repo, ".agentum", "packs", "y"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(repo, ".agentum/packs/y/manifest.yaml", "api: agentum/v1\n")
		if err := gitInRepo(repo, "add", ".gitignore"); err != nil {
			t.Fatal(err)
		}
		if err := gitInRepo(repo, "commit", "--quiet", "-m", "gitignore"); err != nil {
			t.Fatal(err)
		}
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/y")
		if err != nil {
			t.Fatalf("UncommittedChanges: %v", err)
		}
		records := assertCodes(t, changes)
		if !containsRecord(records, "!! .agentum/packs/y/manifest.yaml") {
			t.Fatalf("ignored pack file must be enumerated individually, got %v", records)
		}
	})

	t.Run("path with spaces and non-ascii stays raw", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		mustWrite(repo, ".agentum/packs/x/prompts/промпт с пробелом.md", "body\n")
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/x")
		if err != nil {
			t.Fatalf("UncommittedChanges: %v", err)
		}
		records := assertCodes(t, changes)
		if !containsRecord(records, "?? .agentum/packs/x/prompts/промпт с пробелом.md") {
			t.Fatalf("path with spaces/non-ascii must come back raw, got %v", records)
		}
	})

	t.Run("absent pack dir is empty not an error", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		commitPack(t, repo)
		// git prints a "could not open directory" warning to stderr and exits
		// 0; only a non-zero exit is an error.
		changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/none")
		if err != nil {
			t.Fatalf("absent dir must be an empty result, got error: %v", err)
		}
		if len(changes) != 0 {
			t.Fatalf("changes = %v, want none", assertCodes(t, changes))
		}
	})
}

// gitOutput runs git in dir and returns stdout.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	return string(out), err
}

// excludePathOf resolves the repo's info/exclude path the same way
// EnsureExcludes does.
func excludePathOf(t *testing.T, repo string) string {
	t.Helper()
	out, err := gitOutput(repo, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		t.Fatalf("locate excludes: %v", err)
	}
	resolved := strings.TrimSpace(out)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(repo, resolved)
	}
	return resolved
}

func TestManager_EnsureExcludes(t *testing.T) {
	t.Parallel()

	t.Run("rewrites legacy line and keeps foreign lines", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		if err := initRepoWithCommit(repo); err != nil {
			t.Fatalf("setup: %v", err)
		}
		// The legacy form — whether Agentum's older write or an operator's own
		// line — hides .agentum/packs/ and must be replaced, not appended to.
		legacy := "# operator's own rules\n*.log\n.agentum/\n"
		if err := os.WriteFile(excludePathOf(t, repo), []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := New().EnsureExcludes(t.Context(), repo); err != nil {
			t.Fatalf("EnsureExcludes: %v", err)
		}
		contentBytes, readErr := os.ReadFile(excludePathOf(t, repo))
		if readErr != nil {
			t.Fatal(readErr)
		}
		content := string(contentBytes)
		for _, line := range strings.Split(content, "\n") {
			if strings.TrimSpace(line) == ".agentum/" {
				t.Fatalf("legacy .agentum/ line must be replaced:\n%s", content)
			}
		}
		if !strings.Contains(content, "/.agentum/*\n!/.agentum/packs/\n") {
			t.Fatalf("exclude pair missing:\n%s", content)
		}
		if !strings.Contains(content, "# operator's own rules\n*.log\n") {
			t.Fatalf("foreign lines must be kept:\n%s", content)
		}
		// Idempotent: a second call changes nothing.
		if err := New().EnsureExcludes(t.Context(), repo); err != nil {
			t.Fatalf("EnsureExcludes (second): %v", err)
		}
		afterBytes, readErr := os.ReadFile(excludePathOf(t, repo))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(afterBytes) != content {
			t.Fatalf("second call must be a no-op:\nbefore:\n%s\nafter:\n%s", content, string(afterBytes))
		}
	})

	t.Run("packs committable while worktrees and artifacts stay hidden", func(t *testing.T) {
		t.Parallel()
		repo, wt := setupWorktreeWithIdentity(t)
		// The in-worktree artifact tree must not surface as a change.
		artifactDir := ArtifactDir(wt.Root, "run-commit-dirty", "spec")
		if err := os.MkdirAll(artifactDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(wt.Root, ".agentum/run-commit-dirty/.ag-artifacts/spec/result.json", "{}")
		clean, err := New().IsClean(t.Context(), wt.Root)
		if err != nil {
			t.Fatalf("IsClean: %v", err)
		}
		if !clean {
			t.Fatal("artifact tree inside the worktree must stay excluded from status")
		}
		// A new project pack in the main checkout shows as untracked — and
		// git add takes it without -f.
		if err := os.MkdirAll(filepath.Join(repo, ".agentum", "packs", "my-pack"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(repo, ".agentum/packs/my-pack/manifest.yaml", "api: agentum/v1\n")
		if err := gitInRepo(repo, "add", ".agentum/packs/my-pack/manifest.yaml"); err != nil {
			t.Fatalf("a project pack must be committable without -f: %v", err)
		}
		// The worktrees dir stays out of the user's untracked view: plain
		// status (no --ignored) must not list it. UncommittedChanges lists
		// ignored entries by design, so it is the wrong probe here.
		if err := os.MkdirAll(filepath.Join(repo, ".agentum", "worktrees", "other"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(repo, ".agentum/worktrees/other/touched", "x")
		plainStatus, err := gitOutput(repo, "status", "--porcelain")
		if err != nil {
			t.Fatalf("git status: %v", err)
		}
		if strings.Contains(plainStatus, "worktrees") {
			t.Fatalf("worktrees dir must stay out of plain status:\n%s", plainStatus)
		}
	})
}

// A cap applied while reading status would cut off the entry that decides:
// git prints ignored entries last and in path order, so a pack directory
// ignored as a whole lists its manifest.yaml after every file that sorts
// before it.
func TestManager_UncommittedChanges_ListsEveryEntry(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	mustWrite(repo, ".gitignore", ".agentum/\n")
	if err := os.MkdirAll(filepath.Join(repo, ".agentum", "packs", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	const junkFiles = 250
	for index := 0; index < junkFiles; index++ {
		mustWrite(repo, fmt.Sprintf(".agentum/packs/x/a%03d.swp", index), "junk\n")
	}
	mustWrite(repo, ".agentum/packs/x/manifest.yaml", "api: agentum/v1\n")

	changes, err := New().UncommittedChanges(t.Context(), repo, ".agentum/packs/x")
	if err != nil {
		t.Fatalf("UncommittedChanges: %v", err)
	}
	if len(changes) != junkFiles+1 {
		t.Fatalf("listed %d entries, want %d (nothing truncated)", len(changes), junkFiles+1)
	}
	last := changes[len(changes)-1]
	if last.Code != "!!" || last.Path != ".agentum/packs/x/manifest.yaml" {
		t.Errorf("last entry = %q %q, want the ignored manifest.yaml", last.Code, last.Path)
	}
}

func TestManager_PathDiffersBetween(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	commitPack(t, repo)
	packCommit := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))

	mustWrite(repo, "unrelated.txt", "outside the pack\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "--quiet", "-m", "unrelated")
	unrelatedCommit := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))

	mustWrite(repo, ".agentum/packs/x/manifest.yaml", "api: agentum/v1\nentry: plan\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "--quiet", "-m", "edit pack")
	editedCommit := strings.TrimSpace(mustGit(t, repo, "rev-parse", "HEAD"))

	cases := []struct {
		name        string
		from, to    string
		wantDiffers bool
	}{
		{"same commit", packCommit, packCommit, false},
		{"a commit outside the pack directory", packCommit, unrelatedCommit, false},
		{"a commit editing the pack", packCommit, editedCommit, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			differs, err := New().PathDiffersBetween(t.Context(), repo, testCase.from, testCase.to, ".agentum/packs/x")
			if err != nil {
				t.Fatalf("PathDiffersBetween: %v", err)
			}
			if differs != testCase.wantDiffers {
				t.Errorf("differs = %v, want %v", differs, testCase.wantDiffers)
			}
		})
	}

	if _, err := New().PathDiffersBetween(t.Context(), repo, packCommit, "0000000000000000000000000000000000000000", ".agentum/packs/x"); err == nil {
		t.Error("an unknown commit must be a failed comparison, not a difference")
	}
}
