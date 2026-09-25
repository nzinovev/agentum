package runner

import (
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// stubCatalogAdapter is a scriptAdapter whose catalog facts the evidence
// builder reads. The scripts satisfy the pack; the catalog states are the
// point of these tests.
func newCatalogEvidenceRunner(t *testing.T, stub stubExecution) (*Runner, *fakeManifestService) {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := catalogRefusalPack()
	record := sqlc.Run{ID: "Tev", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{
		stubExecution: stub,
		scripts: map[string]agent.ResultJSON{
			"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter})
	fake := &fakeManifestService{}
	runner.mfst = fake
	return runner, fake
}

// recordedAdapterSection returns the last Adapter section the manifest
// service received, or nil.
func recordedAdapterSection(service *fakeManifestService) *manifest.AdapterEvidence {
	service.mu.Lock()
	defer service.mu.Unlock()
	var section *manifest.AdapterEvidence
	for _, patch := range service.addEvidence {
		if patch.Adapter != nil {
			section = patch.Adapter
		}
	}
	return section
}

// workingStubCatalog lists every model the stub's tiers resolve to, so a run
// against it is genuinely checked-and-passed rather than refused.
func workingStubCatalog() models.Catalog {
	return models.Catalog{
		Available: true,
		Models: []models.CatalogModel{
			{ID: "stub/fast-model", VariantsKnown: true},
			{ID: "stub/strong-model", VariantsKnown: true},
			{ID: "stub/reasoning-model", VariantsKnown: true},
		},
	}
}

// TestRunner_CatalogLabelInEvidence_Failed: a run whose catalog could not be
// obtained records the failure in the adapter section — the unchecked mode
// must be visible after the fact, not absent.
func TestRunner_CatalogLabelInEvidence_Failed(t *testing.T) {
	t.Parallel()
	runner, fake := newCatalogEvidenceRunner(t, stubExecution{
		enumeratesModels: true,
		catalog:          models.Catalog{Reason: "timeout"},
	})
	if runErr := runner.HandleRun(t.Context(), job("run", "Tev", "tn", "us")); runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	section := recordedAdapterSection(fake)
	if section == nil {
		t.Fatal("no adapter section was recorded")
	}
	if section.ModelCatalog != "failed: timeout" {
		t.Errorf("ModelCatalog = %q; want failed: timeout", section.ModelCatalog)
	}
}

// TestRunner_CatalogLabelInEvidence_Ok: a working catalog records the count it
// read, next to the runtime probe label it parallels.
func TestRunner_CatalogLabelInEvidence_Ok(t *testing.T) {
	t.Parallel()
	runner, fake := newCatalogEvidenceRunner(t, stubExecution{
		enumeratesModels: true,
		catalog:          workingStubCatalog(),
	})
	if runErr := runner.HandleRun(t.Context(), job("run", "Tev", "tn", "us")); runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	section := recordedAdapterSection(fake)
	if section == nil {
		t.Fatal("no adapter section was recorded")
	}
	if section.ModelCatalog != "ok (3 models)" {
		t.Errorf("ModelCatalog = %q; want ok (3 models)", section.ModelCatalog)
	}
	if section.RuntimeProbe != "ok" {
		t.Errorf("RuntimeProbe = %q; the catalog label must sit beside it, not replace it", section.RuntimeProbe)
	}
}

// TestRunner_CatalogLabelInEvidence_Unsupported: an adapter that cannot list
// its runtime's models records exactly that — "never checked" stays distinct
// from "checked and passed".
func TestRunner_CatalogLabelInEvidence_Unsupported(t *testing.T) {
	t.Parallel()
	runner, fake := newCatalogEvidenceRunner(t, stubExecution{})
	if runErr := runner.HandleRun(t.Context(), job("run", "Tev", "tn", "us")); runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	section := recordedAdapterSection(fake)
	if section == nil {
		t.Fatal("no adapter section was recorded")
	}
	if section.ModelCatalog != models.CatalogUnsupported {
		t.Errorf("ModelCatalog = %q; want %q", section.ModelCatalog, models.CatalogUnsupported)
	}
}
