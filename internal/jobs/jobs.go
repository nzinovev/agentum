// Package jobs is the Postgres-backed job queue and worker for the execution
// model (04 §7.5). HTTP handlers enqueue a job and return; a worker claims it
// via FOR UPDATE SKIP LOCKED, drives it through the runner (the Handler), and
// records the outcome. A heartbeat lets the boot recovery pass detect a worker
// that died mid-run and re-queue its job, bounded by a poison-attempts limit.
//
// The queue lives in Postgres (no Redis): it is transactional with task state,
// needs no new infrastructure, and survives a single-host restart.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"log/slog"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Handler executes one job. The worker calls it after claiming; nil = success,
// error = the job is failed (last_error recorded). A handler that reaches a
// pause point returns nil (the task is already paused in the DB).
type Handler interface {
	Handle(ctx context.Context, job sqlc.Job) error
}

// Store is the queue surface the worker uses, in plain Go types. The nullable
// columns (worker_id, heartbeat_at, last_error) are adapted by QueueStore so the
// worker logic stays free of sql.Null* handling.
type Store interface {
	ClaimNextJob(ctx context.Context, workerID string) (sqlc.Job, error)
	CompleteJob(ctx context.Context, id int64) error
	FailJob(ctx context.Context, id int64, lastError string) error
	BumpHeartbeat(ctx context.Context, id int64) error
	RequeueStaleJobs(ctx context.Context, before time.Time) ([]sqlc.Job, error)
}

// QueueStore adapts *sqlc.Queries to Store, coercing the worker's plain values
// to the nullable column types sqlc generated.
type QueueStore struct {
	Q *sqlc.Queries
}

func (queue QueueStore) ClaimNextJob(ctx context.Context, workerID string) (sqlc.Job, error) {
	return queue.Q.ClaimNextJob(ctx, nullStr(workerID))
}

func (queue QueueStore) CompleteJob(ctx context.Context, id int64) error {
	return queue.Q.CompleteJob(ctx, id)
}

func (queue QueueStore) FailJob(ctx context.Context, id int64, lastError string) error {
	return queue.Q.FailJob(ctx, sqlc.FailJobParams{ID: id, LastError: nullStr(lastError)})
}

func (queue QueueStore) BumpHeartbeat(ctx context.Context, id int64) error {
	return queue.Q.BumpHeartbeat(ctx, id)
}

func (queue QueueStore) RequeueStaleJobs(ctx context.Context, before time.Time) ([]sqlc.Job, error) {
	return queue.Q.RequeueStaleJobs(ctx, sql.NullTime{Time: before, Valid: true})
}

func nullStr(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// Enqueuer inserts a pending job. Used by HTTP handlers; kept in this package so
// the queue's write surface is named in one place.
type Enqueuer interface {
	EnqueueJob(ctx context.Context, params sqlc.EnqueueJobParams) (sqlc.Job, error)
}

// Defaults for the worker knobs. Tunable via Deps; these match 04 §7.5.
const (
	DefaultPoll        = 500 * time.Millisecond
	DefaultHeartbeat   = 5 * time.Second
	DefaultStaleAfter  = 60 * time.Second
	DefaultMaxAttempts = 3
)

// Worker claims and runs jobs until its context is cancelled. One process runs
// one worker (or a small pool, each its own Worker); they share the table and
// claim concurrently-safe via FOR UPDATE SKIP LOCKED.
type Worker struct {
	store       Store
	handler     Handler
	workerID    string
	poll        time.Duration
	heartbeat   time.Duration
	staleAfter  time.Duration
	maxAttempts int
	log         *slog.Logger
}

// Deps bundles Worker construction.
type Deps struct {
	Store       Store
	Handler     Handler
	WorkerID    string
	Poll        time.Duration
	Heartbeat   time.Duration
	StaleAfter  time.Duration
	MaxAttempts int
	Log         *slog.Logger
}

// New builds a Worker with sensible defaults for unset fields.
func New(deps Deps) *Worker {
	wid := deps.WorkerID
	if wid == "" {
		wid = defaultWorkerID()
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	maxAttempts := deps.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}
	return &Worker{
		store: deps.Store, handler: deps.Handler, workerID: wid,
		poll: orDefault(deps.Poll, DefaultPoll), heartbeat: orDefault(deps.Heartbeat, DefaultHeartbeat),
		staleAfter: orDefault(deps.StaleAfter, DefaultStaleAfter), maxAttempts: maxAttempts, log: log,
	}
}

// Start is the claim loop. It blocks until ctx is cancelled; run it in its own
// goroutine. Errors during claim are logged and the loop backs off briefly so a
// transient DB issue does not spin.
func (worker *Worker) Start(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := worker.store.ClaimNextJob(ctx, worker.workerID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				worker.log.Error("claim job", "error", err)
			}
			if err := sleep(ctx, worker.poll); err != nil {
				return
			}
			continue
		}
		worker.run(ctx, job)
	}
}

// run drives one job: a heartbeat goroutine refreshes heartbeat_at while the
// handler runs, so the recovery pass can detect a dead worker; the outcome is
// recorded as done or failed. The handler sees the worker's ctx so cancelling
// the worker (shutdown) aborts the in-flight run too.
func (worker *Worker) run(ctx context.Context, job sqlc.Job) {
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go worker.heartbeatLoop(heartbeatCtx, job.ID)

	err := worker.handler.Handle(ctx, job)
	stopHeartbeat()

	if err == nil {
		if completeErr := worker.store.CompleteJob(ctx, job.ID); completeErr != nil {
			worker.log.Error("complete job", "job", job.ID, "error", completeErr)
		}
		return
	}
	worker.log.Warn("job failed", "job", job.ID, "task", job.TaskID, "kind", job.Kind, "error", err)
	if failErr := worker.store.FailJob(ctx, job.ID, err.Error()); failErr != nil {
		worker.log.Error("fail job", "job", job.ID, "error", failErr)
	}
}

// heartbeatLoop bumps heartbeat_at until ctx is cancelled (job done or worker
// stop). Named distinctly from the heartbeat field.
func (worker *Worker) heartbeatLoop(ctx context.Context, jobID int64) {
	ticker := time.NewTicker(worker.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := worker.store.BumpHeartbeat(ctx, jobID); err != nil {
				worker.log.Warn("heartbeat", "job", jobID, "error", err)
			}
		}
	}
}

// Recover runs at boot, before the worker starts. It re-queues jobs a previous
// worker died on (heartbeat older than staleAfter), failing any over the poison
// bound so a bad job cannot loop forever (04 §7.5–§7.6).
func (worker *Worker) Recover(ctx context.Context) error {
	cutoff := time.Now().Add(-worker.staleAfter)
	stale, err := worker.store.RequeueStaleJobs(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("recover: requeue stale: %w", err)
	}
	for _, job := range stale {
		if int(job.Attempts) >= worker.maxAttempts {
			// Over the bound: fail it rather than re-queueing forever. The task
			// is left in its current state; the operator inspects and resumes.
			if failErr := worker.store.FailJob(ctx, job.ID, fmt.Sprintf("exceeded max attempts (%d)", worker.maxAttempts)); failErr != nil {
				worker.log.Error("recover: fail poison job", "job", job.ID, "error", failErr)
			}
			worker.log.Warn("recover: poison job failed", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
			continue
		}
		worker.log.Info("recover: requeued stale job", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
	}
	return nil
}

// orDefault returns value when non-zero, else fallback.
func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// sleep returns ctx.Err() when the context was cancelled during the sleep.
func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// defaultWorkerID is a stable-per-process identifier for who claimed a job.
// Overridable in tests via Deps.WorkerID.
var defaultWorkerID = func() string {
	return fmt.Sprintf("worker-%d", time.Now().UnixNano())
}
