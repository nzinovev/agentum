package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sqlc-dev/pqtype"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// writeVerdictFile writes a verdict.json into a stage's artifact dir, mirroring
// what a reviewer adapter does so captureStageOutputs ingests it and the next
// transition's buildTransitionContext can read it from the store.
func writeVerdictFile(artifactDir string, verdict agent.VerdictJSON) error {
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(artifactDir, agent.VerdictFileName), mustMarshalVerdict(verdict), 0o644)
}

// branchPack builds the spec → review ⇄ fix → done pack in Go, mirroring the
// testdata/loop fixture but in-memory (no cross-package pack.Load). The review
// stage branches on verdict; fix is the fixer-role stage the budget binds.
func branchPack(fixCycles int) *pack.Pack {
	stages := map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "review"}}},
		"review": {Gate: pack.GateAuto, Prompt: "review.md", Role: "reviewer", Transitions: []pack.Transition{
			{To: "fix", Condition: `verdict == "changes_requested"`},
			{To: "done", Condition: `verdict == "approved"`},
		}},
		"fix":  {Gate: pack.GateAutoIfClean, Prompt: "fix.md", Role: "fixer", Transitions: []pack.Transition{{To: "review"}}},
		"done": {},
	}
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "loop-test", Version: "0.1.0"},
		Tiers: pack.Tiers{Default: "fast"}, Budgets: pack.Budgets{FixCycles: fixCycles, AskToEdit: 1},
		Entry: "spec", Stages: stages,
		PromptText: map[string]string{"spec": "spec", "review": "review", "fix": "fix"},
	}
}

func changesRequested(summary string) agent.VerdictJSON {
	return agent.VerdictJSON{
		SchemaVersion: agent.VerdictSchemaVersion, Verdict: agent.VerdictChangesRequested,
		Summary:  summary,
		Findings: []agent.Finding{{ID: "F1", Severity: agent.SeverityBlocker, Path: "main.go", Detail: "broken"}},
	}
}

func approvedVerdict(summary string) agent.VerdictJSON {
	return agent.VerdictJSON{
		SchemaVersion: agent.VerdictSchemaVersion, Verdict: agent.VerdictApproved, Summary: summary,
	}
}

// TestLoop_ChangesRequestedThenApproved: the canonical happy loop. review says
// changes_requested → fix → review says approved → done. Asserts the stage
// order spec, review, fix, review, done and that each attempt is a separate
// invocation.
func TestLoop_ChangesRequestedThenApproved(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(2)
	task := sqlc.Task{ID: "T-loop1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)

	// review is called twice: first changes_requested, then approved. The
	// adapter keys verdicts by stage id, so both review calls return the same
	// verdict — to vary per call we use a call-count-aware adapter instead.
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{changesRequested("first pass"), approvedVerdict("second pass")},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: newRecordingStore(),
	})
	// Derive the syncer the same way production does (from an artifacts.SQLStore
	// is not available here; the recording store is not an SQLStore, so the
	// syncer is nil and sync is a no-op — acceptable for this loop test).

	if err := runner.Handle(t.Context(), job("run", "T-loop1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	wantCalls := []string{"spec", "review", "fix", "review"}
	if !equalStringSlices(adapter.calls, wantCalls) {
		t.Errorf("stage call order = %v, want %v", adapter.calls, wantCalls)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Errorf("final state = %q, want awaiting_final_review (reached done)", got)
	}
	// Each attempt is a separate invocation row.
	if count := len(store.invocations); count != 4 {
		t.Errorf("invocation count = %d, want 4 (spec, review, fix, review)", count)
	}
}

// TestLoop_ApprovedOnFirstPass: a positive verdict routes straight to done.
func TestLoop_ApprovedOnFirstPass(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(2)
	task := sqlc.Task{ID: "T-loop2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{approvedVerdict("clean")},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: newRecordingStore(),
	})
	if err := runner.Handle(t.Context(), job("run", "T-loop2", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	wantCalls := []string{"spec", "review"}
	if !equalStringSlices(adapter.calls, wantCalls) {
		t.Errorf("stage call order = %v, want %v", adapter.calls, wantCalls)
	}
	if count := len(store.invocations); count != 2 {
		t.Errorf("invocation count = %d, want 2 (spec, review)", count)
	}
}

// TestLoop_BudgetExhaustedStopsAndPreserves: fix_cycles:1 + always
// changes_requested stops at paused_user_stop/fix_budget_exhausted after one
// fix pass. Everything created (branch via checkpoints, artifact revisions,
// manifest) stays; the manifest is not sealed (a pause is non-terminal).
func TestLoop_BudgetExhaustedStopsAndPreserves(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(1) // budget 1: one fixer entry, then refuse.
	task := sqlc.Task{ID: "T-loop3", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	artStore := newRecordingStore()
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		// Always changes_requested: the second fix entry is refused.
		reviewSequence: []agent.VerdictJSON{changesRequested("p1"), changesRequested("p2"), changesRequested("p3")},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: artStore,
	})
	if err := runner.Handle(t.Context(), job("run", "T-loop3", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Errorf("state = %q, want paused_user_stop", got)
	}
	// Stage order: spec, review, fix, review (the second review says
	// changes_requested, the fixer entry is refused -> stop).
	wantCalls := []string{"spec", "review", "fix", "review"}
	if !equalStringSlices(adapter.calls, wantCalls) {
		t.Errorf("stage call order = %v, want %v", adapter.calls, wantCalls)
	}
	// The stop reason is recorded on the last invocation's stop_reason and on
	// the task.state_changed event. Find the budget stop in the events.
	foundStopReason := false
	for _, event := range store.events {
		if event.Type == EvTaskStateChanged && containsEventField(event, "fix_budget_exhausted") {
			foundStopReason = true
		}
	}
	if !foundStopReason {
		t.Error("no task.state_changed event carrying fix_budget_exhausted")
	}
	// Artifact revisions were captured (verdict.json at minimum) and survived.
	if len(artStore.puts) == 0 {
		t.Error("expected artifact revisions to be captured before the stop")
	}
	// Checkpoints were recorded (base + post-stage for each completed stage).
	if len(store.checkpoints) == 0 {
		t.Error("expected checkpoints to be recorded and preserved at the stop")
	}
}

// TestLoop_WorkerRestartContinuesCycle: seed invocations with cycles, build a
// FRESH Runner, drive an advance job, and assert the cycle continues and the
// budget still binds. Mirrors a worker restart mid-loop: the counter is
// recomputed from committed rows, not process memory.
func TestLoop_WorkerRestartContinuesCycle(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(1) // budget 1: one fixer entry allowed.
	task := sqlc.Task{ID: "T-loop4", TenantID: "tn", UserID: "us", ProjectID: "P1",
		// The advance API handler applies the gate->running FSM transition
		// before enqueuing the advance job, so the runner sees the task already
		// in running — paused_gate is the pre-advance state, not the job's.
		State: "running", CurrentStage: sql.NullString{String: "review", Valid: true},
		PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// Seed the history a crashed worker would leave: spec, review (changes
	// requested), fix (cycle 0 — the one allowed fixer entry), review. The
	// fixer has run once (cycle 0), so fix_cycles_used = 1; budget = 1; the
	// next fixer entry must be refused on restart.
	store.invocations = []sqlc.StageInvocation{
		{ID: "inv-1", TaskID: "T-loop4", TenantID: "tn", Stage: "spec", Sequence: 1, Cycle: 0},
		{ID: "inv-2", TaskID: "T-loop4", TenantID: "tn", Stage: "review", Sequence: 2, Cycle: 0},
		{ID: "inv-3", TaskID: "T-loop4", TenantID: "tn", Stage: "fix", Sequence: 3, Cycle: 0},
		{ID: "inv-4", TaskID: "T-loop4", TenantID: "tn", Stage: "review", Sequence: 4, Cycle: 1},
	}

	// Fresh runner — no in-memory state from the prior process.
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
	}
	// The advance job resolves the review's transition. Seed the latest
	// invocation's result so buildTransitionContext can read the status; and
	// seed a verdict.json revision so the verdict condition resolves.
	store.invocations[3].Result = nullRawMessage(`{"schema_version":"1","status":"complete","summary":"v"}`)
	artStore := newRecordingStore()
	artStore.currentByName = map[string][]byte{
		"review/verdict.json": mustMarshalVerdict(changesRequested("restart")),
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: artStore,
	})
	// advance from the gate at review: the verdict says changes_requested, so
	// the resolver wants to enter fix; but the budget (1) is already spent
	// (one fix at cycle 0), so the entry is refused -> paused_user_stop.
	if err := runner.Handle(t.Context(), job("advance", "T-loop4", "tn", "us")); err != nil {
		t.Fatalf("advance job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Errorf("state after restart-advance = %q, want paused_user_stop (budget binds)", got)
	}
	// No new invocation row was created — the fixer entry was refused before
	// invokeStage ran. The seeded four are all there is.
	if count := len(store.invocations); count != 4 {
		t.Errorf("invocation count after refused entry = %d, want 4 (no new row)", count)
	}
}

// countingVerdictAdapter is a verdictAdapter whose review verdict varies by
// call count, so a loop test can say "first review = changes_requested, second
// = approved" without a per-call key collision.
type countingVerdictAdapter struct {
	stubExecution
	results        map[string]agent.ResultJSON
	reviewSequence []agent.VerdictJSON
	reviewCount    int
	calls          []string
}

func (adapter *countingVerdictAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
	}
}

func (adapter *countingVerdictAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	stageID := stageOf(inv)
	adapter.calls = append(adapter.calls, stageID)
	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		if stageID == "review" && adapter.reviewCount < len(adapter.reviewSequence) {
			verdict := adapter.reviewSequence[adapter.reviewCount]
			adapter.reviewCount++
			if writeErr := writeVerdictFile(inv.ArtifactDir, verdict); writeErr != nil {
				eventCh <- agent.Event{Kind: agent.EventError, Err: writeErr}
				return
			}
		}
		scripted, ok := adapter.results[stageID]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stageID)}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventStream, Chunk: "working..."}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stageID, ResultJSON: scripted}}
	}()
	return eventCh, nil
}

// newRecordingStore returns a fresh recordingArtifactStore for loop tests.
func newRecordingStore() *recordingArtifactStore {
	return &recordingArtifactStore{currentByName: map[string][]byte{}}
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsEventField(event sqlc.Event, want string) bool {
	return indexOf(string(event.Payload), want) >= 0
}

// mustMarshalVerdict is a tiny helper kept local so the test file does not pull
// encoding/json into the package's helper set.
func mustMarshalVerdict(verdict agent.VerdictJSON) []byte {
	raw, _ := json.Marshal(verdict)
	return raw
}

// nullRawMessage builds a pqtype.NullRawMessage from a JSON string for seeding.
func nullRawMessage(jsonStr string) pqtype.NullRawMessage {
	return pqtype.NullRawMessage{RawMessage: []byte(jsonStr), Valid: true}
}

// silence unused import warnings for time if not all helpers use it.
var _ = time.Now

// TestLoop_ResultSummaryCannotOverrideVerdict: the orchestrator routes on the
// parsed verdict.json, never on result.json.summary. A review whose result
// claims approval in its summary but whose verdict says changes_requested must
// route to the fixer.
func TestLoop_ResultSummaryCannotOverrideVerdict(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(2)
	task := sqlc.Task{ID: "T-loop5", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			// The result's summary LIES: it claims approval.
			"review": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "approved, ship it"},
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		// But the verdict says changes_requested -> must route to fix.
		reviewSequence: []agent.VerdictJSON{changesRequested("actually broken"), approvedVerdict("now ok")},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: newRecordingStore(),
	})
	if err := runner.Handle(t.Context(), job("run", "T-loop5", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	// The lying summary did NOT route to done; the verdict routed to fix, then
	// review, then done.
	wantCalls := []string{"spec", "review", "fix", "review"}
	if !equalStringSlices(adapter.calls, wantCalls) {
		t.Errorf("stage call order = %v, want %v (verdict drove routing, not summary)", adapter.calls, wantCalls)
	}
}

// TestLoop_NoVerdictArtifactStops: a verdict-sourcing stage that produces no
// parseable verdict.json halts with paused_user_stop/verdict_unreadable rather
// than silently defaulting to approved.
func TestLoop_NoVerdictArtifactStops(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := branchPack(2)
	task := sqlc.Task{ID: "T-loop6", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	// countingVerdictAdapter with an empty reviewSequence writes no verdict —
	// the reviewer stage sources a verdict condition but produces no verdict.json.
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: nil, // no verdict written
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: newRecordingStore(),
	})
	if err := runner.Handle(t.Context(), job("run", "T-loop6", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Errorf("state = %q, want paused_user_stop (no verdict artifact)", got)
	}
	// The stop reason names verdict_unreadable.
	foundUnreadable := false
	for _, event := range store.events {
		if event.Type == EvTaskStateChanged && containsEventField(event, "verdict_unreadable") {
			foundUnreadable = true
		}
	}
	if !foundUnreadable {
		t.Error("no task.state_changed event carrying verdict_unreadable")
	}
}

// TestLoop_TransitionRecordsPerLap is the D7 assertion that was missing: each
// lap of the review ⇄ fix loop produces one TransitionRecord, and the records
// for BOTH edges (review → fix and fix → review) carry distinct cycles per lap.
// Before the call-site cycle fix, the pure resolver returned FixCyclesUsed for
// a fixer target and 0 for everything else, so every fix → review record
// collapsed to cycle 0 (appendUniqueTransition keys on
// From+To+Condition+Cycle) and the manifest lost the loop shape.
func TestLoop_TransitionRecordsPerLap(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	// budget 3, two changes_requested laps then approved: review → fix → review
	// → fix → review → done. Three laps, so two fixer entries (cycles 0, 1) and
	// three review entries (cycles 0, 1, 2).
	taskPack := branchPack(3)
	task := sqlc.Task{ID: "T-rec", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	manifestFake := &fakeManifestService{}
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"review": {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{
			changesRequested("lap1"), changesRequested("lap2"), approvedVerdict("lap3"),
		},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", Artifacts: newRecordingStore(), Manifest: nil,
	})
	// Inject the fake manifest service after construction (same pattern as
	// checkpoint_test.go:246).
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", "T-rec", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}

	// Collect every TransitionRecord the runner wrote, across AddEvidence patches.
	records := collectTransitionRecords(t, manifestFake)
	// Filter to the loop edges (drop spec → review, review → done) so the
	// assertion is about the loop shape, not the entry/exit edges.
	var reviewToFix, fixToReview []manifest.TransitionRecord
	for _, record := range records {
		if record.From == "review" && record.To == "fix" {
			reviewToFix = append(reviewToFix, record)
		}
		if record.From == "fix" && record.To == "review" {
			fixToReview = append(fixToReview, record)
		}
	}
	// Two laps means two review→fix transitions and two fix→review transitions.
	// (The third review is approved → done, not → fix.)
	if len(reviewToFix) != 2 {
		t.Errorf("review→fix records = %d, want 2 (one per lap): %+v", len(reviewToFix), reviewToFix)
	}
	if len(fixToReview) != 2 {
		t.Errorf("fix→review records = %d, want 2 (one per lap — the regression collapsed these to 1): %+v", len(fixToReview), fixToReview)
	}
	// Both edges' cycles must be distinct per lap (0 then 1), proving the loop
	// shape survived. The regression would have produced [0, 1] for review→fix
	// (fixer target — the buggy FixCyclesUsed path happened to be right for a
	// single-fixer pack) but [0, 0] for fix→review (non-fixer target — the
	// buggy 0 path, which collapsed).
	assertDistinctCycles(t, "review→fix", reviewToFix)
	assertDistinctCycles(t, "fix→review", fixToReview)
}

// collectTransitionRecords flattens every TransitionRecord across all
// AddEvidence patches the fake received. appendUniqueTransition would have
// collapsed duplicates at merge time, so the flattened list IS what the sealed
// manifest would carry (modulo non-transition patches, which contribute none).
func collectTransitionRecords(t *testing.T, service *fakeManifestService) []manifest.TransitionRecord {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	var records []manifest.TransitionRecord
	for _, patch := range service.addEvidence {
		records = append(records, patch.Transitions...)
	}
	return records
}

// assertDistinctCycles fails if two records carry the same Cycle — the
// signature of the collapse the regression caused.
func assertDistinctCycles(t *testing.T, edge string, records []manifest.TransitionRecord) {
	t.Helper()
	seen := map[int]bool{}
	for _, record := range records {
		if seen[record.Cycle] {
			t.Errorf("%s: duplicate Cycle %d across laps — records collapsed (the regression): %+v", edge, record.Cycle, records)
		}
		seen[record.Cycle] = true
	}
}
