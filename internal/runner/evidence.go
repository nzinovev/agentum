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
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// ErrArtifactEscapesWorktree is returned by captureStageOutputs when the agent
// declared an artifact path that resolves outside its own worktree. It is a
// distinct terminal outcome, not a warning: see captureStageOutputs.
var ErrArtifactEscapesWorktree = errors.New("runner: declared artifact escapes the worktree")

// captureStageOutputs reads the artifacts the agent declared in result.json,
// plus result.json itself, and ingests each into the durable revisions store.
// Each captured artifact becomes a new immutable revision chained to the prior
// current revision of its (task, name). The source invocation id is recorded so
// the manifest can resolve "what did this invocation produce."
//
// Two failure modes, deliberately different:
//
//   - A declared file that was never written is a contract gap. Logged and
//     skipped; the manifest's "missing" section records it and the run goes on.
//   - A declared path that resolves outside the worktree is a containment
//     breach, and it fails the stage. The path in result.json is untrusted
//     input, and the orchestrator reads it with its own privileges — honouring
//     "/etc/passwd" or a link the agent planted itself would copy host files
//     into a durable, API-readable evidence store. Nothing is read, the whole
//     capture aborts, and the caller pauses the task for review rather than
//     recording a success built on output it refused to accept.
func (runner *Runner) captureStageOutputs(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, artifactDir string,
	result *agent.ResultJSON,
) ([]manifest.ArtifactRef, error) {
	if runner.art == nil {
		return nil, nil
	}
	container, err := artifacts.OpenContainer(run.worktree.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrArtifactEscapesWorktree, err)
	}
	defer func() { _ = container.Close() }()

	// The declared paths are validated up front, before anything is ingested:
	// a breach must not leave half the stage's output in the store.
	declared, err := runner.resolveDeclaredArtifacts(ctx, container, run, stageID, result)
	if err != nil {
		return nil, err
	}
	// One ref per revision this stage actually stored, accumulated so the
	// caller can fold them into the per-stage manifest write. A refused or
	// skipped ingest contributes no ref — the manifest records only revisions
	// that exist in the store, never a reference to bytes that were not kept.
	outputs := make([]manifest.ArtifactRef, 0)
	// Always capture result.json itself — it is the contract-shaped output of
	// the invocation, and the canonical artifact the next stage reads by path.
	// The artifact dir is orchestrator-constructed, so it needs no containment
	// check of its own.
	outputs = runner.captureFile(ctx, run, stageID, invocationID, artifactDir, "result.json", "result_json", outputs)
	// Capture verdict.json (if the reviewer wrote one) with kind verdict_json.
	// The path is orchestrator-constructed (next to result.json), so it needs no
	// containment check. This is what makes the advance path work after a
	// worktree restore or a worker restart: the verdict is read from the store,
	// not from a file that may have been reset by Restore.
	outputs = runner.captureFile(ctx, run, stageID, invocationID, artifactDir, agent.VerdictFileName, "verdict_json", outputs)
	for _, artifact := range declared {
		bytes, readErr := container.ReadFile(artifact.name)
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				runner.log.Warn("capture artifact: read",
					"task", run.task.ID, "name", artifact.name, "error", readErr)
			}
			continue
		}
		outputs = runner.captureIngest(ctx, run, stageID, invocationID, artifact.name, artifact.kind, bytes, outputs)
	}
	return outputs, nil
}

// captureIngest ingests one agent-declared artifact and, when the store kept
// it, appends an ArtifactRef to outputs. The ref carries the revision id and
// content hash the store returned, so the manifest references exactly the
// revision that holds the agent's bytes rather than a name-only pointer.
func (runner *Runner) captureIngest(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, revisionName, kind string,
	bytes []byte,
	outputs []manifest.ArtifactRef,
) []manifest.ArtifactRef {
	revision, stored := runner.ingest(ctx, run, stageID, invocationID, revisionName, kind, bytes)
	if !stored {
		return outputs
	}
	return append(outputs, manifest.ArtifactRef{
		Name:        revisionName,
		Kind:        kind,
		RevisionID:  revision.ID,
		ContentHash: revision.ContentHash,
		Stage:       stageID,
	})
}

// declaredArtifact is one agent-declared artifact after containment checking:
// the worktree-relative name it is read and indexed under, plus its kind.
type declaredArtifact struct {
	name string
	kind string
}

// resolveDeclaredArtifacts validates every path the agent declared against the
// worktree container. Returns ErrArtifactEscapesWorktree on the first path that
// leaves the worktree — all-or-nothing, so a breach cannot partially land.
func (runner *Runner) resolveDeclaredArtifacts(
	ctx context.Context,
	container *artifacts.Container,
	run stageRun,
	stageID string,
	result *agent.ResultJSON,
) ([]declaredArtifact, error) {
	if result == nil {
		return nil, nil
	}
	resolved := make([]declaredArtifact, 0, len(result.Artifacts))
	for _, declared := range result.Artifacts {
		target, err := container.Resolve(declared.Path)
		if err != nil {
			// A malformed declaration (empty path, unreadable link) is refused
			// alongside an outright escape: both are output the orchestrator
			// cannot safely act on, and guessing at the intent is what let the
			// escape through in the first place.
			reason := "unresolvable"
			if errors.Is(err, artifacts.ErrPathEscapesRoot) {
				reason = "escapes_worktree"
			}
			runner.emit(ctx, run.task, EvArtifactRejected, map[string]any{
				"stage": stageID, "path": declared.Path, "reason": reason,
			})
			return nil, fmt.Errorf("%w: %q: %w", ErrArtifactEscapesWorktree, declared.Path, err)
		}
		kind := declared.Kind
		if kind == "" {
			kind = "file"
		}
		resolved = append(resolved, declaredArtifact{name: target.Name, kind: kind})
	}
	return resolved, nil
}

// captureFile reads artifactDir/name and ingests it under "<stage>/<name>".
// Used for result.json and for any other well-known per-stage output that lives
// under the orchestrator-constructed artifact dir — a trusted path, unlike the
// agent-declared ones, which go through the container. When the store kept the
// artifact, a ref is appended to outputs so the manifest records it.
func (runner *Runner) captureFile(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, artifactDir, name, kind string,
	outputs []manifest.ArtifactRef,
) []manifest.ArtifactRef {
	bytes, err := os.ReadFile(filepath.Join(artifactDir, name))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			runner.log.Warn("capture artifact: read", "task", run.task.ID, "name", name, "error", err)
		}
		return outputs
	}
	revisionName := stageID + "/" + name
	revision, stored := runner.ingest(ctx, run, stageID, invocationID, revisionName, kind, bytes)
	if !stored {
		return outputs
	}
	return append(outputs, manifest.ArtifactRef{
		Name:        revisionName,
		Kind:        kind,
		RevisionID:  revision.ID,
		ContentHash: revision.ContentHash,
		Stage:       stageID,
	})
}

// ingest writes one artifact into the durable revisions store and returns the
// revision that was stored plus whether anything was actually written.
//
// A Put the store refuses (a credential-shaped artifact, a revision conflict)
// is logged and skipped rather than failing the stage: unlike a containment
// breach, nothing was read that should not have been, and the gap is visible in
// the revisions list. The store's own log line names the rule that fired. The
// returned (zero, false) tells the caller that no revision reference should
// enter the manifest for this artifact — recording a ref to a revision that was
// never stored would point the manifest at bytes that do not exist.
//
// A no-op Put (identical content to the current revision) returns the existing
// row with stored=true, which is correct: the revision is real, the agent's
// output is referenced by it, and the manifest should carry that reference even
// though no new chain link was added.
func (runner *Runner) ingest(
	ctx context.Context,
	run stageRun,
	stageID, invocationID, revisionName, kind string,
	bytes []byte,
) (artifacts.Revision, bool) {
	revision, putErr := runner.art.Put(ctx, artifacts.PutParams{
		TenantID: run.task.TenantID,
		UserID:   run.task.UserID,
		TaskID:   run.task.ID,
		Name:     revisionName,
		Kind:     kind,
		Bytes:    bytes,
		Source:   invocationID,
		Actor:    artifacts.ActorAgent,
	})
	if putErr != nil {
		runner.log.Warn("capture artifact: put",
			"task", run.task.ID, "stage", stageID, "name", revisionName, "error", putErr)
		if errors.Is(putErr, artifacts.ErrSecretDetected) {
			// The operator configured reject-on-secret and the store enforced
			// it. Surface it on the event stream: a silently absent artifact is
			// indistinguishable from one the agent never wrote.
			runner.emit(ctx, run.task, EvArtifactRejected, map[string]any{
				"stage": stageID, "path": revisionName, "reason": "secret_detected",
			})
		}
		return artifacts.Revision{}, false
	}
	return revision, true
}

// recordStageEvidence adds the prompt + model + adapter + effective-capability
// + output-artifact evidence for one stage to the manifest. Called after a
// successful stage invocation. No-op when the manifest service is nil (unit
// tests). The artifact refs the stage captured are folded into this same patch
// rather than written in a second AddEvidence round-trip: one manifest write
// per stage keeps the per-stage evidence atomic (the prompt hash and the
// artifact revisions that evidence it describe the same invocation) and avoids
// doubling the write load.
func (runner *Runner) recordStageEvidence(
	ctx context.Context,
	run stageRun,
	stageID string,
	stage pack.Stage,
	model string,
	profile caps.Profile,
	artifactOutputs []manifest.ArtifactRef,
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
			PerStage: []manifest.StageModel{{
				Stage: stageID, Tier: tier, Model: model, AgentName: runner.agentName,
			}},
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
	if len(artifactOutputs) > 0 {
		patch.Artifacts = &manifest.ArtifactEvidence{Outputs: artifactOutputs}
	}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		// Sealed manifest is unexpected mid-run; logged but not fatal — the
		// run continues and the gap is recorded in the manifest's evidence
		// gaps at seal time.
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("record evidence: manifest sealed", "task", run.task.ID)
			return
		}
		runner.log.Warn("record evidence", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "prompts_model_capabilities", stageID, err)
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
// service is nil (unit tests). Unlike the per-stage evidence helpers, a
// failure here is returned: this is the run's provenance root (input, project,
// pack, base commit), and a run whose provenance was never recorded should
// fail rather than proceed — every later piece of evidence chains off this, so
// a silent gap at the root would orphan everything that follows.
func (runner *Runner) recordInitialEvidence(
	ctx context.Context,
	task sqlc.Task,
	project sqlc.Project,
	taskPack *pack.Pack,
) error {
	if runner.mfst == nil {
		return nil
	}
	packHash := ""
	if taskPack.Dir != "" {
		// Best-effort hash of the resolved pack. Empty when the pack was built
		// in memory (override resolver) — the derived `missing` at seal time
		// records the gap if it matters.
		if hash, err := hashDir(taskPack.Dir); err == nil {
			packHash = hash
		}
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
		Adapter: runner.adapterEvidence(),
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		return fmt.Errorf("record initial evidence: %w", err)
	}
	return nil
}

// recordGitEvidence adds the current git lineage (branch, base_commit, latest
// checkpoint, result_commit when known) to the manifest. Called at boundaries
// (base resolve, post-stage checkpoint, terminal teardown). No-op when the
// manifest service is nil.
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
		// git evidence is recorded via Correct, not AddEvidence. The gap is
		// not recorded in that case — the seal refused it, which is the
		// expected post-seal path, not a degraded write.
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record git evidence", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "git", "", err)
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
//
// The revisions actually materialized (not skipped) are recorded as the
// manifest's artifacts.inputs — the record of what the next stage was handed
// to read. This is the input-side counterpart of the output refs captured at
// stage completion.
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
	revisionByName := make(map[string]artifacts.Revision, len(currentRevisions))
	for _, revision := range currentRevisions {
		// Skip the per-stage result_json and verdict_json blobs — those live
		// under .agentum/<task>/.ag-artifacts/<stage>/, not the worktree proper.
		// The agent reads them by path from the artifact dir; writing them into
		// the worktree would put <stage>/verdict.json into the delivery tree.
		if revision.Kind == "result_json" || revision.Kind == "verdict_json" {
			continue
		}
		// The revision name is the in-tree path (the agent wrote it there in a
		// prior stage). Materialize at the same path so the next stage reads
		// the same content.
		targetPath := filepath.Join(run.worktree.Root, revision.Name)
		targets = append(targets, artifacts.SyncTarget{
			Path: targetPath, Name: revision.Name,
		})
		revisionByName[revision.Name] = revision
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
	inputs := make([]manifest.ArtifactRef, 0)
	for _, result := range results {
		if result.Skipped {
			continue
		}
		synced++
		revision, found := revisionByName[result.Target.Name]
		if !found {
			continue
		}
		inputs = append(inputs, manifest.ArtifactRef{
			Name:        revision.Name,
			Kind:        revision.Kind,
			RevisionID:  revision.ID,
			ContentHash: revision.ContentHash,
			Stage:       stageID,
		})
	}
	if synced > 0 {
		runner.emit(ctx, run.task, EvRevisionsSynced, map[string]any{
			"stage": stageID, "synced": synced,
		})
	}
	if len(inputs) > 0 {
		runner.recordArtifactInputs(ctx, run, stageID, inputs)
	}
}

// recordArtifactInputs records the artifact revisions materialized into the
// worktree as the manifest's artifacts.inputs. Best-effort: a sealed manifest
// (late sync after a crash-seal) is logged at debug and dropped; a real write
// failure is logged and recorded as an evidence gap at seal time. No-op when
// the manifest service is nil.
func (runner *Runner) recordArtifactInputs(ctx context.Context, run stageRun, stageID string, inputs []manifest.ArtifactRef) {
	if runner.mfst == nil {
		return
	}
	patch := manifest.Body{Artifacts: &manifest.ArtifactEvidence{Inputs: inputs}}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record artifact inputs evidence", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "artifacts", stageID, err)
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

// recordEvidenceGap records that an evidence write failed, so the fact is
// carried on the sealed manifest instead of swallowed. Best-effort: a failure
// to record the gap is logged and dropped — it does not recurse. A nil
// manifest service (unit tests) is a no-op. Sealed manifests refuse the gap,
// which is expected for the post-seal git-evidence flush and is dropped
// silently (the seal already froze the body).
func (runner *Runner) recordEvidenceGap(ctx context.Context, task sqlc.Task, section, stage string, cause error) {
	if runner.mfst == nil {
		return
	}
	gap := manifest.EvidenceGap{
		Section: section,
		Stage:   stage,
		Reason:  cause.Error(),
		At:      time.Now().UTC(),
	}
	if err := runner.mfst.RecordGap(ctx, task.TenantID, task.ID, gap); err != nil {
		runner.log.Warn("record evidence gap", "task", task.ID, "section", section, "cause", cause, "gap_error", err)
	}
}

// recordTransitionEvidence records one taken conditional transition in the
// manifest's transitions section (D7). Called at the resolution point so the
// branch is auditable even when the next stage never starts (e.g. budget
// exhaustion stops the run before the target runs). Best-effort and a no-op
// when the manifest service is nil (unit tests).
func (runner *Runner) recordTransitionEvidence(ctx context.Context, task sqlc.Task, record manifest.TransitionRecord) {
	if runner.mfst == nil {
		return
	}
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	patch := manifest.Body{Transitions: []manifest.TransitionRecord{record}}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record transition evidence", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "transitions", record.From, err)
	}
}

// recordStopEvidence records one controlled stop in the manifest's stops
// section (D7). Called from applyPauseDecision for EVERY pause — a deliberate
// widening of D7, so the manifest carries the full stop history (budget,
// verdict, gate, adapter_error, etc.), not just the budget/verdict ones.
// (Stage, Reason, Cycle) collapses repeats. Best-effort and a no-op when the
// manifest service is nil.
func (runner *Runner) recordStopEvidence(ctx context.Context, task sqlc.Task, record manifest.StopRecord) {
	if runner.mfst == nil {
		return
	}
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	patch := manifest.Body{Stops: []manifest.StopRecord{record}}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record stop evidence", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "stops", record.Stage, err)
	}
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
