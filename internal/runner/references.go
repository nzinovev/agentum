package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/references"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/worktree"
)

// Caps on what the reference channel renders and records. The snapshot index
// in the pristine store keeps the full lists.
const (
	// maxReferenceListing is the number of file names the routing block lists
	// across all reference paths. The agent lists the directory for the rest.
	maxReferenceListing = 100
	// maxReferenceSkipsRecorded is the number of skipped paths one manifest
	// entry carries.
	maxReferenceSkipsRecorded = 20
	// maxReferenceChangesReported is the number of corrected paths one
	// run.references_restored event carries.
	maxReferenceChangesReported = 20
)

// pinReferences gives the run its reference snapshot: the files under the
// paths the project declared in .agentum.yaml, copied from the local checkout
// into the run's pristine store. The first job of a run takes the snapshot and
// every later job loads it, so an edit of the source directory after the run
// started does not reach the run.
//
// A snapshot that cannot be taken is recorded as an evidence gap and the run
// proceeds without reference files, the same way a failed instruction pin does.
func (runner *Runner) pinReferences(ctx context.Context, run *stageRun, baseCommit string, declared []string) {
	if len(declared) == 0 {
		return
	}
	checkoutPath := checkoutPathOf(run.record, run.project)
	storeDir := worktree.ReferenceStoreDir(checkoutPath, run.record.ID)

	pin, found, loadErr := references.Load(storeDir)
	if loadErr != nil {
		// An unreadable index is replaced by a fresh snapshot below.
		runner.log.Warn("load reference snapshot", "run", run.record.ID, "error", loadErr)
	}
	if found {
		run.references = pin
		return
	}

	pin, snapshotErr := references.Snapshot(ctx, checkoutPath, storeDir, declared,
		trackedAtCommit{manager: runner.wt, repoPath: checkoutPath, commit: baseCommit},
		// Reference files are human-authored material, so the prose rule set
		// applies: it matches credential material itself. The label rules of
		// the full set also match text that only describes a credential.
		referenceScanner{scanner: artifacts.NewProseScanner(artifacts.PolicyReject)})
	if snapshotErr != nil {
		runner.log.Warn("snapshot reference files", "run", run.record.ID, "error", snapshotErr)
		runner.recordEvidenceGap(ctx, run.record, "context.references", "", snapshotErr)
		return
	}
	run.references = pin
	runner.emit(ctx, run.record, EvReferencesPinned, map[string]any{
		"base_commit": baseCommit,
		"references":  referenceRefsForEvidence(pin),
	})
}

// materializeReferences syncs the run's pristine reference snapshot into the
// worktree before a stage: a file an earlier stage edited or deleted is
// rewritten, and a path it added is removed. Every stage therefore reads the
// bytes the snapshot recorded. The copy lives under .agentum/, which git
// ignores, so the sync does not change `git status` or a checkpoint commit.
//
// An IO error is returned and fails the run, as a failed instruction restore
// does: the next stage would read reference files the evidence does not
// describe.
func (runner *Runner) materializeReferences(ctx context.Context, run stageRun, stageID string) error {
	if run.references.Empty() {
		return nil
	}
	storeDir := worktree.ReferenceStoreDir(checkoutPathOf(run.record, run.project), run.record.ID)
	changes, err := references.Materialize(run.references, storeDir, run.worktree.Root, worktree.ReferenceRelDir(run.record.ID))
	if err != nil {
		return fmt.Errorf("materialize reference files for stage %q: %w", stageID, err)
	}
	if len(changes) == 0 {
		return nil
	}
	reported := changes
	if len(reported) > maxReferenceChangesReported {
		reported = reported[:maxReferenceChangesReported]
	}
	runner.log.Info("reference copy restored", "run", run.record.ID, "stage", stageID, "changes", len(changes))
	runner.emit(ctx, run.record, EvReferencesRestored, map[string]any{
		"stage": stageID, "changes": reported, "changes_total": len(changes),
	})
	return nil
}

// removeReferenceStore deletes the run's pristine reference snapshot after its
// worktree was removed. A failure is logged and does not fail the removal that
// already happened: the leftover directory is ignored by git and holds only
// copies of files the checkout still has.
func (runner *Runner) removeReferenceStore(checkoutPath, runID string) {
	if err := runner.wt.RemoveReferenceStore(checkoutPath, runID); err != nil {
		runner.log.Warn("remove reference store", "run", runID, "error", err)
	}
}

// referenceRefsForRender maps the run's snapshot onto the routing block's
// reference section. Location is the absolute path of each copy inside the
// worktree, and the listing names files relative to it.
func referenceRefsForRender(run stageRun) []routing.ReferenceRef {
	if run.references.Empty() {
		return nil
	}
	copyRoot := worktree.ReferenceDir(run.worktree.Root, run.record.ID)
	listingBudget := maxReferenceListing
	refs := make([]routing.ReferenceRef, 0, len(run.references.Entries))
	for _, entry := range run.references.Entries {
		ref := routing.ReferenceRef{
			Path:       entry.Path,
			Location:   filepath.Join(copyRoot, filepath.FromSlash(entry.Path)),
			Files:      len(entry.Files),
			Bytes:      entry.Bytes,
			Skipped:    entry.SkippedTotal,
			Missing:    entry.Missing,
			Incomplete: entry.BudgetExhausted,
		}
		for _, file := range entry.Files {
			relativeName := strings.TrimPrefix(file.Path, entry.Path+"/")
			if relativeName == file.Path {
				// The declared path is the file itself: Location already names it.
				continue
			}
			if listingBudget == 0 {
				ref.ListingOmitted++
				continue
			}
			ref.Listing = append(ref.Listing, relativeName)
			listingBudget--
		}
		refs = append(refs, ref)
	}
	return refs
}

// referenceRefsForEvidence maps a snapshot onto the manifest's context
// section: one ref per declared path with its tree hash, counts, and the first
// skipped paths. Per-file hashes stay in the pristine store's index.
func referenceRefsForEvidence(pin references.Pin) []manifest.ReferenceRef {
	if pin.Empty() {
		return nil
	}
	refs := make([]manifest.ReferenceRef, 0, len(pin.Entries))
	for _, entry := range pin.Entries {
		ref := manifest.ReferenceRef{
			Path:            entry.Path,
			TreeHash:        entry.TreeHash,
			Files:           len(entry.Files),
			Bytes:           entry.Bytes,
			Missing:         entry.Missing,
			BudgetExhausted: entry.BudgetExhausted,
			SkippedTotal:    entry.SkippedTotal,
		}
		for _, skipped := range entry.Skipped {
			if len(ref.Skipped) == maxReferenceSkipsRecorded {
				break
			}
			ref.Skipped = append(ref.Skipped, manifest.ReferenceSkip{Path: skipped.Path, Reason: string(skipped.Reason)})
		}
		refs = append(refs, ref)
	}
	return refs
}

// trackedAtCommit answers references.TrackedLister from the tree of the run's
// base commit. A file that exists there is already in the worktree, so the
// snapshot leaves the checkout's version of it out.
type trackedAtCommit struct {
	manager  *worktree.Manager
	repoPath string
	commit   string
}

func (tracked trackedAtCommit) TrackedPaths(ctx context.Context, dir string) (map[string]bool, error) {
	entries, err := tracked.manager.ListTreeAtCommit(ctx, tracked.repoPath, tracked.commit, dir)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool, len(entries))
	for _, entry := range entries {
		paths[entry.Path] = true
	}
	return paths, nil
}

// referenceScanner adapts the artifact store's credential scanner to
// references.Scanner. Under PolicyReject a finding is an error, which the
// snapshot records as a skipped file.
type referenceScanner struct {
	scanner artifacts.Scanner
}

func (adapter referenceScanner) ScanName(name string) error {
	return adapter.scanner.ScanName(name)
}

func (adapter referenceScanner) ScanContent(name string, content []byte) error {
	_, err := adapter.scanner.Scan(name, "reference", content)
	return err
}
