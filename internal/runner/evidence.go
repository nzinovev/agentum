package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
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
	// Capture the pack-declared approval artifact (plan.md) when this stage is
	// the approval stage (ADR 0003 D2). The path is orchestrator-constructed
	// (inside the stage's artifact dir, next to result.json), so it needs no
	// containment check — the same exemption verdict.json relies on. The kind
	// plan_md is what the sync redirect and the drift check key on.
	if run.taskPack != nil {
		if approval, hasApproval := run.taskPack.SourceWriteApproval(); hasApproval && approval.Stage == stageID {
			outputs = runner.captureFile(ctx, run, stageID, invocationID, artifactDir, approval.Artifact, "plan_md", outputs)
		}
	}
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
	revision, stored := runner.ingest(ctx, run, stageID, invocationID, revisionName, kind, bytes, artifacts.ActorAgent)
	if !stored {
		return outputs
	}
	return append(outputs, manifest.ArtifactRef{
		Name:         revisionName,
		Kind:         kind,
		RevisionID:   revision.ID,
		ContentHash:  revision.ContentHash,
		Stage:        stageID,
		InvocationID: invocationID,
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
	revision, stored := runner.ingest(ctx, run, stageID, invocationID, revisionName, kind, bytes, artifacts.ActorAgent)
	if !stored {
		return outputs
	}
	return append(outputs, manifest.ArtifactRef{
		Name:         revisionName,
		Kind:         kind,
		RevisionID:   revision.ID,
		ContentHash:  revision.ContentHash,
		Stage:        stageID,
		InvocationID: invocationID,
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
	actor artifacts.Actor,
) (artifacts.Revision, bool) {
	revision, putErr := runner.art.Put(ctx, artifacts.PutParams{
		TenantID: run.task.TenantID,
		UserID:   run.task.UserID,
		TaskID:   run.task.ID,
		Name:     revisionName,
		Kind:     kind,
		Bytes:    bytes,
		Source:   invocationID,
		Actor:    actor,
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

// openInvocationEvidence writes the OPEN half of one attempt's manifest record
// (ADR 0005 D7), immediately after the stage_invocation row is created and
// before adapter.Invoke: invocation id, stage coordinates, adapter id + both
// versions (probed, memoized), the model selection, both prompt hashes, and
// the effective capability profile with its role. A crash, timeout, or refused
// start after this point leaves a record of what the attempt was going to run.
// No-op when the manifest service is nil (unit tests).
func (runner *Runner) openInvocationEvidence(
	ctx context.Context,
	run stageRun,
	invocation sqlc.StageInvocation,
	stageID string,
	stage pack.Stage,
	selection models.Selection,
	routingBlock string,
	profile caps.Profile,
) {
	if runner.mfst == nil {
		return
	}
	descriptor := runner.adapter.Describe()
	readiness := runner.adapter.Probe(ctx)
	record := manifest.InvocationEvidence{
		InvocationID: invocation.ID,
		Stage:        stageID,
		Sequence:     invocation.Sequence,
		Cycle:        invocation.Cycle,
		Adapter: manifest.InvocationAdapter{
			ID:             manifest.AdapterID(descriptor.ID),
			AdapterVersion: descriptor.AdapterVersion,
			RuntimeVersion: readiness.RuntimeVersion,
		},
		Model: selection,
		Prompt: manifest.InvocationPrompt{
			StagePromptHash: hashForEvidence(stage.PromptText()),
			// Exactly the bytes the adapter hands the runtime (opencode.go
			// concatenates prompt + "\n\n" + routing block).
			RenderedHash: hashForEvidence(stage.PromptText() + "\n\n" + routingBlock),
		},
		Capabilities: manifest.InvocationCaps{
			Role:    string(profile.Source.Role),
			Profile: marshalProfile(profile),
		},
	}
	patch := manifest.Body{Invocations: []manifest.InvocationEvidence{record}}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("open invocation evidence: manifest sealed", "task", run.task.ID)
			return
		}
		runner.log.Warn("open invocation evidence", "task", run.task.ID, "stage", stageID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "invocations", stageID, err)
	}
}

// closeInvocationEvidence writes the CLOSE half of one attempt's record (ADR
// 0005 D7): telemetry and the stop reason, filled into the record the open
// pass created (matched on invocation id; zero fields leave the open values
// intact). telemetry is nil for a refused start — nothing ran, nothing to
// bill. Called on EVERY terminal path: success, adapter error, parse error,
// artifact rejection, refused start.
func (runner *Runner) closeInvocationEvidence(
	ctx context.Context,
	task sqlc.Task,
	invocationID, stopReason string,
	telemetry *agent.Telemetry,
) {
	if runner.mfst == nil {
		return
	}
	record := manifest.InvocationEvidence{
		InvocationID: invocationID,
		StopReason:   stopReason,
		Telemetry:    invocationTelemetry(telemetry),
	}
	patch := manifest.Body{Invocations: []manifest.InvocationEvidence{record}}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("close invocation evidence: manifest sealed", "task", task.ID)
			return
		}
		runner.log.Warn("close invocation evidence", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "invocations", "", err)
	}
}

// invocationTelemetry maps the adapter's accumulated cost onto the manifest
// shape, or returns nil when there is nothing measured. nil and a zero value
// are NOT interchangeable here: the adapter reports its accumulated cost only
// on the terminal EventResult, so an attempt that was refused or died
// mid-stream has no measurement — and a zero-valued object would read as
// "this attempt was free", not as "unknown".
func invocationTelemetry(telemetry *agent.Telemetry) *manifest.InvocationTelemetry {
	if telemetry == nil {
		return nil
	}
	return &manifest.InvocationTelemetry{
		Tokens: manifest.TokenUsage{
			Total:      telemetry.Tokens.Total,
			Input:      telemetry.Tokens.Input,
			Output:     telemetry.Tokens.Output,
			Reasoning:  telemetry.Tokens.Reasoning,
			CacheRead:  telemetry.Tokens.CacheRead,
			CacheWrite: telemetry.Tokens.CacheWrite,
		},
		Cost: telemetry.Cost,
	}
}

// completeStageEvidence writes everything a SUCCESSFUL attempt leaves behind,
// in ONE manifest transaction: the CLOSE half of the invocation record
// (telemetry; the stop reason is empty on this path), the artifact revisions
// the stage captured, and the ADR 0002 project-context section.
//
// One write rather than three because AddEvidence is a full-document
// read-modify-write under the manifest row's lock — it decodes the whole body,
// merges, and writes the whole body back — so each extra call re-encodes an
// evidence document that grows for the life of the run. It is also atomic:
// three separate writes could leave the attempt closed with its artifacts
// missing, which reads as a stage that produced nothing.
//
// The OPEN half stays its own write, before the adapter starts, because that
// is what it is for: a crash between open and close must leave the record of
// what the attempt was going to run (ADR 0005 D7).
//
// No-op when the manifest service is nil (unit tests).
func (runner *Runner) completeStageEvidence(
	ctx context.Context,
	run stageRun,
	stageID, invocationID string,
	telemetry agent.Telemetry,
	artifactOutputs []manifest.ArtifactRef,
) {
	if runner.mfst == nil {
		return
	}
	patch := manifest.Body{
		Invocations: []manifest.InvocationEvidence{{
			InvocationID: invocationID,
			Telemetry:    invocationTelemetry(&telemetry),
		}},
		Context: contextEvidenceSection(run),
	}
	// The sections this one write covers, so a failure records a gap against
	// each of them: a reviewer asks "is the artifacts section trustworthy",
	// not "which call failed", and a single compound section name would not
	// answer that question.
	sections := []string{"invocations", "context"}
	if len(artifactOutputs) > 0 {
		patch.Artifacts = &manifest.ArtifactEvidence{Outputs: artifactOutputs}
		sections = append(sections, "artifacts")
	}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		// Sealed manifest is unexpected mid-run; logged but not fatal — the
		// run continues and the gap is recorded in the manifest's evidence
		// gaps at seal time.
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("complete stage evidence: manifest sealed", "task", run.task.ID)
			return
		}
		runner.log.Warn("complete stage evidence", "task", run.task.ID, "stage", stageID, "error", err)
		for _, section := range sections {
			runner.recordEvidenceGap(ctx, run.task, section, stageID, err)
		}
		return
	}
	// A failed skill probe is an evidence gap (this run's context evidence is
	// degraded — we do not know what knowledge was in play). An unsupported
	// probe is not: it is a permanent capability gap recorded in the section.
	if probeFailed(run.contextReport.SkillsProbe) {
		runner.recordEvidenceGap(ctx, run.task, "context.skills", stageID,
			fmt.Errorf("skill probe failed: %s", run.contextReport.SkillsError))
	}
}

// adapterEvidence returns the run-level adapter section: the wiring of the
// process that drove the run (ADR 0005 D6) — id, OUR adapter implementation's
// version, the capability categories it declares, and the readiness probe
// outcome. The runtime VERSION is per invocation, not here: a run resumed in
// a new process after an upgrade genuinely has two.
func (runner *Runner) adapterEvidence(ctx context.Context) *manifest.AdapterEvidence {
	descriptor := runner.adapter.Describe()
	readiness := runner.adapter.Probe(ctx)
	declared := runner.adapter.Supported()
	declaredNames := make([]string, 0, len(declared))
	for _, category := range declared {
		declaredNames = append(declaredNames, string(category))
	}
	return &manifest.AdapterEvidence{
		ID:                   manifest.AdapterID(descriptor.ID),
		AdapterVersion:       descriptor.AdapterVersion,
		DeclaredCapabilities: declaredNames,
		RuntimeProbe:         readiness.Label(),
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
	// The revision is the canonical hash of the typed request (ADR 0004 D9),
	// computed from the parsed value so a backfilled or reformatted overrides
	// column cannot perturb it. A malformed column is an invariant break (the
	// API guarantees well-formed overrides): fail the provenance root rather
	// than record a revision nobody can reproduce.
	taskOverrides, overridesErr := taskinput.ParseOverrides(task.Overrides)
	if overridesErr != nil {
		return fmt.Errorf("record initial evidence: parse task overrides: %w", overridesErr)
	}
	taskRequest := taskinput.Request{
		Title: task.Title, Description: task.Description, Overrides: taskOverrides,
	}
	patch := manifest.Body{
		Input: &manifest.InputEvidence{
			TaskID:      task.ID,
			Title:       task.Title,
			Description: task.Description,
			Overrides:   task.Overrides,
			Revision:    taskRequest.Revision(),
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
		Adapter: runner.adapterEvidence(ctx),
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		return fmt.Errorf("record initial evidence: %w", err)
	}
	// A failed readiness probe is an evidence gap, mirroring the skills probe
	// (ADR 0005 D2): the run records "runtime not ready, because …" and the
	// invocation that needs the runtime surfaces the failure itself.
	if readiness := runner.adapter.Probe(ctx); !readiness.Ready {
		runner.recordEvidenceGap(ctx, task, "adapter.runtime", "",
			fmt.Errorf("runtime probe failed: %s", readiness.Reason))
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
		// ADR 0003 D4: a reject fires EventCancel (→ cancelled) but records a
		// task_approvals row with decision="rejected". Distinguish the two so a
		// sealed record does not describe a rejected result as an undifferentiated
		// abort. Read the approval rows; a rejected decision flips the seal reason.
		if approvals, apErr := runner.store.ListApprovalsForTask(ctx, sqlc.ListApprovalsForTaskParams{
			TaskID: task.ID, TenantID: task.TenantID,
		}); apErr == nil {
			for _, approval := range approvals {
				if approval.Decision == "rejected" {
					reason = manifest.SealRejected
					break
				}
			}
		}
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
		// Artifact-dir-resident kinds (result_json, verdict_json, plan_md,
		// diff, diff_stat) live under .agentum/<task>/.ag-artifacts/<stage>/,
		// not the worktree proper. ADR 0003 D6.2: instead of skipping them,
		// materialize them at their artifact-dir path. The revision name is
		// "<stage>/<file>", so the destination is the stage's artifact dir +
		// the file. This is what makes a human edit to plan/plan.md reach the
		// implementer after an advance: the edit lands as a new revision, and
		// the next drive() materializes the NEW bytes into the plan stage's
		// artifact dir, replacing the stale file the planner left on disk.
		// Without it the plan gate is broken end to end.
		//
		// The write still goes through the artifacts.Container rooted at the
		// worktree; the artifact dir is inside the worktree, so containment is
		// unchanged. The .agentum/ tree is in the repo's local excludes, so
		// nothing here enters the delivery diff.
		targetPath := worktreeArtifactPath(run, revision.Name)
		if isArtifactDirKind(revision.Kind) && targetPath != "" {
			targets = append(targets, artifacts.SyncTarget{
				Path: targetPath, Name: revision.Name,
			})
			revisionByName[revision.Name] = revision
			continue
		}
		if isArtifactDirKind(revision.Kind) {
			// An artifact-dir kind whose name we could not split into
			// <stage>/<file> — defensively skipped rather than written to the
			// worktree proper (which would put <stage>/verdict.json into the
			// delivery tree, the original reason for the skip).
			continue
		}
		// The revision name is the in-tree path (the agent wrote it there in a
		// prior stage). Materialize at the same path so the next stage reads
		// the same content.
		targetPath = filepath.Join(run.worktree.Root, revision.Name)
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

// isArtifactDirKind reports whether a revision kind lives in the per-stage
// artifact dir rather than the worktree proper. Such revisions have names of
// the form "<stage>/<file>" and are materialized at the artifact-dir path on
// sync (ADR 0003 D6.2) instead of being skipped — a human edit to plan/plan.md
// must reach the implementer after an advance, which the old skip prevented.
func isArtifactDirKind(kind string) bool {
	switch kind {
	case "result_json", "verdict_json", "plan_md", "diff", "diff_stat":
		return true
	}
	return false
}

// worktreeArtifactPath returns the absolute path inside the worktree where an
// artifact-dir-resident revision should be materialized, or "" if the revision
// name is not "<stage>/<file>". The destination is the stage's artifact dir +
// the file name; the artifact dir is inside the worktree, so containment is
// unchanged and the .agentum/ local excludes keep it out of the delivery diff.
func worktreeArtifactPath(run stageRun, revisionName string) string {
	stage, file, split := splitArtifactRevisionName(revisionName)
	if !split {
		return ""
	}
	return filepath.Join(worktree.ArtifactDir(run.worktree.Root, run.task.ID, stage), file)
}

// splitArtifactRevisionName splits "<stage>/<file>" into its two parts. Returns
// ok=false when the name has no slash or has an empty stage/file — those are not
// artifact-dir revisions and must not be redirected.
func splitArtifactRevisionName(name string) (stage, file string, ok bool) {
	idx := strings.IndexByte(name, '/')
	if idx <= 0 || idx == len(name)-1 {
		return "", "", false
	}
	// Reject names with a second slash (e.g. "a/b/c") — those are not the
	// "<stage>/<file>" shape the artifact dir redirect expects.
	remainder := name[idx+1:]
	if strings.Contains(remainder, "/") {
		return "", "", false
	}
	return name[:idx], remainder, true
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
// Centralized so every caller — both prompt hashes on the invocation record,
// and the instruction/skill hashes in the context section — agrees on the
// canonical hashing scheme.
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
