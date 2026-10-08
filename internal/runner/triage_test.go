package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

type scriptedRouteCatalog struct {
	entries  []pack.Meta
	resolved *pack.Pack
}

type mixedRouteCatalog struct {
	resolved map[string]*pack.Resolved
	entries  []pack.ProjectPackEntry
}

func (catalog mixedRouteCatalog) ListBuiltin(context.Context) ([]pack.Meta, error) {
	return []pack.Meta{{Name: routeDefaultPack, Description: "full route"}}, nil
}
func (catalog mixedRouteCatalog) ListProjectPacks(context.Context, string, string) ([]pack.ProjectPackEntry, error) {
	return catalog.entries, nil
}
func (catalog mixedRouteCatalog) ResolveForFloor(_ context.Context, name, _, _ string) (*pack.Resolved, error) {
	resolved := catalog.resolved[name]
	if resolved == nil {
		return nil, fmt.Errorf("unreadable project pack")
	}
	return resolved, nil
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
	chosen       string
	count        int
	profile      caps.Profile
	routingBlock string
	selection    models.Selection
	delay        time.Duration
	invokeErr    error
}

func (adapter *scriptedTriageAdapter) Supported() []caps.Category {
	return []caps.Category{caps.CatFsRead, caps.CatGitRead, caps.CatArtifactWrite, caps.CatFsWrite, caps.CatExecBash, caps.CatGitWrite}
}
func (adapter *scriptedTriageAdapter) Invoke(_ context.Context, invocation agent.Invocation) (<-chan agent.Event, error) {
	adapter.count++
	adapter.profile = invocation.Profile
	adapter.routingBlock = invocation.RoutingBlock
	adapter.selection = invocation.Model
	if adapter.delay > 0 {
		time.Sleep(adapter.delay)
	}
	if adapter.invokeErr != nil {
		return nil, adapter.invokeErr
	}
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
			store := newFakeStore(record, sqlc.Project{Name: "Readable Project"})
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
			if !strings.Contains(adapter.routingBlock, "--- BEGIN TASK REQUEST ---") || !strings.Contains(adapter.routingBlock, "## Route catalog") || strings.Count(adapter.routingBlock, "## Task") != 1 {
				t.Errorf("triage did not use the routing block: %s", adapter.routingBlock)
			}
			if !strings.Contains(adapter.routingBlock, "Readable Project") || strings.Contains(adapter.routingBlock, "project P1") {
				t.Errorf("triage project name = %s", adapter.routingBlock)
			}
			if selected.RouteTriageInvocationID.String == "" || len(evidence.addEvidence) == 0 || len(evidence.addEvidence[0].Invocations) != 1 {
				t.Error("triage invocation missing from run or evidence")
			}
		})
	}
}

type blockingTriageAdapter struct {
	stubExecution
	started chan struct{}
}

func (adapter *blockingTriageAdapter) Supported() []caps.Category {
	return []caps.Category{caps.CatFsRead, caps.CatGitRead, caps.CatArtifactWrite}
}

func (adapter *blockingTriageAdapter) Invoke(ctx context.Context, invocation agent.Invocation) (<-chan agent.Event, error) {
	if !strings.Contains(invocation.RoutingBlock, "stage **triage**") {
		return nil, fmt.Errorf("stage started after triage cancellation")
	}
	stream := make(chan agent.Event, 1)
	close(adapter.started)
	go func() {
		defer close(stream)
		<-ctx.Done()
		stream <- agent.Event{Kind: agent.EventError, Err: ctx.Err()}
	}()
	return stream, nil
}

// TestCancelDuringTriageStopsBeforeWorktree protects the interval between
// route selection and the first stage from an obsolete run state write.
func TestCancelDuringTriageStopsBeforeWorktree(t *testing.T) {
	repository := t.TempDir()
	if err := initRepoWithCommit(repository); err != nil {
		t.Fatal(err)
	}
	record := sqlc.Run{ID: "triage-cancel", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repository, Name: "Project"})
	adapter := &blockingTriageAdapter{started: make(chan struct{})}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter})
	done := make(chan error, 1)
	go func() { done <- runner.HandleRun(t.Context(), job("run", record.ID, record.TenantID, record.UserID)) }()
	select {
	case <-adapter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("triage did not start")
	}
	if !runner.Cancels().Cancel(record.ID) {
		t.Fatal("triage cancel was not registered")
	}
	if _, err := store.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{ID: record.ID, TenantID: record.TenantID, State: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	if store.taskState() != "cancelled" || store.record.RouteSource.Valid || worktree.DirPresent(worktree.PathFor(repository, record.ID)) {
		t.Errorf("cancelled triage continued: run=%+v", store.record)
	}
	if len(store.invocations) != 1 || store.invocations[0].StopReason.String != "cancelled" {
		t.Errorf("triage evidence was not closed: %+v", store.invocations)
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

// TestTriageUsesConfiguredDefaultTier keeps triage available when the
// operator's tier set has no tier named fast.
func TestTriageUsesConfiguredDefaultTier(t *testing.T) {
	record := sqlc.Run{ID: "custom-tier", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := newFakeStore(record, sqlc.Project{})
	adapter := &scriptedTriageAdapter{chosen: routeDefaultPack}
	configuration := &models.Config{Tiers: map[string]models.TierDefinition{"strong": {Model: "stub/strong-model"}}, Default: "strong"}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter, Models: configuration})
	selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if selected.RouteSource.String != "triage" || adapter.count != 1 || adapter.selection.Tier != "strong" {
		t.Errorf("configured default tier did not run triage: %+v", selected)
	}
}

// TestTriagePrefersFastTier keeps routine route selection on fast when the
// operator defines it alongside a stronger default tier.
func TestTriagePrefersFastTier(t *testing.T) {
	record := sqlc.Run{ID: "fast-tier", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := newFakeStore(record, sqlc.Project{})
	adapter := &scriptedTriageAdapter{chosen: routeDefaultPack}
	configuration := &models.Config{Tiers: map[string]models.TierDefinition{
		"fast": {Model: "stub/fast-model"}, "strong": {Model: "stub/strong-model"},
	}, Default: "strong"}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter, Models: configuration})
	selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if selected.RouteSource.String != "triage" || adapter.selection.Tier != "fast" || adapter.selection.Options.Model != "stub/fast-model" {
		t.Errorf("triage selection = %+v, route = %+v", adapter.selection, selected)
	}
}

type contextCheckingTriageStore struct {
	*fakeStore
	finished []sqlc.FinishStageInvocationParams
}

func (store *contextCheckingTriageStore) FinishStageInvocation(ctx context.Context, params sqlc.FinishStageInvocationParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.finished = append(store.finished, params)
	return store.fakeStore.FinishStageInvocation(ctx, params)
}

type contextCheckingTriageManifest struct{ *fakeManifestService }

func (service *contextCheckingTriageManifest) AddEvidence(ctx context.Context, tenantID, runID string, patch manifest.Body) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return service.fakeManifestService.AddEvidence(ctx, tenantID, runID, patch)
}

// TestSlowTriageClosesInvocationAndEvidence requires the closure deadline to
// start after model output, even when the model call outlasts it.
func TestSlowTriageClosesInvocationAndEvidence(t *testing.T) {
	record := sqlc.Run{ID: "slow-triage", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := &contextCheckingTriageStore{fakeStore: newFakeStore(record, sqlc.Project{})}
	adapter := &scriptedTriageAdapter{chosen: routeDefaultPack, delay: 150 * time.Millisecond}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter})
	runner.triageCloseTimeout = 50 * time.Millisecond
	evidence := &contextCheckingTriageManifest{fakeManifestService: &fakeManifestService{}}
	runner.mfst = evidence
	selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if selected.RouteSource.String != "triage" || len(store.invocations) != 1 || !store.invocations[0].FinishedAt.Valid || store.invocations[0].StopReason.String != "complete" {
		t.Errorf("slow triage invocation = %+v, route = %+v", store.invocations, selected)
	}
	if len(evidence.addEvidence) != 2 || len(evidence.addEvidence[1].Invocations) != 1 || evidence.addEvidence[1].Invocations[0].StopReason != "complete" {
		t.Errorf("slow triage evidence = %+v", evidence.addEvidence)
	}
}

// TestFailedTriageStoresNoResult keeps the result column empty when the
// adapter produced nothing: a stored JSON null would read as a recorded result.
func TestFailedTriageStoresNoResult(t *testing.T) {
	record := sqlc.Run{ID: "failed-triage", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := &contextCheckingTriageStore{fakeStore: newFakeStore(record, sqlc.Project{})}
	adapter := &scriptedTriageAdapter{invokeErr: errors.New("runtime unavailable")}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: adapter})
	selected, err := runner.selectRoute(t.Context(), record, "/repo", "commit")
	if err != nil {
		t.Fatal(err)
	}
	if selected.RouteSource.String != "fallback" || selected.RouteFallbackCode != "triage_adapter_error" {
		t.Errorf("route after failed triage = %+v", selected)
	}
	if len(store.finished) != 1 || store.finished[0].StopReason.String != "adapter_error" || store.finished[0].Result.Valid {
		t.Errorf("failed triage invocation closed as %+v", store.finished)
	}
}

// TestRouteCandidatesSkipsInvalidProjectPacks protects the other choices
// when one project layer fails resolution, validation, or the policy floor.
func TestRouteCandidatesSkipsInvalidProjectPacks(t *testing.T) {
	repository := t.TempDir()
	if err := initRepoWithCommit(repository); err != nil {
		t.Fatal(err)
	}
	baseCommit, err := worktree.New().ResolveRef(t.Context(), repository, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	load := func(name string) *pack.Pack {
		loaded, loadErr := pack.Load("../../packs/small-change")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		loaded.Pack.Name = name
		return loaded
	}
	valid := load("small-change")
	badRole := load("bad-role")
	stage := badRole.Stages["implement"]
	stage.Role = "reviewer"
	badRole.Stages["implement"] = stage
	if badRole.Validate() == nil {
		t.Fatal("fixture must fail within_stage validation")
	}
	badFloor := load("bad-floor")
	badFloor.Capabilities = append(badFloor.Capabilities, "net.fetch")
	catalog := mixedRouteCatalog{
		entries: []pack.ProjectPackEntry{{Name: "unreadable"}, {Name: "bad-role"}, {Name: "bad-floor"}, {Name: "small-change"}},
		resolved: map[string]*pack.Resolved{
			"bad-role":     {Pack: badRole, Origin: pack.OriginProject, ValidateErr: badRole.Validate()},
			"bad-floor":    {Pack: badFloor, Origin: pack.OriginProject},
			"small-change": {Pack: valid, Origin: pack.OriginProject},
		},
	}
	record := sqlc.Run{ID: "route-candidates", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "small task"}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repository})
	adapter := &scriptedTriageAdapter{chosen: "small-change"}
	runner := New(Deps{Store: store, Packs: catalog, Adapter: adapter})
	candidates, err := runner.routeCandidates(t.Context(), record, store.project, catalog, repository, baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates["small-change"].Name != "small-change" || candidates[routeDefaultPack].Name != routeDefaultPack {
		t.Errorf("candidates = %+v", candidates)
	}
	selected, err := runner.selectRoute(t.Context(), record, repository, baseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if selected.PipelinePack != "small-change" || selected.RouteSource.String != "triage" || strings.Contains(adapter.routingBlock, "bad-role") || strings.Contains(adapter.routingBlock, "bad-floor") || strings.Contains(adapter.routingBlock, "unreadable") {
		t.Errorf("triage saw an invalid route: selected=%+v block=%s", selected, adapter.routingBlock)
	}
}

// TestInterruptedTriageClosesPriorAttempt keeps one open evidence row per run
// after a worker restart interrupts the first model call.
func TestInterruptedTriageClosesPriorAttempt(t *testing.T) {
	record := sqlc.Run{ID: "triage-recovery", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: routeDefaultPack, Title: "task"}
	store := newFakeStore(record, sqlc.Project{})
	store.invocations = []sqlc.StageInvocation{{ID: "first", RunID: record.ID, TenantID: record.TenantID, Stage: "triage", Kind: "triage"}}
	runner := New(Deps{Store: store, Packs: scriptedRouteCatalog{entries: []pack.Meta{{Name: routeDefaultPack}}}, Adapter: &scriptedTriageAdapter{chosen: routeDefaultPack}})
	if _, err := runner.selectRoute(t.Context(), record, "/repo", "commit"); err != nil {
		t.Fatal(err)
	}
	if len(store.invocations) != 2 || store.invocations[0].StopReason.String != "interrupted" || !store.invocations[0].FinishedAt.Valid || store.invocations[1].StopReason.String != "complete" {
		t.Errorf("recovered triage attempts = %+v", store.invocations)
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
