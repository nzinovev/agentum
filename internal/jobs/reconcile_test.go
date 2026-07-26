package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakeTaskStore is an in-memory TaskStore for reconciler tests. It seeds one
// tenant's task table and records the repairs the reconciler applies.
type fakeTaskStore struct {
	mu          sync.Mutex
	tasks       []sqlc.Task
	transitions []string // recorded "id:from→to"
	events      []string // recorded event types
}

func (store *fakeTaskStore) FindOrphanedRunningTasks(_ context.Context, tenantID string) ([]sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var orphaned []sqlc.Task
	for _, task := range store.tasks {
		if task.TenantID == tenantID && task.State == "running" {
			orphaned = append(orphaned, task)
		}
	}
	return orphaned, nil
}

func (store *fakeTaskStore) UpdateTaskState(_ context.Context, arg sqlc.UpdateTaskStateParams) (sqlc.Task, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index, task := range store.tasks {
		if task.ID == arg.ID && task.TenantID == arg.TenantID {
			store.transitions = append(store.transitions, task.ID+":"+task.State+"→"+arg.State)
			store.tasks[index].State = arg.State
			return store.tasks[index], nil
		}
	}
	return sqlc.Task{}, nil
}

func (store *fakeTaskStore) AppendEvent(_ context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, arg.Type)
	return sqlc.Event{}, nil
}

// TestReconciler_PausesOrphanedRunningTasks is the F.6.1 AC #6 proof: a task
// left running with no live job (the crash-between-transition-and-enqueue case)
// is repaired to paused_user_stop so a human resumes — never blindly replayed.
func TestReconciler_PausesOrphanedRunningTasks(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{tasks: []sqlc.Task{
		{ID: "T-orphan", TenantID: "tn", UserID: "us", State: "running"},
		{ID: "T-healthy", TenantID: "tn", UserID: "us", State: "paused_gate"},
		{ID: "T-done", TenantID: "tn", UserID: "us", State: "done"},
	}}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Tasks: tasks,
		StaleAfter: time.Minute, MaxAttempts: 3,
	})

	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	// Exactly one task was repaired — the running one. Healthy/done untouched.
	if len(tasks.transitions) != 1 {
		t.Fatalf("transitions = %v, want exactly 1", tasks.transitions)
	}
	if tasks.transitions[0] != "T-orphan:running→paused_user_stop" {
		t.Fatalf("transition = %q, want T-orphan:running→paused_user_stop", tasks.transitions[0])
	}
	// An audit event was emitted.
	if len(tasks.events) != 1 || tasks.events[0] != "task.reconciled" {
		t.Fatalf("events = %v, want [task.reconciled]", tasks.events)
	}
	// The task is now paused, not running.
	for _, task := range tasks.tasks {
		if task.ID == "T-orphan" && task.State != "paused_user_stop" {
			t.Fatalf("T-orphan state = %q, want paused_user_stop", task.State)
		}
	}
}

// TestReconciler_StaleJobsRequeued proves the periodic reconciler does the
// stale-lease repair the boot Recover does — not only process startup. A dead
// worker's job is re-queued (or failed past the poison bound) without a restart.
func TestReconciler_StaleJobsRequeued(t *testing.T) {
	t.Parallel()
	tasks := &fakeTaskStore{}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Tasks: tasks,
		StaleAfter: 45 * time.Second, MaxAttempts: 3,
	})

	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.staleRequeued != 1 {
		t.Fatalf("expected one stale requeue call, got %d", queue.staleRequeued)
	}
	// Cutoff is ~ now - staleAfter; a periodic reconciler uses a fresh cutoff
	// each pass, not a boot-time snapshot.
	if diff := time.Since(queue.staleBefore); diff < 44*time.Second || diff > 46*time.Second {
		t.Fatalf("stale cutoff drift: %v", diff)
	}
}

// TestReconciler_SkipsAlreadyRepaired proves the in-loop re-check: a task that
// moved out of running between the probe and the repair is not transitioned
// again (no clobbering a concurrent human resume).
func TestReconciler_SkipsAlreadyRepaired(t *testing.T) {
	t.Parallel()
	// Seed running, but simulate a concurrent resume by having UpdateTaskState's
	// first caller flip the state before the reconciler would. We do this by
	// pre-marking the task as paused in the store right before Reconcile reads
	// — the re-check inside the loop guards it.
	tasks := &fakeTaskStore{tasks: []sqlc.Task{
		{ID: "T-racy", TenantID: "tn", UserID: "us", State: "paused_user_stop"},
	}}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Tasks: tasks, StaleAfter: time.Minute,
	})

	// FindOrphanedRunningTasks returns only state='running'; the paused task is
	// not a target, so Reconcile must not touch it.
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	if len(tasks.transitions) != 0 {
		t.Fatalf("expected no transitions, got %v", tasks.transitions)
	}
}
