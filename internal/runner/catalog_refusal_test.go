package runner

import (
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// catalogRefusalPack is a single auto stage: the fastest shape that reaches
// the run-start execution plan and would proceed to an invocation.
func catalogRefusalPack() *pack.Pack {
	return scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
}

// newCatalogRefusalRunner wires a runner whose adapter declares a catalog.
// catalog Available with the given models simulates a runtime that answered;
// a zero value simulates one that could not be asked.
func newCatalogRefusalRunner(t *testing.T, catalog models.Catalog) (*Runner, *fakeStore) {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	record := sqlc.Run{ID: "Tcat", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{
		stubExecution: stubExecution{enumeratesModels: true, catalog: catalog},
		scripts: map[string]agent.ResultJSON{
			"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: catalogRefusalPack()}, Adapter: adapter})
	return runner, store
}

// TestRunner_StageOnUnknownModelFailsRunStart: a stage whose tier resolves to
// a model the runtime's catalog does not contain fails the run at start —
// zero stage_invocations, zero subprocesses — and the error names the tier,
// the model, and the adapter.
func TestRunner_StageOnUnknownModelFailsRunStart(t *testing.T) {
	t.Parallel()
	// The catalog lists one unrelated model; every stub tier model is unknown.
	runner, store := newCatalogRefusalRunner(t, models.Catalog{
		Available: true,
		Source:    "stub-agent models",
		Models:    []models.CatalogModel{{ID: "stub/somewhere-else"}},
	})

	runErr := runner.HandleRun(t.Context(), job("run", "Tcat", "tn", "us"))
	if runErr == nil {
		t.Fatal("a run whose stage model is unknown must fail at start")
	}
	if got := store.taskState(); got != "failed" {
		t.Fatalf("state = %q; want failed", got)
	}
	if count := len(store.invocations); count != 0 {
		t.Fatalf("stage invocations = %d; want 0 (the refusal is before the first invocation)", count)
	}
	message := runErr.Error()
	for _, wanted := range []string{`"fast"`, "stub/fast-model", `"stub"`} {
		if !strings.Contains(message, wanted) {
			t.Errorf("error %q does not name %q (tier, model, adapter must all be there)", message, wanted)
		}
	}
}

// TestRunner_UnavailableCatalogLetsTheRunProceed: the same run against a
// catalog that could not be obtained completes normally — "could not check"
// is never a refusal — and the run's evidence records that fact (the label
// assertion lives with the evidence tests).
func TestRunner_UnavailableCatalogLetsTheRunProceed(t *testing.T) {
	t.Parallel()
	runner, store := newCatalogRefusalRunner(t, models.Catalog{Reason: "timeout"})

	if runErr := runner.HandleRun(t.Context(), job("run", "Tcat", "tn", "us")); runErr != nil {
		t.Fatalf("run with an unavailable catalog failed: %v", runErr)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state = %q; want the run to reach the final gate", got)
	}
	if count := len(store.invocations); count != 1 {
		t.Fatalf("stage invocations = %d; want 1 (the stage ran)", count)
	}
}
