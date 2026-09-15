package api

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The event-delivery contract of the model check, against a real Postgres: a
// check accepted through the API leaves exactly three events on the tenant
// stream — started, one model_checked per target, finished — in order, each
// carrying the check_id, with the human actor and no run id (a check is not a
// run). These need the events table; the HTTP-surface behaviour is covered
// without a database in models_test.go.

// modelTestEventHarness bundles one database and one API wired for the model
// check surface.
type modelTestEventHarness struct {
	api       *API
	queries   *sqlc.Queries
	principal authz.Principal
}

func newModelTestEventHarness(t *testing.T) *modelTestEventHarness {
	t.Helper()
	handle := dbtest.Store(t)
	fake := &modelSurfaceAdapter{
		descriptor: modelEventDescriptor(),
		catalog:    modelEventCatalog(),
	}
	apiInst := New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil,
		WithExecutionAdapter(fake),
		WithResolvedTiers(modelEventTiers()),
	)
	return &modelTestEventHarness{
		api:       apiInst,
		queries:   handle.Queries,
		principal: authz.Principal{TenantID: testTenantID, UserID: testUserID},
	}
}

func modelEventDescriptor() agent.Descriptor {
	return agent.Descriptor{
		ID: "event-stub", AdapterVersion: "0.0.0-test",
		ModelOptions:     []models.OptionName{models.OptionModel},
		EnumeratesModels: true,
	}
}

func modelEventCatalog() models.Catalog {
	return models.Catalog{Available: true, Models: []models.CatalogModel{
		{ID: "prov/one-model", VariantsKnown: true},
	}}
}

func modelEventTiers() models.Config {
	return models.Config{
		Tiers:   map[string]string{"fast": "prov/one-model"},
		Default: "fast",
	}
}

// startModelTest accepts a check through the handler and returns its id.
func (harness *modelTestEventHarness) startModelTest(t *testing.T) string {
	t.Helper()
	request := httptest.NewRequest("POST", "/api/v1/models/test", strings.NewReader(`{"tier":"fast"}`))
	request.Header.Set("Idempotency-Key", "key-events")
	request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
	recorder := httptest.NewRecorder()
	harness.api.handleStartModelTest(recorder, request)
	if recorder.Code != 202 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	var accepted struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}
	return accepted.CheckID
}

// TestModelCheckEvents_ArriveInOrderOnTheTenantStream pins the delivery
// contract: started → model_checked (one per target) → finished, every frame
// carrying the check_id, the actor reading human, and no run id mixed in.
func TestModelCheckEvents_ArriveInOrderOnTheTenantStream(t *testing.T) {
	harness := newModelTestEventHarness(t)
	checkID := harness.startModelTest(t)

	var events []sqlc.Event
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, listErr := harness.queries.ListEventsAfter(t.Context(), sqlc.ListEventsAfterParams{
			TenantID: testTenantID, Limit: 100,
		})
		if listErr != nil {
			t.Fatalf("list events: %v", listErr)
		}
		var modelEvents []sqlc.Event
		for _, row := range rows {
			if strings.HasPrefix(row.Type, "models.test_") {
				modelEvents = append(modelEvents, row)
			}
		}
		if len(modelEvents) >= 3 {
			events = modelEvents
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(events) != 3 {
		t.Fatalf("collected %d model-check events; want exactly 3 (started, checked, finished): %+v", len(events), events)
	}

	wantOrder := []string{EvModelsTestStarted, EvModelsTestModelChecked, EvModelsTestFinished}
	for index, wantType := range wantOrder {
		if events[index].Type != wantType {
			t.Errorf("event %d = %q; want %q", index, events[index].Type, wantType)
		}
		if events[index].Actor != string(authz.ActorHuman) {
			t.Errorf("event %q actor = %q; want human — a person asked for this check", events[index].Type, events[index].Actor)
		}
		if events[index].RunID.Valid {
			t.Errorf("event %q carries run_id %q; a check is not a run", events[index].Type, events[index].RunID.String)
		}
		var payload map[string]any
		if err := json.Unmarshal(events[index].Payload, &payload); err != nil {
			t.Fatalf("decode %q payload: %v", events[index].Type, err)
		}
		if payload["check_id"] != checkID {
			t.Errorf("event %q check_id = %v; want %q", events[index].Type, payload["check_id"], checkID)
		}
	}

	// The finishing event carries the result table — the durable copy a
	// reconnecting client replays from the stream after the registry's TTL.
	var finishedPayload struct {
		Results []struct {
			Model   string `json:"model"`
			Outcome string `json:"outcome"`
		} `json:"results"`
	}
	if err := json.Unmarshal(events[2].Payload, &finishedPayload); err != nil {
		t.Fatalf("decode finished payload: %v", err)
	}
	if len(finishedPayload.Results) != 1 || finishedPayload.Results[0].Model != "prov/one-model" {
		t.Errorf("finished payload results = %+v; want the one target", finishedPayload.Results)
	}
}
