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

func (s QueueStore) ClaimNextJob(ctx context.Context, workerID string) (sqlc.Job, error) {
	return s.Q.ClaimNextJob(ctx, nullStr(workerID))
}

func (s QueueStore) CompleteJob(ctx context.Context, id int64) error {
	return s.Q.CompleteJob(ctx, id)
}

func (s QueueStore) FailJob(ctx context.Context, id int64, lastError string) error {
	return s.Q.FailJob(ctx, sqlc.FailJobParams{ID: id, LastError: nullStr(lastError)})
}

func (s QueueStore) BumpHeartbeat(ctx context.Context, id int64) error {
	return s.Q.BumpHeartbeat(ctx, id)
}

func (s QueueStore) RequeueStaleJobs(ctx context.Context, before time.Time) ([]sqlc.Job, error) {
	return s.Q.RequeueStaleJobs(ctx, sql.NullTime{Time: before, Valid: true})
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// Enqueuer inserts a pending job. Used by HTTP handlers; kept in this package so
// the queue's write surface is named in one place.
type Enqueuer interface {
	EnqueueJob(ctx context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error)
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
func New(d Deps) *Worker {
	wid := d.WorkerID
	if wid == "" {
		wid = defaultWorkerID()
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	max := d.MaxAttempts
	if max == 0 {
		max = DefaultMaxAttempts
	}
	return &Worker{
		store: d.Store, handler: d.Handler, workerID: wid,
		poll: orDefault(d.Poll, DefaultPoll), heartbeat: orDefault(d.Heartbeat, DefaultHeartbeat),
		staleAfter: orDefault(d.StaleAfter, DefaultStaleAfter), maxAttempts: max, log: log,
	}
}

// Start is the claim loop. It blocks until ctx is cancelled; run it in its own
// goroutine. Errors during claim are logged and the loop backs off briefly so a
// transient DB issue does not spin.
func (w *Worker) Start(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		job, err := w.store.ClaimNextJob(ctx, w.workerID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				w.log.Error("claim job", "error", err)
			}
			if err := sleep(ctx, w.poll); err != nil {
				return
			}
			continue
		}
		w.run(ctx, job)
	}
}

// run drives one job: a heartbeat goroutine refreshes heartbeat_at while the
// handler runs, so the recovery pass can detect a dead worker; the outcome is
// recorded as done or failed. The handler sees the worker's ctx so cancelling
// the worker (shutdown) aborts the in-flight run too.
func (w *Worker) run(ctx context.Context, job sqlc.Job) {
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go w.heartbeatLoop(hbCtx, job.ID)

	err := w.handler.Handle(ctx, job)
	stopHB()

	if err == nil {
		if cerr := w.store.CompleteJob(ctx, job.ID); cerr != nil {
			w.log.Error("complete job", "job", job.ID, "error", cerr)
		}
		return
	}
	w.log.Warn("job failed", "job", job.ID, "task", job.TaskID, "kind", job.Kind, "error", err)
	if ferr := w.store.FailJob(ctx, job.ID, err.Error()); ferr != nil {
		w.log.Error("fail job", "job", job.ID, "error", ferr)
	}
}

// heartbeatLoop bumps heartbeat_at until ctx is cancelled (job done or worker
// stop). Named distinctly from the heartbeat field.
func (w *Worker) heartbeatLoop(ctx context.Context, jobID int64) {
	t := time.NewTicker(w.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.store.BumpHeartbeat(ctx, jobID); err != nil {
				w.log.Warn("heartbeat", "job", jobID, "error", err)
			}
		}
	}
}

// Recover runs at boot, before the worker starts. It re-queues jobs a previous
// worker died on (heartbeat older than staleAfter), failing any over the poison
// bound so a bad job cannot loop forever (04 §7.5–§7.6).
func (w *Worker) Recover(ctx context.Context) error {
	cutoff := time.Now().Add(-w.staleAfter)
	stale, err := w.store.RequeueStaleJobs(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("recover: requeue stale: %w", err)
	}
	for _, job := range stale {
		if int(job.Attempts) >= w.maxAttempts {
			// Over the bound: fail it rather than re-queueing forever. The task
			// is left in its current state; the operator inspects and resumes.
			if ferr := w.store.FailJob(ctx, job.ID, fmt.Sprintf("exceeded max attempts (%d)", w.maxAttempts)); ferr != nil {
				w.log.Error("recover: fail poison job", "job", job.ID, "error", ferr)
			}
			w.log.Warn("recover: poison job failed", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
			continue
		}
		w.log.Info("recover: requeued stale job", "job", job.ID, "task", job.TaskID, "attempts", job.Attempts)
	}
	return nil
}

// orDefault returns v when non-zero, else def.
func orDefault(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// sleep returns ctx.Err() when the context was cancelled during the sleep.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// defaultWorkerID is a stable-per-process identifier for who claimed a job.
// Overridable in tests via Deps.WorkerID.
var defaultWorkerID = func() string {
	return fmt.Sprintf("worker-%d", time.Now().UnixNano())
}
