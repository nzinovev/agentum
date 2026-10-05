package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

func TestRunReadKeepsStoredRouteWhenPackResolutionFails(t *testing.T) {
	t.Parallel()
	api := New(nil, nil, slog.New(slog.DiscardHandler), nil,
		WithPackCatalog(&fakePackCatalog{resolveErr: errors.New("pack missing at pinned commit")}))
	record := sqlc.Run{
		ID: "run", PipelinePack: "project-route", CheckoutPath: "/repo",
		BaseCommit:  sql.NullString{String: "commit", Valid: true},
		RouteSource: sql.NullString{String: "triage", Valid: true},
		RouteReason: "The task needs this route.",
	}
	response := toRunResponse(record)
	api.populateRunRoute(t.Context(), record, &response)
	if response.PipelinePack != "project-route" || response.RouteReason != record.RouteReason || response.RouteGraph != nil || response.RouteResolveError == nil {
		t.Fatalf("route response = %+v", response)
	}
	if response.RouteResolveError.Code != "pack_resolve_failed" || !strings.Contains(response.RouteResolveError.Message, "pinned commit") {
		t.Errorf("route error = %+v", response.RouteResolveError)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"route_resolve_error":{"code":"pack_resolve_failed"`) {
		t.Errorf("route error not serialized: %s", encoded)
	}
}

func TestGetRunShowsPinnedPackResolutionError(t *testing.T) {
	harness := newRegistrationHarness(t)
	repositoryPath := t.TempDir()
	initRegistrationRepo(t, repositoryPath, "missing pinned route")
	project := harness.registerProject(repositoryPath)
	record := harness.seedRun(project.ID, "created", repositoryPath)
	baseCommit, err := worktree.New().ResolveRef(t.Context(), repositoryPath, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.SetBaseCommit(t.Context(), sqlc.SetBaseCommitParams{
		ID: record.ID, TenantID: testTenantID, BaseCommit: sql.NullString{String: baseCommit, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := harness.queries.SelectRunRoute(t.Context(), sqlc.SelectRunRouteParams{
		ID: record.ID, TenantID: testTenantID, PipelinePack: "missing-route",
		RouteSource: sql.NullString{String: "triage", Valid: true}, RouteReason: "Selected for this task",
	}); err != nil {
		t.Fatal(err)
	}
	harness.api.packs = &fakePackCatalog{resolveErr: errors.New("pack missing at pinned commit")}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+record.ID, nil)
	request.SetPathValue("id", record.ID)
	request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
	response := httptest.NewRecorder()
	harness.api.handleGetRun(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET run: %d %s", response.Code, response.Body.String())
	}
	var decoded runResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PipelinePack != "missing-route" || decoded.RouteReason != "Selected for this task" || decoded.RouteGraph != nil || decoded.RouteResolveError == nil || decoded.RouteResolveError.Code != "pack_resolve_failed" {
		t.Errorf("unresolved route response = %+v", decoded)
	}
}
