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
func New(d Deps) *Runner {
	cancels := d.Cancels
	if cancels == nil {
		cancels = NewCancelRegistry()
	}
	wt := d.Worktrees
	if wt == nil {
		wt = worktree.New()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		store: d.Store, packs: d.Packs, adapter: d.Adapter, models: d.Models,
		wt: wt, cancels: cancels, sink: d.Sink, agentName: d.AgentName, log: log,
	}
}

// Cancels returns the runner's cancel registry, so the cancel HTTP handler can
// abort an in-flight run.
func (r *Runner) Cancels() *CancelRegistry { return r.cancels }

// Handle is the job-worker entry point. It dispatches by job kind; run /
// continue / advance all enter the shared stage loop from different entry
// points. cancel is a no-op here — the cancel HTTP handler aborts the active
// run via the registry and drives the FSM transition directly (04 §7.5).
func (r *Runner) Handle(ctx context.Context, job sqlc.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch job.Kind {
	case "run", "continue", "advance":
		return r.drive(ctx, job)
	case "cancel":
		return nil
	default:
		return fmt.Errorf("runner: unknown job kind %q", job.Kind)
	}
}

// drive performs the shared setup (load task + project + pack, create worktree)
// and enters the stage loop. It registers a cancel for the task so the cancel
// handler can abort the run mid-stage; a child context carries that cancellation
// down to the adapter.
func (r *Runner) drive(ctx context.Context, job sqlc.Job) error {
	task, err := r.store.GetTask(ctx, sqlc.GetTaskParams{ID: job.TaskID, TenantID: job.TenantID})
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	project, err := r.store.GetProject(ctx, sqlc.GetProjectParams{ID: task.ProjectID, TenantID: task.TenantID})
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	pk, err := r.packs.Resolve(ctx, task.PipelinePack)
	if err != nil {
		return r.failTask(ctx, task, fmt.Errorf("resolve pack %q: %w", task.PipelinePack, err))
	}

	wt, err := r.wt.Create(ctx, project.RepoPath, task.ID)
	if err != nil {
		return r.failTask(ctx, task, fmt.Errorf("create worktree: %w", err))
	}

	startStage, resumeSession, err := r.entryPoint(ctx, job, task, pk)
	if err != nil {
		return r.failTask(ctx, task, err)
	}

	// Register a cancel for this task so the cancel handler can abort the
	// in-flight run. The child context propagates that cancellation to the
	// adapter (the §5.1 seam).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.cancels.Register(job.TaskID, cancel)
	defer r.cancels.Unregister(job.TaskID)

	return r.runLoop(runCtx, task, project, pk, wt, startStage, resumeSession)
}

// entryPoint resolves where the loop starts and whether it resumes a session.
func (r *Runner) entryPoint(ctx context.Context, job sqlc.Job, task sqlc.Task, pk *pack.Pack) (stage, resume string, err error) {
	switch job.Kind {
	case "run":
		// A fresh run starts at the pack entry, unless a previous attempt set
		// current_stage before a crash — resume there.
		if task.CurrentStage.Valid {
			return task.CurrentStage.String, "", nil
		}
		return pk.Entry, "", nil
	case "continue":
		// Resume the current stage from its captured session id (non-destructive).
		latest, lerr := r.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{
			TaskID: task.ID, TenantID: task.TenantID,
		})
		if lerr != nil {
			return "", "", fmt.Errorf("find resume session: %w", lerr)
		}
		return task.CurrentStage.String, latest.SessionID.String, nil
	case "advance":
		// Past the gate: move to the current stage's declared transition target.
		cur, ok := pk.Stages[task.CurrentStage.String]
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
// state. resumeSession applies only to the first iteration.
func (r *Runner) runLoop(ctx context.Context, task sqlc.Task, project sqlc.Project, pk *pack.Pack, wt *worktree.Worktree, startStage, resumeSession string) error {
	stageID := startStage
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		stage, ok := pk.Stages[stageID]
		if !ok {
			return r.failTask(ctx, task, fmt.Errorf("pack stage %q not found", stageID))
		}

		// A terminal stage (no transitions) is an engine marker, not an agent
		// invocation: reaching it means the pipeline is complete. Fire the final
		// gate directly, without invoking the adapter (terminal stages omit a
		// prompt by the pack convention).
		if stage.Terminal() {
			task, err := r.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
				ID: task.ID, TenantID: task.TenantID,
				CurrentStage: nullStr(stageID), State: string(engine.StateRunning),
			})
			if err != nil {
				return fmt.Errorf("update current_stage (terminal): %w", err)
			}
			newState, ferr := engine.Next(engine.TaskState(task.State), engine.EventReachFinalGate)
			if ferr != nil {
				return r.failTask(ctx, task, fmt.Errorf("fsm reach_final_gate: %w", ferr))
			}
			if _, err := r.store.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
				ID: task.ID, TenantID: task.TenantID, State: string(newState),
			}); err != nil {
				return fmt.Errorf("persist final: %w", err)
			}
			r.emit(ctx, task, EvTaskStateChanged, map[string]any{"from": task.State, "to": string(newState), "stage": stageID})
			return nil
		}

		// Record current position; the task stays running through auto-advances.
		task, err := r.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
			ID: task.ID, TenantID: task.TenantID,
			CurrentStage: nullStr(stageID), State: string(engine.StateRunning),
		})
		if err != nil {
			return fmt.Errorf("update current_stage: %w", err)
		}
		r.emit(ctx, task, EvStageStarted, map[string]any{"stage": stageID, "gate": string(stage.Gate)})

		res, adapterErr, parseErr := r.invokeStage(ctx, task, project, pk, wt, stageID, stage, resumeSession)
		resumeSession = "" // only the first iteration resumes

		// If the run was cancelled (the cancel handler aborts it via the
		// registry), bow out without touching the FSM — the handler owns the
		// transition to cancelled. Otherwise the adapter_error pause would race
		// and overwrite it.
		if err := ctx.Err(); err != nil {
			return nil
		}

		dec, derr := Evaluate(StageInput{
			Result:       res,
			Stage:        stage,
			StageID:      stageID,
			Clean:        r.isClean(project.RepoPath, task.ID),
			AdapterError: adapterErr,
			ParseError:   parseErr,
		})
		if derr != nil {
			return r.failTask(ctx, task, fmt.Errorf("evaluate stage %q: %w", stageID, derr))
		}

		switch dec.Action {
		case ActionAdvance:
			stageID = dec.NextStage
			continue

		case ActionPause:
			newState, ferr := engine.Next(engine.TaskState(task.State), dec.FSMEvent)
			if ferr != nil {
				return r.failTask(ctx, task, fmt.Errorf("fsm %s --%s-->: %w", task.State, dec.FSMEvent, ferr))
			}
			if _, err := r.store.UpdateTaskStage(ctx, sqlc.UpdateTaskStageParams{
				ID: task.ID, TenantID: task.TenantID,
				CurrentStage: nullStr(stageID), State: string(newState),
			}); err != nil {
				return fmt.Errorf("persist pause: %w", err)
			}
			r.emit(ctx, task, EvTaskStateChanged, map[string]any{
				"from": task.State, "to": string(newState), "stop_reason": dec.StopReason, "stage": stageID,
			})
			return nil

		case ActionFinal:
			newState, ferr := engine.Next(engine.TaskState(task.State), engine.EventReachFinalGate)
			if ferr != nil {
				return r.failTask(ctx, task, fmt.Errorf("fsm reach_final_gate: %w", ferr))
			}
			if _, err := r.store.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
				ID: task.ID, TenantID: task.TenantID, State: string(newState),
			}); err != nil {
				return fmt.Errorf("persist final: %w", err)
			}
			r.emit(ctx, task, EvTaskStateChanged, map[string]any{"from": task.State, "to": string(newState), "stage": stageID})
			return nil
		}
	}
}

// invokeStage runs one stage through the adapter and records the outcome. It
// creates the stage_invocation row at start (so a crash leaves a partial
// record), drains the stream (forwarding chunks to the sink), and finalizes the
// row with session_id / stop_reason / parsed result.
func (r *Runner) invokeStage(ctx context.Context, task sqlc.Task, project sqlc.Project, pk *pack.Pack, wt *worktree.Worktree, stageID string, stage pack.Stage, resumeSession string) (res *agent.ResultJSON, adapterErr bool, parseErr bool) {
	artifactDir := worktree.ArtifactDir(wt.Root, task.ID, stageID)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		r.log.Error("create artifact dir", "dir", artifactDir, "error", err)
		return nil, true, false
	}

	tier := stage.Tier
	if tier == "" {
		tier = pk.Tiers.Default
	}
	model, _ := models.Resolve(r.models, r.agentName, tier) // best-effort; empty is acceptable

	block := routing.Render(routing.Block{
		TaskID: task.ID, ProjectName: project.Name, Stage: stageID,
		Gate: string(stage.Gate), ArtifactDir: artifactDir,
	})

	// Next sequence number + resume_of for this task.
	var seq int32 = 1
	resumeOf := sql.NullString{}
	if latest, err := r.store.LatestStageForTask(ctx, sqlc.LatestStageForTaskParams{TaskID: task.ID, TenantID: task.TenantID}); err == nil {
		seq = latest.Sequence + 1
		resumeOf = nullStr(latest.ID)
	}

	inv, err := r.store.CreateStageInvocation(ctx, sqlc.CreateStageInvocationParams{
		TenantID: task.TenantID, UserID: task.UserID, TaskID: task.ID,
		Stage: stageID, Sequence: seq, ResumeOf: resumeOf,
	})
	if err != nil {
		r.log.Error("create stage invocation", "task", task.ID, "stage", stageID, "error", err)
		return nil, true, false
	}

	ch, ierr := r.adapter.Invoke(ctx, agent.Invocation{
		Workdir: wt.Root, ArtifactDir: artifactDir,
		Prompt: stage.PromptText(), RoutingBlock: block,
		ResumeSession: resumeSession, Model: model,
	})
	if ierr != nil {
		r.finalize(ctx, inv, task, "", "adapter_error", nil)
		return nil, true, false
	}

	var (
		sessionID string
		tel       agent.Telemetry
		terminal  *agent.Result
		errTerm   error
	)
	for ev := range ch {
		switch ev.Kind {
		case agent.EventStream:
			if r.sink != nil && ev.Chunk != "" {
				r.sink(task.ID, stageID, ev.Chunk)
			}
		case agent.EventResult:
			terminal = ev.Result
		case agent.EventError:
			errTerm = ev.Err
		}
	}
	if terminal != nil {
		sessionID = terminal.SessionID
		tel = terminal.Telemetry
		r.finalize(ctx, inv, task, sessionID, "", &terminal.ResultJSON)
		r.emit(ctx, task, EvStageStopped, map[string]any{
			"stage": stageID, "session_id": sessionID, "status": string(terminal.ResultJSON.Status),
			"tokens": tel.Tokens.Total, "cost": tel.Cost,
		})
		return &terminal.ResultJSON, false, false
	}
	// EventError: classify. A result.json read/parse failure is a parse error
	// (the agent ran but its output was unusable); anything else is an adapter
	// error (crash, stream failure, cancellation).
	reason := "adapter_error"
	if errTerm != nil && strings.Contains(errTerm.Error(), "result.json") {
		reason = "parse_error"
	}
	r.finalize(ctx, inv, task, sessionID, reason, nil)
	return nil, reason == "adapter_error", reason == "parse_error"
}

// finalize writes the stage_invocation outcome. result may be nil.
func (r *Runner) finalize(ctx context.Context, inv sqlc.StageInvocation, task sqlc.Task, sessionID, stopReason string, result any) {
	var raw json.RawMessage
	if result != nil {
		if data, err := json.Marshal(result); err == nil {
			raw = data
		}
	}
	if err := r.store.FinishStageInvocation(ctx, sqlc.FinishStageInvocationParams{
		ID:         inv.ID,
		TenantID:   task.TenantID,
		SessionID:  nullStr(sessionID),
		StopReason: nullStr(stopReason),
		Result:     toNullRaw(raw),
	}); err != nil {
		r.log.Error("finish stage invocation", "invocation", inv.ID, "error", err)
	}
}

// failTask transitions the task to failed and emits the reason. Used when the
// runner cannot proceed (bad pack, missing stage, evaluator error) — these are
// genuine failures, not retryable pause points.
func (r *Runner) failTask(ctx context.Context, task sqlc.Task, cause error) error {
	r.log.Error("runner failing task", "task", task.ID, "error", cause)
	if _, err := r.store.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: task.TenantID, State: string(engine.StateFailed),
	}); err != nil {
		return fmt.Errorf("%w (and failed to mark task failed: %v)", cause, err)
	}
	r.emit(ctx, task, EvTaskStateChanged, map[string]any{"from": task.State, "to": string(engine.StateFailed), "error": cause.Error()})
	return cause
}

// emit appends a meaningful event to the durable log (04 §7.1.5).
func (r *Runner) emit(ctx context.Context, task sqlc.Task, typ string, payload any) {
	var raw json.RawMessage = json.RawMessage("{}")
	if payload != nil {
		if data, err := json.Marshal(payload); err == nil {
			raw = data
		}
	}
	if _, err := r.store.AppendEvent(ctx, sqlc.AppendEventParams{
		TenantID: task.TenantID, UserID: task.UserID, TaskID: nullStr(task.ID), Type: typ, Payload: raw,
	}); err != nil {
		r.log.Warn("emit event", "type", typ, "task", task.ID, "error", err)
	}
}

// isClean reports whether the worktree has no uncommitted changes outside the
// ignored .agentum/ artifact tree. Drives the auto_if_clean gate. Approximation
// for MVP: any porcelain entry ⇒ not clean (conservative — surfaces for review
// rather than wrongly auto-advancing). result.json lives under .agentum/, which
// ensureIgnored excludes, so it does not count as a change.
func (r *Runner) isClean(repoPath, taskID string) bool {
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
)

// CancelRegistry lets the cancel HTTP handler abort an in-flight run by task id.
type CancelRegistry struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

// NewCancelRegistry returns an empty registry.
func NewCancelRegistry() *CancelRegistry {
	return &CancelRegistry{m: make(map[string]context.CancelFunc)}
}

// Register associates cancel with the task's in-flight run.
func (r *CancelRegistry) Register(taskID string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.m[taskID] = cancel
	r.mu.Unlock()
}

// Unregister removes a task's registration. Safe to call when not registered.
func (r *CancelRegistry) Unregister(taskID string) {
	r.mu.Lock()
	delete(r.m, taskID)
	r.mu.Unlock()
}

// Cancel aborts the task's in-flight run, if any. Returns whether a run was
// active. Does not touch the FSM — the caller owns the transition.
func (r *CancelRegistry) Cancel(taskID string) bool {
	r.mu.Lock()
	c, ok := r.m[taskID]
	delete(r.m, taskID)
	r.mu.Unlock()
	if !ok {
		return false
	}
	c()
	return true
}

// nullStr builds a sql.NullString; empty → invalid (NULL).
func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
