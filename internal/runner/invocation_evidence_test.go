package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// invocationRecordsFromPatches folds the Invocations patches a fake manifest
// service recorded into one record per invocation id, applying the same
// fill-non-zero semantics the manifest merge applies (open pass then close
// pass). The manifest package's own merge is tested there; this local fold
// exists so the runner tests can read what the runner WROTE.
func invocationRecordsFromPatches(patches []manifest.Body) map[string]manifest.InvocationEvidence {
	records := make(map[string]manifest.InvocationEvidence)
	for _, patch := range patches {
		for _, record := range patch.Invocations {
			existing, found := records[record.InvocationID]
			if !found {
				records[record.InvocationID] = record
				continue
			}
			if record.Telemetry != nil {
				existing.Telemetry = record.Telemetry
			}
			if record.StopReason != "" {
				existing.StopReason = record.StopReason
			}
			records[record.InvocationID] = existing
		}
	}
	return records
}

// reviewRecords collects the folded records for one stage, in cycle order.
func reviewRecords(records map[string]manifest.InvocationEvidence, stageID string) []manifest.InvocationEvidence {
	out := make([]manifest.InvocationEvidence, 0, 2)
	for _, record := range records {
		if record.Stage == stageID {
			out = append(out, record)
		}
	}
	// Order by cycle so assertions can address attempts deterministically.
	for index := 1; index < len(out); index++ {
		for back := index; back > 0 && out[back-1].Cycle > out[back].Cycle; back-- {
			out[back-1], out[back] = out[back], out[back-1]
		}
	}
	return out
}

// evidencePackDir writes a spec → review ⇄ fix → done pack (with real prompt
// files, so stage prompt hashes are non-empty) into a temp dir and returns the
// dir for pack.Load. The in-memory branchPack fixture cannot do this: prompt
// text is set by the loader through a private setter.
func evidencePackDir(t *testing.T) string {
	t.Helper()
	packDir := t.TempDir()
	promptsDir := filepath.Join(packDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	manifest := `api: agentum/v1
pack: {name: evidence-test, version: 0.1.0}
tiers: {default: fast}
budgets: {fix_cycles: 2, ask_to_edit: 1}
entry: spec
stages:
  spec:
    gate: auto
    prompt: prompts/spec.md
    transitions: [{to: review}]
  review:
    gate: auto
    role: reviewer
    prompt: prompts/review.md
    transitions:
      - {to: fix, condition: 'verdict == "changes_requested"'}
      - {to: done, condition: 'verdict == "approved"'}
  fix:
    gate: auto_if_clean
    role: fixer
    prompt: prompts/fix.md
    transitions: [{to: review}]
  done: {}
`
	if err := os.WriteFile(filepath.Join(packDir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for stageID, text := range map[string]string{
		"spec": "Plan the requested behaviour.\n", "review": "Review the delivered change.\n", "fix": "Fix the findings.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, stageID+".md"), []byte(text), 0o600); err != nil {
			t.Fatalf("write prompt %s: %v", stageID, err)
		}
	}
	return packDir
}

// TestInvocationEvidence_FixCycleLeavesTwoReviewRecords is the runner half of
// the headline acceptance criterion (ADR 0005 D6/D7): a fix cycle re-running
// `review` writes TWO invocation records — each with its own model selection,
// capability profile, and rendered prompt hash — not one overwritten summary.
// The runtime version on every record is the probe's memoized value.
func TestInvocationEvidence_FixCycleLeavesTwoReviewRecords(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack, err := pack.Load(evidencePackDir(t))
	if err != nil {
		t.Fatalf("load pack: %v", err)
	}
	task := sqlc.Task{ID: "T-inv-evidence", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "evidence-test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &countingVerdictAdapter{
		results: map[string]agent.ResultJSON{
			"review": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "reviewed"},
			"spec":   {SchemaVersion: "1", Status: agent.StatusComplete},
			"fix":    {SchemaVersion: "1", Status: agent.StatusComplete},
		},
		reviewSequence: []agent.VerdictJSON{changesRequested("broken"), approvedVerdict("now ok")},
	}
	fakeManifest := &fakeManifestService{}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, Artifacts: newRecordingStore(),
	})
	runner.mfst = fakeManifest

	if err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if want := []string{"spec", "review", "fix", "review"}; !equalStringSlices(adapter.calls, want) {
		t.Fatalf("stage call order = %v, want %v (a fix cycle must have run)", adapter.calls, want)
	}

	records := invocationRecordsFromPatches(fakeManifest.addEvidence)
	reviews := reviewRecords(records, "review")
	if len(reviews) != 2 {
		t.Fatalf("review records = %d, want 2 (one per attempt): %+v", len(reviews), reviews)
	}
	first, second := reviews[0], reviews[1]
	if first.Cycle != 0 || second.Cycle != 1 {
		t.Errorf("review cycles = %d / %d, want 0 / 1", first.Cycle, second.Cycle)
	}
	if first.InvocationID == second.InvocationID || first.InvocationID == "" {
		t.Errorf("records must be keyed by distinct invocation ids: %q / %q", first.InvocationID, second.InvocationID)
	}
	// Model, capability profile, and both prompt hashes are independently
	// readable off EACH record — the schema-1 merge erased the first attempt's.
	for _, record := range []manifest.InvocationEvidence{first, second} {
		if record.Model.Tier == "" || record.Model.Options.Model == "" {
			t.Errorf("record %q carries no model selection: %+v", record.InvocationID, record.Model)
		}
		if record.Capabilities.Role == "" || len(record.Capabilities.Profile) == 0 {
			t.Errorf("record %q carries no capability profile: %+v", record.InvocationID, record.Capabilities)
		}
		if record.Prompt.StagePromptHash == "" || record.Prompt.RenderedHash == "" {
			t.Errorf("record %q carries no prompt hashes: %+v", record.InvocationID, record.Prompt)
		}
		// The runtime version is the probe's memoized value (stubExecution's).
		if record.Adapter.RuntimeVersion != "1.0.0-stub" || record.Adapter.ID != "stub" {
			t.Errorf("record %q adapter = %+v; want the stub probe's id and version", record.InvocationID, record.Adapter)
		}
	}
	// The rendered hash is what distinguishes two attempts at the same stage:
	// the second review saw a different diff and prior-stage refs.
	if first.Prompt.RenderedHash == second.Prompt.RenderedHash {
		t.Errorf("two review attempts must carry distinct rendered hashes: %q", first.Prompt.RenderedHash)
	}
	if first.Prompt.StagePromptHash != second.Prompt.StagePromptHash {
		t.Errorf("the pack prompt is the same for both attempts: %q / %q",
			first.Prompt.StagePromptHash, second.Prompt.StagePromptHash)
	}
}

// refusingAdapter is an adapter whose Invoke always refuses to start — the
// refused-start path of the two-pass evidence write (ADR 0005 D7).
type refusingAdapter struct {
	stubExecution
}

func (adapter *refusingAdapter) Supported() []caps.Category { return fullSupport }

func (adapter *refusingAdapter) Invoke(_ context.Context, _ agent.Invocation) (<-chan agent.Event, error) {
	return nil, errors.New("refused for the test")
}

// TestInvocationEvidence_RefusedStartRecordsStopReasonWithoutTelemetry (ADR
// 0005 D7): an invocation that never started still produces a record — the
// open half landed before Invoke — with a stop reason and no telemetry
// (nothing ran, nothing to bill).
func TestInvocationEvidence_RefusedStartRecordsStopReasonWithoutTelemetry(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T-refused", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	fakeManifest := &fakeManifestService{}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: &refusingAdapter{}})
	runner.mfst = fakeManifest

	if err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}

	records := invocationRecordsFromPatches(fakeManifest.addEvidence)
	if len(records) != 1 {
		t.Fatalf("records = %d, want exactly the refused attempt: %+v", len(records), records)
	}
	for _, record := range records {
		if record.StopReason != "adapter_error" {
			t.Errorf("StopReason = %q, want adapter_error", record.StopReason)
		}
		if record.Telemetry != nil {
			t.Errorf("a refused start must record no telemetry: %+v", record.Telemetry)
		}
		if record.Model.Tier == "" || record.Prompt.RenderedHash == "" || record.Capabilities.Role == "" {
			t.Errorf("the open half must have landed before the refusal: %+v", record)
		}
	}
}

// TestResolveExecutionPlan_UnresolvableTierFailsBeforeAnyInvocation (ADR 0005
// D4, point two): a pack stage naming a tier no configuration defines fails
// the task at run start — before any stage_invocation row is created, naming
// the stage and the tier.
func TestResolveExecutionPlan_UnresolvableTierFailsBeforeAnyInvocation(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Tier: "exotic", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T-bad-tier", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us"))
	if err == nil {
		t.Fatal("an unresolvable tier must fail the run")
	}
	if !contains(err.Error(), "exotic") || !contains(err.Error(), "spec") {
		t.Errorf("failure must name the stage and the tier: %v", err)
	}
	if got := store.taskState(); got != "failed" {
		t.Errorf("task state = %q, want failed", got)
	}
	store.mu.Lock()
	invocationCount := len(store.invocations)
	store.mu.Unlock()
	if invocationCount != 0 {
		t.Errorf("stage invocations created = %d, want 0 (fail before the first invocation)", invocationCount)
	}
}

// TestResolveExecutionPlan_ValidatesEveryStageOfThePack (ADR 0005 D4): a late
// stage's bad tier fails the run at start even though the entry stage resolves
// fine — validation covers the whole pack, not just the first stage.
func TestResolveExecutionPlan_ValidatesEveryStageOfThePack(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "late"}}},
		"late": {Gate: pack.GateAuto, Tier: "exotic", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	task := sqlc.Task{ID: "T-late-tier", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter})

	err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us"))
	if err == nil {
		t.Fatal("a late stage's unresolvable tier must fail the run at start")
	}
	if !contains(err.Error(), "late") {
		t.Errorf("failure must name the offending stage: %v", err)
	}
	store.mu.Lock()
	invocationCount := len(store.invocations)
	store.mu.Unlock()
	if invocationCount != 0 {
		t.Errorf("stage invocations created = %d, want 0", invocationCount)
	}
}

// contains is a tiny substring helper (the test package's indexOf works on
// indexed payloads; this reads plain strings).
func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}
