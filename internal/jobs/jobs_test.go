package jobs

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakeQueue is an in-memory jobs.Store for worker tests. It records outcomes
// and lets a test script what ClaimNextJob returns.
type fakeQueue struct {
	mu            sync.Mutex
	claimQ        []sqlc.Job // pending claims, FIFO
	completed     []int64
	failed        map[int64]string
	heartbeats    []int64
	staleRequeued int
	staleBefore   time.Time

	claimErr error // inject
}

func newFakeQueue(jobs ...sqlc.Job) *fakeQueue {
	return &fakeQueue{claimQ: append([]sqlc.Job{}, jobs...), failed: map[int64]string{}}
}

func (q *fakeQueue) ClaimNextJob(_ context.Context, _ string) (sqlc.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.claimErr != nil {
		return sqlc.Job{}, q.claimErr
	}
	if len(q.claimQ) == 0 {
		return sqlc.Job{}, sql.ErrNoRows
	}
	j := q.claimQ[0]
	q.claimQ = q.claimQ[1:]
	return j, nil
}
func (q *fakeQueue) CompleteJob(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.completed = append(q.completed, id)
	return nil
}
func (q *fakeQueue) FailJob(_ context.Context, id int64, lastError string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[id] = lastError
	return nil
}
func (q *fakeQueue) BumpHeartbeat(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeats = append(q.heartbeats, id)
	return nil
}
func (q *fakeQueue) RequeueStaleJobs(_ context.Context, before time.Time) ([]sqlc.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.staleBefore = before
	q.staleRequeued++
	return nil, nil
}

// recordingHandler counts Handle calls and lets each job's outcome be scripted.
type recordingHandler struct {
	mu     sync.Mutex
	calls  []string
	byKind map[string]error // kind → error to return
}

func (h *recordingHandler) Handle(_ context.Context, job sqlc.Job) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, job.Kind)
	if err, ok := h.byKind[job.Kind]; ok {
		return err
	}
	return nil
}

func TestWorker_RunsJobsToCompletion(t *testing.T) {
	t.Parallel()
	queue := newFakeQueue(
		sqlc.Job{ID: 1, Kind: "run", TaskID: "T1"},
		sqlc.Job{ID: 2, Kind: "advance", TaskID: "T1"},
	)
	handler := &recordingHandler{}
	w := New(Deps{Store: queue, Handler: handler, Heartbeat: 10 * time.Millisecond, Poll: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Start(ctx); close(done) }()

	waitFor(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return len(handler.calls) == 2
	}, 2*time.Second, "both jobs handled")

	cancel()
	<-done

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.completed) != 2 {
		t.Fatalf("completed = %v, want 2", queue.completed)
	}
	if len(queue.failed) != 0 {
		t.Fatalf("unexpected failures: %v", queue.failed)
	}
}

func TestWorker_FailsJobOnHandlerError(t *testing.T) {
	t.Parallel()
	queue := newFakeQueue(sqlc.Job{ID: 7, Kind: "run", TaskID: "T7"})
	handler := &recordingHandler{byKind: map[string]error{"run": errors.New("boom")}}
	w := New(Deps{Store: queue, Handler: handler, Heartbeat: 10 * time.Millisecond, Poll: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Start(ctx); close(done) }()

	waitFor(t, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		_, failed := queue.failed[7]
		return failed
	}, 2*time.Second, "job 7 recorded as failed")

	cancel()
	<-done

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.failed[7] != "boom" {
		t.Fatalf("failed[7] = %q, want boom", queue.failed[7])
	}
	if len(queue.completed) != 0 {
		t.Fatalf("expected no completions, got %v", queue.completed)
	}
}

func TestWorker_HeartbeatsDuringRun(t *testing.T) {
	t.Parallel()
	var started atomic.Bool
	release := make(chan struct{})
	queue := newFakeQueue(sqlc.Job{ID: 9, Kind: "run", TaskID: "T9"})

	// A handler that blocks until released, so the heartbeat has time to fire.
	blocking := &blockingHandler{started: &started, release: release}
	w := New(Deps{Store: queue, Handler: blocking, Heartbeat: 5 * time.Millisecond, Poll: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Start(ctx); close(done) }()

	waitFor(t, started.Load, time.Second, "handler started")
	// The heartbeat ticker should have bumped at least once while blocked.
	waitFor(t, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return len(queue.heartbeats) > 0
	}, time.Second, "heartbeat bumped")

	close(release) // let the handler finish → CompleteJob
	waitFor(t, func() bool {
		queue.mu.Lock()
		defer queue.mu.Unlock()
		return len(queue.completed) == 1
	}, time.Second, "job completed after release")

	cancel()
	<-done
}

func TestWorker_RecoverRequeuesStale(t *testing.T) {
	t.Parallel()
	queue := newFakeQueue()
	w := New(Deps{Store: queue, Handler: &recordingHandler{}, StaleAfter: 30 * time.Second})

	if err := w.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.staleRequeued != 1 {
		t.Fatalf("expected one requeue call, got %d", queue.staleRequeued)
	}
	// Cutoff should be ~now - staleAfter.
	if diff := time.Since(queue.staleBefore); diff < 29*time.Second || diff > 31*time.Second {
		t.Fatalf("stale cutoff drift: %v", diff)
	}
}

// blockingHandler signals started, then blocks on release before returning nil.
type blockingHandler struct {
	started *atomic.Bool
	release <-chan struct{}
}

func (h *blockingHandler) Handle(ctx context.Context, _ sqlc.Job) error {
	h.started.Store(true)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting: " + msg)
}
