package runner

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type scriptedRouteCatalog struct {
	entries  []pack.Meta
	resolved *pack.Pack
}

func (catalog scriptedRouteCatalog) ListBuiltin(context.Context) ([]pack.Meta, error) {
	return catalog.entries, nil
}
func (catalog scriptedRouteCatalog) ListProjectPacks(context.Context, string, string) ([]pack.ProjectPackEntry, error) {
	return nil, nil
}
func (catalog scriptedRouteCatalog) ResolveForFloor(context.Context, string, string, string) (*pack.Resolved, error) {
	if catalog.resolved != nil {
		return &pack.Resolved{Pack: catalog.resolved, Origin: pack.OriginBuiltin}, nil
	}
	return nil, fmt.Errorf("unexpected project route")
}

type scriptedTriageAdapter struct {
	stubExecution
	chosen  string
	count   int
	profile caps.Profile
}

func (adapter *scriptedTriageAdapter) Supported() []caps.Category {
	return []caps.Category{caps.CatFsRead, caps.CatGitRead, caps.CatArtifactWrite, caps.CatFsWrite, caps.CatExecBash, caps.CatGitWrite}
}
func (adapter *scriptedTriageAdapter) Invoke(_ context.Context, invocation agent.Invocation) (<-chan agent.Event, error) {
	adapter.count++
	adapter.profile = invocation.Profile
	stream := make(chan agent.Event, 1)
	stream <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{ResultJSON: agent.ResultJSON{
		SchemaVersion: "1", Status: agent.StatusComplete,
		Summary: fmt.Sprintf(`{"pack":%q,"reason":"Selected for this task"}`, adapter.chosen),
	}}}
	close(stream)
	return stream, nil
}

func TestTriageSelectsAndPinsThreeRoutes(t *testing.T) {
	t.Parallel()
	catalog := scriptedRouteCatalog{entries: []pack.Meta{
		{Name: "backend-development", Description: "complex development"},
		{Name: "small-change", Description: "small clear change"},
		{Name: "research-first", Description: "unknown cause"},
	}}
	for _, routeName := range []string{"small-change", "backend-development", "research-first"} {
		t.Run(routeName, func(t *testing.T) {
			record := sqlc.Run{ID: "T-" + routeName, TenantID: "tn", UserID: "us", ProjectID: "P1", PipelinePack: routeDefaultPack, Title: routeName, Description: "task"}
			store := newFakeStore(record, sqlc.Project{})
			adapter := &scriptedTriageAdapter{chosen: routeName}
			runner := New(Deps{Store: store, Packs: catalog, Adapter: adapter})
			evidence := &fakeManifestService{}
			runner.mfst = evidence
			selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
			if err != nil {
				t.Fatal(err)
			}
			if selected.PipelinePack != routeName || selected.RouteSource.String != "triage" || selected.RouteReason == "" {
				t.Errorf("selected route = %+v", selected)
			}
			if adapter.count != 1 || adapter.profile.Has(caps.CatFsWrite) || adapter.profile.Has(caps.CatGitWrite) || adapter.profile.Has(caps.CatExecBash) {
				t.Errorf("triage count=%d profile=%+v", adapter.count, adapter.profile)
			}
			if selected.RouteTriageInvocationID.String == "" || len(evidence.addEvidence) == 0 || len(evidence.addEvidence[0].Invocations) != 1 {
				t.Error("triage invocation missing from run or evidence")
			}
		})
	}
}

func TestTriageUnknownPackFallsBackWithReason(t *testing.T) {
	t.Parallel()
	record := sqlc.Run{ID: "T-fallback", TenantID: "tn", UserID: "us", ProjectID: "P1", PipelinePack: routeDefaultPack, Title: "investigate", Description: "unknown cause"}
	store := newFakeStore(record, sqlc.Project{})
	adapter := &scriptedTriageAdapter{chosen: "not-in-catalog"}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter})
	selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if selected.PipelinePack != routeDefaultPack || selected.RouteSource.String != "fallback" || selected.RouteFallbackCode != "triage_unknown_pack" || !strings.Contains(selected.RouteFallbackMessage, "not-in-catalog") {
		t.Errorf("fallback = %+v", selected)
	}
}

func TestStoredRouteResumesWithoutTriage(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	if err := initRepoWithCommit(repository); err != nil {
		t.Fatal(err)
	}
	runPack := scriptPack("chosen_stage", map[string]pack.Stage{
		"chosen_stage": {Gate: pack.GateAuto, Prompt: "chosen.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":         {},
	})
	runPack.Pack.Name = "selected-route"
	record := sqlc.Run{
		ID: "T-stored-route", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running",
		PipelinePack: runPack.Pack.Name, RouteSource: sql.NullString{String: "triage", Valid: true},
		RouteReason: "Selected before the worker restarted",
	}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repository, Name: "Project"})
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"chosen_stage": {SchemaVersion: "1", Status: agent.StatusComplete},
	}}
	runner := New(Deps{
		Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}, resolved: runPack}, Adapter: adapter,
	})
	if err := runner.HandleRun(t.Context(), job("run", record.ID, record.TenantID, record.UserID)); err != nil {
		t.Fatal(err)
	}
	if store.taskState() != "awaiting_final_review" || len(store.invocations) != 1 || store.invocations[0].Stage != "chosen_stage" {
		t.Errorf("stored route was not resumed: state=%s invocations=%+v", store.taskState(), store.invocations)
	}
	if store.record.PipelinePack != runPack.Pack.Name || store.record.RouteReason != record.RouteReason {
		t.Errorf("stored route changed: %+v", store.record)
	}
}
