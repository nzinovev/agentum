package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakePackCatalog serves fixed resolutions and listings. The handlers' logic
// under test is authz, the ref contract, merging, and rendering — resolution
// itself is covered by internal/pack's tests against a real ProjectSource.
type fakePackCatalog struct {
	builtin        []pack.Meta
	builtinPack    map[string]*pack.Resolved
	project        []pack.ProjectPackEntry
	projectPack    map[string]*pack.Resolved
	resolveErr     error
	listProjectErr error
	listBuiltinErr error
}

func (catalog *fakePackCatalog) ResolveForCommit(_ context.Context, ref, _, _ string) (*pack.Resolved, error) {
	if catalog.resolveErr != nil {
		return nil, catalog.resolveErr
	}
	resolved, ok := catalog.projectPack[packNameOf(ref)]
	if !ok {
		return nil, pack.ErrPackNotFound
	}
	return resolved, nil
}

func (catalog *fakePackCatalog) ResolveBuiltin(_ context.Context, ref string) (*pack.Resolved, error) {
	if catalog.resolveErr != nil {
		return nil, catalog.resolveErr
	}
	resolved, ok := catalog.builtinPack[packNameOf(ref)]
	if !ok {
		return nil, pack.ErrPackNotFound
	}
	return resolved, nil
}

func (catalog *fakePackCatalog) ListProjectPacks(_ context.Context, _, _ string) ([]pack.ProjectPackEntry, error) {
	if catalog.listProjectErr != nil {
		return nil, catalog.listProjectErr
	}
	return catalog.project, nil
}

func (catalog *fakePackCatalog) ListBuiltin(_ context.Context) ([]pack.Meta, error) {
	if catalog.listBuiltinErr != nil {
		return nil, catalog.listBuiltinErr
	}
	return catalog.builtin, nil
}

// packNameOf strips a version constraint off a ref ("x@^1" → "x").
func packNameOf(ref string) string {
	if index := strings.IndexByte(ref, '@'); index >= 0 {
		return ref[:index]
	}
	return ref
}

// fakeRefResolver resolves every ref to a fixed commit.
type fakeRefResolver struct{ commit string }

func (resolver fakeRefResolver) ResolveRef(_ context.Context, _, _ string) (string, error) {
	return resolver.commit, nil
}

// catalogTestPack builds a minimal resolved pack for rendering assertions.
// FieldOrigins rides only the inherited shape, the way ProjectSource builds it.
func catalogTestPack(name, version string, origin pack.Origin) *pack.Resolved {
	resolved := &pack.Resolved{
		Pack: &pack.Pack{
			API: pack.APIVersion, Pack: pack.Meta{Name: name, Version: version, Persona: "engineering"},
			Budgets: pack.Budgets{FixCycles: 2, AskToEdit: 3},
			Tiers:   pack.Tiers{Default: "fast"},
			Entry:   "plan",
			Stages: map[string]pack.Stage{
				"plan": {Gate: pack.GateHumanApproval, Role: "analyst", Prompt: "prompts/plan.md",
					Transitions: []pack.Transition{{To: "done"}}},
				"done": {},
			},
		},
		Origin: origin,
	}
	if origin == pack.OriginProjectOverBuiltin {
		resolved.FieldOrigins = map[string]string{"stages.plan.gate": "project"}
	}
	return resolved
}

func principalRequest(method, target string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	return request.WithContext(authz.WithPrincipal(request.Context(),
		authz.Principal{TenantID: "tn", UserID: "us"}))
}

func TestHandleListPacks_BuiltinOnly(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{
			builtin: []pack.Meta{
				{Name: "backend-development", Version: "0.1.0", Persona: "engineering"},
				{Name: "minimal", Version: "0.1.0", Persona: "engineering"},
			},
		}))
	recorder := httptest.NewRecorder()
	api.handleListPacks(recorder, principalRequest(http.MethodGet, "/api/v1/packs"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response packListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Packs) != 2 {
		t.Fatalf("packs = %+v, want the two builtins", response.Packs)
	}
	if response.Packs[0].Origin != string(pack.OriginBuiltin) {
		t.Errorf("origin = %q, want builtin", response.Packs[0].Origin)
	}
	if response.Commit != "" {
		t.Errorf("commit = %q, want empty without a project", response.Commit)
	}
}

func TestHandleListPacks_RequiresAccess(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil, WithPackCatalog(&fakePackCatalog{}))
	recorder := httptest.NewRecorder()
	api.handleListPacks(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/packs", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a principal", recorder.Code)
	}
}

func TestHandleListPacks_NotWiredIsServiceUnavailable(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil)
	recorder := httptest.NewRecorder()
	api.handleListPacks(recorder, principalRequest(http.MethodGet, "/api/v1/packs"))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the catalog is not wired", recorder.Code)
	}
}

// The project-scoped listing requires an explicit ref — the same model run
// creation's base_ref follows; no silent HEAD. Checked before the project
// lookup, so the 400 names the caller's own input and leaks nothing.
func TestHandleListPacks_ProjectRequiresRef(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{}), WithGitRefs(fakeRefResolver{commit: "commit-abc"}))
	recorder := httptest.NewRecorder()
	api.handleListPacks(recorder, principalRequest(http.MethodGet, "/api/v1/packs?project_id=P1"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without ref", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "ref is required") {
		t.Fatalf("body = %s, want the ref-required hint", recorder.Body.String())
	}
}

// mergePackListings is the catalog's shadowing rule: a project pack replaces
// the builtin of the same name (one row, the project's origin), an overrides
// directory lists as project+builtin, a broken project pack keeps its entry
// with empty metadata, and the result is sorted.
func TestMergePackListings(t *testing.T) {
	t.Parallel()
	builtin := []pack.Meta{
		{Name: "backend-development", Version: "0.1.0", Persona: "engineering", Description: "base pack"},
		{Name: "minimal", Version: "0.1.0", Persona: "engineering"},
	}
	project := []pack.ProjectPackEntry{
		{Name: "backend-development", Kind: "overrides"},
		{Name: "my-flow", Kind: "manifest"},
	}
	identity := func(name string) (pack.Meta, bool) {
		if name == "my-flow" {
			return pack.Meta{Name: "my-flow", Version: "9.9.9", Persona: "engineering"}, true
		}
		// backend-development does not resolve (a broken overrides document,
		// say): the entry survives with empty metadata, not the whole listing
		// failing.
		return pack.Meta{}, false
	}
	merged := mergePackListings(builtin, project, identity)
	if len(merged) != 3 {
		t.Fatalf("merged = %+v, want 3 entries (minimal + two project packs)", merged)
	}
	byName := map[string]packListEntry{}
	for _, entry := range merged {
		byName[entry.Name] = entry
	}
	if entry, ok := byName["backend-development"]; !ok || entry.Origin != string(pack.OriginProjectOverBuiltin) || entry.Version != "" {
		t.Errorf("backend-development = %+v (%v), want one project+builtin row with degraded metadata", entry, ok)
	}
	if entry, ok := byName["minimal"]; !ok || entry.Origin != string(pack.OriginBuiltin) {
		t.Errorf("minimal = %+v (%v), want the builtin row", entry, ok)
	}
	if entry, ok := byName["my-flow"]; !ok || entry.Origin != string(pack.OriginProject) || entry.Version != "9.9.9" {
		t.Errorf("my-flow = %+v (%v), want a project row with resolved identity", entry, ok)
	}
	if !(merged[0].Name == "backend-development" && merged[1].Name == "minimal" && merged[2].Name == "my-flow") {
		t.Errorf("merged = %+v, want sorted by name", merged)
	}
}

func TestHandleGetPack_BuiltinDetail(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{
			builtinPack: map[string]*pack.Resolved{
				"backend-development": catalogTestPack("backend-development", "0.1.0", pack.OriginBuiltin),
			},
		}))
	recorder := httptest.NewRecorder()
	request := principalRequest(http.MethodGet, "/api/v1/packs/backend-development")
	request.SetPathValue("name", "backend-development")
	api.handleGetPack(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var detail packDetailResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Origin != string(pack.OriginBuiltin) {
		t.Errorf("origin = %q, want builtin", detail.Origin)
	}
	if detail.Entry != "plan" || len(detail.Stages) != 2 {
		t.Errorf("detail = %+v, want the full graph", detail)
	}
	if detail.Stages[0].ID != "plan" || detail.Stages[1].ID != "done" {
		t.Errorf("stages = %+v, want entry-first graph order", detail.Stages)
	}
	if detail.Budgets.FixCycles != 2 || detail.Budgets.AskToEdit != 3 {
		t.Errorf("budgets = %+v, want the pack's declared budgets", detail.Budgets)
	}
	if detail.FieldOrigins != nil {
		t.Errorf("field_origins = %v, want absent for a builtin pack", detail.FieldOrigins)
	}
}

func TestHandleGetPack_InheritedDetailCarriesFieldOrigins(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{
			builtinPack: map[string]*pack.Resolved{
				"backend-development": catalogTestPack("backend-development", "0.1.0", pack.OriginProjectOverBuiltin),
			},
		}))
	recorder := httptest.NewRecorder()
	request := principalRequest(http.MethodGet, "/api/v1/packs/backend-development@^0")
	request.SetPathValue("name", "backend-development@^0")
	api.handleGetPack(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var detail packDetailResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Origin != string(pack.OriginProjectOverBuiltin) {
		t.Errorf("origin = %q, want project+builtin", detail.Origin)
	}
	if detail.FieldOrigins["stages.plan.gate"] != "project" {
		t.Errorf("field_origins = %v, want the patched gate attributed to the project", detail.FieldOrigins)
	}
}

func TestHandleGetPack_UnknownIsNotFound(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil, WithPackCatalog(&fakePackCatalog{}))
	recorder := httptest.NewRecorder()
	request := principalRequest(http.MethodGet, "/api/v1/packs/no-such-pack")
	request.SetPathValue("name", "no-such-pack")
	api.handleGetPack(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestHandleGetPack_ConfigurationErrorIsBadRequest(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{resolveErr: pack.ErrBothPackFiles}))
	recorder := httptest.NewRecorder()
	request := principalRequest(http.MethodGet, "/api/v1/packs/probe")
	request.SetPathValue("name", "probe")
	api.handleGetPack(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a sentinel configuration error", recorder.Code)
	}
}

// A failure to read the pack's bytes is the server's own: a 500 that does not
// hand the caller git's stderr, never a 400 blaming the request.
func TestHandleGetPack_ReadFailureIsInternal(t *testing.T) {
	t.Parallel()
	readFailure := fmt.Errorf("list tree: %w: %w", pack.ErrPackReadFailed, errors.New("fatal: not a git repository"))
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{resolveErr: readFailure}))
	recorder := httptest.NewRecorder()
	request := principalRequest(http.MethodGet, "/api/v1/packs/probe")
	request.SetPathValue("name", "probe")
	api.handleGetPack(recorder, request)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a read failure", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "not a git repository") {
		t.Errorf("body = %s, must not carry the underlying read error", recorder.Body.String())
	}
}

// TestHandleListProjectPacks_ReadFailures: a tree read failure returns 500
// without Git diagnostics whether it happens during discovery or resolution.
func TestHandleListProjectPacks_ReadFailures(t *testing.T) {
	harness := newRegistrationHarness(t)
	repo := t.TempDir()
	initRegistrationRepo(t, repo, "pack catalog read failure")
	project := harness.registerProject(repo)
	harness.api.gitRefs = fakeRefResolver{commit: "commit-abc"}
	readFailure := fmt.Errorf("tree read: %w: %w", pack.ErrPackReadFailed, errors.New("fatal: private checkout path"))
	cases := []struct {
		name    string
		catalog *fakePackCatalog
	}{
		{name: "discovery", catalog: &fakePackCatalog{listProjectErr: readFailure}},
		{name: "resolution", catalog: &fakePackCatalog{
			project: []pack.ProjectPackEntry{{Name: "probe", Kind: "manifest"}}, resolveErr: readFailure,
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness.api.packs = testCase.catalog
			request := httptest.NewRequest(http.MethodGet,
				"/api/v1/packs?project_id="+project.ID+"&ref=main", nil)
			request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
			recorder := httptest.NewRecorder()
			harness.api.handleListPacks(recorder, request)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s; want 500", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "private checkout path") {
				t.Errorf("response exposed Git diagnostics: %s", recorder.Body.String())
			}
		})
	}
}

// The gate helpers must propagate a resolution failure instead of silently
// falling back: a fallback would write run_approvals under the builtin's
// approval name while the runner reads the project pack's, leaving
// source-write locked for the rest of the run.
func TestResolveRunPack_ErrorPropagates_NoBuiltinFallback(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{resolveErr: errors.New("unreadable tree")}))

	// A started run (base_commit + checkout pinned): the error surfaces.
	started := sqlc.Run{PipelinePack: "probe", BaseCommit: sql.NullString{String: "commit-1", Valid: true}, CheckoutPath: "/repo"}
	if _, err := api.planApprovalName(t.Context(), started); err == nil {
		t.Fatal("planApprovalName must propagate the resolution error for a started run")
	}
	if _, _, err := api.planApprovalForStage(t.Context(), started, "plan"); err == nil {
		t.Fatal("planApprovalForStage must propagate the resolution error for a started run")
	}
	budgetRequest := principalRequest(http.MethodPost, "/")
	if _, err := api.packAskToEditBudget(budgetRequest, started); err == nil {
		t.Fatal("packAskToEditBudget must propagate the resolution error for a started run")
	}

	// A run that never started (no pins): no pack, no error — the pre-start
	// behavior every handler keeps.
	fresh := sqlc.Run{PipelinePack: "probe"}
	name, err := api.planApprovalName(t.Context(), fresh)
	if err != nil || name != "" {
		t.Fatalf("planApprovalName = (%q, %v), want empty and no error pre-start", name, err)
	}
}
