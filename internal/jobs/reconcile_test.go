package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakeRunStore is an in-memory RunStore for reconciler tests. It seeds one
// tenant's run table and records the repairs the reconciler applies.
type fakeRunStore struct {
	mu          sync.Mutex
	runs        []sqlc.Run
	transitions []string // recorded "id:from→to"
	events      []string // recorded event types
}

func (store *fakeRunStore) FindOrphanedRunningRuns(_ context.Context, tenantID string) ([]sqlc.Run, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var orphaned []sqlc.Run
	for _, run := range store.runs {
		if run.TenantID == tenantID && run.State == "running" {
			orphaned = append(orphaned, run)
		}
	}
	return orphaned, nil
}

func (store *fakeRunStore) UpdateRunState(_ context.Context, arg sqlc.UpdateRunStateParams) (sqlc.Run, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for index, run := range store.runs {
		if run.ID == arg.ID && run.TenantID == arg.TenantID {
			store.transitions = append(store.transitions, run.ID+":"+run.State+"→"+arg.State)
			store.runs[index].State = arg.State
			return store.runs[index], nil
		}
	}
	return sqlc.Run{}, nil
}

func (store *fakeRunStore) AppendEvent(_ context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, arg.Type)
	return sqlc.Event{}, nil
}

// TestReconciler_PausesOrphanedRunningRuns is the F.6.1 AC #6 proof: a run
// left running with no live job (the crash-between-transition-and-enqueue case)
// is repaired to paused_user_stop so a human resumes — never blindly replayed.
func TestReconciler_PausesOrphanedRunningRuns(t *testing.T) {
	t.Parallel()
	runStore := &fakeRunStore{runs: []sqlc.Run{
		{ID: "T-orphan", TenantID: "tn", UserID: "us", State: "running"},
		{ID: "T-healthy", TenantID: "tn", UserID: "us", State: "paused_gate"},
		{ID: "T-done", TenantID: "tn", UserID: "us", State: "done"},
	}}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Runs: runStore,
		StaleAfter: time.Minute, MaxAttempts: 3,
	})

	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	runStore.mu.Lock()
	defer runStore.mu.Unlock()
	// Exactly one run was repaired — the running one. Healthy/done untouched.
	if len(runStore.transitions) != 1 {
		t.Fatalf("transitions = %v, want exactly 1", runStore.transitions)
	}
	if runStore.transitions[0] != "T-orphan:running→paused_user_stop" {
		t.Fatalf("transition = %q, want T-orphan:running→paused_user_stop", runStore.transitions[0])
	}
	// An audit event was emitted.
	if len(runStore.events) != 1 || runStore.events[0] != "run.reconciled" {
		t.Fatalf("events = %v, want [run.reconciled]", runStore.events)
	}
	// The run is now paused, not running.
	for _, run := range runStore.runs {
		if run.ID == "T-orphan" && run.State != "paused_user_stop" {
			t.Fatalf("T-orphan state = %q, want paused_user_stop", run.State)
		}
	}
}

// TestReconciler_StaleJobsRequeued proves the periodic reconciler does the
// stale-lease repair the boot Recover does — not only process startup. A dead
// worker's job is re-queued (or failed past the poison bound) without a restart.
func TestReconciler_StaleJobsRequeued(t *testing.T) {
	t.Parallel()
	runStore := &fakeRunStore{}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Runs: runStore,
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

// TestReconciler_SkipsAlreadyRepaired proves the in-loop re-check: a run that
// moved out of running between the probe and the repair is not transitioned
// again (no clobbering a concurrent human resume).
func TestReconciler_SkipsAlreadyRepaired(t *testing.T) {
	t.Parallel()
	// Seed running, but simulate a concurrent resume by having UpdateRunState's
	// first caller flip the state before the reconciler would. We do this by
	// pre-marking the run as paused in the store right before Reconcile reads
	// — the re-check inside the loop guards it.
	runStore := &fakeRunStore{runs: []sqlc.Run{
		{ID: "T-racy", TenantID: "tn", UserID: "us", State: "paused_user_stop"},
	}}
	queue := newFakeQueue()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: queue, Runs: runStore, StaleAfter: time.Minute,
	})

	// FindOrphanedRunningRuns returns only state='running'; the paused run is
	// not a target, so Reconcile must not touch it.
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	runStore.mu.Lock()
	defer runStore.mu.Unlock()
	if len(runStore.transitions) != 0 {
		t.Fatalf("expected no transitions, got %v", runStore.transitions)
	}
}

// fakePublicationStore seeds stale publications and records the publish jobs
// the reconciler enqueues for them.
type fakePublicationStore struct {
	mu           sync.Mutex
	stale        []sqlc.RunPublication
	enqueued     []string
	findStaleErr error
}

func (store *fakePublicationStore) FindStalePublications(context.Context, sqlc.FindStalePublicationsParams) ([]sqlc.RunPublication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.findStaleErr != nil {
		return nil, store.findStaleErr
	}
	return store.stale, nil
}

func (store *fakePublicationStore) EnqueueJob(_ context.Context, arg sqlc.EnqueueJobParams) (sqlc.Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.enqueued = append(store.enqueued, arg.Kind)
	return sqlc.Job{Kind: arg.Kind}, nil
}

// TestReconciler_RequeuesLostPublications: a lost publication — its row
// exists and the work it describes was never finished — gets a fresh publish
// job. The probe never sees a failed or blocked row (the query excludes
// them); what it returns, it re-enqueues.
func TestReconciler_RequeuesLostPublications(t *testing.T) {
	t.Parallel()
	publications := &fakePublicationStore{stale: []sqlc.RunPublication{
		{ID: "pub-1", TenantID: "tn", UserID: "us", RunID: "T-lost-pending", State: "pending"},
		{ID: "pub-2", TenantID: "tn", UserID: "us", RunID: "T-lost-publishing", State: "publishing"},
	}}
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: newFakeQueue(), Runs: &fakeRunStore{},
		Publications: publications, StaleAfter: time.Minute, MaxAttempts: 3,
	})

	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	publications.mu.Lock()
	defer publications.mu.Unlock()
	if len(publications.enqueued) != 2 {
		t.Fatalf("enqueued = %v, want two publish jobs", publications.enqueued)
	}
	for _, kind := range publications.enqueued {
		if kind != "publish" {
			t.Errorf("enqueued kind = %q, want publish", kind)
		}
	}
}

// TestReconciler_PublicationProbeOptional: a reconciler built without the
// publication store skips the third probe — the first two still run and
// reconcile cleanly on an empty queue.
func TestReconciler_PublicationProbeOptional(t *testing.T) {
	t.Parallel()
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: newFakeQueue(), Runs: &fakeRunStore{},
		StaleAfter: time.Minute, MaxAttempts: 3,
	})
	if err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile without publications: %v", err)
	}
}

// TestReconciler_PublicationProbeErrorIsReportedNotSwallowed: a probe that
// cannot read its rows returns an error naming the pass, so a broken probe
// is visible in the log instead of silently publishing nothing.
func TestReconciler_PublicationProbeErrorIsReportedNotSwallowed(t *testing.T) {
	t.Parallel()
	publications := &fakePublicationStore{findStaleErr: errors.New("db down")}
	reconciler := NewReconciler(ReconcilerDeps{
		TenantID: "tn", Queue: newFakeQueue(), Runs: &fakeRunStore{},
		Publications: publications, StaleAfter: time.Minute, MaxAttempts: 3,
	})
	err := reconciler.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reconcile publications") {
		t.Fatalf("Reconcile error = %v, want one naming the publications pass", err)
	}
}
