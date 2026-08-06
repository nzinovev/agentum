// Package instructions handles the project-instruction channel declared in
// ADR 0002: which repo-relative instruction files enter a run, the bytes pinned
// from base_commit, the caps and truncation applied before delivery, and the
// pre-stage tamper check that restores a worktree copy to its pinned form.
//
// The package is pure and dependency-light on purpose. It does not touch git,
// the runner, or the adapter — the runner supplies the file reader (the
// FileAtCommit seam) and executes the restore plan. That keeps the rules
// testable in isolation and keeps the agent-immutability argument (pinned bytes
// come from a commit the agent cannot reach) readable in one place.
package instructions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Byte budgets for the instruction channel. A single instruction file is capped
// at MaxFileBytes; the whole set is capped at MaxTotalBytes. Both exist because
// context is finite and a 10 MiB AGENTS.md would silently displace the task,
// and because truncation must be a recorded fact rather than a silent shrink
// (ADR 0002 D3). Files are processed in declaration order; a file that would
// cross the per-file budget is cut at the last line boundary that fits, and
// once the total budget is spent the remaining files are delivered as zero
// bytes — every such outcome is recorded per file.
const (
	MaxFileBytes  = 64 << 10  // 64 KiB per file
	MaxTotalBytes = 192 << 10 // 192 KiB across the whole set

	maxDeclaredPaths = 8 // ADR 0002 D1: at most 8 project-declared paths
)

// truncationMarker is appended to a file cut by Cap so the model can see the
// cut happened, not just that the text ended. Carries the byte count so a
// reviewer reading evidence can reconcile delivered_bytes with the original.
const truncationMarker = "\n<!-- agentum: truncated at %d bytes -->\n"

// Reader is the FileAtCommit-shaped seam the runner supplies. Decoupled from
// internal/worktree so this package stays pure and testable with a fake reader.
// A path absent at the commit is signalled as os.ErrNotExist; any other error
// is a real read failure.
type Reader interface {
	FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error)
}

// Source labels where an instruction file came from, so evidence can distinguish
// the runtime-injected baseline (AGENTS.md, loaded by opencode itself) from a
// path the project declared in .agentum.yaml.
type Source string

const (
	SourceRuntime  Source = "runtime"  // the runtime loads it itself (AGENTS.md at the root)
	SourceDeclared Source = "declared" // the project listed it under instructions:
)

// File is one pinned project-instruction file: the repo-relative path it
// occupies in the project, the exact bytes read from base_commit, and the
// (possibly truncated) bytes that will reach the model.
//
// SourceHash is the sha256 of the ORIGINAL base_commit bytes (pre-cap) — it is
// the file's identity in evidence and in tamper checks, stable even when the
// delivered form is truncated. DeliveredHash is the sha256 of the post-truncate
// bytes the model actually saw. Recording both is what makes truncation an
// attributable fact: a file whose SourceHash matches across runs but whose
// DeliveredHash differs was cut differently.
type File struct {
	RepoPath        string // repo-relative, e.g. "AGENTS.md" — identity in evidence and deny rules
	Source          Source // runtime | declared
	OriginalContent []byte // the bytes read from base_commit, before capping
	SourceContent   []byte // the bytes that will be delivered (capped + truncated form)
	SourceHash      string // sha256 hex of OriginalContent
	DeliveredHash   string // sha256 hex of SourceContent
	DeliveredBytes  int    // len(SourceContent)
	Truncated       bool   // true when SourceContent is shorter than OriginalContent
	MissingAtCommit bool   // true when the path was absent at base_commit (recorded, not delivered)
}

// Restoration is one entry in the pre-stage tamper plan (ADR 0002 D4). It is
// produced by Verify without touching the filesystem, so the plan is inspectable
// before anything runs; Execute applies it. FoundHash is the sha256 of what the
// worktree held at verification time (empty when the file was absent), with
// CRLF folded to LF so a checkout under core.autocrlf=true does not read as
// tampering against the LF bytes git stores in the object.
type Restoration struct {
	Path       string        // repo-relative
	Action     RestoreAction // keep | restore | remove
	FoundHash  string        // sha256 hex of the worktree content at verify time (LF-normalised)
	PinnedHash string        // sha256 hex of the pinned source (empty when removing)
}

// RestoreAction is D4's table as a closed set.
type RestoreAction string

const (
	ActionKeep    RestoreAction = "keep"    // worktree matches the pin
	ActionRestore RestoreAction = "restore" // worktree differs or is absent; rewrite pinned bytes
	ActionRemove  RestoreAction = "remove"  // worktree holds a file the pin does not; delete it
)

// ValidatePath applies ADR 0002 D1's rules for one project-instruction path:
// repo-relative (no absolute path), no ".." escape, forward slashes, non-empty.
// Mirrors checks.pathEscapes but lives here so the rule is owned by the channel
// that enforces it. Backslashes are rejected because the delivery channel and
// the edit-deny patterns are written in forward-slash form; a mixed convention
// would silently fail to match.
//
// The path is normalised to forward slashes first, so the absolute-path and
// escape checks are OS-independent: a unix-shaped "/etc/x" is rejected on
// Windows too (filepath.IsAbs would not catch it there, since it expects a
// drive letter), and a Windows-shaped "C:/x" is rejected on unix.
func ValidatePath(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return errors.New("instruction path is empty")
	}
	if path != trimmed {
		return fmt.Errorf("instruction path %q has surrounding whitespace", path)
	}
	if strings.ContainsAny(path, "\\") {
		return fmt.Errorf("instruction path %q must use forward slashes", path)
	}
	if isAbsoluteForwardSlash(path) {
		return fmt.Errorf("instruction path %q must be repo-relative, not absolute", path)
	}
	cleaned := filepath.Clean(filepath.FromSlash(path))
	cleanedForward := filepath.ToSlash(cleaned)
	if cleanedForward == ".." || strings.HasPrefix(cleanedForward, "../") {
		return fmt.Errorf("instruction path %q escapes the repo root", path)
	}
	return nil
}

// isAbsoluteForwardSlash reports whether a forward-slash path is absolute on
// either platform: a leading "/" (unix) or a "DRIVE:/" / "DRIVE:\\" root
// (Windows). The backslash form is already rejected upstream, so only the drive
// case is handled here.
func isAbsoluteForwardSlash(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	// Windows drive letter, e.g. "C:/Users" or "c:/x".
	if len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\') {
		lower := path[0]
		if lower >= 'A' && lower <= 'Z' || lower >= 'a' && lower <= 'z' {
			return true
		}
	}
	return false
}

// ValidateDeclaredSet validates a list of project-declared paths (from
// .agentum.yaml) as a whole: each path passes ValidatePath, there are no
// duplicates, and the set is within maxDeclaredPaths. Problems are returned as
// a slice so the caller can fold them into its own multi-error pass, matching
// the checks config validator's style.
func ValidateDeclaredSet(paths []string) []string {
	var problems []string
	if len(paths) > maxDeclaredPaths {
		problems = append(problems, fmt.Sprintf("instructions: at most %d entries, got %d", maxDeclaredPaths, len(paths)))
	}
	seen := make(map[string]bool, len(paths))
	for index, path := range paths {
		if err := ValidatePath(path); err != nil {
			problems = append(problems, fmt.Sprintf("instructions[%d]: %v", index, err))
			continue
		}
		if seen[path] {
			problems = append(problems, fmt.Sprintf("instructions[%d]: path %q is duplicated", index, path))
		}
		seen[path] = true
	}
	return problems
}

// Cap truncates content to MaxFileBytes at the last line boundary that fits,
// appending the truncation marker. If the content already fits it is returned
// unchanged with truncated=false. The marker carries the original length so
// evidence reconciles with OriginalContent without a second hash.
//
// The line-boundary cut keeps the delivered text parseable: a markdown fence or
// a code block cut mid-line would confuse the model about its own instructions,
// and the marker is itself a comment-shaped line so it does not add structure.
func Cap(content []byte) ([]byte, bool) {
	if len(content) <= MaxFileBytes {
		return content, false
	}
	cut := MaxFileBytes
	if newline := bytesLastIndex(content[:cut], '\n'); newline >= 0 {
		cut = newline + 1 // include the trailing newline of the last full line
	}
	marker := []byte(fmt.Sprintf(truncationMarker, len(content)))
	capped := make([]byte, 0, cut+len(marker))
	capped = append(capped, content[:cut]...)
	capped = append(capped, marker...)
	return capped, true
}

// bytesLastIndex is a thin wrapper kept for readability and to avoid importing
// the bytes package solely for one call.
func bytesLastIndex(buf []byte, b byte) int {
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] == b {
			return i
		}
	}
	return -1
}

// Pin reads every instruction path via reader from baseCommit and returns the
// pinned set. declared are the project-declared paths (from .agentum.yaml);
// auto are the runtime-injected baseline paths (opencode loads AGENTS.md at the
// root itself — ADR 0002 D1). The two sources are unioned, de-duplicated by
// repo-relative path, and pinned in a stable order: auto first (they are the
// baseline), then declared in their listed order.
//
// Per-file and total budgets are applied in processing order. A path absent at
// base_commit is not an error: its File carries MissingAtCommit=true and is
// recorded in evidence (the caller collects it) rather than delivered. Any
// other read error is returned.
func Pin(ctx context.Context, reader Reader, repoPath, commit string, declared, auto []string) (pinned []File, err error) {
	ordered := assembleOrder(declared, auto)
	pinned = make([]File, 0, len(ordered))
	totalBudget := MaxTotalBytes
	for _, entry := range ordered {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		original, readErr := reader.FileAtCommit(ctx, repoPath, commit, entry.path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				pinned = append(pinned, File{
					RepoPath:        entry.path,
					Source:          entry.source,
					MissingAtCommit: true,
				})
				continue
			}
			return nil, fmt.Errorf("pin instruction %q: %w", entry.path, readErr)
		}
		sourceHash := hashBytes(original)
		// Apply the per-file cap first, then the remaining total budget.
		cappedContent, fileTruncated := Cap(original)
		delivered := cappedContent
		deliveredTruncated := fileTruncated
		if len(delivered) > totalBudget {
			// The total budget is spent. Deliver zero bytes but keep the source
			// hash so the file's identity is still attributable in evidence.
			delivered = nil
			deliveredTruncated = true
		}
		pinned = append(pinned, File{
			RepoPath:        entry.path,
			Source:          entry.source,
			OriginalContent: original,
			SourceContent:   delivered,
			SourceHash:      sourceHash,
			DeliveredHash:   hashBytes(delivered),
			DeliveredBytes:  len(delivered),
			Truncated:       deliveredTruncated,
		})
		if len(delivered) > 0 {
			totalBudget -= len(delivered)
		}
	}
	return pinned, nil
}

// pinEntry pairs a repo-relative path with its source label.
type pinEntry struct {
	path   string
	source Source
}

// assembleOrder unions auto and declared into a stable, de-duplicated order:
// the runtime baseline first, then declared paths in their listed order. A path
// appearing in both is kept as runtime (the runtime loads it regardless, and
// the runtime label is the honest attribution).
func assembleOrder(declared, auto []string) []pinEntry {
	entries := make([]pinEntry, 0, len(auto)+len(declared))
	seen := make(map[string]bool, len(auto)+len(declared))
	for _, path := range auto {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		entries = append(entries, pinEntry{path: path, source: SourceRuntime})
	}
	for _, path := range declared {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		entries = append(entries, pinEntry{path: path, source: SourceDeclared})
	}
	return entries
}

// Verify compares each instruction path's worktree content against the pinned
// source and returns the restore plan (ADR 0002 D4's table) WITHOUT executing
// it. The plan is pure: it reads the worktree only to classify state.
//
// Line endings are normalised (CRLF → LF) before hashing, because a project
// under core.autocrlf=true checks out text files with CRLF while git stores LF
// in the object — a semantically-identical file would otherwise trip a
// false-positive restore every stage, and the orchestrator-authored rewrite
// would re-introduce LF that checkout immediately re-converts, looping. The
// pin's SourceHash is over the original (LF) bytes, so normalising the worktree
// read before comparison is what keeps the two on the same axis.
//
// Files marked MissingAtCommit are skipped: there is nothing to restore to, and
// a worktree file at that path is the agent creating a file the project never
// declared at the lineage anchor — left to the ordinary review path, not to the
// instruction restore.
func Verify(worktreeRoot string, pinned []File) []Restoration {
	plan := make([]Restoration, 0, len(pinned))
	for _, file := range pinned {
		if file.MissingAtCommit {
			continue
		}
		absolutePath := filepath.Join(worktreeRoot, filepath.FromSlash(file.RepoPath))
		found, readErr := os.ReadFile(absolutePath)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				// Absent in the worktree, present in the pin: the runtime deleted
				// it, or a fresh worktree lost it. Rewrite the pinned bytes.
				plan = append(plan, Restoration{
					Path:       file.RepoPath,
					Action:     ActionRestore,
					PinnedHash: file.SourceHash,
				})
				continue
			}
			// An unreadable file is an IO problem Execute will surface; classify
			// it as a restore so the runner attempts the rewrite and gets the
			// real error there.
			plan = append(plan, Restoration{
				Path:       file.RepoPath,
				Action:     ActionRestore,
				PinnedHash: file.SourceHash,
			})
			continue
		}
		foundHash := hashBytes(normaliseLineEndings(found))
		pinnedHash := file.SourceHash
		if foundHash == pinnedHash {
			plan = append(plan, Restoration{Path: file.RepoPath, Action: ActionKeep, FoundHash: foundHash, PinnedHash: pinnedHash})
			continue
		}
		// Differs after line-ending normalisation: the agent (or a prior stage)
		// rewrote it. Rewrite the pinned bytes over the tampered copy so the
		// next invocation sees the original.
		plan = append(plan, Restoration{
			Path:       file.RepoPath,
			Action:     ActionRestore,
			FoundHash:  foundHash,
			PinnedHash: pinnedHash,
		})
	}
	return plan
}

// normaliseLineEndings converts CRLF to LF so a worktree checked out under
// core.autocrlf=true compares equal to the LF bytes git stores in the object.
// A lone CR (old Mac) is left alone; only the CRLF pair is folded.
func normaliseLineEndings(data []byte) []byte {
	if !containsCRLF(data) {
		return data
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if i+1 < len(data) && data[i] == '\r' && data[i+1] == '\n' {
			out = append(out, '\n')
			i++
			continue
		}
		out = append(out, data[i])
	}
	return out
}

// containsCRLF reports whether data holds at least one CRLF pair.
func containsCRLF(data []byte) bool {
	for i := 0; i+1 < len(data); i++ {
		if data[i] == '\r' && data[i+1] == '\n' {
			return true
		}
	}
	return false
}

// Execute applies a restoration plan to the worktree and returns one entry per
// restoration it acted on (ActionKeep omitted). It is the only function in this
// package that writes. A write or remove IO error is returned as error so the
// runner can fail the task — matching the existing precedent that a broken
// invariant at the delivery boundary fails rather than proceeds on a claim we
// cannot stand behind (runner.ErrDirtyTreeAtDeliveryBoundary).
//
// pinned is passed so Execute can look up the exact bytes to restore by path.
func Execute(plan []Restoration, pinned []File, worktreeRoot string) (done []Restoration, err error) {
	byPath := make(map[string]File, len(pinned))
	for _, file := range pinned {
		byPath[file.RepoPath] = file
	}
	done = make([]Restoration, 0, len(plan))
	for _, restoration := range plan {
		switch restoration.Action {
		case ActionKeep:
			continue
		case ActionRestore:
			file, ok := byPath[restoration.Path]
			if !ok {
				// A restoration for a path we did not pin is a caller bug; fail
				// loudly rather than guess.
				return done, fmt.Errorf("instructions: restore plan references unpinned path %q", restoration.Path)
			}
			absolutePath := filepath.Join(worktreeRoot, filepath.FromSlash(restoration.Path))
			if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
				return done, fmt.Errorf("instructions: create dir for %q: %w", restoration.Path, err)
			}
			if err := os.WriteFile(absolutePath, file.SourceContent, 0o644); err != nil {
				return done, fmt.Errorf("instructions: restore %q: %w", restoration.Path, err)
			}
			done = append(done, restoration)
		case ActionRemove:
			absolutePath := filepath.Join(worktreeRoot, filepath.FromSlash(restoration.Path))
			if err := os.Remove(absolutePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return done, fmt.Errorf("instructions: remove %q: %w", restoration.Path, err)
			}
			done = append(done, restoration)
		}
	}
	return done, nil
}

// hashBytes returns the lowercase sha256 hex of buf. Empty input yields the
// well-known empty-hash, which is a legitimate delivered hash for a file cut to
// zero by the total budget — the evidence then reads "delivered_hash is the
// empty hash, delivered_bytes is 0," which is honest.
func hashBytes(buf []byte) string {
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}
