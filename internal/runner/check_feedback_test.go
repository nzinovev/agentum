package runner

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// checkFeedbackHeading is the routing-block heading that carries failed check
// output to the agent; only the feedback invocation may see it.
const checkFeedbackHeading = "### Failed on your checkpoint commit"

// requireFixedCheck is a mandatory check that passes once the agent has
// written the file it looks for, and says what is missing when it fails.
const requireFixedCheck = "api: agentum/v1\nchecks:\n  - name: build\n    command: [\"sh\", \"-c\", \"test -f fixed || { echo 'fixed is missing' >&2; exit 1; }\"]\n    required: true\n"

// checkFeedbackAdapter records every invocation and, when fixes is set,
// writes the file the check needs only when the routing block carries the
// failed checks — the agent fixes what it was told about.
type checkFeedbackAdapter struct {
	stubExecution
	fixes bool

	mu    sync.Mutex
	calls []agent.Invocation
}

func (adapter *checkFeedbackAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite,
	}
}

func (adapter *checkFeedbackAdapter) Invoke(_ context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	adapter.mu.Lock()
	adapter.calls = append(adapter.calls, inv)
	adapter.mu.Unlock()
	if adapter.fixes && strings.Contains(inv.RoutingBlock, checkFeedbackHeading) {
		if err := os.WriteFile(filepath.Join(inv.Workdir, "fixed"), []byte("ok\n"), 0o644); err != nil {
			return nil, err
		}
	}
	sessionID := inv.ResumeSession
	if sessionID == "" {
		sessionID = "sess-" + stageOf(inv)
	}
	events := make(chan agent.Event, 1)
	events <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{
		SessionID: sessionID, ResultJSON: agent.ResultJSON{SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	close(events)
	return events, nil
}

func (adapter *checkFeedbackAdapter) stages() []string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	var stages []string
	for _, call := range adapter.calls {
		stages = append(stages, stageOf(call))
	}
	return stages
}

// TestRunner_CheckFeedbackRound: a mandatory check failure goes back to the
// agent that wrote the code — its own session, not the reviewer's — with the
// check output, and the run checks again without another review. One round
// only: a second failure stands.
func TestRunner_CheckFeedbackRound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		agentFixes bool
		wantState  string
		wantLog    string
	}{
		{name: "the fix passes and the run reaches final review", agentFixes: true, wantState: "awaiting_final_review"},
		{name: "a second failure fails the run", agentFixes: false, wantState: "failed", wantLog: "check build: fail (exit 1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := seedRepoWithChecks(t, requireFixedCheck)
			runPack := scriptPack("implement", map[string]pack.Stage{
				"implement": {Gate: pack.GateAuto, Prompt: "implement.md", Transitions: []pack.Transition{{To: "review"}}},
				"review":    {Gate: pack.GateAuto, Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
				"done":      {},
			})
			record := sqlc.Run{ID: "T-check-feedback", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
			project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
			store := newFakeStore(record, project)
			adapter := &checkFeedbackAdapter{fixes: tc.agentFixes}
			runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, CheckExec: checks.NewExecutor(checks.ExecutorDeps{})})

			handleErr := runner.HandleRun(t.Context(), job("run", record.ID, "tn", "us"))
			assertHandleError(t, handleErr, tc.wantState == "failed")
			if got := store.taskState(); got != tc.wantState {
				t.Fatalf("state = %q, want %q", got, tc.wantState)
			}

			if got, want := adapter.stages(), []string{"implement", "review", "implement"}; !slices.Equal(got, want) {
				t.Fatalf("invoked stages = %v, want %v: the round resumes the writer and skips review", got, want)
			}
			for index, call := range adapter.calls[:2] {
				if strings.Contains(call.RoutingBlock, checkFeedbackHeading) {
					t.Errorf("invocation %d saw check failures before any check ran", index+1)
				}
			}
			feedbackCall := adapter.calls[2]
			if feedbackCall.ResumeSession != "sess-implement" {
				t.Errorf("feedback resumed session %q, want the implementer's sess-implement", feedbackCall.ResumeSession)
			}
			for _, fragment := range []string{checkFeedbackHeading, "#### build: fail (exit 1)", "fixed is missing"} {
				if !strings.Contains(feedbackCall.RoutingBlock, fragment) {
					t.Errorf("feedback routing block lacks %q", fragment)
				}
			}
			feedbackInvocation := store.invocations[2]
			if feedbackInvocation.ResumeOf.String != store.invocations[0].ID || feedbackInvocation.Cycle != store.invocations[0].Cycle {
				t.Errorf("feedback invocation resume_of = %q cycle %d, want the implementer's %q cycle %d",
					feedbackInvocation.ResumeOf.String, feedbackInvocation.Cycle, store.invocations[0].ID, store.invocations[0].Cycle)
			}
			if got := countStoreEvents(store, EvCheckFeedbackSent); got != 1 {
				t.Errorf("%s events = %d, want 1", EvCheckFeedbackSent, got)
			}
			if got := countStoreEvents(store, EvProjectChecksRun); got != 2 {
				t.Errorf("%s events = %d, want 2", EvProjectChecksRun, got)
			}
			if tc.wantLog != "" && !strings.Contains(store.record.FailureLog, tc.wantLog) {
				t.Errorf("failure log lacks %q:\n%s", tc.wantLog, store.record.FailureLog)
			}
		})
	}
}

// TestHumanFixCheckFeedbackRoundBeforePause: a human-requested fix gets the
// same single round, sent to the fixer that wrote the correction; only a
// failure after it pauses for the person.
func TestHumanFixCheckFeedbackRoundBeforePause(t *testing.T) {
	repo := seedRepoWithChecks(t, requireFixedCheck)
	manager := worktree.New()
	baseCommit, err := manager.ResolveRef(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := manager.Create(t.Context(), repo, "human-fix-feedback", baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	runPack := scriptPack("fix", map[string]pack.Stage{
		"fix":    {Gate: pack.GateAuto, Role: "fixer", Prompt: "fix.md", Transitions: []pack.Transition{{To: "review"}}},
		"review": {Gate: pack.GateAuto, Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":   {},
	})
	record := sqlc.Run{ID: "T-human-fix-feedback", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0", BaseCommit: sql.NullString{String: baseCommit, Valid: true},
		CurrentStage:               sql.NullString{String: "done", Valid: true},
		ActiveFixRequestRevisionID: sql.NullString{String: "request-revision", Valid: true}}
	project := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, project)
	store.invocations = []sqlc.StageInvocation{
		{ID: "inv-1", RunID: record.ID, TenantID: "tn", Stage: "fix", Sequence: 1, Kind: "stage", SessionID: sql.NullString{String: "sess-fix", Valid: true}},
		{ID: "inv-2", RunID: record.ID, TenantID: "tn", Stage: "review", Sequence: 2, Kind: "stage", SessionID: sql.NullString{String: "sess-review", Valid: true}},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, CheckExec: checks.NewExecutor(checks.ExecutorDeps{})})
	run := stageRun{record: record, project: project, runPack: runPack, worktree: checkout}

	first, err := runner.runDeliveryChecks(t.Context(), run, "done")
	if err != nil || first.paused || first.feedback == nil {
		t.Fatalf("first failure = %+v, err %v; want a feedback round", first, err)
	}
	if first.feedback.stage != "fix" || first.feedback.session != "sess-fix" || first.feedback.feedback.returnStage != "done" {
		t.Errorf("feedback entry = %+v, want the fixer's session returning to done", first.feedback)
	}

	second, err := runner.runDeliveryChecks(t.Context(), run, "done")
	if err != nil || !second.paused {
		t.Fatalf("second failure = %+v, err %v; want the fix_checks_failed pause", second, err)
	}
	if store.record.State != "paused_user_stop" || store.record.StopReason != "fix_checks_failed" {
		t.Errorf("run after the spent round = %s/%s", store.record.State, store.record.StopReason)
	}
}

// TestCheckFailuresRefBudgets: the agent gets the end of each failed check's
// output, within a per-check and a total budget, and never a passing or
// optional check.
func TestCheckFailuresRefBudgets(t *testing.T) {
	longOutput := strings.Repeat("x", checkFeedbackOutputBytes) + "\nFAIL: TestThing"
	outcome := func(name string, required bool, status checks.Status, stderr string) checks.Outcome {
		return checks.Outcome{
			Item:   checks.Item{Definition: checks.Definition{Name: name}, Required: required},
			Status: status, ExitCode: 1, Stderr: stderr,
		}
	}
	report := checks.Report{Commit: "abc1234", Outcomes: []checks.Outcome{
		outcome("passing", true, checks.StatusPass, "ignored"),
		outcome("optional", false, checks.StatusFail, "ignored"),
		outcome("first", true, checks.StatusFail, longOutput),
		outcome("second", true, checks.StatusFail, longOutput),
		outcome("third", true, checks.StatusFail, longOutput),
		outcome("fourth", true, checks.StatusFail, longOutput),
	}}
	ref := checkFailuresRef(report)
	var names []string
	total := 0
	for _, failure := range ref.Failed {
		names = append(names, failure.Name)
		total += len(failure.Output)
	}
	if want := []string{"first", "second", "third", "fourth"}; !slices.Equal(names, want) {
		t.Fatalf("rendered checks = %v, want %v", names, want)
	}
	if !strings.HasSuffix(ref.Failed[0].Output, "FAIL: TestThing") || len(ref.Failed[0].Output) > checkFeedbackOutputBytes+32 {
		t.Errorf("first output must keep the end within the per-check cap: %d bytes", len(ref.Failed[0].Output))
	}
	if total > checkFeedbackTotalBytes+4*64 {
		t.Errorf("total output = %d bytes, over the %d budget", total, checkFeedbackTotalBytes)
	}
	if !strings.Contains(ref.Failed[3].Output, "budget for this block is spent") {
		t.Errorf("the check past the budget must say its output was omitted: %q", ref.Failed[3].Output)
	}
}

func countStoreEvents(store *fakeStore, eventType string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for _, event := range store.events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}
