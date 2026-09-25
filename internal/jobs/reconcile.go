package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"log/slog"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// RunStore is the run-state surface the reconciler needs beyond the job
// queue: probing for orphaned runs and repairing them via the FSM. Declared
// here so the jobs package does not import the runner; the server wires the
// sqlc querier (or its tx wrapper) behind it.
type RunStore interface {
	FindOrphanedRunningRuns(ctx context.Context, tenantID string) ([]sqlc.Run, error)
	UpdateRunState(ctx context.Context, arg sqlc.UpdateRunStateParams) (sqlc.Run, error)
	AppendEvent(ctx context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error)
}

// PublicationStore is the publication-queue surface the reconciler's third
// probe needs: finding lost publications and re-enqueueing their jobs. The
// probe restores work the system did not finish; it does not retry outcomes
// the publication coordinator recorded, and that boundary lives in the
// query, not here. Declared as a separate interface (and optional in Deps)
// so a test reconciler — or a build without publication — runs the first
// two probes alone.
type PublicationStore interface {
	FindStalePublications(ctx context.Context, arg sqlc.FindStalePublicationsParams) ([]sqlc.RunPublication, error)
	EnqueueJob(ctx context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error)
}

// Reconciler repairs the queue and run state that a worker crash or an
// enqueue/transition race leaves behind (F.6.1 AC #6). It runs at boot AND on a
// periodic ticker — not only process startup — so a dead worker's stale lease
// or a run whose desired runnable state lost its job is repaired without a
// restart.
//
// Three probes:
//   - Stale jobs: status='running' whose heartbeat is older than staleAfter.
//     Re-queued (or failed past the poison bound).
//   - Orphaned runs: state='running' with no live (pending/running) job — the
//     outcome of a crash between the FSM transition and EnqueueJob, or a job
//     that exhausted attempts. Repaired to paused_user_stop (interrupted) so a
//     human explicitly resumes; a half-run stage is never blindly replayed.
//   - Lost publications: pending with no live publish job, or publishing with
//     an expired lease — the row exists, the work it describes was never
//     finished. Re-enqueued, bounded by the same attempts bound as the queue.
//     A failed or blocked publication is never returned by the probe: the
//     system restores unfinished work, it does not retry a recorded outcome.
type Reconciler struct {
	tenantID     string
	queue        Store
	runs         RunStore
	publications PublicationStore
	stale        time.Duration
	maxAtt       int
	log          *slog.Logger
}

// ReconcilerDeps bundles Reconciler construction.
type ReconcilerDeps struct {
	TenantID     string
	Queue        Store
	Runs         RunStore
	Publications PublicationStore
	StaleAfter   time.Duration
	MaxAttempts  int
	Log          *slog.Logger
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
		tenantID: deps.TenantID, queue: deps.Queue, runs: deps.Runs,
		publications: deps.Publications,
		stale:        stale, maxAtt: maxAtt, log: log,
	}
}

// Reconcile runs one pass of every probe. Safe to call from boot recovery or
// the periodic loop. Errors are returned but each sub-step is best-effort: a
// failure in one does not skip the others.
func (rec *Reconciler) Reconcile(ctx context.Context) error {
	staleErr := rec.requeueStaleJobs(ctx)
	orphanErr := rec.repairOrphanedRuns(ctx)
	publicationErr := rec.requeueLostPublications(ctx)
	if staleErr != nil {
		return fmt.Errorf("reconcile stale jobs: %w", staleErr)
	}
	if orphanErr != nil {
		return fmt.Errorf("reconcile orphaned runs: %w", orphanErr)
	}
	if publicationErr != nil {
		return fmt.Errorf("reconcile publications: %w", publicationErr)
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
			rec.log.Warn("reconcile: poison job failed", "job", job.ID, "run", job.RunID, "attempts", job.Attempts)
			continue
		}
		rec.log.Info("reconcile: requeued stale job", "job", job.ID, "run", job.RunID, "attempts", job.Attempts)
	}
	return nil
}

// repairOrphanedRuns transitions running runs with no live job to
// paused_user_stop (interrupted). Conservative by design (04 §7.6): a human
// resumes explicitly. Session-id resume keeps the re-run cheap if a session was
// captured; a side-effectful stage is never blindly replayed.
func (rec *Reconciler) repairOrphanedRuns(ctx context.Context) error {
	orphaned, err := rec.runs.FindOrphanedRunningRuns(ctx, rec.tenantID)
	if err != nil {
		return fmt.Errorf("find orphaned: %w", err)
	}
	for _, run := range orphaned {
		// Re-check the state inside the loop — another reconciler pass or a human
		// resume may have moved the run between the probe and the repair.
		if engine.RunState(run.State) != engine.StateRunning {
			continue
		}
		if _, transitionErr := rec.runs.UpdateRunState(ctx, sqlc.UpdateRunStateParams{
			ID: run.ID, TenantID: run.TenantID, State: string(engine.StatePausedUserStop),
		}); transitionErr != nil {
			rec.log.Error("reconcile: pause orphaned run", "run", run.ID, "error", transitionErr)
			continue
		}
		if _, emitErr := rec.runs.AppendEvent(ctx, sqlc.AppendEventParams{
			TenantID: run.TenantID, UserID: run.UserID,
			RunID: nullStrEvent(run.ID), Type: "run.reconciled",
			Payload: []byte(`{"from":"running","to":"paused_user_stop","reason":"interrupted"}`),
			Actor:   string(authz.ActorSystem),
		}); emitErr != nil {
			rec.log.Warn("reconcile: emit event", "run", run.ID, "error", emitErr)
		}
		rec.log.Info("reconcile: paused orphaned run", "run", run.ID, "from", "running", "to", "paused_user_stop")
	}
	return nil
}

// requeueLostPublications restores publications whose work was lost: a row
// in pending whose job never enqueued (or already finished without the row
// moving), and a row in publishing whose lease expired — its worker died
// mid-attempt. Each gets a fresh publish job; the lease in the row is what
// keeps two workers from delivering the same run. The attempts bound is
// applied by the probe's query, so a publication the system keeps losing is
// eventually left for a person to see.
func (rec *Reconciler) requeueLostPublications(ctx context.Context) error {
	if rec.publications == nil {
		return nil
	}
	lost, err := rec.publications.FindStalePublications(ctx, sqlc.FindStalePublicationsParams{
		TenantID: rec.tenantID, Attempts: int32(rec.maxAtt),
	})
	if err != nil {
		return fmt.Errorf("find lost: %w", err)
	}
	for _, publication := range lost {
		if _, enqueueErr := rec.publications.EnqueueJob(ctx, sqlc.EnqueueJobParams{
			TenantID: publication.TenantID, UserID: publication.UserID,
			RunID: publication.RunID, Kind: "publish", Payload: []byte("{}"),
		}); enqueueErr != nil {
			rec.log.Error("reconcile: requeue publication", "run", publication.RunID, "error", enqueueErr)
			continue
		}
		rec.log.Info("reconcile: requeued publication", "run", publication.RunID, "state", publication.State)
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

// nullStrEvent adapts a run id to the nullable uuid shape AppendEvent expects.
// Empty → NULL (a non-run-scoped event); present → the run id.
func nullStrEvent(runID string) sql.NullString {
	return sql.NullString{String: runID, Valid: runID != ""}
}
