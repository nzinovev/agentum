package instructions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeReader is an in-memory Reader for Pin tests. paths maps repo-relative
// path to content; a path in missing simulates os.ErrNotExist at the commit.
type fakeReader struct {
	paths   map[string][]byte
	missing map[string]bool
	err     error // returned for every call when set (a real read failure)
}

func (reader fakeReader) FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error) {
	if reader.err != nil {
		return nil, reader.err
	}
	if reader.missing[path] {
		return nil, os.ErrNotExist
	}
	content, ok := reader.paths[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return content, nil
}

func TestValidatePath(t *testing.T) {
	cases := map[string]struct {
		path    string
		wantErr bool
	}{
		"root file":            {path: "AGENTS.md", wantErr: false},
		"nested file":          {path: "docs/conventions.md", wantErr: false},
		"deep nested":          {path: "src/team/rules.md", wantErr: false},
		"empty":                {path: "", wantErr: true},
		"whitespace only":      {path: "   ", wantErr: true},
		"surrounding space":    {path: " AGENTS.md ", wantErr: true},
		"absolute unix":        {path: "/etc/passwd", wantErr: true},
		"absolute windows":     {path: "C:/Users/x", wantErr: true},
		"parent escape":        {path: "../secret", wantErr: true},
		"nested parent escape": {path: "docs/../../secret", wantErr: true},
		"backslash":            {path: "docs\\conventions.md", wantErr: true},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidatePath(testCase.path)
			if testCase.wantErr && err == nil {
				t.Fatalf("ValidatePath(%q) expected error, got nil", testCase.path)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("ValidatePath(%q) unexpected error: %v", testCase.path, err)
			}
		})
	}
}

func TestValidateDeclaredSet(t *testing.T) {
	tooMany := make([]string, maxDeclaredPaths+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("docs/file%d.md", i)
	}
	cases := map[string]struct {
		paths      []string
		wantErrs   int
		wantSubstr []string // every entry must appear in some problem
	}{
		"empty set":              {paths: nil, wantErrs: 0},
		"valid set":              {paths: []string{"AGENTS.md", "docs/x.md"}, wantErrs: 0},
		"duplicate":              {paths: []string{"AGENTS.md", "AGENTS.md"}, wantErrs: 1, wantSubstr: []string{"duplicated"}},
		"absolute rejected":      {paths: []string{"/etc/x"}, wantErrs: 1, wantSubstr: []string{"absolute"}},
		"parent escape rejected": {paths: []string{"../x"}, wantErrs: 1, wantSubstr: []string{"escapes"}},
		"over cap":               {paths: tooMany, wantErrs: 1, wantSubstr: []string{"at most"}},
		"mix of problems":        {paths: []string{"/abs", "../escape", "ok.md", "ok.md"}, wantErrs: 3},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			problems := ValidateDeclaredSet(testCase.paths)
			if len(problems) != testCase.wantErrs {
				t.Fatalf("got %d problems %v, want %d", len(problems), problems, testCase.wantErrs)
			}
			for _, want := range testCase.wantSubstr {
				found := false
				for _, problem := range problems {
					if strings.Contains(problem, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("want a problem containing %q; got %v", want, problems)
				}
			}
		})
	}
}

func TestCap(t *testing.T) {
	t.Run("under budget returns unchanged", func(t *testing.T) {
		content := []byte("short content\n")
		capped, truncated := Cap(content)
		if truncated {
			t.Fatal("expected truncated=false for under-budget content")
		}
		if string(capped) != string(content) {
			t.Fatal("under-budget content was modified")
		}
	})
	t.Run("exactly at budget is not truncated", func(t *testing.T) {
		content := make([]byte, MaxFileBytes)
		capped, truncated := Cap(content)
		if truncated {
			t.Fatal("content at exactly the budget should not be truncated")
		}
		if len(capped) != MaxFileBytes {
			t.Fatalf("len = %d, want %d", len(capped), MaxFileBytes)
		}
	})
	t.Run("over budget cuts at a line boundary and appends marker", func(t *testing.T) {
		// Build content where the line boundary sits well inside the budget.
		line := []byte("line of text that is reasonably long but not too long\n")
		var content []byte
		for len(content) <= MaxFileBytes {
			content = append(content, line...)
		}
		originalLen := len(content)
		capped, truncated := Cap(content)
		if !truncated {
			t.Fatal("expected truncated=true for over-budget content")
		}
		if len(capped) > MaxFileBytes+len(truncationMarker)+64 {
			t.Fatalf("capped length %d exceeds budget + marker", len(capped))
		}
		if !strings.Contains(string(capped), "agentum: truncated at") {
			t.Fatal("capped content is missing the truncation marker")
		}
		if !strings.Contains(string(capped), fmt.Sprintf("at %d bytes", originalLen)) {
			t.Fatalf("marker does not carry the original length %d", originalLen)
		}
		// The cut should end at a newline (the line boundary).
		cutBody := capped
		if idx := strings.Index(string(cutBody), "<!-- agentum:"); idx >= 0 {
			cutBody = cutBody[:idx]
		}
		if len(cutBody) > 0 && cutBody[len(cutBody)-1] != '\n' {
			t.Fatal("cut body should end at a line boundary (newline)")
		}
	})
}

func TestPin(t *testing.T) {
	t.Run("reads and hashes each path in order", func(t *testing.T) {
		reader := fakeReader{paths: map[string][]byte{
			"AGENTS.md":           []byte("root rules\n"),
			"docs/conventions.md": []byte("conventions\n"),
		}}
		pinned, err := Pin(context.Background(), reader, "repo", "commit",
			[]string{"docs/conventions.md"}, []string{"AGENTS.md"})
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		if len(pinned) != 2 {
			t.Fatalf("got %d files, want 2", len(pinned))
		}
		// Auto (runtime baseline) comes first.
		if pinned[0].RepoPath != "AGENTS.md" || pinned[0].Source != SourceRuntime {
			t.Fatalf("first pinned = %+v, want AGENTS.md/runtime", pinned[0])
		}
		if pinned[1].RepoPath != "docs/conventions.md" || pinned[1].Source != SourceDeclared {
			t.Fatalf("second pinned = %+v, want docs/conventions.md/declared", pinned[1])
		}
		// SourceHash is over the original; DeliveredHash over the (here equal) delivered.
		if pinned[0].SourceHash == "" || pinned[0].SourceHash != pinned[0].DeliveredHash {
			t.Fatalf("source/delivered hash mismatch or empty: %+v", pinned[0])
		}
		if pinned[0].DeliveredBytes != len("root rules\n") {
			t.Fatalf("delivered bytes = %d, want %d", pinned[0].DeliveredBytes, len("root rules\n"))
		}
		if pinned[0].Truncated {
			t.Fatal("small file should not be truncated")
		}
	})
	t.Run("missing at commit is recorded not errored", func(t *testing.T) {
		reader := fakeReader{paths: map[string][]byte{"AGENTS.md": []byte("x")}, missing: map[string]bool{"docs/absent.md": true}}
		pinned, err := Pin(context.Background(), reader, "repo", "commit",
			[]string{"docs/absent.md"}, []string{"AGENTS.md"})
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		var missing File
		for _, file := range pinned {
			if file.RepoPath == "docs/absent.md" {
				missing = file
			}
		}
		if !missing.MissingAtCommit {
			t.Fatal("absent file should be MissingAtCommit=true")
		}
		if missing.SourceHash != "" || missing.DeliveredHash != "" {
			t.Fatal("missing file should have empty hashes")
		}
	})
	t.Run("read error propagates", func(t *testing.T) {
		reader := fakeReader{err: errors.New("git cat-file failed")}
		_, err := Pin(context.Background(), reader, "repo", "commit", nil, []string{"AGENTS.md"})
		if err == nil || !strings.Contains(err.Error(), "git cat-file failed") {
			t.Fatalf("expected propagated read error, got %v", err)
		}
	})
	t.Run("de-duplicates auto and declared", func(t *testing.T) {
		reader := fakeReader{paths: map[string][]byte{"AGENTS.md": []byte("x")}}
		pinned, err := Pin(context.Background(), reader, "repo", "commit",
			[]string{"AGENTS.md"}, []string{"AGENTS.md"})
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		if len(pinned) != 1 {
			t.Fatalf("got %d files, want 1 (de-duplicated)", len(pinned))
		}
		if pinned[0].Source != SourceRuntime {
			t.Fatalf("duplicate should resolve to runtime source, got %s", pinned[0].Source)
		}
	})
	t.Run("total budget exhaustion delivers zero bytes for later files", func(t *testing.T) {
		// Three files each just under MaxFileBytes: together they exceed
		// MaxTotalBytes (3*64 = 192 KiB == budget, so a fourth must be cut).
		big := make([]byte, MaxFileBytes-1)
		reader := fakeReader{paths: map[string][]byte{
			"a.md": big, "b.md": big, "c.md": big, "d.md": big,
		}}
		pinned, err := Pin(context.Background(), reader, "repo", "commit",
			[]string{"a.md", "b.md", "c.md", "d.md"}, nil)
		if err != nil {
			t.Fatalf("Pin: %v", err)
		}
		if len(pinned) != 4 {
			t.Fatalf("got %d files, want 4", len(pinned))
		}
		// The first three consume the budget; the fourth delivers zero.
		var zeroDelivered int
		for _, file := range pinned {
			if file.DeliveredBytes == 0 && file.SourceHash != "" {
				zeroDelivered++
			}
		}
		if zeroDelivered == 0 {
			t.Fatal("expected at least one file delivered as zero bytes once the budget was spent")
		}
		// The zero-delivered file still carries its SourceHash (identity preserved).
		for _, file := range pinned {
			if file.DeliveredBytes == 0 && file.SourceHash == "" {
				t.Fatal("a budget-cut file lost its SourceHash; identity must be preserved")
			}
		}
	})
}

func TestVerify_FourWorktreeStates(t *testing.T) {
	original := []byte("MARKER-ORIGINAL\n")
	other := []byte("TAMPERED-CONTENT\n")
	root := t.TempDir()

	// state_match: worktree file == pin
	matchPath := "match.md"
	if err := os.WriteFile(filepath.Join(root, matchPath), original, 0o644); err != nil {
		t.Fatal(err)
	}
	// state_differs: worktree file != pin
	differsPath := "differs.md"
	if err := os.WriteFile(filepath.Join(root, differsPath), other, 0o644); err != nil {
		t.Fatal(err)
	}
	// state_absent_pinned_present: no worktree file
	absentPath := "absent.md"
	// state_present_pinned_absent: worktree file exists, pin marks it MissingAtCommit
	presentUnpinnedPath := "unpinned.md"
	if err := os.WriteFile(filepath.Join(root, presentUnpinnedPath), other, 0o644); err != nil {
		t.Fatal(err)
	}

	pinned := []File{
		{RepoPath: matchPath, OriginalContent: original, SourceContent: original, SourceHash: hashBytes(original)},
		{RepoPath: differsPath, OriginalContent: original, SourceContent: original, SourceHash: hashBytes(original)},
		{RepoPath: absentPath, OriginalContent: original, SourceContent: original, SourceHash: hashBytes(original)},
		{RepoPath: presentUnpinnedPath, MissingAtCommit: true},
	}

	plan := Verify(root, pinned)
	byPath := make(map[string]Restoration, len(plan))
	for _, restoration := range plan {
		byPath[restoration.Path] = restoration
	}
	if restoration, ok := byPath[matchPath]; !ok || restoration.Action != ActionKeep {
		t.Errorf("match: got %+v, want ActionKeep", restoration)
	}
	if restoration, ok := byPath[differsPath]; !ok || restoration.Action != ActionRestore {
		t.Errorf("differs: got %+v, want ActionRestore", restoration)
	} else if restoration.FoundHash != hashBytes(other) {
		t.Errorf("differs: FoundHash = %s, want %s", restoration.FoundHash, hashBytes(other))
	}
	if restoration, ok := byPath[absentPath]; !ok || restoration.Action != ActionRestore {
		t.Errorf("absent: got %+v, want ActionRestore", restoration)
	} else if restoration.FoundHash != "" {
		t.Errorf("absent: FoundHash should be empty, got %s", restoration.FoundHash)
	}
	// D4 row 4: a worktree file at a path absent at base_commit (the agent
	// authored an instruction file the project never declared) is planned for
	// removal, not skipped — that is the substitution attack wearing a different
	// hat, and leaving it would let an implementer author the reviewer's rules.
	if restoration, ok := byPath[presentUnpinnedPath]; !ok || restoration.Action != ActionRemove {
		t.Errorf("present-pinned-absent: got %+v, want ActionRemove (D4 row 4)", restoration)
	}
}

func TestExecute_RestoresAndRemoves(t *testing.T) {
	root := t.TempDir()
	original := []byte("ORIGINAL\n")
	tampered := []byte("TAMPERED\n")

	// File to restore (currently tampered in the worktree).
	restorePath := "restore.md"
	if err := os.WriteFile(filepath.Join(root, restorePath), tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	// File to remove (exists in worktree, not in pin).
	removePath := "remove.md"
	if err := os.WriteFile(filepath.Join(root, removePath), tampered, 0o644); err != nil {
		t.Fatal(err)
	}

	pinned := []File{{RepoPath: restorePath, SourceContent: original, SourceHash: hashBytes(original)}}
	plan := []Restoration{
		{Path: restorePath, Action: ActionRestore, FoundHash: hashBytes(tampered), PinnedHash: hashBytes(original)},
		{Path: removePath, Action: ActionRemove, FoundHash: hashBytes(tampered)},
	}
	done, err := Execute(plan, pinned, root)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(done) != 2 {
		t.Fatalf("got %d actions, want 2", len(done))
	}
	// Restore wrote the original bytes.
	restored, err := os.ReadFile(filepath.Join(root, restorePath))
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Errorf("restored content = %q, want %q", restored, original)
	}
	// Remove deleted the file.
	if _, err := os.Stat(filepath.Join(root, removePath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove.md should be gone, got err=%v", err)
	}
}

func TestExecute_CreatesParentDirsForNestedPath(t *testing.T) {
	root := t.TempDir()
	original := []byte("NESTED\n")
	nestedPath := "docs/deep/rules.md"
	pinned := []File{{RepoPath: nestedPath, SourceContent: original, SourceHash: hashBytes(original)}}
	plan := []Restoration{{Path: nestedPath, Action: ActionRestore, PinnedHash: hashBytes(original)}}
	done, err := Execute(plan, pinned, root)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("got %d actions, want 1", len(done))
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(nestedPath)))
	if err != nil {
		t.Fatalf("read restored nested file: %v", err)
	}
	if string(content) != string(original) {
		t.Errorf("nested content = %q, want %q", content, original)
	}
}

func TestHashStability(t *testing.T) {
	if hashBytes(nil) != hashBytes([]byte{}) {
		t.Error("nil and empty slice should hash equal")
	}
	if hashBytes([]byte("x")) != hashBytes([]byte("x")) {
		t.Error("equal inputs should hash equal")
	}
	if hashBytes([]byte("x")) == hashBytes([]byte("y")) {
		t.Error("different inputs hashed equal")
	}
}

// TestVerify_LineEndingNormalisationIsNotTamper (ADR 0002 D4): line-ending
// differences must not read as tampering in EITHER direction.
//
//   - autocrlf checkout: git stores LF, worktree holds CRLF. A one-sided
//     normalise (worktree only) is the fix for this case.
//   - CRLF-committed repo: git stores CRLF, worktree holds CRLF. A one-sided
//     normalise against a raw (CRLF) SourceHash would false-positive forever.
//
// Verify normalises BOTH sides, so a byte-identical file classifies as Keep
// regardless of which ending the object carries. SourceHash stays over the raw
// bytes for evidence identity; the comparison uses a normalised hash.
func TestVerify_LineEndingNormalisationIsNotTamper(t *testing.T) {
	t.Run("autocrlf checkout: LF object, CRLF worktree", func(t *testing.T) {
		root := t.TempDir()
		lfOriginal := []byte("MARKER-ORIGINAL\n")
		crlfCopy := []byte("MARKER-ORIGINAL\r\n")
		matchPath := "match.md"
		if err := os.WriteFile(filepath.Join(root, matchPath), crlfCopy, 0o644); err != nil {
			t.Fatal(err)
		}
		pinned := []File{{RepoPath: matchPath, OriginalContent: lfOriginal, SourceContent: lfOriginal, SourceHash: hashBytes(lfOriginal)}}
		plan := Verify(root, pinned)
		if len(plan) != 1 || plan[0].Action != ActionKeep {
			t.Fatalf("CRLF worktree copy of an LF pin should be ActionKeep; got %+v", plan)
		}
	})
	t.Run("CRLF-committed repo: CRLF object, CRLF worktree", func(t *testing.T) {
		root := t.TempDir()
		crlfOriginal := []byte("MARKER-ORIGINAL\r\n")
		crlfCopy := []byte("MARKER-ORIGINAL\r\n")
		matchPath := "match.md"
		if err := os.WriteFile(filepath.Join(root, matchPath), crlfCopy, 0o644); err != nil {
			t.Fatal(err)
		}
		// SourceHash is over the raw CRLF bytes (evidence identity); the
		// comparison must still treat the byte-identical worktree file as Keep.
		pinned := []File{{RepoPath: matchPath, OriginalContent: crlfOriginal, SourceContent: crlfOriginal, SourceHash: hashBytes(crlfOriginal)}}
		plan := Verify(root, pinned)
		if len(plan) != 1 || plan[0].Action != ActionKeep {
			t.Fatalf("CRLF-committed byte-identical file should be ActionKeep; got %+v", plan)
		}
	})
	t.Run("a genuinely different file still trips a restore", func(t *testing.T) {
		root := t.TempDir()
		lfOriginal := []byte("MARKER-ORIGINAL\n")
		differPath := "differ.md"
		if err := os.WriteFile(filepath.Join(root, differPath), []byte("TAMPERED\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		pinnedDiffer := []File{{RepoPath: differPath, OriginalContent: lfOriginal, SourceContent: lfOriginal, SourceHash: hashBytes(lfOriginal)}}
		planDiffer := Verify(root, pinnedDiffer)
		if len(planDiffer) != 1 || planDiffer[0].Action != ActionRestore {
			t.Fatalf("a tampered file must still trip ActionRestore; got %+v", planDiffer)
		}
	})
}

// TestExecute_RestoresOriginalBytesNotTruncated (ADR 0002 D4 + findings F1):
// when a pinned file was truncated for delivery, Execute must restore the
// ORIGINAL base_commit bytes, not the truncated SourceContent. Writing the
// truncated form would re-truncate on every stage, land a mutilated file (with
// the truncation marker inside it) in the checkpoint commit, and never
// converge — the next Verify still sees a file that differs from the original.
// Restoring the original converges: Verify on the rewritten file reports Keep.
func TestExecute_RestoresOriginalBytesNotTruncated(t *testing.T) {
	root := t.TempDir()
	original := bytes.Repeat([]byte("line of original content\n"), 4000) // ~96 KiB > MaxFileBytes
	truncated, wasTruncated := Cap(original)
	if !wasTruncated {
		t.Fatal("fixture: expected Cap to truncate the over-budget original")
	}
	pinned := []File{{
		RepoPath:        "big.md",
		OriginalContent: original,
		SourceContent:   truncated,
		SourceHash:      hashBytes(original),
		DeliveredHash:   hashBytes(truncated),
		Truncated:       true,
	}}
	// Worktree holds a tampered short file so Verify plans a restore.
	tamperedPath := filepath.Join(root, "big.md")
	if err := os.WriteFile(tamperedPath, []byte("TAMPERED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := Verify(root, pinned)
	if len(plan) != 1 || plan[0].Action != ActionRestore {
		t.Fatalf("Verify: got %+v, want one ActionRestore", plan)
	}
	done, err := Execute(plan, pinned, root)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(done) != 1 {
		t.Fatalf("Execute: got %d actions, want 1", len(done))
	}
	// The restored file holds the ORIGINAL bytes, not the truncated form.
	restored, err := os.ReadFile(tamperedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Errorf("restore wrote %d bytes (truncated form?), want %d original bytes", len(restored), len(original))
	}
	if strings.Contains(string(restored), "agentum: truncated") {
		t.Error("restore wrote the truncation marker into the worktree file")
	}
	// Convergence: Verify on the restored file now reports Keep, not Restore.
	convergence := Verify(root, pinned)
	if len(convergence) != 1 || convergence[0].Action != ActionKeep {
		t.Fatalf("Verify after restore did not converge to Keep; got %+v (restore loop)", convergence)
	}
}
