package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// captureStageOutputs reads the artifacts the agent declared in result.json,
// plus result.json itself, and ingests each into the durable revisions store.
// Best-effort: a missing file is logged and skipped; the manifest's "missing"
// section records the gap. Each captured artifact becomes a new immutable
// revision chained to the prior current revision of its (task, name). The
// source invocation id is recorded so the manifest can resolve "what did this
// invocation produce."
func (runner *Runner) captureStageOutputs(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, artifactDir string,
	result *agent.ResultJSON,
) {
	if runner.art == nil {
		return
	}
	// Always capture result.json itself — it is the contract-shaped output of
	// the invocation, and the canonical artifact the next stage reads by path.
	runner.captureFile(ctx, run, stageID, invocationID, artifactDir, "result.json", "result_json")
	if result == nil {
		return
	}
	for _, declared := range result.Artifacts {
		// Agent-declared paths may be relative to the worktree root or
		// absolute within it. Resolve relative to the worktree root so we read
		// what the agent actually wrote, not the artifact-dir.
		path := declared.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(run.worktree.Root, path)
		}
		// The name in the revisions index is the worktree-relative path. Use
		// the agent-declared Path verbatim — that is the contract-shaped name
		// the next stage will look up.
		name := strings.TrimPrefix(declared.Path, "/")
		kind := declared.Kind
		if kind == "" {
			kind = "file"
		}
		runner.captureFilePath(ctx, run, stageID, invocationID, path, name, kind)
	}
}

// captureFile reads artifactDir/name and ingests it. Used for result.json and
// for any other well-known per-stage output that lives under artifactDir.
func (runner *Runner) captureFile(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, artifactDir, name, kind string,
) {
	fullPath := filepath.Join(artifactDir, name)
	revisionName := stageID + "/" + name
	runner.captureFilePath(ctx, run, stageID, invocationID, fullPath, revisionName, kind)
}

// captureFilePath reads the file at fullPath and ingests it under revisionName.
// Missing file is logged and skipped — the agent may have declared a target it
// did not end up writing, and that is a contract gap the manifest records.
func (runner *Runner) captureFilePath(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, fullPath, revisionName, kind string,
) {
	bytes, err := os.ReadFile(fullPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			runner.log.Warn("capture artifact: read", "task", run.task.ID, "name", revisionName, "error", err)
		}
		return
	}
	if _, putErr := runner.art.Put(ctx, artifacts.PutParams{
		TenantID: run.task.TenantID,
		UserID:   run.task.UserID,
		TaskID:   run.task.ID,
		Name:     revisionName,
		Kind:     kind,
		Bytes:    bytes,
		Source:   invocationID,
		Actor:    artifacts.ActorAgent,
	}); putErr != nil {
		runner.log.Warn("capture artifact: put", "task", run.task.ID, "name", revisionName, "error", putErr)
	}
}

// recordStageEvidence adds the prompt + model + adapter + effective-capability
// evidence for one stage to the manifest. Called after a successful stage
// invocation. No-op when the manifest service is nil (unit tests).
func (runner *Runner) recordStageEvidence(
	ctx context.Context,
	run stageRun,
	stageID string,
	stage pack.Stage,
	model string,
	profile caps.Profile,
) {
	if runner.mfst == nil {
		return
	}
	promptHash := hashForEvidence(stage.PromptText())
	tier := stage.Tier
	if tier == "" {
		tier = run.taskPack.Tiers.Default
	}
	stageProfileJSON, _ := json.Marshal(profile)
	patch := manifest.Body{
		Prompts: []manifest.PromptRevision{{StageID: stageID, Hash: promptHash}},
		Model: &manifest.ModelEvidence{
			Tier: tier, Model: model, AgentName: runner.agentName,
		},
		Adapter: runner.adapterEvidence(),
		Capabilities: &manifest.CapabilityProfile{
			Effective: []manifest.StageCapabilityProfile{{
				Stage:   stageID,
				Role:    string(profile.Source.Role),
				Profile: stageProfileJSON,
			}},
		},
	}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		// Sealed manifest is unexpected mid-run; logged but not fatal — the
		// run continues and the missing evidence shows up under the
		// manifest's Missing section.
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("record evidence: manifest sealed", "task", run.task.ID)
			return
		}
		runner.log.Warn("record evidence", "task", run.task.ID, "error", err)
	}
}

// adapterEvidence returns the adapter section for the manifest. Capabilities
// are inert (declared = passed at MVP); Epic 6 grows this section.
func (runner *Runner) adapterEvidence() *manifest.AdapterEvidence {
	return &manifest.AdapterEvidence{
		Name:    runner.agentName,
		Version: runner.adapterV,
	}
}

// recordInitialEvidence seeds the manifest with the task / project / pack /
// git lineage evidence at run start. Idempotent — AddEvidence merges by
// section. Called once at the start of drive(). No-op when the manifest
// service is nil.
func (runner *Runner) recordInitialEvidence(
	ctx context.Context,
	task sqlc.Task,
	project sqlc.Project,
	taskPack *pack.Pack,
) {
	if runner.mfst == nil {
		return
	}
	packHash := ""
	if taskPack.Dir != "" {
		// Best-effort hash of the resolved pack. Empty when the pack was built
		// in memory (override resolver) — the manifest's Missing section
		// records the gap if it matters.
		if hash, err := hashDir(taskPack.Dir); err == nil {
			packHash = hash
		}
	}
	missing := []string{
		"memory", // Epic 1 — project memory not wired yet
	}
	patch := manifest.Body{
		Input: &manifest.InputEvidence{
			TaskID:      task.ID,
			Title:       task.Title,
			Input:       task.Input,
			Revision:    hashForEvidenceBytes(task.Input),
			PipelineRef: task.PipelinePack,
		},
		Project: &manifest.ProjectEvidence{
			ProjectID:  project.ID,
			RepoPath:   project.RepoPath,
			Name:       project.Name,
			BaseRef:    task.BaseRef,
			BaseCommit: nullStringOr(task.BaseCommit),
		},
		Pack: &manifest.PackEvidence{
			Ref:         taskPack.BaseRef,
			Name:        taskPack.Pack.Name,
			Version:     taskPack.Pack.Version,
			ContentHash: packHash,
			Forked:      taskPack.Forked,
		},
		Capabilities: &manifest.CapabilityProfile{
			Declared: taskPack.Capabilities,
		},
		Missing: missing,
		Adapter: runner.adapterEvidence(),
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		runner.log.Warn("record initial evidence", "task", task.ID, "error", err)
	}
}

// recordGitEvidence adds the current git lineage (branch, base_commit, latest
// checkpoint, result_commit when known) to the manifest. Called at boundaries
// (base resolve, post-stage checkpoint, terminal teardown). No-op when the
// manifest service is nil.

// recordInstructionsEvidence records the project instruction sources the run
// consumed: the repository's AGENTS.md working agreement, read from the task's
// base_commit (agent-immutable, like the checks registry — an agent editing
// AGENTS.md in its worktree cannot retcon the instructions that shaped its run).
// A missing AGENTS.md is recorded as Present=false so the manifest surfaces the
// gap rather than hiding it. No-op when the manifest service is nil.
func (runner *Runner) recordInstructionsEvidence(ctx context.Context, task sqlc.Task, repoPath string) {
	if runner.mfst == nil {
		return
	}
	agentsRef := manifest.InstructionRef{Path: agentsMDPath}
	baseCommit := task.BaseCommit.String
	if task.BaseCommit.Valid && baseCommit != "" {
		if raw, err := runner.wt.FileAtCommit(ctx, repoPath, baseCommit, agentsMDPath); err == nil {
			agentsRef.Present = true
			agentsRef.Hash = hashForEvidenceBytes(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			runner.log.Warn("read AGENTS.md for manifest", "task", task.ID, "error", err)
		}
	}
	patch := manifest.Body{Instructions: &manifest.InstructionsEvidence{AgentsMD: agentsRef}}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record instructions evidence", "task", task.ID, "error", err)
	}
}

// agentsMDPath is the repository-relative path to the build-side working
// agreement the pack prompts instruct agents to follow.
const agentsMDPath = "AGENTS.md"

func (runner *Runner) recordGitEvidence(ctx context.Context, task sqlc.Task) {
	if runner.mfst == nil {
		return
	}
	checkpoints, err := runner.store.ListCheckpointsForTask(ctx, sqlc.ListCheckpointsForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		runner.log.Warn("list checkpoints for manifest", "task", task.ID, "error", err)
		return
	}
	manifestCheckpoints := make([]manifest.CheckpointRef, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		manifestCheckpoints = append(manifestCheckpoints, manifest.CheckpointRef{
			Label: checkpoint.Label, Commit: checkpoint.CommitSha,
		})
	}
	patch := manifest.Body{
		Git: &manifest.GitEvidence{
			Branch:       worktreeBranchFor(task.ID),
			BaseCommit:   nullStringOr(task.BaseCommit),
			ResultCommit: nullStringOr(task.ResultCommit),
			Checkpoints:  manifestCheckpoints,
		},
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		// Sealed manifest means the terminal seal already ran; subsequent
		// git evidence is recorded via Correct, not AddEvidence. Logged at
		// debug since the seal path handles this.
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record git evidence", "task", task.ID, "error", err)
	}
}

// sealManifestAtTerminal finalizes the manifest when the task reaches a
// terminal state. The reason maps the terminal state to a seal reason for the
// audit trail. No-op when the manifest service is nil.
func (runner *Runner) sealManifestAtTerminal(ctx context.Context, task sqlc.Task) {
	if runner.mfst == nil {
		return
	}
	// Final git evidence flush before sealing — result_commit may have been
	// recorded between the last stage and the teardown job.
	runner.recordGitEvidence(ctx, task)
	reason := manifest.SealCompleted
	switch engine.TaskState(task.State) {
	case engine.StateCancelled:
		reason = manifest.SealCancelled
	case engine.StateFailed:
		reason = manifest.SealFailed
	}
	if err := runner.mfst.Seal(ctx, task.TenantID, task.UserID, task.ID, reason); err != nil {
		runner.log.Warn("seal manifest", "task", task.ID, "state", task.State, "error", err)
	}
}

// syncRevisionsIntoWorktree materializes the current revisions of artifacts
// the upcoming stage will read back into the worktree before the agent runs.
// Used on resume / advance to honor "the chosen revision syncs into the
// agent's working environment." No-op when the syncer is nil.
func (runner *Runner) syncRevisionsIntoWorktree(ctx context.Context, run stageRun, stageID string) {
	if runner.syncer == nil {
		return
	}
	currentRevisions, err := runner.currentRevisionList(ctx, run.task.TenantID, run.task.ID)
	if err != nil {
		runner.log.Warn("sync revisions: list current", "task", run.task.ID, "error", err)
		return
	}
	if len(currentRevisions) == 0 {
		return
	}
	targets := make([]artifacts.SyncTarget, 0, len(currentRevisions))
	for _, revision := range currentRevisions {
		// Skip the per-stage result_json blobs — those live under
		// .agentum/<task>/.ag-artifacts/<stage>/, not the worktree proper. The
		// agent reads them by path from the artifact dir; we should not write
		// them into the worktree at the top level.
		if revision.Kind == "result_json" {
			continue
		}
		// The revision name is the in-tree path (the agent wrote it there in a
		// prior stage). Materialize at the same path so the next stage reads
		// the same content.
		targetPath := filepath.Join(run.worktree.Root, revision.Name)
		targets = append(targets, artifacts.SyncTarget{
			Path: targetPath, Name: revision.Name,
		})
	}
	if len(targets) == 0 {
		return
	}
	results, err := runner.syncer.Sync(ctx, run.task.TenantID, run.task.ID, run.worktree.Root, targets)
	if err != nil {
		runner.log.Warn("sync revisions into worktree", "task", run.task.ID, "error", err)
		return
	}
	synced := 0
	for _, result := range results {
		if !result.Skipped {
			synced++
		}
	}
	if synced > 0 {
		runner.emit(ctx, run.task, EvRevisionsSynced, map[string]any{
			"stage": stageID, "synced": synced,
		})
	}
}

// currentRevisionList is the bridge between the artifacts Store and the
// runner's sync helper. Returns an empty slice when the store is nil.
func (runner *Runner) currentRevisionList(ctx context.Context, tenantID, taskID string) ([]artifacts.Revision, error) {
	if runner.art == nil {
		return nil, nil
	}
	return runner.art.ListCurrent(ctx, tenantID, taskID)
}

// hashForEvidence returns the sha256 hex of the bytes of the prompt text.
// Centralized so two callers (recordStageEvidence, the manifest diff) agree on
// the canonical hashing scheme.
func hashForEvidence(text string) string {
	return hashForEvidenceBytes([]byte(text))
}

func hashForEvidenceBytes(bytes []byte) string {
	if len(bytes) == 0 {
		return ""
	}
	sum := sha256.Sum256(bytes)
	return hex.EncodeToString(sum[:])
}

// hashDir returns a deterministic hash of a directory's contents (file paths
// + file bytes, sorted by path). Used to content-hash a resolved pack dir so
// two runs against the same pack hash the same value, and a pack edit is
// detectable.
func hashDir(dir string) (string, error) {
	hasher := sha256.New()
	files := make([]string, 0)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk pack dir %s: %w", dir, err)
	}
	for _, filePath := range sortStrings(files) {
		rel, relErr := filepath.Rel(dir, filePath)
		if relErr != nil {
			return "", fmt.Errorf("rel %s under %s: %w", filePath, dir, relErr)
		}
		hasher.Write([]byte(rel))
		hasher.Write([]byte{0})
		bytes, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return "", fmt.Errorf("read %s: %w", filePath, readErr)
		}
		hasher.Write(bytes)
		hasher.Write([]byte{0})
	}
	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum), nil
}

func sortStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	for index := 1; index < len(out); index++ {
		for back := index; back > 0 && out[back-1] > out[back]; back-- {
			out[back-1], out[back] = out[back], out[back-1]
		}
	}
	return out
}

// worktreeBranchFor is the canonical branch name for a task. Re-declared here
// (rather than importing worktree) so the manifest recording does not pull a
// new dependency cycle through the worktree package's tests. The string is
// identical to worktree.BranchFor.
func worktreeBranchFor(taskID string) string { return "agentum/" + taskID }

// nullStringOr returns the String value when Valid, else "". Lifted to the
// runner so the manifest / git evidence paths do not need their own helper.
func nullStringOr(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
