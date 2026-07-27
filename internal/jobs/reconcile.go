package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"log/slog"

	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TaskStore is the task-state surface the reconciler needs beyond the job
// queue: probing for orphaned tasks and repairing them via the FSM. Declared
// here so the jobs package does not import the runner; the server wires the
// sqlc querier (or its tx wrapper) behind it.
type TaskStore interface {
	FindOrphanedRunningTasks(ctx context.Context, tenantID string) ([]sqlc.Task, error)
	UpdateTaskState(ctx context.Context, arg sqlc.UpdateTaskStateParams) (sqlc.Task, error)
	AppendEvent(ctx context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error)
}

// Reconciler repairs the queue and task state that a worker crash or an
// enqueue/transition race leaves behind (F.6.1 AC #6). It runs at boot AND on a
// periodic ticker — not only process startup — so a dead worker's stale lease
// or a task whose desired runnable state lost its job is repaired without a
// restart.
//
// Two probes:
//   - Stale jobs: status='running' whose heartbeat is older than staleAfter.
//     Re-queued (or failed past the poison bound).
//   - Orphaned tasks: state='running' with no live (pending/running) job — the
//     outcome of a crash between the FSM transition and EnqueueJob, or a job
//     that exhausted attempts. Repaired to paused_user_stop (interrupted) so a
//     human explicitly resumes; a half-run stage is never blindly replayed.
type Reconciler struct {
	tenantID string
	queue    Store
	tasks    TaskStore
	stale    time.Duration
	maxAtt   int
	log      *slog.Logger
}

// ReconcilerDeps bundles Reconciler construction.
type ReconcilerDeps struct {
	TenantID    string
	Queue       Store
	Tasks       TaskStore
	StaleAfter  time.Duration
	MaxAttempts int
	Log         *slog.Logger
}

const (
	// DefaultReconcileInterval is how often the background reconciler runs.
	// Tuned for "fast enough to notice a dead worker, slow enough to be cheap on
	// a single-host Postgres."
	DefaultReconcileInterval = 30 * time.Second
)

// NewReconciler builds a Reconciler with sensible defaults for unset fields.
func NewReconciler(deps ReconcilerDeps) *Reconciler {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	stale := deps.StaleAfter
	if stale == 0 {
		stale = DefaultStaleAfter
	}
	maxAtt := deps.MaxAttempts
	if maxAtt == 0 {
		maxAtt = DefaultMaxAttempts
	}
	return &Reconciler{
		tenantID: deps.TenantID, queue: deps.Queue, tasks: deps.Tasks,
		stale: stale, maxAtt: maxAtt, log: log,
	}
}

// Reconcile runs one pass of both probes. Safe to call from boot recovery or
// the periodic loop. Errors are returned but each sub-step is best-effort: a
// failure in one does not skip the others.
func (rec *Reconciler) Reconcile(ctx context.Context) error {
	staleErr := rec.requeueStaleJobs(ctx)
	orphanErr := rec.repairOrphanedTasks(ctx)
	if staleErr != nil {
		return fmt.Errorf("reconcile stale jobs: %w", staleErr)
	}
	if orphanErr != nil {
		return fmt.Errorf("reconcile orphaned tasks: %w", orphanErr)
	}
	return nil
}

// requeueStaleJobs mirrors Worker.Recover's queue pass. Shared here so the
// periodic loop repairs stale leases without a process restart.
func (rec *Reconciler) requeueStaleJobs(ctx context.Context) error {
	cutoff := time.Now().Add(-rec.stale)
	stale, err := rec.queue.RequeueStaleJobs(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("requeue stale: %w", err)
	}
	for _, job := range stale {
		if int(job.Attempts) >= rec.maxAtt {
			if failErr := rec.queue.FailJob(ctx, job.ID, fmt.Sprintf("exceeded max attempts (%d)", rec.maxAtt)); failErr != nil {
				rec.log.Error("reconcile: fail poison job", "job", job.ID, "error", failErr)
			}
			rec.log.Warn("reconcile: poison job failed", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
			continue
		}
		rec.log.Info("reconcile: requeued stale job", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
	}
	return nil
}

// repairOrphanedTasks transitions running tasks with no live job to
// paused_user_stop (interrupted). Conservative by design (04 §7.6): a human
// resumes explicitly. Session-id resume keeps the re-run cheap if a session was
// captured; a side-effectful stage is never blindly replayed.
func (rec *Reconciler) repairOrphanedTasks(ctx context.Context) error {
	orphaned, err := rec.tasks.FindOrphanedRunningTasks(ctx, rec.tenantID)
	if err != nil {
		return fmt.Errorf("find orphaned: %w", err)
	}
	for _, task := range orphaned {
		// Re-check the state inside the loop — another reconciler pass or a human
		// resume may have moved the task between the probe and the repair.
		if engine.TaskState(task.State) != engine.StateRunning {
			continue
		}
		if _, transitionErr := rec.tasks.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
			ID: task.ID, TenantID: task.TenantID, State: string(engine.StatePausedUserStop),
		}); transitionErr != nil {
			rec.log.Error("reconcile: pause orphaned task", "task", task.ID, "error", transitionErr)
			continue
		}
		if _, emitErr := rec.tasks.AppendEvent(ctx, sqlc.AppendEventParams{
			TenantID: task.TenantID, UserID: task.UserID,
			TaskID: nullStrEvent(task.ID), Type: "task.reconciled",
			Payload: []byte(`{"from":"running","to":"paused_user_stop","reason":"interrupted"}`),
		}); emitErr != nil {
			rec.log.Warn("reconcile: emit event", "task", task.ID, "error", emitErr)
		}
		rec.log.Info("reconcile: paused orphaned task", "task", task.ID, "from", "running", "to", "paused_user_stop")
	}
	return nil
}

// Start runs Reconcile on a ticker until ctx is cancelled. The first pass runs
// immediately (so boot is covered) and then on interval. Errors are logged, not
// fatal — a transient DB issue should not kill the reconciler.
func (rec *Reconciler) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	if err := rec.Reconcile(ctx); err != nil {
		rec.log.Warn("reconcile (boot)", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := rec.Reconcile(ctx); err != nil {
				rec.log.Warn("reconcile (periodic)", "error", err)
			}
		}
	}
}

// nullStrEvent adapts a task id to the nullable uuid shape AppendEvent expects.
// Empty → NULL (a non-task-scoped event); present → the task id.
func nullStrEvent(taskID string) sql.NullString {
	return sql.NullString{String: taskID, Valid: taskID != ""}
}
