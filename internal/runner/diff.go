package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/worktree"
)

// diffPatchCap is the maximum size of the patch the orchestrator materializes
// for a reviewer (ADR 0003 D5). 1 MiB is large enough for a focused single-task
// change set and small enough that the routing block and the revision store are
// not dominated by it. When the patch exceeds the cap it is truncated on a hunk
// boundary with an explicit marker, and the stat (never truncated) plus direct
// file reads cover the remainder.
const diffPatchCap = 1 << 20

// produceDiff writes the orchestrator-produced change set into the stage's
// artifact dir and captures both files as immutable revisions (ADR 0003 D5).
// diff.patch is `git diff base..HEAD`, capped at diffPatchCap on a hunk
// boundary with an explicit truncation marker; diff.stat is `git diff --stat`,
// never truncated. Both survive teardown because they are revisions in a
// content-addressed store outside any worktree.
//
// Called at the start of a reviewer-role stage (strictly before invokeStage,
// next to restoreInstructions) so HEAD is the post-stage checkpoint the
// orchestrator itself authored — the patch describes a real commit range. The
// read does not run between the isClean sample and recordStageCheckpoint
// (those run inside invokeStage, which is later).
//
// A secret-scanner refusal is recorded as an EvidenceGap plus a
// stage.artifact_rejected event — never a quiet skip, since losing the diff
// silently would leave a reviewer with nothing to read.
func (runner *Runner) produceDiff(ctx context.Context, run stageRun, stageID string) *routing.DiffRef {
	if runner.art == nil {
		return nil
	}
	baseCommit := run.task.BaseCommit.String
	if baseCommit == "" {
		return nil
	}
	headCommit, err := runner.wt.HeadCommit(ctx, run.worktree.Root)
	if err != nil {
		runner.log.Warn("produce diff: resolve HEAD", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "artifacts", "",
			fmt.Errorf("delivery diff: could not resolve HEAD: %w", err))
		return nil
	}
	patchRaw, err := runner.wt.Diff(ctx, run.worktree.Root, baseCommit, headCommit, false)
	if err != nil {
		runner.log.Warn("produce diff: git diff", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "artifacts", "",
			fmt.Errorf("delivery diff: git diff failed: %w", err))
		return nil
	}
	statRaw, err := runner.wt.Diff(ctx, run.worktree.Root, baseCommit, headCommit, true)
	if err != nil {
		runner.log.Warn("produce diff: git diff --stat", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "artifacts", "",
			fmt.Errorf("delivery diff: git diff --stat failed: %w", err))
		return nil
	}
	patchBody, truncated := capDiffPatch(patchRaw, diffPatchCap)
	artifactDir := worktree.ArtifactDir(run.worktree.Root, run.task.ID, stageID)
	if mkErr := os.MkdirAll(artifactDir, 0o755); mkErr != nil {
		runner.log.Warn("produce diff: mkdir artifact dir", "task", run.task.ID, "error", mkErr)
		return nil
	}
	ref := &routing.DiffRef{
		BaseCommit: baseCommit,
		HeadCommit: headCommit,
		Truncated:  truncated,
		PatchPath:  filepath.Join(artifactDir, "diff.patch"),
		StatPath:   filepath.Join(artifactDir, "diff.stat"),
	}
	// Write the materialized copies so the agent reads them by path with
	// fs.read alone (it has no shell). The orchestrator-constructed artifact dir
	// needs no containment check, like verdict.json and plan.md.
	if writeErr := os.WriteFile(ref.PatchPath, patchBody, 0o644); writeErr != nil {
		runner.log.Warn("produce diff: write patch", "task", run.task.ID, "error", writeErr)
		return nil
	}
	if writeErr := os.WriteFile(ref.StatPath, statRaw, 0o644); writeErr != nil {
		runner.log.Warn("produce diff: write stat", "task", run.task.ID, "error", writeErr)
		return nil
	}
	// Capture both as immutable revisions with ActorSystem — the orchestrator
	// produced this diff, not the agent, so the record attributes it correctly
	// and D5's "the implementer cannot shape what its reviewer sees" is
	// verifiable from the actor on the revision row. A refused Put (secret
	// scanner) is recorded as a gap + event rather than vanishing — a diff of a
	// real change is the single most likely artifact to trip the scanner.
	patchRev, patchStored := runner.ingest(ctx, run, stageID, "", stageID+"/diff.patch", "diff", patchBody, artifacts.ActorSystem)
	if patchStored {
		ref.PatchRevisionID = patchRev.ID
	} else {
		runner.recordEvidenceGap(ctx, run.task, "artifacts", "",
			fmt.Errorf("delivery diff: patch revision refused by the artifact store (secret policy?)"))
		runner.emit(ctx, run.task, EvArtifactRejected, map[string]any{
			"stage": stageID, "name": stageID + "/diff.patch", "kind": "diff",
		})
	}
	statRev, statStored := runner.ingest(ctx, run, stageID, "", stageID+"/diff.stat", "diff_stat", statRaw, artifacts.ActorSystem)
	if statStored {
		ref.StatRevisionID = statRev.ID
	}
	return ref
}

// capDiffPatch truncates patch to at most cap bytes on a hunk boundary ("@@"),
// appending artifacts.DiffTruncationMarker so a reader knows the patch was cut
// (the final-review payload detects the marker in the stored bytes). Returns
// (body, truncated). When the patch fits, it is returned verbatim and truncated
// is false. Truncating on a hunk boundary keeps every retained hunk complete and
// reviewable, rather than cutting mid-hunk.
func capDiffPatch(patch []byte, cap int) (body []byte, truncated bool) {
	if len(patch) <= cap {
		return patch, false
	}
	// Walk backward from the cap to the last hunk header that fits entirely
	// before it, so the body ends on a complete hunk.
	cutoff := cap
	lastHunk := bytes.LastIndex(patch[:cutoff], []byte("\n@@"))
	if lastHunk > 0 {
		cutoff = lastHunk + 1 // include the newline before the hunk header
	}
	marker := []byte(artifacts.DiffTruncationMarker)
	truncated = true
	return append(patch[:cutoff], marker...), truncated
}
