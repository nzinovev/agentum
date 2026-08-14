package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/instructions"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// Store is the subset of sqlc.Queries the runner uses. Declaring it here decouples
// the loop from the generated layer and makes it unit-testable with a fake.
type Store interface {
	GetTask(ctx context.Context, arg sqlc.GetTaskParams) (sqlc.Task, error)
	GetProject(ctx context.Context, arg sqlc.GetProjectParams) (sqlc.Project, error)
	UpdateTaskState(ctx context.Context, arg sqlc.UpdateTaskStateParams) (sqlc.Task, error)
	UpdateTaskStage(ctx context.Context, arg sqlc.UpdateTaskStageParams) (sqlc.Task, error)
	SetBaseCommit(ctx context.Context, arg sqlc.SetBaseCommitParams) (sqlc.Task, error)
	SetResultCommit(ctx context.Context, arg sqlc.SetResultCommitParams) (sqlc.Task, error)
	CreateStageInvocation(ctx context.Context, arg sqlc.CreateStageInvocationParams) (sqlc.StageInvocation, error)
	FinishStageInvocation(ctx context.Context, arg sqlc.FinishStageInvocationParams) error
	LatestStageForTask(ctx context.Context, arg sqlc.LatestStageForTaskParams) (sqlc.StageInvocation, error)
	// MaxCycleForStages is the durable fix-cycle counter: the highest cycle any
	// of the given stages has reached for the task. Returns -1 when none has run
	// (the query's COALESCE sentinel); the runner maps -1 -> 0 entries.
	MaxCycleForStages(ctx context.Context, arg sqlc.MaxCycleForStagesParams) (int32, error)
	// ListStageInvocationsForTask backs GET /tasks/{id}/invocations and makes
	// "each attempt visible separately" checkable.
	ListStageInvocationsForTask(ctx context.Context, arg sqlc.ListStageInvocationsForTaskParams) ([]sqlc.StageInvocation, error)
	LatestCheckpointForTask(ctx context.Context, arg sqlc.LatestCheckpointForTaskParams) (sqlc.TaskCheckpoint, error)
	ListCheckpointsForTask(ctx context.Context, arg sqlc.ListCheckpointsForTaskParams) ([]sqlc.TaskCheckpoint, error)
	CreateCheckpoint(ctx context.Context, arg sqlc.CreateCheckpointParams) (sqlc.TaskCheckpoint, error)
	AppendEvent(ctx context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error)
	EnqueueJob(ctx context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error)
	// GetApproval reads one human-approval decision row by (tenant, task, name),
	// or returns sql.ErrNoRows when undecided (ADR 0003 D3). The runner uses it
	// to decide whether the source_write withholding applies.
	GetApproval(ctx context.Context, arg sqlc.GetApprovalParams) (sqlc.TaskApproval, error)
	// ListApprovalsForTask reads every approval decision for a task, oldest
	// first (ADR 0003). Used by sealManifestAtTerminal to distinguish a reject
	// (cancel fired from a human reject at a gate) from a plain cancel, and by
	// the final-review payload.
	ListApprovalsForTask(ctx context.Context, arg sqlc.ListApprovalsForTaskParams) ([]sqlc.TaskApproval, error)
	// CurrentArtifactRevisionForName is the durable read of the current revision
	// for a (task, name), used by the plan_revision_drift check. Shared with the
	// artifacts sync path.
	CurrentArtifactRevisionForName(ctx context.Context, arg sqlc.CurrentArtifactRevisionForNameParams) (sqlc.ArtifactRevision, error)
}

// Sink forwards a live stream chunk to subscribers (e.g. an in-memory SSE broker).
// nil chunks are drained and discarded; the runner accumulates telemetry either
// way. The durable event log carries only meaningful events (04 §7.1.5).
type Sink func(taskID, stageID, chunk string)

// Runner drives a task through its pack's stages. It implements the job
// worker's Handler: the worker claims a job and calls Handle, which runs the
// stage loop (04 §7.2) until a pause point or terminal state.
type Runner struct {
	store     Store
	packs     pack.Source
	adapter   agent.Adapter
	models    *models.Config // nil → built-in default for AgentName
	wt        *worktree.Manager
	cancels   *CancelRegistry
	sink      Sink
	agentName string
	adapterV  string // adapter binary version, best-effort; recorded in manifest
	log       *slog.Logger

	// hardTimeout / idleTimeout are the per-invocation caps the runner layers
	// onto every effective capability profile (zero = no cap). Sourced from
	// config; the profile carries them to the adapter, which wraps ctx with the
	// hard cap and watches the stream for the idle cap.
	hardTimeout time.Duration
	idleTimeout time.Duration

	// art is the immutable artifact revisions store. nil in unit tests that
	// don't exercise evidence capture; captureArtifacts is a no-op then.
	art artifacts.Store
	// syncer materializes current revisions back into the worktree on resume.
	// nil when art is nil.
	syncer *artifacts.Syncer
	// mfst is the evidence manifest service. nil in unit tests; manifest
	// operations are no-ops then. Typed as manifestService (a runner-local
	// interface) so unit tests can inject a fake without a database; the
	// production *manifest.Service satisfies it.
	mfst manifestService
	// checkExec runs the orchestrator-owned project checks at the delivery
	// boundary. nil in unit tests; project checks are skipped then.
	checkExec *checks.Executor
}

// manifestService is the subset of the manifest.Service surface the runner
// uses. Extracted as an interface so unit tests can substitute a fake that
// records calls or fails on demand; the production *manifest.Service satisfies
// it without changes.
type manifestService interface {
	AddEvidence(ctx context.Context, tenantID, taskID string, patch manifest.Body) error
	Seal(ctx context.Context, tenantID, userID, taskID string, reason manifest.SealReason) error
	RecordGap(ctx context.Context, tenantID, taskID string, gap manifest.EvidenceGap) error
	// ChecksCommit returns the commit the delivery checks verified
	// (body.checks.commit). Empty when no checks section was recorded. Used by
	// teardown to compare result_commit against the verified commit directly,
	// rather than a checkpoint proxy whose correctness depends on an FSM
	// property a future feature could break silently.
	ChecksCommit(ctx context.Context, tenantID, taskID string) (string, error)
}

// manifestServiceOrNil returns service as a manifestService, or nil when
// service is a nil *manifest.Service. This dodges the typed-nil-in-interface
// trap: assigning a nil *manifest.Service to an interface value yields a
// non-nil interface holding a nil pointer, so the `mfst == nil` guards the
// runner relies on would stop firing. The nil-pointer concrete type never
// reaches the interface here.
func manifestServiceOrNil(service *manifest.Service) manifestService {
	if service == nil {
		return nil
	}
	return service
}

// Deps bundles Runner construction. AgentName is the adapter's identity for
// model resolution (e.g. "opencode"); Models may be nil to use built-in defaults.
type Deps struct {
	Store     Store
	Packs     pack.Source
	Adapter   agent.Adapter
	Models    *models.Config
	Worktrees *worktree.Manager
	Cancels   *CancelRegistry
	Sink      Sink
	AgentName string
	// AdapterVersion is the adapter binary version, surfaced in the manifest
	// for cross-run comparison. Empty when unknown.
	AdapterVersion string
	Log            *slog.Logger

	// Artifacts is the durable artifact revisions store. May be nil in unit
	// tests; capture and sync become no-ops then.
	Artifacts artifacts.Store
	// Syncer materializes current revisions into the worktree on resume.
	// Required when Artifacts is set; the runner derives one if nil.
	Syncer *artifacts.Syncer
	// Manifest is the evidence manifest service. May be nil in unit tests;
	// manifest operations become no-ops then.
	Manifest *manifest.Service

	// CheckExec runs the orchestrator-owned project checks at the delivery
	// boundary. May be nil in unit tests; project checks are skipped then. When
	// set, the runner enforces the project baseline (plus pack/task requests)
	// after the final-stage checkpoint and before the task reaches the review
	// gate; a mandatory failure blocks delivery by failing the task.
	CheckExec *checks.Executor

	// HardTimeout / IdleTimeout are the per-invocation caps applied to every
	// stage invocation (zero = no cap). Sourced from config; carried by the
	// effective capability profile to the adapter.
	HardTimeout time.Duration
	IdleTimeout time.Duration
}

// New builds a Runner. Cancels/Worktrees/Log default to fresh instances.
func New(deps Deps) *Runner {
	cancels := deps.Cancels
	if cancels == nil {
		cancels = NewCancelRegistry()
	}
	worktreeManager := deps.Worktrees
	if worktreeManager == nil {
		worktreeManager = worktree.New()
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	syncer := deps.Syncer
	if syncer == nil {
		if sqlStore, ok := deps.Artifacts.(*artifacts.SQLStore); ok {
			syncer = artifacts.NewSyncer(sqlStore)
		}
	}
	return &Runner{
		store: deps.Store, packs: deps.Packs, adapter: deps.Adapter, models: deps.Models,
		wt: worktreeManager, cancels: cancels, sink: deps.Sink,
		agentName: deps.AgentName, adapterV: deps.AdapterVersion, log: log,
		art: deps.Artifacts, syncer: syncer, mfst: manifestServiceOrNil(deps.Manifest),
		checkExec:   deps.CheckExec,
		hardTimeout: deps.HardTimeout, idleTimeout: deps.IdleTimeout,
	}
}

// Cancels returns the runner's cancel registry, so the cancel HTTP handler can
// abort an in-flight run.
func (runner *Runner) Cancels() *CancelRegistry { return runner.cancels }

// Handle is the job-worker entry point. It dispatches by job kind; run /
// continue / advance all enter the shared stage loop from different entry
// points. cancel is a no-op here — the cancel HTTP handler aborts the active
// run via the registry and drives the FSM transition directly (04 §7.5).
func (runner *Runner) Handle(ctx context.Context, job sqlc.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch job.Kind {
	case "run", "continue", "advance":
		return runner.drive(ctx, job)
	case "teardown":
		return runner.teardown(ctx, job)
	case "cleanup":
		return runner.cleanup(ctx, job)
	case "cancel":
		return nil
	default:
		return fmt.Errorf("runner: unknown job kind %q", job.Kind)
	}
}

// teardown removes the task's worktree once it has reached a terminal state
// (done/cancelled/failed). F.6.1: the worktree is disposable, the branch is not
// — RemoveWorktree deletes the working tree only; agentum/<task-id> and any
// committed recovery/delivery work remain resolvable. Idempotent: a missing
// worktree is a no-op. Enqueued by the cancel/approve handlers and by failTask;
// the worker claims it after the driving run job is done, so it never races the
// runner (04 §7.1.3). Branch deletion is a separate explicit cleanup action.
//
// Before removing the worktree, teardown captures the agentum/<task-id> tip as
// result_commit — the immutable record of what was delivered (done) or recovered
// (cancelled/failed). The branch survives teardown, so result_commit is always
// resolvable after the fact; recording it here keeps the API free of git.
func (runner *Runner) teardown(ctx context.Context, job sqlc.Job) error {
	task, err := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: job.TaskID, TenantID: job.TenantID})
	if err != nil {
		return fmt.Errorf("teardown: load task: %w", err)
	}
	project, err := runner.store.GetProject(ctx, sqlc.GetProjectParams{ID: task.ProjectID, TenantID: task.TenantID})
	if err != nil {
		return fmt.Errorf("teardown: load project: %w", err)
	}
	runner.recordResultCommit(ctx, task, project.RepoPath)
	// ADR 0003 D8: result_commit is now pinned at the final gate (the commit the
	// human reviewed). Detect a divergence between the live branch tip and the
	// recorded result_commit: something moved the branch between review and
	// teardown. Same non-fatal treatment as the checks-commit divergence (gap +
	// event); the human already approved.
	runner.verifyResultCommitMatchesLiveTip(ctx, task, project.RepoPath)
	// Refresh task state (recordResultCommit may have set result_commit) and
	// detect any divergence between what was delivered and what the checks
	// verified, before sealing. The seal captures the final git lineage; a sealed
	// manifest is the immutable record a comparison / reproduction reads.
	updatedTask, refreshErr := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: task.ID, TenantID: task.TenantID})
	if refreshErr == nil {
		runner.verifyDeliveryCommitBinding(ctx, updatedTask)
		runner.sealManifestAtTerminal(ctx, updatedTask)
	} else {
		runner.log.Warn("teardown: reload task for seal", "task", task.ID, "error", refreshErr)
		runner.verifyDeliveryCommitBinding(ctx, task)
		runner.sealManifestAtTerminal(ctx, task)
	}
	if err := runner.wt.RemoveWorktree(ctx, project.RepoPath, task.ID); err != nil {
		runner.log.Error("teardown worktree", "task", task.ID, "error", err)
		return err
	}
	runner.emit(ctx, task, EvWorktreeRemoved, map[string]any{"stage": task.CurrentStage.String})
	return nil
}

// cleanup is the explicit, idempotent, audited deletion of a terminal task's
// delivery artifacts (F.6.1 AC #4). Triggered by POST /tasks/{id}/cleanup, it
// removes the agentum/<task-id> branch AND any lingering worktree (the latter
// idempotent — a task whose teardown already ran has only the branch left).
// Distinct from teardown (worktree-only at terminal state) and from cancel
// (terminal abort): cleanup is the operator saying "I am done with this
// delivery." Branch deletion is forced: a delivered task's commits are reviewed
// via result_commit / the branch before cleanup; -D is the intent.
func (runner *Runner) cleanup(ctx context.Context, job sqlc.Job) error {
	task, err := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: job.TaskID, TenantID: job.TenantID})
	if err != nil {
		return fmt.Errorf("cleanup: load task: %w", err)
	}
	project, err := runner.store.GetProject(ctx, sqlc.GetProjectParams{ID: task.ProjectID, TenantID: task.TenantID})
	if err != nil {
		return fmt.Errorf("cleanup: load project: %w", err)
	}
	// Remove any lingering worktree first (idempotent). A branch that is
	// checked out in a worktree cannot be deleted; clearing the worktree frees it.
	if err := runner.wt.RemoveWorktree(ctx, project.RepoPath, task.ID); err != nil {
		runner.log.Error("cleanup: remove worktree", "task", task.ID, "error", err)
		return err
	}
	if err := runner.wt.DeleteBranch(ctx, project.RepoPath, task.ID); err != nil {
		runner.log.Error("cleanup: delete branch", "task", task.ID, "error", err)
		return err
	}
	runner.emit(ctx, task, EvTaskCleanedUp, map[string]any{"branch": worktree.BranchFor(task.ID)})
	return nil
}

// recordResultCommit captures the agentum/<task-id> tip as result_commit if it
// is not already recorded and the branch is resolvable. Best-effort: a branch
// that is already gone (cleanup ran, or never created) is a no-op. result_commit
// remains queryable as the diff target against base_commit after teardown.
func (runner *Runner) recordResultCommit(ctx context.Context, task sqlc.Task, repoPath string) {
	if task.ResultCommit.Valid && task.ResultCommit.String != "" {
		return
	}
	tip, err := runner.wt.ResolveRef(ctx, repoPath, worktree.BranchFor(task.ID))
	if err != nil {
		// Branch not resolvable (never created, or already cleaned up). Nothing
		// to record — leave result_commit NULL rather than guessing.
		return
	}
	if _, err := runner.store.SetResultCommit(ctx, sqlc.SetResultCommitParams{
		ID: task.ID, TenantID: task.TenantID, ResultCommit: nullStr(tip),
	}); err != nil {
		runner.log.Warn("record result_commit", "task", task.ID, "error", err)
	}
}

// verifyDeliveryCommitBinding detects a divergence between the commit the
// delivery checks verified and the commit recorded as delivered (result_commit).
// The two are separated by a human approval: the checks run before the task
// reaches awaiting_final_review, teardown runs after approval, and in between a
// continue job, a human artifact edit, or a filesystem change can move the
// branch tip. Nothing previously compared the two, so the sealed manifest could
// assert "mandatory checks passed at X" alongside "delivered Y" with no signal
// they differ.
//
// On a mismatch this does NOT fail the task — the human already approved, and
// failing at teardown after approval would be a confusing terminal state.
// Instead the divergence is recorded as an EvidenceGap (so the sealed manifest
// carries it and evidence_complete reads false) and emitted as a distinct event
// (so it is visible on the stream). The sealed manifest's incompleteness is the
// signal a reviewer acts on. This is the cheaper of the two readings the plan
// considered; re-running checks inside teardown was rejected because it puts an
// arbitrarily long build on the terminal path and can fail after approval.
//
// The verified commit is read directly from body.checks.commit, not proxied
// through the latest checkpoint. The proxy (last post-stage checkpoint == the
// commit the checks verified) happens to hold today because the FSM has no path
// from awaiting_final_review back into a stage, but that property is nowhere
// asserted or tested, and a future ask-to-edit / add-context feature (Epic 2
// stubs in internal/api/stubs.go) would add exactly that path — at which point a
// post-checks stage would mint a new checkpoint, result_commit would match it,
// and the proxy comparison would fall silent in the very scenario it exists to
// catch. Reading the recorded value removes the hidden dependency.
//
// A comparison that cannot run is itself recorded as a gap. Skipping quietly
// would leave the manifest unable to distinguish "checked, no divergence" from
// "never checked" — the same fail-open shape the comparison was added to close.
//
// When the manifest service is nil (unit tests without a DB), the verified
// commit falls back to the latest checkpoint SHA. This is the degraded path:
// correct only while the FSM property above holds, and used solely so the
// teardown flow does not skip the check entirely in tests. The persisted
// result_commit is used (the refreshed task), not the value this teardown would
// have written — recordResultCommit is a no-op when result_commit is already
// set, so comparing against a not-yet-persisted tip would miss a divergence that
// a prior teardown already recorded.
func (runner *Runner) verifyDeliveryCommitBinding(ctx context.Context, task sqlc.Task) {
	if !task.ResultCommit.Valid || task.ResultCommit.String == "" {
		return
	}
	verifiedCommit, err := runner.checksVerifiedCommit(ctx, task)
	if err != nil {
		// The comparison could not run. Record that as a gap rather than
		// returning quietly: "the check found no divergence" and "the check
		// never happened" are different claims, and a manifest silent about
		// both is the fail-open shape this whole comparison exists to remove.
		runner.log.Warn("verify delivery binding: read verified commit", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "checks", "",
			fmt.Errorf("could not compare result_commit against the checks-verified commit: %w", err))
		return
	}
	if verifiedCommit == "" {
		// No recorded checks commit (e.g. a task that never reached delivery, or
		// the manifest service is nil and no checkpoint exists). Nothing to
		// compare against — an absence, not a failure.
		return
	}
	if task.ResultCommit.String == verifiedCommit {
		return
	}
	// Divergence: the delivered commit is not the one the checks verified. Record
	// it as a gap so the sealed manifest carries both SHAs and reads incomplete,
	// and emit a distinct event so the divergence is visible on the stream rather
	// than buried in the manifest body.
	cause := fmt.Errorf(
		"result_commit %s != checks-verified commit %s; the delivered commit was not the one the delivery checks verified",
		task.ResultCommit.String, verifiedCommit,
	)
	runner.recordEvidenceGap(ctx, task, "checks", "", cause)
	runner.emit(ctx, task, EvDeliveryCommitDiverged, map[string]any{
		"result_commit": task.ResultCommit.String,
		"checks_commit": verifiedCommit,
	})
}

// verifyResultCommitMatchesLiveTip detects a divergence between the recorded
// result_commit and the live agentum/<task-id> branch tip at teardown (ADR 0003
// D8). result_commit is pinned at the final gate (the commit the human
// reviewed); if the branch moved between review and teardown, the recorded
// commit no longer names what is actually delivered. Same non-fatal treatment
// as verifyDeliveryCommitBinding: a gap + event, never a post-approval failure.
// No-op when result_commit is unset or the branch is unresolvable (already
// cleaned up).
func (runner *Runner) verifyResultCommitMatchesLiveTip(ctx context.Context, task sqlc.Task, repoPath string) {
	if !task.ResultCommit.Valid || task.ResultCommit.String == "" {
		return
	}
	liveTip, err := runner.wt.ResolveRef(ctx, repoPath, worktree.BranchFor(task.ID))
	if err != nil {
		// Branch not resolvable — already cleaned up, or never created. Nothing
		// to compare; an absence, not a divergence.
		return
	}
	if liveTip == task.ResultCommit.String {
		return
	}
	cause := fmt.Errorf(
		"result_commit %s != live branch tip %s; the branch moved between review and teardown",
		task.ResultCommit.String, liveTip,
	)
	runner.recordEvidenceGap(ctx, task, "checks", "", cause)
	runner.emit(ctx, task, EvDeliveryCommitDiverged, map[string]any{
		"result_commit": task.ResultCommit.String,
		"live_tip":      liveTip,
	})
}

// checksVerifiedCommit returns the commit the delivery checks verified, reading
// body.checks.commit directly when the manifest service is available, and
// falling back to the latest checkpoint SHA when it is not (unit tests). The
// fallback is a degraded proxy — see verifyDeliveryCommitBinding for why it is
// not the primary path.
//
// An empty commit with a nil error means "nothing was recorded to compare
// against"; a non-nil error means "the recorded value could not be read." The
// caller must distinguish them, because only the second one is a gap in the
// evidence.
func (runner *Runner) checksVerifiedCommit(ctx context.Context, task sqlc.Task) (string, error) {
	if runner.mfst != nil {
		return runner.mfst.ChecksCommit(ctx, task.TenantID, task.ID)
	}
	checkpoint, err := runner.store.LatestCheckpointForTask(ctx, sqlc.LatestCheckpointForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A task that never recorded a checkpoint has nothing to compare
			// against; that is an absence, not a read failure.
			return "", nil
		}
		return "", fmt.Errorf("load checkpoint fallback: %w", err)
	}
	return checkpoint.CommitSha, nil
}

// drive performs the shared setup (load task + project + pack, resolve the
// lineage anchor, reconcile any partially-modified worktree, create the
// worktree off base_commit, record the base checkpoint) and enters the stage
// loop. It registers a cancel for the task so the cancel handler can abort the
// run mid-stage; a child context carries that cancellation down to the adapter.
func (runner *Runner) drive(ctx context.Context, job sqlc.Job) error {
	task, err := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: job.TaskID, TenantID: job.TenantID})
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	project, err := runner.store.GetProject(ctx, sqlc.GetProjectParams{ID: task.ProjectID, TenantID: task.TenantID})
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	taskPack, err := runner.packs.Resolve(ctx, task.PipelinePack)
	if err != nil {
		return runner.failTask(ctx, task, fmt.Errorf("resolve pack %q: %w", task.PipelinePack, err))
	}

	// Resolve the lineage anchor once. base_commit is what the worktree branches
	// from and what checkpoints diff against; recording it immutably before any
	// work means a later move of base_ref cannot retcon the task's lineage.
	task, err = runner.resolveBaseCommit(ctx, task, project.RepoPath)
	if err != nil {
		return runner.failTask(ctx, task, err)
	}
	baseCommit := task.BaseCommit.String

	taskWorktree, err := runner.wt.Create(ctx, project.RepoPath, task.ID, baseCommit)
	if err != nil {
		return runner.failTask(ctx, task, fmt.Errorf("create worktree: %w", err))
	}

	// Record the base as a checkpoint so a crash before the first stage completes
	// still has a restore target. Idempotent: ON CONFLICT replaces the SHA.
	runner.recordCheckpoint(ctx, task, "base", baseCommit)

	// Reconcile before driving a side-effectful stage. A crashed worktree may be
	// clean, safely resumable, restorable to the last checkpoint, or in a state
	// that needs a human — never blindly replayed.
	if err := runner.reconcileWorktree(ctx, task, project.RepoPath, baseCommit, taskWorktree.Root); err != nil {
		return err
	}
	runner.emit(ctx, task, EvWorktreeCreated, map[string]any{
		"base_commit": baseCommit, "branch": worktree.BranchFor(task.ID),
	})

	// Record the initial manifest evidence (input, project, pack, declared
	// capabilities, adapter) once the lineage anchor is set. This is the run's
	// provenance root — a failure here fails the run, because every later
	// piece of evidence chains off it and a silent gap at the root would
	// orphan everything that follows.
	if evidenceErr := runner.recordInitialEvidence(ctx, task, project, taskPack); evidenceErr != nil {
		return runner.failTask(ctx, task, evidenceErr)
	}
	runner.recordGitEvidence(ctx, task)

	startStage, resumeSession, halt, err := runner.entryPoint(ctx, job, task, taskPack)
	if err != nil {
		return runner.failTask(ctx, task, err)
	}
	// A halt (budget exhausted / verdict unreadable on the advance path) is a
	// controlled pause, not a failure: apply it and stop without entering the
	// loop. This must not flow through err, which drive turns into failTask.
	if halt != nil {
		return runner.applyPauseDecision(ctx, task, halt.decision, halt.stageID)
	}

	// Register a cancel for this task so the cancel handler can abort the
	// in-flight run. The child context propagates that cancellation to the
	// adapter (the §5.1 seam).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner.cancels.Register(job.TaskID, cancel)
	defer runner.cancels.Unregister(job.TaskID)

	run := stageRun{task: task, project: project, taskPack: taskPack, worktree: taskWorktree}
	// Read the source_write approval state ONCE for the whole run (ADR 0003 D3):
	// whether the pack declares an approval, whether the human has recorded it,
	// and the revision id the decision is bound to. A durable Postgres read, not
	// process state — a worker restart, a worktree restore, and a Restore to a
	// checkpoint all leave it intact.
	run.sourceWriteUnlock = runner.readSourceWriteUnlock(ctx, task, taskPack)
	// Prepare the project-context channel (ADR 0002): read .agentum.yaml from
	// base_commit once, pin the declared + auto-injected instruction files,
	// probe the runtime's skills, and resolve the check set for rendering. This
	// runs after the worktree exists (project-local skills come from it) and
	// before the first stage. A malformed config or an unresolvable check set
	// fails the task; a pin or probe failure degrades evidence and the run
	// proceeds with whatever context was pinnable.
	if contextErr := runner.prepareProjectContext(ctx, &run, baseCommit); contextErr != nil {
		return runner.failTask(ctx, run.task, contextErr)
	}
	// On resume / advance, sync the current artifact revisions back into the
	// worktree so the agent starts from the same content the prior invocation
	// produced. No-op for a fresh run (no revisions yet).
	runner.syncRevisionsIntoWorktree(ctx, run, startStage)
	return runner.runLoop(runCtx, run, startStage, resumeSession)
}

// readSourceWriteUnlock reads the durable approval state for the run (ADR 0003
// D3). Required is true when the pack declares a source_write approval; Granted
// is true when the matching task_approvals row exists; ApprovedRevisionID
// carries the revision id the decision is bound to (used to detect
// plan_revision_drift before entering a source-writing stage). A pack with no
// approvals block yields the zero value — the withholding never applies and
// every existing fixture behaves exactly as before.
//
// This is a Postgres read, not a manifest read: a security precondition must
// not read from a store whose write path is allowed to fail quietly (manifest
// AddEvidence failures are logged and turned into evidence gaps).
func (runner *Runner) readSourceWriteUnlock(ctx context.Context, task sqlc.Task, taskPack *pack.Pack) approvalState {
	approval, hasApproval := taskPack.SourceWriteApproval()
	if !hasApproval {
		return approvalState{}
	}
	state := approvalState{Required: true}
	row, err := runner.store.GetApproval(ctx, sqlc.GetApprovalParams{
		TenantID: task.TenantID, TaskID: task.ID, Name: approval.Name,
	})
	if err != nil {
		// sql.ErrNoRows means "not yet decided" — the row is absent, which is
		// the normal pre-approval state and not an error. Any other read failure
		// is logged but fails closed (Granted stays false): the lock is the
		// default, the approval is the exception.
		if !errors.Is(err, sql.ErrNoRows) {
			runner.log.Warn("read source_write approval", "task", task.ID, "name", approval.Name, "error", err)
		}
		return state
	}
	if row.Decision == "approved" {
		state.Granted = true
		if row.ArtifactRevisionID.Valid {
			state.ApprovedRevisionID = row.ArtifactRevisionID.String
		}
	}
	return state
}

// resolveBaseCommit resolves task.BaseRef to a full SHA exactly once and pins it
// on the row. SetBaseCommit's `WHERE base_commit IS NULL` makes a concurrent
// resolver a no-op; we re-read to pick up the canonical value either way. The
// worktree branches from this SHA, so a missing/unknown ref fails the task
// before any side effect rather than mid-stage.
func (runner *Runner) resolveBaseCommit(ctx context.Context, task sqlc.Task, repoPath string) (sqlc.Task, error) {
	if task.BaseCommit.Valid && task.BaseCommit.String != "" {
		return task, nil
	}
	baseRef := task.BaseRef
	if baseRef == "" {
		baseRef = "HEAD"
	}
	sha, err := runner.wt.ResolveRef(ctx, repoPath, baseRef)
	if err != nil {
		return task, fmt.Errorf("resolve base_ref %q: %w", baseRef, err)
	}
	if _, err := runner.store.SetBaseCommit(ctx, sqlc.SetBaseCommitParams{
		ID: task.ID, TenantID: task.TenantID, BaseCommit: nullStr(sha),
	}); err != nil {
		return task, fmt.Errorf("persist base_commit: %w", err)
	}
	// Re-read so the returned task carries the canonical base_commit (ours if we
	// won the race, the other resolver's if we lost).
	return runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: task.ID, TenantID: task.TenantID})
}

// reconcileWorktree enforces the F.6.1 "never blindly replay a side-effectful
// stage" invariant. The worktree is classified; restorable trees are restored
// to the last checkpoint (so the next stage starts from a known-good commit),
// needs-attention trees fail the task rather than guessing, and clean/resumable
// trees proceed as-is.
func (runner *Runner) reconcileWorktree(ctx context.Context, task sqlc.Task, repoPath, baseCommit, wtRoot string) error {
	lastCheckpoint := ""
	if cp, err := runner.store.LatestCheckpointForTask(ctx, sqlc.LatestCheckpointForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	}); err == nil {
		lastCheckpoint = cp.CommitSha
	} else if !errors.Is(err, sql.ErrNoRows) {
		runner.log.Warn("load last checkpoint for reconcile", "task", task.ID, "error", err)
	}

	state, err := runner.wt.Reconcile(ctx, repoPath, task.ID, baseCommit, lastCheckpoint)
	if err != nil {
		return runner.failTask(ctx, task, fmt.Errorf("reconcile worktree: %w", err))
	}
	switch state.Class {
	case worktree.ClassClean, worktree.ClassResumable:
		runner.emit(ctx, task, EvWorktreeReconciled, map[string]any{
			"class": state.Class.String(), "head": state.HeadCommit,
		})
		return nil
	case worktree.ClassRestorable:
		// Restore to the checkpoint before the stage runs — uncommitted work from
		// a crashed run must not bleed into the retry.
		if err := runner.wt.Restore(ctx, wtRoot, state.CheckpointCommit); err != nil {
			return runner.failTask(ctx, task, fmt.Errorf("restore worktree to checkpoint: %w", err))
		}
		runner.emit(ctx, task, EvWorktreeReconciled, map[string]any{
			"class": state.Class.String(), "restored_to": state.CheckpointCommit,
		})
		return nil
	default:
		return runner.failTask(ctx, task, fmt.Errorf("worktree needs human attention (class=%s)", state.Class))
	}
}

// recordCheckpoint captures an orchestrator-owned boundary SHA. Idempotent per
// label — a retry after a crash that re-crosses the same boundary upserts
// rather than duplicates. Best-effort: a checkpoint write failure is logged,
// not fatal (the lineage and result_commit are independent of checkpoints).
func (runner *Runner) recordCheckpoint(ctx context.Context, task sqlc.Task, label, commit string) {
	if commit == "" {
		return
	}
	if _, err := runner.store.CreateCheckpoint(ctx, sqlc.CreateCheckpointParams{
		TenantID: task.TenantID, UserID: task.UserID, TaskID: task.ID,
		Label: label, CommitSha: commit,
	}); err != nil {
		runner.log.Warn("record checkpoint", "task", task.ID, "label", label, "error", err)
		return
	}
	runner.emit(ctx, task, EvCheckpointRecorded, map[string]any{"label": label, "commit": commit})
}

// recordStageCheckpoint commits the worktree's working state on the task branch
// and records the resulting SHA as a post-stage boundary checkpoint. The label
// is `post-<stage>`. The orchestrator authors the commit itself (the
// git.delivery privilege no agent role carries): without this, a checkpoint
// would be whatever the agent happened to leave at HEAD — often still the base,
// since nothing in the orchestrator committed — and the agent's uncommitted
// work would be discarded at the next Restore or at teardown. A stage that
// produced no change records the unchanged HEAD honestly (Commit returns
// created=false for a clean tree) rather than creating an empty commit. A
// commit failure is logged and skipped — the lineage anchor and result_commit
// do not depend on it, but a checkpoint that cannot be created cannot lie about
// having captured the boundary.
func (runner *Runner) recordStageCheckpoint(ctx context.Context, run stageRun, stageID string) {
	message := fmt.Sprintf("agentum: checkpoint after stage %s", stageID)
	head, _, err := runner.wt.Commit(ctx, run.worktree.Root, message)
	if err != nil {
		runner.log.Warn("commit stage checkpoint", "task", run.task.ID, "stage", stageID, "error", err)
		return
	}
	runner.recordCheckpoint(ctx, run.task, "post-"+stageID, head)
}

// stageRun bundles the per-task state the loop and adapter invocation share.
// Loading it once in drive() keeps runLoop/invokeStage signatures under the
// parameter-count lint bound and makes the per-stage data flow explicit.
type stageRun struct {
	task     sqlc.Task
	project  sqlc.Project
	taskPack *pack.Pack
	worktree *worktree.Worktree

	// instructionFiles is the pinned project-instruction set for the run (ADR
	// 0002): bytes read from base_commit, capped/truncated, with source and
	// delivered hashes. Built once in drive() and carried to every stage
	// invocation so the adapter stages them and denies them in edit rules.
	instructionFiles []instructions.File
	// contextReport is the runtime's non-prompt context (auto-injected
	// instructions + enumerated skills), probed once in drive() after the
	// worktree exists. Carried to every stage for evidence.
	contextReport agent.ContextReport
	// resolvedChecks is the resolved project-check set, cached for RENDERING
	// ONLY (ADR 0002 D8). enforceProjectChecks keeps its own independent load
	// and resolve at the delivery boundary — sharing this cached value would
	// re-open the commit-binding seam closed by PR #23.
	resolvedChecks []routing.CheckRef
	// sourceWriteUnlock records whether the run's source_write approval is in
	// place (ADR 0003 D3). Read ONCE in drive() from the pack's approval
	// declaration plus the durable task_approvals row, so a worker restart, a
	// worktree restore, and a Restore to a checkpoint all leave the decision
	// intact. While Required && !Granted, computeProfile withholds
	// caps.SourceWriteCategories for every stage and processStage refuses entry
	// to implementer/fixer stages.
	sourceWriteUnlock approvalState
	// diff carries the orchestrator-produced diff refs for the current reviewer
	// stage (ADR 0003 D5). Produced by produceDiff at the start of a
	// reviewer-role stage and rendered into that stage's routing block. nil for
	// non-reviewer stages and before produceDiff runs.
	diff *routing.DiffRef
}

// approvalState is the durable read of whether a pack-declared approval has
// been recorded for this run (ADR 0003 D3). Required is true when the pack
// declares a source_write approval; Granted is true when the task_approvals
// row exists; ApprovedRevisionID is the artifact revision id the human bound
// their decision to, used to detect plan_revision_drift.
type approvalState struct {
	Required           bool
	Granted            bool
	ApprovedRevisionID string
}

// haltDecision carries a pause Decision out of entryPoint without surfacing it
// as an error. drive turns any entryPoint error into failTask; a budget halt or
// a verdict_unreadable halt is a controlled pause, not a failure, so it must
// travel a separate channel. When entryPoint returns a non-nil halt, drive
// applies it via applyPauseDecision and returns without entering the loop.
type haltDecision struct {
	decision Decision
	stageID  string
}

// entryPoint resolves where the loop starts and whether it resumes a session.
// The returned halt is non-nil only for the advance job when the resolved
// transition halts (fix_budget_exhausted / verdict_unreadable) — those are
// controlled pauses, not errors, so they must not flow through err (which drive
// turns into failTask).
func (runner *Runner) entryPoint(ctx context.Context, job sqlc.Job, task sqlc.Task, taskPack *pack.Pack) (stage, resume string, halt *haltDecision, err error) {
	switch job.Kind {
	case "run":
		// A fresh run starts at the pack entry, unless a previous attempt set
		// current_stage before a crash — resume there.
		if task.CurrentStage.Valid {
			return task.CurrentStage.String, "", nil, nil
		}
		return taskPack.Entry, "", nil, nil
	case "continue":
		// Resume the current stage from its captured session id (non-destructive).
		latest, latestErr := runner.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{
			TaskID: task.ID, TenantID: task.TenantID,
		})
		if latestErr != nil {
			return "", "", nil, fmt.Errorf("find resume session: %w", latestErr)
		}
		return task.CurrentStage.String, latest.SessionID.String, nil, nil
	case "advance":
		// Past the gate: resolve the current stage's transition through the SAME
		// resolver the loop path uses (D3: one resolver, not two). A halt
		// (budget exhausted / verdict unreadable) returns a pause Decision via
		// halt rather than err, so drive applies a controlled pause instead of
		// failing the task.
		currentStageID := task.CurrentStage.String
		currentStage, ok := taskPack.Stages[currentStageID]
		if !ok {
			return "", "", nil, fmt.Errorf("advance: current stage %q not in pack", currentStageID)
		}
		if len(currentStage.Transitions) == 0 {
			return "", "", nil, fmt.Errorf("advance: stage %q has no transition", currentStageID)
		}
		// On the advance path the prior stage already produced its result; read
		// the status from the latest invocation's stored result so the condition
		// matches on what the stage actually reported.
		latestResult := runner.latestStoredResult(ctx, task)
		transitionContext, transitionErr := runner.buildTransitionContext(ctx, task, taskPack, currentStageID, latestResult)
		if transitionErr != nil {
			return "", "", nil, fmt.Errorf("advance: build transition context: %w", transitionErr)
		}
		resolution, resolveErr := ResolveTransition(currentStage, currentStageID, transitionContext)
		if resolveErr != nil {
			return "", "", nil, fmt.Errorf("advance: resolve transition: %w", resolveErr)
		}
		if resolution.StopReason != "" {
			decision := Decision{
				Action: ActionPause, FSMEvent: engine.EventStopUser,
				StopReason: resolution.StopReason,
			}
			// Emit the transition resolution (even though no stage starts) so
			// the would-be branch is auditable.
			runner.emitStageTransition(ctx, task, verdictPayload{
				From: currentStageID, Condition: resolution.Condition,
				Cycle: resolution.Cycle, Verdict: resolution.Verdict,
			})
			return "", "", &haltDecision{decision: decision, stageID: currentStageID}, nil
		}
		// Fill the prospective cycle for the target invocation at the call site
		// (the pure resolver leaves it zero; see processStage's ActionAdvance
		// branch for why). Using nextCycleForStage keeps the record and the
		// invocation row in agreement.
		targetCycle, cycleErr := runner.nextCycleForStage(ctx, task, resolution.To, nil)
		if cycleErr != nil {
			return "", "", nil, fmt.Errorf("advance: resolve target cycle for stage %q: %w", resolution.To, cycleErr)
		}
		resolution.Cycle = int(targetCycle)
		runner.emitStageTransition(ctx, task, verdictPayload{
			From: currentStageID, To: resolution.To,
			Condition: resolution.Condition, Cycle: resolution.Cycle, Verdict: resolution.Verdict,
		})
		// Record the taken branch (the same record processStage writes on the
		// loop path), so an advance-job resolution is auditable from the sealed
		// manifest too (D7).
		runner.recordTransitionEvidence(ctx, task, manifest.TransitionRecord{
			From: currentStageID, To: resolution.To,
			Condition: resolution.Condition, Cycle: resolution.Cycle,
			Verdict: resolution.Verdict, At: time.Now().UTC(),
		})
		return resolution.To, "", nil, nil
	}
	return "", "", nil, fmt.Errorf("entryPoint: unsupported kind %q", job.Kind)
}

// latestStoredResult reads the parsed result.json off the task's most recent
// stage invocation, so the advance path can match transition conditions on the
// status the prior stage actually reported. Returns nil when there is no stored
// result (the condition will treat status as empty, which is correct for a
// verdict-only stage).
func (runner *Runner) latestStoredResult(ctx context.Context, task sqlc.Task) *agent.ResultJSON {
	latest, err := runner.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	})
	if err != nil {
		return nil
	}
	if !latest.Result.Valid || len(latest.Result.RawMessage) == 0 {
		return nil
	}
	parsed, parseErr := agent.ParseResultJSON(latest.Result.RawMessage)
	if parseErr != nil {
		return nil
	}
	return &parsed
}

// runLoop walks the pack's stages from startStage, invoking the adapter per
// stage and applying the evaluator's decision, until a pause point or terminal
// state. resumeSession applies only to the first iteration. The per-stage body
// lives in processStage; runLoop stays a flat claim-retry loop.
func (runner *Runner) runLoop(ctx context.Context, run stageRun, startStage, resumeSession string) error {
	stageID := startStage
	transition := stageTransition{} // empty for the entry stage
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, err := runner.processStage(ctx, run, stageID, resumeSession, transition)
		if err != nil {
			return err
		}
		if outcome.done {
			return nil
		}
		stageID = outcome.nextStage
		transition = outcome.transition
		resumeSession = "" // only the first iteration resumes
	}
}

// stageOutcome is processStage's verdict for one iteration of runLoop. Exactly
// one of done or nextStage applies: done ends the loop (terminal/pause/final);
// nextStage advances the loop to the next pack stage. transition carries the
// resolved edge to the next stage so the next iteration can hand the fixer the
// findings (commit 7) and commit 8 can record the TransitionRecord.
type stageOutcome struct {
	nextStage  string
	done       bool
	transition stageTransition
}

// refuseSourceWriteBeforeApproval implements ADR 0003 D3 layer 2. It returns a
// halt when the stage's effective role is implementer or fixer AND the run's
// source_write approval is required but not granted — the runner refuses to
// enter a source-writing stage under a withheld profile rather than running a
// crippled agent. It also checks plan_revision_drift when the approval IS
// granted: if the current revision of the approval artifact no longer matches
// the one the human approved (someone edited the plan after approving it), the
// implementer would work from a plan the human never saw, so the same
// controlled-stop shape fires. Returns nil when the stage may proceed.
//
// Runs at the START of a stage, next to restoreInstructions, and strictly
// before invokeStage — never between the isClean sample and the checkpoint
// commit. The capability profile (layer 1) already withholds source-write
// categories for every stage while the approval is pending; this layer protects
// against an agent that would otherwise report a misleading "complete" without
// writing anything.
//
// Both halts pin current_stage to the APPROVAL stage, not the refused stage.
// This is load-bearing: the advance job resolves the CURRENT stage's
// transition (entryPoint's advance branch), so a paused_gate stop must sit at a
// stage that already ran. Pinning the refused (never-invoked) stage made
// advance resolve that stage's transition — silently skipping the implementer
// and reaching review against an empty diff. Pinning the approval stage makes
// recovery exactly the ordinary plan-gate advance: the handler's
// planApprovalForStage guard matches (current_stage == approval.Stage), the
// approval row is written bound to the current plan revision, and entryPoint
// resolves the approval stage's transition — the implementer runs with the
// grant. For drift the approval row already exists (CreateApproval is a no-op
// on conflict), so advance re-checks, drifts again, and pauses here once more
// — never a skip, and cancel is the exit (drift cannot be cleared by advance:
// re-editing the plan mints a revision the approval is not bound to). Note the
// retry is free only when the approval stage transitions straight into the
// source-writing stage (the shipped pack's shape); with intermediate stages —
// well-formed under the validator's layer-3 pass-through rule — each advance
// re-runs them before re-hitting the refusal, so each retry costs real
// invocations.
func (runner *Runner) refuseSourceWriteBeforeApproval(ctx context.Context, run stageRun, stageID string, stage pack.Stage) *haltDecision {
	if !run.sourceWriteUnlock.Required {
		return nil
	}
	role := pack.EffectiveRole(stageID, stage)
	if role != "implementer" && role != "fixer" {
		return nil
	}
	approval, hasApproval := run.taskPack.SourceWriteApproval()
	if !hasApproval {
		// Unreachable in practice: sourceWriteUnlock.Required is derived from
		// this same pack's declaration. If it ever diverges, do not halt —
		// layer 1 (the capability withholding) is the guarantee, and a halt
		// without a rewind target would pin a never-run stage.
		return nil
	}
	if !run.sourceWriteUnlock.Granted {
		// EventStopGate (not EventStopUser): a gate is what this is. The human
		// resolves it by advancing (which records the plan approval, because the
		// pause sits at the approval stage) or by rejecting/cancelling.
		// paused_user_stop's only forward action is continue, which would resume
		// the PLANNER's session — not an exit at all.
		return &haltDecision{
			stageID: approval.Stage,
			decision: Decision{
				Action:     ActionPause,
				FSMEvent:   engine.EventStopGate,
				StopReason: "plan_not_approved",
			},
		}
	}
	// Granted — but the approval binds to a plan revision. If the current
	// revision of the approval artifact no longer matches the approved one,
	// someone edited the plan AFTER approving it; the implementer would then
	// work within a plan the human never saw. Same rewind: advance re-checks
	// and pauses here again (the approval row exists, so the advance write is a
	// no-op and the drift persists) — visible, bounded, and never a skip of the
	// implementer. Cancel is the exit; reject 409s here because the plan gate
	// was already decided approved.
	if runner.detectPlanRevisionDrift(ctx, run) {
		return &haltDecision{
			stageID: approval.Stage,
			decision: Decision{
				Action:     ActionPause,
				FSMEvent:   engine.EventStopGate,
				StopReason: "plan_revision_drift",
			},
		}
	}
	return nil
}

// detectPlanRevisionDrift reports whether the approval artifact's current
// revision differs from the one the human approved (ADR 0003 D3). The approval
// record carries the revision id it bound to; the artifact store carries the
// current revision. A mismatch means "the plan was edited after approval" and
// the implementer must not proceed. Returns false when there is no approval
// revision to compare against (the approval predates the revision-binding
// feature, or the artifact has no current revision).
func (runner *Runner) detectPlanRevisionDrift(ctx context.Context, run stageRun) bool {
	approved := run.sourceWriteUnlock.ApprovedRevisionID
	if approved == "" {
		return false
	}
	approval, hasApproval := run.taskPack.SourceWriteApproval()
	if !hasApproval {
		return false
	}
	revisionName := approval.Stage + "/" + approval.Artifact
	current, err := runner.store.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: run.task.ID, TenantID: run.task.TenantID, Name: revisionName,
	})
	if err != nil {
		// No current revision (sql.ErrNoRows) or a read failure — both mean we
		// cannot prove drift, so we do not stop. A missing revision is the
		// pre-write state; a read failure is logged elsewhere and fail-closed at
		// the approval gate, not here.
		return false
	}
	return current.ID != approved
}

// approvedPlanRef builds the routing pointer to the approved plan revision (ADR
// 0003 D2). Returns nil when the approval is not granted or has no bound
// revision, so the template renders nothing for stages before the gate. The
// path is the materialized location inside the approval stage's artifact dir.
func (runner *Runner) approvedPlanRef(ctx context.Context, run stageRun, approval pack.Approval) *routing.PlanRef {
	if !run.sourceWriteUnlock.Granted || run.sourceWriteUnlock.ApprovedRevisionID == "" {
		return nil
	}
	revisionName := approval.Stage + "/" + approval.Artifact
	current, err := runner.store.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: run.task.ID, TenantID: run.task.TenantID, Name: revisionName,
	})
	if err != nil {
		return nil
	}
	return &routing.PlanRef{
		Stage:       approval.Stage,
		Path:        filepath.Join(worktree.ArtifactDir(run.worktree.Root, run.task.ID, approval.Stage), approval.Artifact),
		RevisionID:  current.ID,
		ContentHash: current.ContentHash,
	}
}

// priorStageRefs builds the cross-stage artifact references for the routing
// block (ADR 0003 D6.1). For every prior stage that has a current result.json
// revision, render {Stage, Path} where Path is the artifact-dir location of
// that stage's result.json. Read from the durable revision list (not a directory
// walk) so it survives a worktree Restore and a worker restart. The current
// stage is excluded — it has no "prior" self.
func (runner *Runner) priorStageRefs(ctx context.Context, run stageRun, currentStage string) []routing.PriorStage {
	revisions, err := runner.currentRevisionList(ctx, run.task.TenantID, run.task.ID)
	if err != nil || len(revisions) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(revisions))
	var refs []routing.PriorStage
	for _, revision := range revisions {
		if revision.Kind != "result_json" {
			continue
		}
		stage, _, ok := splitArtifactRevisionName(revision.Name)
		if !ok || stage == currentStage || seen[stage] {
			continue
		}
		seen[stage] = true
		refs = append(refs, routing.PriorStage{
			Stage: stage,
			Path:  filepath.Join(worktree.ArtifactDir(run.worktree.Root, run.task.ID, stage), "result.json"),
		})
	}
	return refs
}

// processStage runs one iteration: look up the stage, dispatch (terminal marker
// vs adapter invocation), evaluate the outcome, and apply the resulting
// decision. transitionIn is the edge that brought the run to stageID (empty for
// the entry stage), threaded so invokeStage can render the reviewer-findings
// hand-off without re-reading the artifact. The caller owns loop control
// (continue / stop) via stageOutcome.
func (runner *Runner) processStage(ctx context.Context, run stageRun, stageID, resumeSession string, transitionIn stageTransition) (stageOutcome, error) {
	stage, ok := run.taskPack.Stages[stageID]
	if !ok {
		return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("pack stage %q not found", stageID))
	}

	// A terminal stage (no transitions) is an engine marker, not an agent
	// invocation: reaching it means the pipeline is complete. Enforce the
	// orchestrator-owned project checks first (against the last post-stage
	// checkpoint, i.e. the worktree HEAD), then fire the final gate. A mandatory
	// check failure blocks delivery: the task fails rather than reaching the
	// review gate, and the check evidence in the manifest is the record.
	if stage.Terminal() {
		if err := runner.runDeliveryChecks(ctx, run); err != nil {
			return stageOutcome{}, err
		}
		if err := runner.reachTerminalStage(ctx, run.task, run.project.RepoPath, stageID); err != nil {
			return stageOutcome{}, err
		}
		return stageOutcome{done: true}, nil
	}

	// Record current position; the task stays running through auto-advances.
	updatedTask, err := runner.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
		ID: run.task.ID, TenantID: run.task.TenantID,
		CurrentStage: nullStr(stageID), State: string(engine.StateRunning),
	})
	if err != nil {
		return stageOutcome{}, fmt.Errorf("update current_stage: %w", err)
	}
	run.task = updatedTask
	runner.emit(ctx, run.task, EvStageStarted, map[string]any{"stage": stageID, "gate": string(stage.Gate)})

	// ADR 0003 D3 layer 2 — entry refusal. The capability withholding (layer 1)
	// makes a pre-approval implementer refuse every write at the runtime layer,
	// but running a crippled implementer that writes nothing and reports
	// complete would be worse than stopping: it would burn a cycle and produce a
	// misleading result. So the runner refuses to ENTER a source-writing stage
	// (effective role implementer or fixer) while the source_write unlock is
	// absent, as a controlled stop (plan_not_approved). This is the second line
	// of defence; the guarantee is layer 1, which holds even if this check is
	// wrong.
	if halt := runner.refuseSourceWriteBeforeApproval(ctx, run, stageID, stage); halt != nil {
		// halt.stageID is the APPROVAL stage (the rewind target), not the
		// refused stage — see refuseSourceWriteBeforeApproval for why the pause
		// must sit there for advance to resolve the right transition.
		return stageOutcome{done: true}, runner.applyPauseDecision(ctx, run.task, halt.decision, halt.stageID)
	}

	// ADR 0002 D4 layer 2: restore any instruction files the worktree drifted
	// off the pin BEFORE the invocation. The edit deny (layer 1) does not cover
	// bash; this hash check is what catches a bash-side rewrite. Runs strictly
	// before invokeStage, never between the isClean sample and the checkpoint
	// commit (the load-bearing ordering preserved below). A restore IO error
	// fails the task — we cannot stand behind a run whose reviewer might be
	// reading rewritten rules.
	if restoreErr := runner.restoreInstructions(ctx, run, stageID); restoreErr != nil {
		return stageOutcome{}, runner.failTask(ctx, run.task, restoreErr)
	}

	// ADR 0003 D5: produce the orchestrator-owned diff for reviewer-role
	// stages before invoking the adapter. HEAD here is the post-stage
	// checkpoint the orchestrator authored on the prior implementer/fixer, so
	// the patch describes a real commit range; the reviewer reads it with
	// fs.read alone (no exec.bash to run git). Runs strictly before
	// invokeStage, never between the isClean sample and the checkpoint commit.
	if pack.EffectiveRole(stageID, stage) == "reviewer" {
		run.diff = runner.produceDiff(ctx, run, stageID)
	}

	outcome := runner.invokeStage(ctx, run, stageID, stage, resumeSession, transitionIn)

	// If the run was cancelled (the cancel handler aborts it via the registry),
	// bow out without touching the FSM — the handler owns the transition to
	// cancelled. Otherwise the adapter_error pause would race and overwrite it.
	if err := ctx.Err(); err != nil {
		return stageOutcome{done: true}, nil
	}

	// A successful stage invocation crossed a boundary: capture the worktree's
	// HEAD as an orchestrator-owned checkpoint so a later crash can restore to
	// this point rather than blindly replaying the next side-effectful stage.
	// Skipped on any failure flag — there is no trustworthy commit to record.
	//
	// The clean check for the auto_if_clean gate MUST be sampled before the
	// checkpoint commit, not after. The gate's purpose is to surface "the agent
	// touched files beyond its declared edit_targets" — that is a property of
	// the working tree the agent left, which recordStageCheckpoint destroys by
	// committing it. Sampling after would make the tree permanently clean and
	// the gate unreachable (the mirror image of the PR B defect where isClean was
	// permanently false); git add -A would also sweep those undeclared files into
	// the delivery commit. Sampling before preserves the signal the gate exists
	// to send.
	cleanBeforeCommit := runner.isClean(run.project.RepoPath, run.task.ID)
	if outcome.result != nil && !outcome.adapterErr && !outcome.parseErr && !outcome.rejected {
		runner.recordStageCheckpoint(ctx, run, stageID)
		// The new checkpoint belongs in the manifest's git lineage. Best-effort;
		// the manifest service is nil in unit tests.
		runner.recordGitEvidence(ctx, run.task)
	}

	// Build the transition context (verdict, status, fix-cycle counter, budget)
	// before Evaluate so the gate logic and the branch logic share one pure
	// function. A build failure halts the run rather than guessing a route.
	transitionContext, transitionErr := runner.buildTransitionContext(ctx, run.task, run.taskPack, stageID, outcome.result)
	if transitionErr != nil {
		return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("build transition context for stage %q: %w", stageID, transitionErr))
	}

	decision, err := Evaluate(StageInput{
		Result:           outcome.result,
		Stage:            stage,
		StageID:          stageID,
		Transition:       transitionContext,
		Clean:            cleanBeforeCommit,
		AdapterError:     outcome.adapterErr,
		ParseError:       outcome.parseErr,
		ArtifactRejected: outcome.rejected,
	})
	if err != nil {
		return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("evaluate stage %q: %w", stageID, err))
	}

	switch decision.Action {
	case ActionAdvance:
		// Fill the prospective cycle for the target invocation. The pure
		// ResolveTransition leaves it zero (it cannot know the per-stage cycle
		// without a store read, and deriving it from the fixer-set max would be
		// wrong for a non-fixer back-edge and for multi-fixer packs). Compute it
		// here via nextCycleForStage — the SAME function that assigns the
		// invocation row's cycle — so the record and the row can never disagree.
		targetCycle, cycleErr := runner.nextCycleForStage(ctx, run.task, decision.Transition.To, nil)
		if cycleErr != nil {
			return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("resolve target cycle for stage %q: %w", decision.Transition.To, cycleErr))
		}
		decision.Transition.Cycle = int(targetCycle)
		// Emit the transition resolution so the branch is auditable even when
		// the next stage never starts. The cycle carried on the Resolution is
		// the prospective cycle for the next invocation; from is the stage that
		// just ran.
		runner.emitStageTransition(ctx, run.task, verdictPayload{
			From: stageID, To: decision.Transition.To,
			Condition: decision.Transition.Condition,
			Cycle:     decision.Transition.Cycle, Verdict: decision.Transition.Verdict,
		})
		// Record the taken branch in the manifest's transitions section so the
		// review ⇄ fix loop is auditable from the sealed manifest (D7).
		runner.recordTransitionEvidence(ctx, run.task, manifest.TransitionRecord{
			From: stageID, To: decision.Transition.To,
			Condition: decision.Transition.Condition, Cycle: decision.Transition.Cycle,
			Verdict: decision.Transition.Verdict, At: time.Now().UTC(),
		})
		// Carry the resolved edge to the next iteration so invokeStage can hand
		// the fixer the predecessor's findings (commit 7) and commit 8 can
		// record the TransitionRecord without re-resolving.
		next := stageTransition{
			from: stageID, condition: decision.Transition.Condition,
			verdict: decision.Transition.Verdict, cycle: decision.Transition.Cycle,
		}
		// When the edge was verdict-conditioned, the predecessor's verdict.json
		// path and finding count come from the transition context we built
		// above; carry them so the fixer's routing block points at the findings.
		if decision.Transition.Verdict != "" {
			next.verdictPath = filepath.Join(worktree.ArtifactDir(run.worktree.Root, run.task.ID, stageID), agent.VerdictFileName)
			next.findingsCount = transitionContext.FindingsCount
		}
		return stageOutcome{nextStage: decision.NextStage, transition: next}, nil
	case ActionPause:
		return stageOutcome{done: true}, runner.applyPauseDecision(ctx, run.task, decision, stageID)
	case ActionFinal:
		// Defensive double-enforcement: eval only returns ActionFinal for a
		// terminal stage, and terminal stages short-circuit to the branch above
		// before invoking the adapter. Should the final outcome arise here
		// anyway, the project checks still gate delivery.
		if err := runner.runDeliveryChecks(ctx, run); err != nil {
			return stageOutcome{}, err
		}
		return stageOutcome{done: true}, runner.transitionToFinalState(ctx, run.task, run.project.RepoPath, stageID)
	}
	return stageOutcome{}, fmt.Errorf("runner: unknown decision action %d", decision.Action)
}

// reachTerminalStage pins current_stage on a terminal (no-transitions) stage,
// then fires the final gate.
func (runner *Runner) reachTerminalStage(ctx context.Context, task sqlc.Task, repoPath, stageID string) error {
	updatedTask, err := runner.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
		ID: task.ID, TenantID: task.TenantID,
		CurrentStage: nullStr(stageID), State: string(engine.StateRunning),
	})
	if err != nil {
		return fmt.Errorf("update current_stage (terminal): %w", err)
	}
	return runner.transitionToFinalState(ctx, updatedTask, repoPath, stageID)
}

// applyPauseDecision records the pause: pin current_stage, advance the FSM to
// the paused state named by the decision's event, emit, and stop the loop.
func (runner *Runner) applyPauseDecision(ctx context.Context, task sqlc.Task, decision Decision, stageID string) error {
	newState, fsmErr := engine.Next(engine.TaskState(task.State), decision.FSMEvent)
	if fsmErr != nil {
		return runner.failTask(ctx, task, fmt.Errorf("fsm %s --%s-->: %w", task.State, decision.FSMEvent, fsmErr))
	}
	if _, err := runner.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
		ID: task.ID, TenantID: task.TenantID,
		CurrentStage: nullStr(stageID), State: string(newState),
	}); err != nil {
		return fmt.Errorf("persist pause: %w", err)
	}
	runner.emit(ctx, task, EvTaskStateChanged, map[string]any{
		"from": task.State, "to": string(newState), "stop_reason": decision.StopReason, "stage": stageID,
	})
	// Record EVERY pause in the manifest's stops section (D7, deliberately
	// widened beyond budget/verdict): budget exhaustion, verdict_unreadable,
	// gate, adapter_error, parse_error, open_questions. (Stage, Reason, Cycle)
	// collapses repeats. The cycle is the prospective fix-cycle count at the
	// stop point when the resolver populated it (budget/verdict halts); 0 for
	// the non-branching pauses.
	runner.recordStopEvidence(ctx, task, manifest.StopRecord{
		Stage: stageID, Reason: decision.StopReason,
		Cycle: decision.Transition.Cycle, At: time.Now().UTC(),
	})
	return nil
}

// transitionToFinalState advances the FSM to awaiting_final_review and emits
// the state change. Shared by reachTerminalStage (terminal marker reached) and
// the ActionFinal path (complete outcome on a non-terminal stage) — both reach
// the same final gate, only the pin-current_stage step differs.
//
// ADR 0003 D8: result_commit is recorded HERE, not only at teardown, so the
// final-review payload can name the commit the human is asked to review. The
// commit recorded is the branch tip at the gate — which is the checkpoint the
// delivery checks verified. recordResultCommit is a no-op when result_commit
// is already set, so a re-entry does not overwrite.
func (runner *Runner) transitionToFinalState(ctx context.Context, task sqlc.Task, repoPath, stageID string) error {
	runner.recordResultCommit(ctx, task, repoPath)
	newState, fsmErr := engine.Next(engine.TaskState(task.State), engine.EventReachFinalGate)
	if fsmErr != nil {
		return runner.failTask(ctx, task, fmt.Errorf("fsm reach_final_gate: %w", fsmErr))
	}
	if _, err := runner.store.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: task.TenantID, State: string(newState),
	}); err != nil {
		return fmt.Errorf("persist final: %w", err)
	}
	runner.emit(ctx, task, EvTaskStateChanged, map[string]any{"from": task.State, "to": string(newState), "stage": stageID})
	return nil
}

// invocationOutcome is invokeStage's verdict for one adapter run. The three
// failure flags are mutually exclusive and map one-to-one onto the evaluator's
// stop reasons; result is nil unless the run produced a usable result.json that
// the orchestrator was willing to act on.
type invocationOutcome struct {
	result     *agent.ResultJSON
	adapterErr bool
	parseErr   bool
	// rejected is set when the agent declared an artifact path outside its
	// worktree. The run itself succeeded; its output did not survive
	// containment checking.
	rejected bool
}

// invokeStage runs one stage through the adapter and records the outcome. It
// creates the stage_invocation row at start (so a crash leaves a partial
// record), drains the stream (forwarding chunks to the sink), and finalizes the
// row with session_id / stop_reason / parsed result.
func (runner *Runner) invokeStage(ctx context.Context, run stageRun, stageID string, stage pack.Stage, resumeSession string, transitionIn stageTransition) invocationOutcome {
	artifactDir := worktree.ArtifactDir(run.worktree.Root, run.task.ID, stageID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		runner.log.Error("create artifact dir", "dir", artifactDir, "error", err)
		return invocationOutcome{adapterErr: true}
	}

	tier := stage.Tier
	if tier == "" {
		tier = run.taskPack.Tiers.Default
	}
	model, _ := models.Resolve(runner.models, runner.agentName, tier) // best-effort; empty is acceptable

	// Effective capability profile: host ∩ pack ∩ stage(inherit) ∩ role, with
	// the configured timeouts layered on. Computed before the invocation row is
	// created so the profile is persisted even when the adapter refuses to
	// start (an unenforceable profile is itself audit evidence). The
	// source-write withholding (ADR 0003 D3 layer 1) applies while the run's
	// approval is pending — run-scoped, not role-scoped, so a pack that
	// mislabels a source-writing stage gains nothing.
	var withheld []caps.Category
	var withheldReason string
	if run.sourceWriteUnlock.Required && !run.sourceWriteUnlock.Granted {
		withheld = caps.SourceWriteCategories
		withheldReason = "plan approval not recorded"
	}
	profile := runner.computeProfile(run.taskPack, stageID, stage, runner.adapter.Supported(),
		runner.hardTimeout, runner.idleTimeout, withheld, withheldReason)
	profileBytes := marshalProfile(profile)

	// VerdictPath is set when this stage sources a verdict condition (detected
	// via parsed conditions, never substring-scanned). ReviewFindings is set
	// when the stage was entered through a verdict-conditioned transition, so
	// the fixer is pointed at the predecessor's findings artifact rather than a
	// log. Both render nothing when unset.
	routingBlock := routing.Block{
		TaskID: run.task.ID, ProjectName: run.project.Name, Stage: stageID,
		Gate: string(stage.Gate), ArtifactDir: artifactDir,
		Capabilities: profileTokens(profile),
		Checks:       run.resolvedChecks,
	}
	if stage.SourcesVerdict() {
		routingBlock.VerdictPath = filepath.Join(artifactDir, agent.VerdictFileName)
	}
	// ADR 0003 D2/D6: the plan-approval routing pointers. PlanPath is set when
	// this stage is the pack's approval stage (the planner writes plan.md to the
	// path the orchestrator captures). ApprovedPlan is set for every stage after
	// the approval, pointing at the approved revision. PriorStages lists every
	// prior stage that produced a result.json, read from the durable revision
	// list so it survives a worktree Restore and a worker restart.
	if approval, hasApproval := run.taskPack.SourceWriteApproval(); hasApproval {
		if approval.Stage == stageID {
			routingBlock.PlanPath = filepath.Join(artifactDir, approval.Artifact)
		}
		routingBlock.ApprovedPlan = runner.approvedPlanRef(ctx, run, approval)
	}
	routingBlock.PriorStages = runner.priorStageRefs(ctx, run, stageID)
	// ADR 0003 D5: the orchestrator-produced diff is rendered for reviewer-role
	// stages. The produceDiff call runs at the start of the reviewer stage,
	// before invokeStage, so by the time we reach here the diff revisions exist.
	if pack.EffectiveRole(stageID, stage) == "reviewer" {
		routingBlock.Diff = run.diff
	}
	if transitionIn.verdict != "" && transitionIn.verdictPath != "" {
		routingBlock.ReviewFindings = &routing.ReviewRef{
			Stage: transitionIn.from, Path: transitionIn.verdictPath,
			Count: transitionIn.findingsCount,
		}
	}
	block := routing.Render(routingBlock)

	// Next sequence number + resume_of for this task. resume_of is set ONLY when
	// this invocation resumes a captured session (resumeSession != ""); setting
	// it for every invocation made the resume chain a lie — it pointed at the
	// previous invocation even for a fresh entry, blocking "was this a retry or
	// a resume" from being readable off the row. sequence keeps incrementing
	// unconditionally (every attempt is a distinct row).
	var seq int32 = 1
	var resumeOf sql.NullString
	var resumed *sqlc.StageInvocation
	if latest, err := runner.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{TaskID: run.task.ID, TenantID: run.task.TenantID}); err == nil {
		seq = latest.Sequence + 1
		if resumeSession != "" {
			resumeOf = nullStr(latest.ID)
			resumed = &latest
		}
	}

	cycle, err := runner.nextCycleForStage(ctx, run.task, stageID, resumed)
	if err != nil {
		runner.log.Error("compute cycle for stage", "task", run.task.ID, "stage", stageID, "error", err)
		return invocationOutcome{adapterErr: true}
	}

	invocation, err := runner.store.CreateStageInvocation(ctx, sqlc.CreateStageInvocationParams{
		TenantID: run.task.TenantID, UserID: run.task.UserID, TaskID: run.task.ID,
		Stage: stageID, Sequence: seq, ResumeOf: resumeOf,
		CapabilityProfile: toNullRaw(profileBytes),
		Cycle:             cycle,
	})
	if err != nil {
		runner.log.Error("create stage invocation", "task", run.task.ID, "stage", stageID, "error", err)
		return invocationOutcome{adapterErr: true}
	}

	// Record the effective profile as audit evidence before the run starts. A
	// denied capability is as much a part of the record as a granted one: this
	// is what makes "the invocation was deny-by-default" reconstructible later.
	runner.emit(ctx, run.task, EvCapabilityEnforced, map[string]any{
		"stage": stageID, "invocation": invocation.ID,
		"role": string(profile.Source.Role), "profile": profile,
		"cycle": invocation.Cycle,
	})

	eventCh, invokeErr := runner.adapter.Invoke(ctx, agent.Invocation{
		Workdir: run.worktree.Root, ArtifactDir: artifactDir,
		Prompt: stage.PromptText(), RoutingBlock: block,
		ResumeSession: resumeSession, Model: model,
		Instructions: agentInstructionFiles(run.instructionFiles),
		Profile:      profile,
	})
	if invokeErr != nil {
		// An unenforceable profile (caps.ErrUnenforceable) is a distinct stop
		// reason — the invocation never started because the runtime could not
		// honor the profile. Anything else is a plain adapter_error.
		stopReason := "adapter_error"
		if errors.Is(invokeErr, caps.ErrUnenforceable) {
			stopReason = "capability_unenforceable"
		}
		runner.finalize(ctx, invocation, run.task, "", stopReason, nil)
		runner.log.Error("invoke refused", "task", run.task.ID, "stage", stageID, "reason", stopReason, "error", invokeErr)
		return invocationOutcome{adapterErr: true}
	}

	var (
		sessionID  string
		telemetry  agent.Telemetry
		terminal   *agent.Result
		terminalEr error
	)
	for event := range eventCh {
		sessionID, telemetry, terminal, terminalEr = runner.observeEvent(event, run.task.ID, stageID, sessionID, telemetry, terminal, terminalEr)
	}
	if terminal != nil {
		sessionID = terminal.SessionID
		telemetry = terminal.Telemetry
		// Capture produced artifacts (result.json + the agent-declared artifact
		// paths) into the durable revisions store. This runs before the
		// invocation is finalized as successful, because a declared path that
		// escapes the worktree is a terminal outcome for the stage: the
		// orchestrator refuses to read it, so the stage did not produce the
		// output it claims and must not be recorded as though it had.
		artifactOutputs, captureErr := runner.captureStageOutputs(ctx, run, stageID, invocation.ID, artifactDir, &terminal.ResultJSON)
		if captureErr != nil {
			runner.finalize(ctx, invocation, run.task, sessionID, "artifact_rejected", nil)
			runner.log.Error("stage output rejected",
				"task", run.task.ID, "stage", stageID, "error", captureErr)
			return invocationOutcome{rejected: true}
		}
		runner.finalize(ctx, invocation, run.task, sessionID, "", &terminal.ResultJSON)
		// Record evidence of the prompt + model + effective capability profile
		// the adapter saw, plus the artifact revisions this stage produced. The
		// manifest service is nil in unit tests; AddEvidence is a no-op then.
		runner.recordStageEvidence(ctx, run, stageID, stage, model, profile, artifactOutputs)
		// ADR 0002: record the project-context channel (pinned instructions +
		// enumerated skills) and emit the live pinning signal. The section is
		// written on every successful stage so an empty project still seals
		// evidence_complete; a failed skill probe additionally records an
		// evidence gap.
		runner.recordContextEvidence(ctx, run, stageID)
		runner.emit(ctx, run.task, EvContextPinned, contextPinnedPayload(stageID, run))
		runner.emit(ctx, run.task, EvStageStopped, map[string]any{
			"stage": stageID, "session_id": sessionID, "status": string(terminal.ResultJSON.Status),
			"tokens": telemetry.Tokens.Total, "cost": telemetry.Cost,
		})
		return invocationOutcome{result: &terminal.ResultJSON}
	}
	// EventError: classify. A result.json read/parse failure is a parse error
	// (the agent ran but its output was unusable); anything else is an adapter
	// error (crash, stream failure, cancellation).
	reason := classifyAdapterFailure(terminalEr)
	runner.finalize(ctx, invocation, run.task, sessionID, reason, nil)
	return invocationOutcome{
		adapterErr: reason == "adapter_error",
		parseErr:   reason == "parse_error",
	}
}

// observeEvent folds one adapter stream event into the accumulator the drain
// loop carries. Stream chunks are forwarded live to the sink; result and error
// events are captured for post-loop classification. The accumulator is returned
// so the loop body stays a single expression with no local branching state.
func (runner *Runner) observeEvent(
	event agent.Event, taskID, stageID, sessionID string,
	telemetry agent.Telemetry, terminal *agent.Result, terminalEr error,
) (string, agent.Telemetry, *agent.Result, error) {
	switch event.Kind {
	case agent.EventStream:
		if runner.sink != nil && event.Chunk != "" {
			runner.sink(taskID, stageID, event.Chunk)
		}
	case agent.EventResult:
		terminal = event.Result
	case agent.EventError:
		terminalEr = event.Err
	}
	return sessionID, telemetry, terminal, terminalEr
}

// classifyAdapterFailure maps an adapter error to a stop reason. A result.json
// read/parse failure is a parse error (the agent ran but its output was
// unusable); anything else is an adapter error (crash, stream failure,
// cancellation). A nil error is treated as an adapter error only by the caller's
// contract — here nil simply yields the default adapter_error.
func classifyAdapterFailure(terminalEr error) string {
	if terminalEr != nil && strings.Contains(terminalEr.Error(), "result.json") {
		return "parse_error"
	}
	return "adapter_error"
}

// finalize writes the stage_invocation outcome. result may be nil.
func (runner *Runner) finalize(ctx context.Context, invocation sqlc.StageInvocation, task sqlc.Task, sessionID, stopReason string, result any) {
	var raw json.RawMessage
	if result != nil {
		if data, err := json.Marshal(result); err == nil {
			raw = data
		}
	}
	if err := runner.store.FinishStageInvocation(ctx, sqlc.FinishStageInvocationParams{
		ID:         invocation.ID,
		TenantID:   task.TenantID,
		SessionID:  nullStr(sessionID),
		StopReason: nullStr(stopReason),
		Result:     toNullRaw(raw),
	}); err != nil {
		runner.log.Error("finish stage invocation", "invocation", invocation.ID, "error", err)
	}
}

// nextCycleForStage computes the cycle for a fresh invocation of stageID per
// D4's table. This is the single place cycles are computed:
//
//   - a resume (resumed != nil) inherits the resumed invocation's cycle, so a
//     `continue` does not consume budget or inflate the counter;
//   - a fresh entry takes MAX(cycle for (task, stageID)) + 1. MaxCycleForStages
//     returns -1 when the stage never ran (the COALESCE sentinel), which maps
//     to cycle 0 — the first entry.
//
// Durability follows from deriving the value off committed rows: a worker
// restart recomputes the same answer, and a crash between "transition chosen"
// and "invocation created" leaves no row, so the recomputed value is identical.
func (runner *Runner) nextCycleForStage(ctx context.Context, task sqlc.Task, stageID string, resumed *sqlc.StageInvocation) (int32, error) {
	if resumed != nil {
		return resumed.Cycle, nil
	}
	maxCycle, err := runner.store.MaxCycleForStages(ctx, sqlc.MaxCycleForStagesParams{
		TaskID: task.ID, TenantID: task.TenantID, Column3: []string{stageID},
	})
	if err != nil {
		return 0, fmt.Errorf("load max cycle for stage %q: %w", stageID, err)
	}
	if maxCycle < 0 {
		// -1 sentinel: the stage has never run. First entry is cycle 0.
		return 0, nil
	}
	return maxCycle + 1, nil
}

// failTask transitions the task to failed and emits the reason. Used when the
// runner cannot proceed (bad pack, missing stage, evaluator error) — these are
// genuine failures, not retryable pause points.
func (runner *Runner) failTask(ctx context.Context, task sqlc.Task, cause error) error {
	runner.log.Error("runner failing task", "task", task.ID, "error", cause)
	if _, err := runner.store.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: task.TenantID, State: string(engine.StateFailed),
	}); err != nil {
		return fmt.Errorf("%w (and failed to mark task failed: %v)", cause, err)
	}
	runner.emit(ctx, task, EvTaskStateChanged, map[string]any{"from": task.State, "to": string(engine.StateFailed), "error": cause.Error()})
	// Seal the manifest with reason=failed so the partial evidence is still
	// the immutable record of what was attempted. The teardown job will run
	// the git-evidence + seal again; the seal is idempotent.
	failedTask, refreshErr := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: task.ID, TenantID: task.TenantID})
	if refreshErr == nil {
		runner.sealManifestAtTerminal(ctx, failedTask)
	}
	// Best-effort: schedule worktree teardown. A failed task's worktree is not
	// needed for recovery (the session, if any, is gone); remove it. Enqueuing
	// (not removing inline) serializes with the still-running driving job.
	if _, teardownErr := runner.store.EnqueueJob(ctx, sqlc.EnqueueJobParams{
		TenantID: task.TenantID, UserID: task.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
	}); teardownErr != nil {
		runner.log.Warn("enqueue teardown for failed task", "task", task.ID, "error", teardownErr)
	}
	return cause
}

// emit appends a meaningful event to the durable log (04 §7.1.5).
func (runner *Runner) emit(ctx context.Context, task sqlc.Task, eventType string, payload any) {
	var raw json.RawMessage = json.RawMessage("{}")
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			raw = data
		}
	}
	if _, err := runner.store.AppendEvent(ctx, sqlc.AppendEventParams{
		TenantID: task.TenantID, UserID: task.UserID, TaskID: nullStr(task.ID), Type: eventType, Payload: raw,
	}); err != nil {
		runner.log.Warn("emit event", "type", eventType, "task", task.ID, "error", err)
	}
}

// isClean reports whether the worktree has no uncommitted changes outside the
// ignored .agentum/ artifact tree. Drives the auto_if_clean gate. Approximation
// for MVP: any porcelain entry ⇒ not clean (conservative — surfaces for review
// rather than wrongly auto-advancing). result.json lives under .agentum/, which
// ensureIgnored excludes, so it does not count as a change.
func (runner *Runner) isClean(repoPath, taskID string) bool {
	wtPath := worktree.PathFor(repoPath, taskID)
	out, err := execGit(wtPath, "status", "--porcelain")
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(out)) == 0
}

// Event types the runner emits. The runner owns its taxonomy; the SSE layer
// frames whatever string the events table carries.
const (
	EvTaskStateChanged   = "task.state_changed"
	EvStageStarted       = "stage.started"
	EvStageStopped       = "stage.stopped"
	EvWorktreeCreated    = "task.worktree_created"
	EvWorktreeRemoved    = "task.worktree_removed"
	EvWorktreeReconciled = "task.worktree_reconciled"
	EvCheckpointRecorded = "task.checkpoint_recorded"
	EvTaskCleanedUp      = "task.cleaned_up"
	// EvStageTransition records a conditional transition resolution ({from, to,
	// condition, cycle, verdict}), emitted at the resolution point so the
	// branch is auditable even when the next stage never starts (e.g. budget
	// exhaustion stops the run before the target stage runs). Exhaustion itself
	// needs no new event type: task.state_changed already carries stop_reason.
	EvStageTransition = "stage.transition"
	// EvCapabilityEnforced records the effective capability profile granted to
	// a stage invocation, emitted before the adapter is invoked. The profile
	// (grants + denials + role + source inputs) is the audit evidence that the
	// invocation was deny-by-default; a later review reconstructs "what could
	// this run do" from it.
	EvCapabilityEnforced = "stage.capability_enforced"
	EvRevisionsSynced    = "task.revisions_synced"
	// EvArtifactRejected records that the orchestrator refused to ingest an
	// artifact the agent declared — because the path resolved outside the
	// worktree, or because the artifact scanner found credential-shaped content
	// under a reject policy. The event is the durable record that a declared
	// output is deliberately absent from the store, which a missing revision
	// alone cannot express.
	EvArtifactRejected = "stage.artifact_rejected"
	// EvProjectChecksRun records the orchestrator-owned project-check outcome at
	// the delivery boundary. The manifest body carries the full per-check
	// results; this event is the durable signal that Agentum ran its own checks
	// (not the agent's claim) against the checkpoint commit.
	EvProjectChecksRun = "task.project_checks_run"
	// EvDeliveryCommitDiverged records that the commit recorded as delivered
	// (result_commit) differs from the one the delivery checks verified. Emitted
	// at teardown when the two diverge — a human approval, a continue job, or a
	// filesystem change moved the branch tip between the checks and teardown.
	// The task is not failed (the human already approved); the divergence is
	// recorded as an evidence gap and the sealed manifest reads incomplete, which
	// is the signal a reviewer acts on.
	EvDeliveryCommitDiverged = "task.delivery_commit_diverged"
	// EvContextPinned records that the project-context channel was pinned for a
	// stage: how many instruction files and skills were in play, and how many
	// instruction files were truncated against the cap (ADR 0002). The manifest
	// body carries the full refs (hashes and sizes only); this event is the live
	// signal that the channel is active.
	EvContextPinned = "stage.context_pinned"
	// EvInstructionsRestored records one tamper reversal: a pre-stage hash check
	// found a worktree instruction copy had drifted from the pinned bytes and the
	// runner rewrote or removed it (ADR 0002 D4 layer 2). Carries the stage, the
	// path, the action, and the tampered hash — the tamper and its reversal both
	// land in the git lineage via the next checkpoint commit.
	EvInstructionsRestored = "task.instructions_restored"
)

// CancelRegistry lets the cancel HTTP handler abort an in-flight run by task id.
type CancelRegistry struct {
	mu     sync.Mutex
	byTask map[string]context.CancelFunc
}

// NewCancelRegistry returns an empty registry.
func NewCancelRegistry() *CancelRegistry {
	return &CancelRegistry{byTask: make(map[string]context.CancelFunc)}
}

// Register associates cancel with the task's in-flight run.
func (reg *CancelRegistry) Register(taskID string, cancel context.CancelFunc) {
	reg.mu.Lock()
	reg.byTask[taskID] = cancel
	reg.mu.Unlock()
}

// Unregister removes a task's registration. Safe to call when not registered.
func (reg *CancelRegistry) Unregister(taskID string) {
	reg.mu.Lock()
	delete(reg.byTask, taskID)
	reg.mu.Unlock()
}

// Cancel aborts the task's in-flight run, if any. Returns whether a run was
// active. Does not touch the FSM — the caller owns the transition.
func (reg *CancelRegistry) Cancel(taskID string) bool {
	reg.mu.Lock()
	cancelFn, ok := reg.byTask[taskID]
	delete(reg.byTask, taskID)
	reg.mu.Unlock()
	if !ok {
		return false
	}
	cancelFn()
	return true
}

// nullStr builds a sql.NullString; empty → invalid (NULL).
func nullStr(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
