package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// sequenceAdapter plays one scripted result per invocation, in order, and
// records every Invocation it is handed. Keyed by order rather than by stage
// because the continue scenarios need the SAME stage to answer differently on
// its resumed attempts. Each invocation reports a fresh session id, as a real
// runtime does when a continued session is assigned its current id.
type sequenceAdapter struct {
	stubExecution
	results     []agent.ResultJSON
	invocations []agent.Invocation
}

func (adapter *sequenceAdapter) Supported() []caps.Category { return fullSupport }

func (adapter *sequenceAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	adapter.invocations = append(adapter.invocations, inv)
	if len(adapter.invocations) > len(adapter.results) {
		return nil, errors.New("no scripted result left")
	}
	scripted := adapter.results[len(adapter.invocations)-1]
	sessionID := "sess-1"
	if len(adapter.invocations) > 1 {
		sessionID = "sess-2"
	}
	if len(adapter.invocations) > 2 {
		sessionID = "sess-3"
	}
	eventCh := make(chan agent.Event, 1)
	go func() {
		defer close(eventCh)
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{
			SessionID: sessionID, ResultJSON: scripted,
		}}
	}()
	return eventCh, nil
}

// continuePack is the two-stage shape the delivery scenarios need: spec pauses
// on open questions, then completes and auto-advances into a fresh impl
// invocation.
func continuePack() *pack.Pack {
	return scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
}

// continueJob builds a continue job carrying the payload the API would have
// stored for the given user text.
func continueJob(runID, tenant, user, text string) (sqlc.Job, error) {
	continuation := struct {
		Text string `json:"text"`
	}{Text: text}
	payload, marshalErr := json.Marshal(continuation)
	if marshalErr != nil {
		return sqlc.Job{}, marshalErr
	}
	if text == "" {
		payload = json.RawMessage("{}")
	}
	return sqlc.Job{Kind: "continue", RunID: runID, TenantID: tenant, UserID: user, Payload: payload}, nil
}

// TestRunner_ContinueTextReachesResumedInvocation is the defect fix itself:
// the text saved on the continue job must reach the adapter inside the resumed
// invocation's routing block — exact text, same session id, same stage — and
// must NOT reach the fresh invocation of the next stage. Before the fix the
// runner never read the payload, so the HTTP call succeeded while the text
// silently died in the job row.
func TestRunner_ContinueTextReachesResumedInvocation(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	const userText = "Используем PostgreSQL 17. Новую таблицу создавать не нужно."
	record := sqlc.Run{
		ID: "Tc1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
		Title:        "Add export endpoint", Description: "Export runs as CSV.",
		Overrides: json.RawMessage(`{"checks":{"required":["verify"]}}`),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		// spec asks, then (resumed with the user's text) completes; impl runs fresh.
		{SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which database?"}},
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done with the answer"},
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
	}}
	src := &staticSource{pk: continuePack()}

	runRunner := New(Deps{Store: store, Packs: src, Adapter: adapter})
	if err := runRunner.HandleRun(t.Context(), job("run", "Tc1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_open_questions" {
		t.Fatalf("setup: state = %q, want paused_open_questions", got)
	}

	// A NEW runner instance for the continue job: delivery must depend on the
	// stored job row, not on any state of the HTTP process that accepted it.
	continueRunner := New(Deps{Store: store, Packs: src, Adapter: adapter})
	continueWithText, jobErr := continueJob("Tc1", "tn", "us", userText)
	if jobErr != nil {
		t.Fatalf("build continue job: %v", jobErr)
	}
	if err := continueRunner.HandleContinue(t.Context(), continueWithText); err != nil {
		t.Fatalf("continue job: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state after continue = %q, want awaiting_final_review", got)
	}
	if count := len(adapter.invocations); count != 3 {
		t.Fatalf("invocations = %d, want 3 (spec, resumed spec, fresh impl)", count)
	}

	resumed := adapter.invocations[1]
	if !strings.Contains(resumed.RoutingBlock, userText) {
		t.Errorf("resumed invocation's routing block does not carry the user text; got:\n%s", resumed.RoutingBlock)
	}
	if !strings.Contains(resumed.RoutingBlock, "--- BEGIN USER CONTINUATION ---") {
		t.Errorf("resumed invocation's routing block has no continuation markers; got:\n%s", resumed.RoutingBlock)
	}
	if resumed.ResumeSession != "sess-1" {
		t.Errorf("resumed invocation ResumeSession = %q, want the captured session sess-1", resumed.ResumeSession)
	}
	if stage := stageOf(resumed); stage != "spec" {
		t.Errorf("resumed invocation stage = %q, want spec (resume stays on the current stage)", stage)
	}
	// The original request survives beside the answer, and the overrides stay
	// orchestrator-only.
	if !strings.Contains(resumed.RoutingBlock, "Add export endpoint") || !strings.Contains(resumed.RoutingBlock, "Export runs as CSV.") {
		t.Errorf("resumed invocation's routing block lost the original request; got:\n%s", resumed.RoutingBlock)
	}
	if strings.Contains(resumed.RoutingBlock, `"required"`) || strings.Contains(resumed.RoutingBlock, `"checks":{`) {
		t.Errorf("resumed invocation's routing block leaks the raw overrides; got:\n%s", resumed.RoutingBlock)
	}

	fresh := adapter.invocations[2]
	if strings.Contains(fresh.RoutingBlock, userText) {
		t.Errorf("the fresh impl invocation received the text a second time; got:\n%s", fresh.RoutingBlock)
	}
	if fresh.ResumeSession != "" {
		t.Errorf("fresh impl invocation ResumeSession = %q, want empty", fresh.ResumeSession)
	}
}

// TestRunner_TwoSequentialAnswersEachReachTheirOwnResume: two open-questions
// pauses and two continues — each answer lands in its own resumed invocation
// only, the resume chain (resume_of) stays intact, and a resume does not
// increment the stage's fix-cycle counter.
func TestRunner_TwoSequentialAnswersEachReachTheirOwnResume(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{ID: "Tc2", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which framework?"}},
		{SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which database?"}},
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done"},
	}}
	src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})}

	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleRun(t.Context(), job("run", "Tc2", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	firstAnswer, jobErr := continueJob("Tc2", "tn", "us", "Use the standard library only.")
	if jobErr != nil {
		t.Fatalf("build first continue job: %v", jobErr)
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), firstAnswer); err != nil {
		t.Fatalf("first continue job: %v", err)
	}
	if got := store.taskState(); got != "paused_open_questions" {
		t.Fatalf("state after first continue = %q, want paused_open_questions again", got)
	}
	secondAnswer, jobErr := continueJob("Tc2", "tn", "us", "Use PostgreSQL 17.")
	if jobErr != nil {
		t.Fatalf("build second continue job: %v", jobErr)
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), secondAnswer); err != nil {
		t.Fatalf("second continue job: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state after second continue = %q, want awaiting_final_review", got)
	}

	if !strings.Contains(adapter.invocations[1].RoutingBlock, "Use the standard library only.") {
		t.Errorf("first answer missing from the first resumed invocation; got:\n%s", adapter.invocations[1].RoutingBlock)
	}
	if strings.Contains(adapter.invocations[1].RoutingBlock, "Use PostgreSQL 17.") {
		t.Errorf("second answer leaked into the first resumed invocation; got:\n%s", adapter.invocations[1].RoutingBlock)
	}
	if !strings.Contains(adapter.invocations[2].RoutingBlock, "Use PostgreSQL 17.") {
		t.Errorf("second answer missing from the second resumed invocation; got:\n%s", adapter.invocations[2].RoutingBlock)
	}
	if strings.Contains(adapter.invocations[2].RoutingBlock, "Use the standard library only.") {
		t.Errorf("first answer replayed into the second resumed invocation; got:\n%s", adapter.invocations[2].RoutingBlock)
	}
	if adapter.invocations[1].ResumeSession != "sess-1" || adapter.invocations[2].ResumeSession != "sess-2" {
		t.Errorf("resume sessions = %q, %q; want sess-1, sess-2 (each answer resumes the session its question left)",
			adapter.invocations[1].ResumeSession, adapter.invocations[2].ResumeSession)
	}

	// The resume chain and the cycle: invocation 2 resumes invocation 1,
	// invocation 3 resumes invocation 2, and all three stay on cycle 0 — a
	// resume is the same attempt continuing, not a fix-cycle re-entry.
	second, third := store.invocations[1], store.invocations[2]
	if !second.ResumeOf.Valid || second.ResumeOf.String != store.invocations[0].ID {
		t.Errorf("invocation 2 resume_of = %+v; want invocation %s", second.ResumeOf, store.invocations[0].ID)
	}
	if !third.ResumeOf.Valid || third.ResumeOf.String != second.ID {
		t.Errorf("invocation 3 resume_of = %+v; want invocation %s", third.ResumeOf, second.ID)
	}
	for index, invocation := range store.invocations {
		if invocation.Cycle != 0 {
			t.Errorf("invocation %d cycle = %d; a resume must not increment the fix cycle", index+1, invocation.Cycle)
		}
	}
}

// TestRunner_ContinueWithoutTextKeepsPriorBehavior: a continue job with no
// payload content ({}, null, or empty bytes) resumes the session exactly as it
// did before the text existed — the block carries no continuation section.
func TestRunner_ContinueWithoutTextKeepsPriorBehavior(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload json.RawMessage
	}{
		{"empty object", json.RawMessage(`{}`)},
		{"json null", json.RawMessage(`null`)},
		{"empty bytes", nil},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			if err := initRepoWithCommit(repo); err != nil {
				t.Fatalf("setup repo: %v", err)
			}
			record := sqlc.Run{ID: "Tce", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
			proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
			store := newFakeStore(record, proj)
			adapter := &sequenceAdapter{results: []agent.ResultJSON{
				{SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which framework?"}},
				{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done"},
			}}
			src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
				"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
				"done": {},
			})}
			if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleRun(t.Context(), job("run", "Tce", "tn", "us")); err != nil {
				t.Fatalf("run job: %v", err)
			}
			emptyJob := sqlc.Job{Kind: "continue", RunID: "Tce", TenantID: "tn", UserID: "us", Payload: testCase.payload}
			if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), emptyJob); err != nil {
				t.Fatalf("continue job: %v", err)
			}
			if got := store.taskState(); got != "awaiting_final_review" {
				t.Fatalf("state = %q, want awaiting_final_review", got)
			}
			resumed := adapter.invocations[1]
			if strings.Contains(resumed.RoutingBlock, "USER CONTINUATION") {
				t.Errorf("a textless continue rendered a continuation section; got:\n%s", resumed.RoutingBlock)
			}
			if resumed.ResumeSession != "sess-1" {
				t.Errorf("ResumeSession = %q; a textless continue still resumes the captured session", resumed.ResumeSession)
			}
		})
	}
}

// hasStopReason reports whether any run.state_changed event carries the given
// stop_reason — the diagnosis channel a continuation fault must land on.
func hasStopReason(events []sqlc.Event, wantReason string) bool {
	for _, event := range events {
		if event.Type != EvRunStateChanged {
			continue
		}
		var payload struct {
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if payload.StopReason == wantReason {
			return true
		}
	}
	return false
}

// TestRunner_ContinueTextWithoutSessionPausesWithoutInvoking: text with no
// captured session has no delivery target. The run must stop in a managed
// pause naming the fault — never start a fresh session the user never saw, and
// never silently drop the text after a 200.
func TestRunner_ContinueTextWithoutSessionPausesWithoutInvoking(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{
		ID: "Tns", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CurrentStage: nullStr("spec"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	// The prior invocation captured no session id (the adapter died before
	// reporting one).
	store.invocations = []sqlc.StageInvocation{{
		ID: "inv-1", RunID: "Tns", TenantID: "tn", Stage: "spec", Sequence: 1, Cycle: 0,
	}}
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "never reached"},
	}}
	src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})}
	continueWithText, jobErr := continueJob("Tns", "tn", "us", "an answer with nowhere to go")
	if jobErr != nil {
		t.Fatalf("build continue job: %v", jobErr)
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), continueWithText); err != nil {
		t.Fatalf("continue job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop (managed pause, not a failure)", got)
	}
	if len(adapter.invocations) != 0 {
		t.Fatalf("adapter invoked %d time(s); the text-less session must stop before any invocation", len(adapter.invocations))
	}
	if !hasStopReason(store.events, "resume_session_missing") {
		t.Errorf("no run.state_changed event carries stop_reason resume_session_missing; events: %+v", store.events)
	}
}

// TestRunner_ContinueTextWithNoInvocationStartsFirstStage: a pre-invocation
// stop can carry a note to the first stage through the Task section.
func TestRunner_ContinueTextWithNoInvocationStartsFirstStage(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{
		ID: "Tni", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CurrentStage: nullStr("spec"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	// No invocations seeded: the run never produced one.
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "first stage reached"},
	}}
	src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})}
	continueWithText, jobErr := continueJob("Tni", "tn", "us", "an answer with no invocation to ride on")
	if jobErr != nil {
		t.Fatalf("build continue job: %v", jobErr)
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), continueWithText); err != nil {
		t.Fatalf("continue job: %v", err)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state = %q, want awaiting_final_review", got)
	}
	if len(adapter.invocations) != 1 {
		t.Fatalf("adapter invoked %d time(s), want one first-stage invocation", len(adapter.invocations))
	}
	if !strings.Contains(adapter.invocations[0].RoutingBlock, "an answer with no invocation to ride on") {
		t.Errorf("first invocation did not receive the note: %s", adapter.invocations[0].RoutingBlock)
	}
}

// TestRunner_ContinueWithoutInvocationStartsFirstStage pins the fixed shape:
// a textless continue on a run paused before its first stage (an unresolvable
// base_ref, a drifted pack directory) starts the run at the pack entry
// instead of failing it. Before the fix this path failed the run through
// LatestStageForRun's sql.ErrNoRows, which made every pre-execution pause a
// dead end only cancel could exit.
func TestRunner_ContinueWithoutInvocationStartsFirstStage(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{
		ID: "Tnf", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CurrentStage: nullStr("spec"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "first stage ran"},
	}}
	src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})}
	textlessJob := sqlc.Job{Kind: "continue", RunID: "Tnf", TenantID: "tn", UserID: "us", Payload: json.RawMessage(`{}`)}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), textlessJob); err != nil {
		t.Fatalf("a textless continue with no invocation row must start the first stage: %v", err)
	}
	if got := len(store.invocations); got != 1 {
		t.Fatalf("invocations = %d, want 1 (the entry stage must run)", got)
	}
	if got := store.taskState(); got == "failed" {
		t.Fatal("the run must drive forward, not fail on the textless resume")
	}
}

// TestRunner_ContinueCorruptPayloadPausesWithoutInvoking: a non-empty payload
// of an unknown shape is a fault, not a dropped key — an old job written by
// other code must not silently change meaning. The run pauses with the
// diagnosis and the agent is not invoked.
func TestRunner_ContinueCorruptPayloadPausesWithoutInvoking(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{
		ID: "Tcp", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0", CurrentStage: nullStr("spec"),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	store.invocations = []sqlc.StageInvocation{{
		ID: "inv-1", RunID: "Tcp", TenantID: "tn", Stage: "spec", Sequence: 1, Cycle: 0,
		SessionID: nullStr("sess-1"),
	}}
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "never reached"},
	}}
	src := &staticSource{pk: scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})}
	corruptJob := sqlc.Job{
		Kind: "continue", RunID: "Tcp", TenantID: "tn", UserID: "us",
		Payload: json.RawMessage(`{"answers":["a legacy shape that must not be guessed into text"]}`),
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), corruptJob); err != nil {
		t.Fatalf("continue job: %v", err)
	}
	if got := store.taskState(); got != "paused_user_stop" {
		t.Fatalf("state = %q, want paused_user_stop", got)
	}
	if len(adapter.invocations) != 0 {
		t.Fatalf("adapter invoked %d time(s); an unreadable payload must stop before any invocation", len(adapter.invocations))
	}
	if !hasStopReason(store.events, "continue_payload_unreadable") {
		t.Errorf("no run.state_changed event carries stop_reason continue_payload_unreadable; events: %+v", store.events)
	}
}

// TestRunner_ContinueTextKeepsSourceWriteWithheld: the continuation text
// changes what the agent is TOLD, never what it is ALLOWED to do. Under a pack
// whose source_write approval is not granted, the resumed invocation carries
// the withheld profile — fs.write stays off — while the block carries both the
// original request and the user's answer.
func TestRunner_ContinueTextKeepsSourceWriteWithheld(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{
		ID: "Tsw", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: "test@0.1.0",
		Title:        "Add export endpoint", Description: "Export runs as CSV.",
		Overrides: json.RawMessage(`{"checks":{"required":["verify"]}}`),
	}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	// No approvals seeded: the pack's source_write approval is required and
	// not granted.
	adapter := &sequenceAdapter{results: []agent.ResultJSON{
		{SchemaVersion: "1", Status: agent.StatusBlocked, OpenQuestions: []string{"which database?"}},
		{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "plan done with the answer"},
	}}
	src := &staticSource{pk: approvalGatePack()}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleRun(t.Context(), job("run", "Tsw", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_open_questions" {
		t.Fatalf("setup: state = %q, want paused_open_questions", got)
	}
	continueWithText, jobErr := continueJob("Tsw", "tn", "us", "Use PostgreSQL 17.")
	if jobErr != nil {
		t.Fatalf("build continue job: %v", jobErr)
	}
	if err := New(Deps{Store: store, Packs: src, Adapter: adapter}).HandleContinue(t.Context(), continueWithText); err != nil {
		t.Fatalf("continue job: %v", err)
	}
	resumed := adapter.invocations[1]
	if !strings.Contains(resumed.RoutingBlock, "Use PostgreSQL 17.") {
		t.Errorf("resumed invocation's routing block does not carry the answer; got:\n%s", resumed.RoutingBlock)
	}
	withheld := make(map[caps.Category]bool, len(resumed.Profile.Source.Withheld))
	for _, category := range resumed.Profile.Source.Withheld {
		withheld[category] = true
	}
	for _, category := range caps.SourceWriteCategories {
		if !withheld[category] {
			t.Errorf("category %q is not withheld on the resumed invocation; the user text must not unlock source writes", category)
		}
	}
	if strings.Contains(resumed.RoutingBlock, `"required"`) || strings.Contains(resumed.RoutingBlock, `"checks":{`) {
		t.Errorf("resumed invocation's routing block leaks the raw overrides; got:\n%s", resumed.RoutingBlock)
	}
}
