package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"log/slog"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/engine"
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
	CreateStageInvocation(ctx context.Context, arg sqlc.CreateStageInvocationParams) (sqlc.StageInvocation, error)
	FinishStageInvocation(ctx context.Context, arg sqlc.FinishStageInvocationParams) error
	LatestStageForTask(ctx context.Context, arg sqlc.LatestStageForTaskParams) (sqlc.StageInvocation, error)
	AppendEvent(ctx context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error)
	EnqueueJob(ctx context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error)
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
	log       *slog.Logger
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
	Log       *slog.Logger
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
	return &Runner{
		store: deps.Store, packs: deps.Packs, adapter: deps.Adapter, models: deps.Models,
		wt: worktreeManager, cancels: cancels, sink: deps.Sink, agentName: deps.AgentName, log: log,
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
	case "cancel":
		return nil
	default:
		return fmt.Errorf("runner: unknown job kind %q", job.Kind)
	}
}

// teardown removes the task's worktree once it has reached a terminal state
// (done/cancelled/failed). Idempotent: a missing worktree is a no-op. Enqueued
// by the cancel/approve handlers and by failTask; the worker claims it after the
// driving run job is done, so it never races the runner (04 §7.1.3).
func (runner *Runner) teardown(ctx context.Context, job sqlc.Job) error {
	task, err := runner.store.GetTask(ctx, sqlc.GetTaskParams{ID: job.TaskID, TenantID: job.TenantID})
	if err != nil {
		return fmt.Errorf("teardown: load task: %w", err)
	}
	project, err := runner.store.GetProject(ctx, sqlc.GetProjectParams{ID: task.ProjectID, TenantID: task.TenantID})
	if err != nil {
		return fmt.Errorf("teardown: load project: %w", err)
	}
	if err := runner.wt.Remove(ctx, project.RepoPath, task.ID); err != nil {
		runner.log.Error("teardown worktree", "task", task.ID, "error", err)
		return err
	}
	runner.emit(ctx, task, EvWorktreeRemoved, map[string]any{"stage": task.CurrentStage.String})
	return nil
}

// drive performs the shared setup (load task + project + pack, create worktree)
// and enters the stage loop. It registers a cancel for the task so the cancel
// handler can abort the run mid-stage; a child context carries that cancellation
// down to the adapter.
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

	taskWorktree, err := runner.wt.Create(ctx, project.RepoPath, task.ID)
	if err != nil {
		return runner.failTask(ctx, task, fmt.Errorf("create worktree: %w", err))
	}

	startStage, resumeSession, err := runner.entryPoint(ctx, job, task, taskPack)
	if err != nil {
		return runner.failTask(ctx, task, err)
	}

	// Register a cancel for this task so the cancel handler can abort the
	// in-flight run. The child context propagates that cancellation to the
	// adapter (the §5.1 seam).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runner.cancels.Register(job.TaskID, cancel)
	defer runner.cancels.Unregister(job.TaskID)

	run := stageRun{task: task, project: project, taskPack: taskPack, worktree: taskWorktree}
	return runner.runLoop(runCtx, run, startStage, resumeSession)
}

// stageRun bundles the per-task state the loop and adapter invocation share.
// Loading it once in drive() keeps runLoop/invokeStage signatures under the
// parameter-count lint bound and makes the per-stage data flow explicit.
type stageRun struct {
	task     sqlc.Task
	project  sqlc.Project
	taskPack *pack.Pack
	worktree *worktree.Worktree
}

// entryPoint resolves where the loop starts and whether it resumes a session.
func (runner *Runner) entryPoint(ctx context.Context, job sqlc.Job, task sqlc.Task, taskPack *pack.Pack) (stage, resume string, err error) {
	switch job.Kind {
	case "run":
		// A fresh run starts at the pack entry, unless a previous attempt set
		// current_stage before a crash — resume there.
		if task.CurrentStage.Valid {
			return task.CurrentStage.String, "", nil
		}
		return taskPack.Entry, "", nil
	case "continue":
		// Resume the current stage from its captured session id (non-destructive).
		latest, latestErr := runner.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{
			TaskID: task.ID, TenantID: task.TenantID,
		})
		if latestErr != nil {
			return "", "", fmt.Errorf("find resume session: %w", latestErr)
		}
		return task.CurrentStage.String, latest.SessionID.String, nil
	case "advance":
		// Past the gate: move to the current stage's declared transition target.
		cur, ok := taskPack.Stages[task.CurrentStage.String]
		if !ok {
			return "", "", fmt.Errorf("advance: current stage %q not in pack", task.CurrentStage.String)
		}
		if len(cur.Transitions) == 0 {
			return "", "", fmt.Errorf("advance: stage %q has no transition", task.CurrentStage.String)
		}
		return cur.Transitions[0].To, "", nil
	}
	return "", "", fmt.Errorf("entryPoint: unsupported kind %q", job.Kind)
}

// runLoop walks the pack's stages from startStage, invoking the adapter per
// stage and applying the evaluator's decision, until a pause point or terminal
// state. resumeSession applies only to the first iteration. The per-stage body
// lives in processStage; runLoop stays a flat claim-retry loop.
func (runner *Runner) runLoop(ctx context.Context, run stageRun, startStage, resumeSession string) error {
	stageID := startStage
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, err := runner.processStage(ctx, run, stageID, resumeSession)
		if err != nil {
			return err
		}
		if outcome.done {
			return nil
		}
		stageID = outcome.nextStage
		resumeSession = "" // only the first iteration resumes
	}
}

// stageOutcome is processStage's verdict for one iteration of runLoop. Exactly
// one of done or nextStage applies: done ends the loop (terminal/pause/final);
// nextStage advances the loop to the next pack stage.
type stageOutcome struct {
	nextStage string
	done      bool
}

// processStage runs one iteration: look up the stage, dispatch (terminal marker
// vs adapter invocation), evaluate the outcome, and apply the resulting
// decision. The caller owns loop control (continue / stop) via stageOutcome.
func (runner *Runner) processStage(ctx context.Context, run stageRun, stageID, resumeSession string) (stageOutcome, error) {
	stage, ok := run.taskPack.Stages[stageID]
	if !ok {
		return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("pack stage %q not found", stageID))
	}

	// A terminal stage (no transitions) is an engine marker, not an agent
	// invocation: reaching it means the pipeline is complete. Fire the final
	// gate directly, without invoking the adapter (terminal stages omit a
	// prompt by the pack convention).
	if stage.Terminal() {
		if err := runner.reachTerminalStage(ctx, run.task, stageID); err != nil {
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

	result, adapterErr, parseErr := runner.invokeStage(ctx, run, stageID, stage, resumeSession)

	// If the run was cancelled (the cancel handler aborts it via the registry),
	// bow out without touching the FSM — the handler owns the transition to
	// cancelled. Otherwise the adapter_error pause would race and overwrite it.
	if err := ctx.Err(); err != nil {
		return stageOutcome{done: true}, nil
	}

	decision, err := Evaluate(StageInput{
		Result:       result,
		Stage:        stage,
		StageID:      stageID,
		Clean:        runner.isClean(run.project.RepoPath, run.task.ID),
		AdapterError: adapterErr,
		ParseError:   parseErr,
	})
	if err != nil {
		return stageOutcome{}, runner.failTask(ctx, run.task, fmt.Errorf("evaluate stage %q: %w", stageID, err))
	}

	switch decision.Action {
	case ActionAdvance:
		return stageOutcome{nextStage: decision.NextStage}, nil
	case ActionPause:
		return stageOutcome{done: true}, runner.applyPauseDecision(ctx, run.task, decision, stageID)
	case ActionFinal:
		return stageOutcome{done: true}, runner.transitionToFinalState(ctx, run.task, stageID)
	}
	return stageOutcome{}, fmt.Errorf("runner: unknown decision action %d", decision.Action)
}

// reachTerminalStage pins current_stage on a terminal (no-transitions) stage,
// then fires the final gate.
func (runner *Runner) reachTerminalStage(ctx context.Context, task sqlc.Task, stageID string) error {
	updatedTask, err := runner.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
		ID: task.ID, TenantID: task.TenantID,
		CurrentStage: nullStr(stageID), State: string(engine.StateRunning),
	})
	if err != nil {
		return fmt.Errorf("update current_stage (terminal): %w", err)
	}
	return runner.transitionToFinalState(ctx, updatedTask, stageID)
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
	return nil
}

// transitionToFinalState advances the FSM to awaiting_memory_commit and emits
// the state change. Shared by reachTerminalStage (terminal marker reached) and
// the ActionFinal path (complete outcome on a non-terminal stage) — both reach
// the same final gate, only the pin-current_stage step differs.
func (runner *Runner) transitionToFinalState(ctx context.Context, task sqlc.Task, stageID string) error {
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

// invokeStage runs one stage through the adapter and records the outcome. It
// creates the stage_invocation row at start (so a crash leaves a partial
// record), drains the stream (forwarding chunks to the sink), and finalizes the
// row with session_id / stop_reason / parsed result. Returns the parsed result
// (or nil) plus adapter-error / parse-error flags for the evaluator.
func (runner *Runner) invokeStage(ctx context.Context, run stageRun, stageID string, stage pack.Stage, resumeSession string) (*agent.ResultJSON, bool, bool) {
	artifactDir := worktree.ArtifactDir(run.worktree.Root, run.task.ID, stageID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		runner.log.Error("create artifact dir", "dir", artifactDir, "error", err)
		return nil, true, false
	}

	tier := stage.Tier
	if tier == "" {
		tier = run.taskPack.Tiers.Default
	}
	model, _ := models.Resolve(runner.models, runner.agentName, tier) // best-effort; empty is acceptable

	block := routing.Render(routing.Block{
		TaskID: run.task.ID, ProjectName: run.project.Name, Stage: stageID,
		Gate: string(stage.Gate), ArtifactDir: artifactDir,
	})

	// Next sequence number + resume_of for this task.
	var seq int32 = 1
	resumeOf := sql.NullString{}
	if latest, err := runner.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{TaskID: run.task.ID, TenantID: run.task.TenantID}); err == nil {
		seq = latest.Sequence + 1
		resumeOf = nullStr(latest.ID)
	}

	invocation, err := runner.store.CreateStageInvocation(ctx, sqlc.CreateStageInvocationParams{
		TenantID: run.task.TenantID, UserID: run.task.UserID, TaskID: run.task.ID,
		Stage: stageID, Sequence: seq, ResumeOf: resumeOf,
	})
	if err != nil {
		runner.log.Error("create stage invocation", "task", run.task.ID, "stage", stageID, "error", err)
		return nil, true, false
	}

	eventCh, invokeErr := runner.adapter.Invoke(ctx, agent.Invocation{
		Workdir: run.worktree.Root, ArtifactDir: artifactDir,
		Prompt: stage.PromptText(), RoutingBlock: block,
		ResumeSession: resumeSession, Model: model,
	})
	if invokeErr != nil {
		runner.finalize(ctx, invocation, run.task, "", "adapter_error", nil)
		return nil, true, false
	}

	var (
		sessionID  string
		telemetry  agent.Telemetry
		terminal   *agent.Result
		terminalEr error
	)
	for event := range eventCh {
		switch event.Kind {
		case agent.EventStream:
			if runner.sink != nil && event.Chunk != "" {
				runner.sink(run.task.ID, stageID, event.Chunk)
			}
		case agent.EventResult:
			terminal = event.Result
		case agent.EventError:
			terminalEr = event.Err
		}
	}
	if terminal != nil {
		sessionID = terminal.SessionID
		telemetry = terminal.Telemetry
		runner.finalize(ctx, invocation, run.task, sessionID, "", &terminal.ResultJSON)
		runner.emit(ctx, run.task, EvStageStopped, map[string]any{
			"stage": stageID, "session_id": sessionID, "status": string(terminal.ResultJSON.Status),
			"tokens": telemetry.Tokens.Total, "cost": telemetry.Cost,
		})
		return &terminal.ResultJSON, false, false
	}
	// EventError: classify. A result.json read/parse failure is a parse error
	// (the agent ran but its output was unusable); anything else is an adapter
	// error (crash, stream failure, cancellation).
	reason := "adapter_error"
	if terminalEr != nil && strings.Contains(terminalEr.Error(), "result.json") {
		reason = "parse_error"
	}
	runner.finalize(ctx, invocation, run.task, sessionID, reason, nil)
	return nil, reason == "adapter_error", reason == "parse_error"
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
	EvTaskStateChanged = "task.state_changed"
	EvStageStarted     = "stage.started"
	EvStageStopped     = "stage.stopped"
	EvWorktreeCreated  = "task.worktree_created"
	EvWorktreeRemoved  = "task.worktree_removed"
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
