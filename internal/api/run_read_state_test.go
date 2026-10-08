package api

import (
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
	if response.RouteResolveError.Code != "pack_resolve_failed" || !strings.Contains(response.RouteResolveError.Message, "base commit") || strings.Contains(response.RouteResolveError.Message, "/repo") {
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

// TestSafeRouteResolveErrorRedactsPaths keeps local checkout paths out of
// the polled run response while preserving a machine-readable failure code.
func TestSafeRouteResolveErrorRedactsPaths(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		cause error
		code  string
	}{
		{name: "missing", cause: fmt.Errorf("/private/repo: %w", pack.ErrPackNotFound), code: "pack_not_found"},
		{name: "read", cause: fmt.Errorf("/private/repo: %w", pack.ErrPackReadFailed), code: "pack_read_failed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			failure := safeRouteResolveError(testCase.cause)
			if failure.Code != testCase.code || strings.Contains(failure.Message, "/private/") {
				t.Errorf("route error = %+v", failure)
			}
		})
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

// TestSelectRunRouteCannotOverwriteCancellation protects the durable state
// when a triage adapter returns after the cancel handler commits.
func TestSelectRunRouteCannotOverwriteCancellation(t *testing.T) {
	harness := newRegistrationHarness(t)
	repositoryPath := t.TempDir()
	initRegistrationRepo(t, repositoryPath, "cancel route selection")
	project := harness.registerProject(repositoryPath)
	record := harness.seedRun(project.ID, "created", repositoryPath)
	if _, err := harness.queries.UpdateRunState(t.Context(), sqlc.UpdateRunStateParams{
		ID: record.ID, TenantID: testTenantID, State: "cancelled",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := harness.queries.SelectRunRoute(t.Context(), sqlc.SelectRunRouteParams{
		ID: record.ID, TenantID: testTenantID, PipelinePack: "small-change",
		RouteSource: sql.NullString{String: "triage", Valid: true}, RouteReason: "late answer",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("late route selection error = %v, want sql.ErrNoRows", err)
	}
	stored, err := harness.queries.GetRun(t.Context(), sqlc.GetRunParams{ID: record.ID, TenantID: testTenantID})
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "cancelled" || stored.RouteSource.Valid {
		t.Errorf("late triage changed cancelled run: %+v", stored)
	}
}
