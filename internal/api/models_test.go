package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// modelSurfaceAdapter fakes the adapter behind the model surface: a fixed
// descriptor + catalog, and a ModelTester that counts invocations, records the
// selections it was handed, and can be held mid-check so "running" is
// observable deterministically.
type modelSurfaceAdapter struct {
	descriptor agent.Descriptor
	catalog    models.Catalog
	// testCalls counts TestModel invocations.
	testCalls int
	// selections records every selection TestModel was handed, so a test can
	// assert the pair that reached the adapter rather than the pair the
	// request carried.
	selections []models.Selection
	// release, when non-nil, is closed by TestModel before it returns: the
	// check blocks on gate until the test lets it finish.
	gate chan struct{}
	mu   sync.Mutex
}

func (fake *modelSurfaceAdapter) Invoke(context.Context, agent.Invocation) (<-chan agent.Event, error) {
	return nil, errors.New("the model surface must not invoke the adapter")
}
func (fake *modelSurfaceAdapter) Supported() []caps.Category { return nil }
func (fake *modelSurfaceAdapter) Describe() agent.Descriptor { return fake.descriptor }
func (fake *modelSurfaceAdapter) Probe(context.Context) agent.Readiness {
	return agent.Readiness{Ready: true}
}
func (fake *modelSurfaceAdapter) Catalog(context.Context) models.Catalog { return fake.catalog }

func (fake *modelSurfaceAdapter) TestModel(ctx context.Context, selection models.Selection, deadline time.Duration) agent.ModelCheck {
	fake.mu.Lock()
	fake.testCalls++
	fake.selections = append(fake.selections, selection)
	fake.mu.Unlock()
	if fake.gate != nil {
		// A blocked check is still deadline-bounded; the tests release long
		// before that.
		select {
		case <-fake.gate:
		case <-time.After(deadline):
		}
	}
	latency := 25 * time.Millisecond
	return agent.ModelCheck{
		Model: selection.Options.Model, Variant: selection.Options.Variant, Tier: selection.Tier,
		Outcome: agent.ModelCheckOK, Latency: latency, CheckedAt: time.Now().UTC(),
	}
}

func (fake *modelSurfaceAdapter) calls() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.testCalls
}

// handedSelections returns a copy of the selections TestModel received.
func (fake *modelSurfaceAdapter) handedSelections() []models.Selection {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]models.Selection(nil), fake.selections...)
}

// newModelSurfaceAPI builds an API wired for the model-surface tests: the
// fake adapter, three tiers (two naming one model — the dedupe shape), and
// the default test limits. queries is nil: these tests assert the HTTP
// surface and the registry, not the event stream (which the dbtest suite
// covers).
func newModelSurfaceAPI(t *testing.T, gate chan struct{}) (*API, *modelSurfaceAdapter) {
	t.Helper()
	descriptor := agent.Descriptor{
		ID:               "surface-stub",
		AdapterVersion:   "0.0.0-test",
		ModelOptions:     []models.OptionName{models.OptionModel},
		EnumeratesModels: true,
	}
	fake := &modelSurfaceAdapter{
		descriptor: descriptor,
		catalog: models.Catalog{
			Available: true,
			Source:    "surface-stub models",
			Models: []models.CatalogModel{
				{ID: "prov/one-model", VariantsKnown: true},
				{ID: "prov/other-model", VariantsKnown: true},
			},
		},
		gate: gate,
	}
	apiInst := New(nil, nil, nil, nil,
		WithExecutionAdapter(fake),
		WithResolvedTiers(models.Config{
			Tiers: map[string]models.TierDefinition{
				"fast": {Model: "prov/one-model"},
				// strong names the same pair as fast, so the empty-body
				// dedupe shape survives; deep carries the variant.
				"strong": {Model: "prov/one-model"},
				"deep":   {Model: "prov/other-model", Variant: "high"},
			},
			Default: "strong",
		}),
	)
	return apiInst, fake
}

// modelSurfaceRequest serves one request against the matching handler with a
// principal in the context, and returns the recorder.
func modelSurfaceRequest(t *testing.T, apiInst *API, method, target, idempotencyKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	return modelSurfaceRequestAs(t, apiInst, authzPrincipal(), method, target, idempotencyKey, body)
}

// modelSurfaceRequestAs is modelSurfaceRequest for an explicit principal —
// the multi-tenant probes need a second caller.
func modelSurfaceRequestAs(t *testing.T, apiInst *API, principal authz.Principal, method, target, idempotencyKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	request = request.WithContext(authz.WithPrincipal(request.Context(), principal))
	recorder := httptest.NewRecorder()
	switch {
	case method == "GET" && target == "/api/v1/models":
		apiInst.handleListModels(recorder, request)
	case method == "POST" && target == "/api/v1/models/test":
		apiInst.handleStartModelTest(recorder, request)
	case method == "GET" && strings.HasPrefix(target, "/api/v1/models/test/"):
		request.SetPathValue("id", strings.TrimPrefix(target, "/api/v1/models/test/"))
		apiInst.handleGetModelTest(recorder, request)
	default:
		t.Fatalf("no handler wired for %s %s", method, target)
	}
	return recorder
}

// TestModelsSurface_ListTiersAndCatalogStatus: GET /models serves the
// resolved tiers (sorted, with their models), the default tier, the adapter
// id, and the catalog probe's status with its count. A remote client cannot
// read the server's models.yaml — this handle is what it has.
func TestModelsSurface_ListTiersAndCatalogStatus(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)

	recorder := modelSurfaceRequest(t, apiInst, "GET", "/api/v1/models", "", "")
	if recorder.Code != 200 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	var response struct {
		Adapter     string `json:"adapter"`
		DefaultTier string `json:"default_tier"`
		Catalog     struct {
			Status string `json:"status"`
			Models int    `json:"models"`
		} `json:"catalog"`
		Tiers []struct {
			Tier      string `json:"tier"`
			Model     string `json:"model"`
			Variant   string `json:"variant"`
			InCatalog bool   `json:"in_catalog"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body)
	}
	if response.Adapter != "surface-stub" {
		t.Errorf("adapter = %q", response.Adapter)
	}
	if response.DefaultTier != "strong" {
		t.Errorf("default_tier = %q", response.DefaultTier)
	}
	if response.Catalog.Status != "ok (2 models)" || response.Catalog.Models != 2 {
		t.Errorf("catalog = %+v; want ok (2 models)", response.Catalog)
	}
	if len(response.Tiers) != 3 {
		t.Fatalf("tiers = %+v; want the three configured", response.Tiers)
	}
	for _, expected := range []struct{ tier, model, variant string }{
		{"deep", "prov/other-model", "high"},
		{"fast", "prov/one-model", ""},
		{"strong", "prov/one-model", ""},
	} {
		matched := false
		for _, tier := range response.Tiers {
			if tier.Tier == expected.tier && tier.Model == expected.model && tier.Variant == expected.variant && tier.InCatalog {
				matched = true
			}
		}
		if !matched {
			t.Errorf("tiers = %+v; want %s -> %s/%s, in catalog", response.Tiers, expected.tier, expected.model, expected.variant)
		}
	}
}

// TestModelsSurface_EmptyBodyChecksAllTiersDeduplicated: an empty body means
// every configured tier, and (model, variant) pairs are deduplicated — two
// tiers naming one model are ONE paid call.
func TestModelsSurface_EmptyBodyChecksAllTiersDeduplicated(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-dedupe", "")
	if recorder.Code != 202 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	var accepted struct {
		CheckID string `json:"check_id"`
		Targets []struct {
			Tier  string `json:"tier"`
			Model string `json:"model"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body)
	}
	if len(accepted.Targets) != 2 {
		t.Fatalf("targets = %+v; want 2 (fast+strong collapse onto one model)", accepted.Targets)
	}
	waitForModelCheckCalls(t, fake, 2)
}

// TestModelsSurface_IdempotentReplayReturnsSameCheck: the same key with the
// same body returns the same check_id and does not start a second check — the
// fake adapter's call count must stay at one.
func TestModelsSurface_IdempotentReplayReturnsSameCheck(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	first := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-once", `{"tier":"fast"}`)
	if first.Code != 202 {
		t.Fatalf("first status = %d; body %s", first.Code, first.Body)
	}
	waitForModelCheckCalls(t, fake, 1)
	second := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-once", `{"tier":"fast"}`)
	if second.Code != 202 {
		t.Fatalf("replay status = %d; body %s", second.Code, second.Body)
	}
	var firstBody, secondBody struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if firstBody.CheckID == "" {
		t.Fatal("first response carries no check_id")
	}
	if firstBody.CheckID != secondBody.CheckID {
		t.Errorf("replay returned %q; want the original %q", secondBody.CheckID, firstBody.CheckID)
	}
	if calls := fake.calls(); calls != 1 {
		t.Errorf("adapter tested %d times; a replay must not double-bill", calls)
	}
}

// TestModelsSurface_ReusedKeyWithDifferentBodyConflicts: a key names one
// intent; reusing it for another request is a 409, never a
// substitution.
func TestModelsSurface_ReusedKeyWithDifferentBodyConflicts(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)

	first := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-conflict", `{"tier":"fast"}`)
	if first.Code != 202 {
		t.Fatalf("first status = %d; body %s", first.Code, first.Body)
	}
	conflict := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-conflict", `{"tier":"deep"}`)
	if conflict.Code != 409 {
		t.Fatalf("conflict status = %d; body %s", conflict.Code, conflict.Body)
	}
	if !strings.Contains(conflict.Body.String(), "Idempotency-Key") {
		t.Errorf("body = %s; want it to name the header", conflict.Body)
	}
}

// TestModelsSurface_MissingHeaderIsRefused: the idempotency key is required,
// not advisory — without it, a retried request runs a second paid check.
// The 400 names the header.
func TestModelsSurface_MissingHeaderIsRefused(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "", `{"tier":"fast"}`)
	if recorder.Code != 400 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "Idempotency-Key") {
		t.Errorf("body = %s; want the header named", recorder.Body)
	}
	if calls := fake.calls(); calls != 0 {
		t.Errorf("adapter tested %d times; a refused request must test nothing", calls)
	}
}

// TestModelsSurface_BadRequests covers the validation table: an unknown tier,
// a timeout above the ceiling (named in the message), an unknown body field,
// tier+model together, a variant overriding a tier's own, and a variant with
// no target at all.
func TestModelsSurface_BadRequests(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown tier", `{"tier":"turbo"}`, "unknown tier"},
		{"timeout above ceiling", `{"tier":"fast","timeout_seconds":9999}`, "between 1 and 120"},
		{"unknown body field", `{"tiers":"fast"}`, "parse body"},
		{"tier and model together", `{"tier":"fast","model":"prov/one-model"}`, "mutually exclusive"},
		// A tier carries its own variant from models.yaml; a request that
		// overrode it would check a pair the process never runs.
		{"variant with a tier", `{"tier":"deep","variant":"low"}`, "variant is not accepted with a tier"},
		{"variant without a target", `{"variant":"high"}`, "variant needs a model"},
	}
	for _, testCase := range cases {
		recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-"+testCase.name, testCase.body)
		if recorder.Code != 400 {
			t.Errorf("%s: status = %d; body %s", testCase.name, recorder.Code, recorder.Body)
			continue
		}
		if !strings.Contains(recorder.Body.String(), testCase.want) {
			t.Errorf("%s: body = %s; want it to contain %q", testCase.name, recorder.Body, testCase.want)
		}
	}
	if calls := fake.calls(); calls != 0 {
		t.Errorf("adapter tested %d times; a refused request must test nothing", calls)
	}
}

// TestModelsSurface_CheckStateLifecycle: the GET handle reports running while
// the (gated) check is in flight, finished with results once it completes,
// and 404 for an unknown id. This is also the proof the POST answers before
// the check finishes: the 202 arrives while the fake is still blocked.
func TestModelsSurface_CheckStateLifecycle(t *testing.T) {
	gate := make(chan struct{})
	apiInst, _ := newModelSurfaceAPI(t, gate)

	accepted := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-lifecycle", `{"tier":"fast"}`)
	if accepted.Code != 202 {
		t.Fatalf("status = %d; body %s", accepted.Code, accepted.Body)
	}
	var acceptedBody struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(accepted.Body.Bytes(), &acceptedBody); err != nil {
		t.Fatalf("decode accepted: %v", err)
	}

	// The check is gated shut, so this read observes it mid-flight.
	running := modelSurfaceRequest(t, apiInst, "GET", "/api/v1/models/test/"+acceptedBody.CheckID, "", "")
	if running.Code != 200 {
		t.Fatalf("running status = %d; body %s", running.Code, running.Body)
	}
	var runningBody struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(running.Body.Bytes(), &runningBody); err != nil {
		t.Fatalf("decode running: %v", err)
	}
	if runningBody.State != "running" {
		t.Fatalf("state = %q; want running while the check is in flight", runningBody.State)
	}

	close(gate)
	finished := waitForModelCheckState(t, apiInst, acceptedBody.CheckID)
	if finished.State != "finished" {
		t.Fatalf("state = %q; want finished", finished.State)
	}
	if len(finished.Results) != 1 {
		t.Fatalf("results = %+v; want the one target", finished.Results)
	}
	result := finished.Results[0]
	if result.Outcome != "ok" || result.Model != "prov/one-model" || result.Tier != "fast" {
		t.Errorf("result = %+v; want ok on the fast tier's model", result)
	}
	if result.LatencyMs < 0 {
		t.Errorf("latency_ms = %d", result.LatencyMs)
	}

	unknown := modelSurfaceRequest(t, apiInst, "GET", "/api/v1/models/test/chk_doesnotexist", "", "")
	if unknown.Code != 404 {
		t.Errorf("unknown id status = %d; want 404", unknown.Code)
	}
}

// TestModelsSurface_UnauthenticatedIsRefused: all three handles answer an
// unauthenticated caller with the same 401 the rest of the surface gives.
// (A forbidden caller is not reachable with the single-owner policy — Can
// allows every resolved principal — so the 401 shape is the testable half of
// the contract.)
func TestModelsSurface_UnauthenticatedIsRefused(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)
	listRecorder := httptest.NewRecorder()
	apiInst.handleListModels(listRecorder, httptest.NewRequest("GET", "/api/v1/models", nil))
	if listRecorder.Code != 401 {
		t.Errorf("GET /models without a principal = %d; want 401", listRecorder.Code)
	}
	startRecorder := httptest.NewRecorder()
	apiInst.handleStartModelTest(startRecorder, httptest.NewRequest("POST", "/api/v1/models/test", nil))
	if startRecorder.Code != 401 {
		t.Errorf("POST /models/test without a principal = %d; want 401", startRecorder.Code)
	}
	stateRecorder := httptest.NewRecorder()
	apiInst.handleGetModelTest(stateRecorder, httptest.NewRequest("GET", "/api/v1/models/test/chk_x", nil))
	if stateRecorder.Code != 401 {
		t.Errorf("GET /models/test/{id} without a principal = %d; want 401", stateRecorder.Code)
	}
}

// TestModelsSurface_TierTargetCarriesItsVariant: a check named by tier hands
// the adapter the tier's full pair — model and variant — asserted from the
// selection the fake received, not from the request body.
func TestModelsSurface_TierTargetCarriesItsVariant(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-tier-pair", `{"tier":"deep"}`)
	if recorder.Code != 202 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	var accepted struct {
		Targets []struct {
			Tier    string `json:"tier"`
			Model   string `json:"model"`
			Variant string `json:"variant"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body)
	}
	if len(accepted.Targets) != 1 {
		t.Fatalf("targets = %+v; want the one tier", accepted.Targets)
	}
	target := accepted.Targets[0]
	if target.Model != "prov/other-model" || target.Variant != "high" {
		t.Errorf("target = %+v; want the tier's model and variant", target)
	}
	waitForModelCheckCalls(t, fake, 1)
	for _, selection := range fake.handedSelections() {
		if selection.Options.Variant != "high" || selection.Options.Model != "prov/other-model" {
			t.Errorf("adapter received %+v; want the tier's pair", selection.Options)
		}
	}
}

// TestModelsSurface_ExplicitPairIsCheckedAsOneUnit: {model, variant} names
// the pair directly. The same model under two variants is TWO paid calls —
// they answer different questions — while the identical pair is one.
func TestModelsSurface_ExplicitPairIsCheckedAsOneUnit(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-explicit-pair",
		`{"model":"prov/one-model","variant":"high"}`)
	if recorder.Code != 202 {
		t.Fatalf("status = %d; body %s", recorder.Code, recorder.Body)
	}
	var accepted struct {
		Targets []struct {
			Model   string `json:"model"`
			Variant string `json:"variant"`
		} `json:"targets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body)
	}
	if len(accepted.Targets) != 1 || accepted.Targets[0].Variant != "high" {
		t.Fatalf("targets = %+v; want the explicit pair", accepted.Targets)
	}
	waitForModelCheckCalls(t, fake, 1)
	selections := fake.handedSelections()
	if len(selections) != 1 || selections[0].Options.Variant != "high" {
		t.Errorf("adapter received %+v; want the explicit pair", selections)
	}
}

// TestModelsSurface_VariantsOfOneModelAreDistinctTargets: the same model
// under two variants is TWO paid calls — they answer different questions —
// while two tiers naming the identical pair stay one (the empty-body dedupe
// the other test pins). Driven as two requests because one request names one
// intent.
func TestModelsSurface_VariantsOfOneModelAreDistinctTargets(t *testing.T) {
	apiInst, fake := newModelSurfaceAPI(t, nil)

	first := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-pair-a", `{"model":"prov/one-model"}`)
	if first.Code != 202 {
		t.Fatalf("first status = %d; body %s", first.Code, first.Body)
	}
	second := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-pair-b", `{"model":"prov/one-model","variant":"high"}`)
	if second.Code != 202 {
		t.Fatalf("second status = %d; body %s", second.Code, second.Body)
	}
	waitForModelCheckCalls(t, fake, 2)
	variants := map[string]bool{}
	for _, selection := range fake.handedSelections() {
		variants[selection.Options.Variant] = true
	}
	if !variants[""] || !variants["high"] {
		t.Errorf("adapter saw variants %v; want both the bare model and the variant pair", variants)
	}
}

// TestModelsSurface_ReusedKeyWithDifferentPairConflicts: the fingerprint
// hashes the targets, so the same Idempotency-Key naming a different pair is
// the same 409 as a different model — a key names one intent, and the intent
// includes the variant.
func TestModelsSurface_ReusedKeyWithDifferentPairConflicts(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)

	first := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-pair-conflict", `{"model":"prov/one-model"}`)
	if first.Code != 202 {
		t.Fatalf("first status = %d; body %s", first.Code, first.Body)
	}
	conflict := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-pair-conflict", `{"model":"prov/one-model","variant":"high"}`)
	if conflict.Code != 409 {
		t.Fatalf("conflict status = %d; body %s", conflict.Code, conflict.Body)
	}
	if !strings.Contains(conflict.Body.String(), "Idempotency-Key") {
		t.Errorf("body = %s; want it to name the header", conflict.Body)
	}
}

// waitForModelCheckCalls polls the fake until the expected number of TestModel
// invocations has happened (the check runs in the background).
func waitForModelCheckCalls(t *testing.T, fake *modelSurfaceAdapter, expected int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fake.calls() >= expected {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("adapter tested %d times; want %d within the deadline", fake.calls(), expected)
}

// waitForModelCheckState polls the GET handle until the check finishes and
// returns its decoded body.
func waitForModelCheckState(t *testing.T, apiInst *API, checkID string) struct {
	State   string `json:"state"`
	Results []struct {
		Tier      string `json:"tier"`
		Model     string `json:"model"`
		Outcome   string `json:"outcome"`
		LatencyMs int64  `json:"latency_ms"`
	} `json:"results"`
} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		recorder := modelSurfaceRequest(t, apiInst, "GET", "/api/v1/models/test/"+checkID, "", "")
		var body struct {
			State   string `json:"state"`
			Results []struct {
				Tier      string `json:"tier"`
				Model     string `json:"model"`
				Outcome   string `json:"outcome"`
				LatencyMs int64  `json:"latency_ms"`
			} `json:"results"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode state: %v (%s)", err, recorder.Body)
		}
		if body.State == "finished" {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("check did not finish within the deadline")
	return struct {
		State   string `json:"state"`
		Results []struct {
			Tier      string `json:"tier"`
			Model     string `json:"model"`
			Outcome   string `json:"outcome"`
			LatencyMs int64  `json:"latency_ms"`
		} `json:"results"`
	}{}
}

// TestModelCheckRegistry_RetentionForgets: FINISHED entries past the TTL are
// gone from both maps — an idempotency key dropped by age behaves like a key
// never seen (a new check), and a check id dropped by age reads as 404
// upstream. A RUNNING check is never swept, however long the queue in front
// of it: a client watching it must not see it disappear into a 404.
func TestModelCheckRegistry_RetentionForgets(t *testing.T) {
	t.Parallel()
	registry := newModelCheckRegistry(10 * time.Millisecond)
	check, replay, conflict, queued := registry.accept("tenant-a", "key-ttl", "print", []modelCheckTarget{{Model: "prov/one-model"}})
	if replay || conflict || queued {
		t.Fatalf("accept = replay:%v conflict:%v queued:%v; want a fresh registration", replay, conflict, queued)
	}
	if registry.lookup("tenant-a", check.checkID) == nil {
		t.Fatal("fresh check not readable")
	}
	// A running check outlives its would-be TTL.
	time.Sleep(20 * time.Millisecond)
	if registry.lookup("tenant-a", check.checkID) == nil {
		t.Fatal("running check swept before finishing; the TTL counts from completion")
	}
	registry.finish(check)
	time.Sleep(20 * time.Millisecond)
	if registry.lookup("tenant-a", check.checkID) != nil {
		t.Error("finished check survived its TTL; the registry must forget")
	}
	again, replay, conflict, queued := registry.accept("tenant-a", "key-ttl", "print", []modelCheckTarget{{Model: "prov/one-model"}})
	if replay || conflict || queued {
		t.Errorf("expired key: replay=%v conflict=%v queued=%v; an expired key must behave like a never-seen key", replay, conflict, queued)
	}
	if again.checkID == check.checkID {
		t.Error("re-accept after TTL returned the dead check's id")
	}
}

// TestModelsSurface_RegistryIsTenantScoped: the check registry is the one
// place a diagnostic carries state, and the multi-tenant seam must not grow
// its first exception there. The probe drives two principals through the
// exact sequence that used to leak: a foreign key must not 409, a foreign
// key with the same body must mint the tenant's OWN check, and a foreign
// check id must read as 404 — indistinguishable from an absent one.
func TestModelsSurface_RegistryIsTenantScoped(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)
	otherTenant := authz.Principal{TenantID: "tenant-2", UserID: "user-2"}

	first := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "shared-key", `{"tier":"fast"}`)
	if first.Code != 202 {
		t.Fatalf("tenant-1 accept status = %d; body %s", first.Code, first.Body)
	}
	var firstBody struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode tenant-1 accept: %v", err)
	}

	// Same key, different body, different tenant: NOT a conflict — the key
	// namespace is per tenant, so this is a fresh intent.
	foreignConflict := modelSurfaceRequestAs(t, apiInst, otherTenant, "POST", "/api/v1/models/test", "shared-key", `{"tier":"deep"}`)
	if foreignConflict.Code == 409 {
		t.Fatal("a foreign tenant's idempotency key produced a 409; keys must be scoped per tenant")
	}
	if foreignConflict.Code != 202 {
		t.Fatalf("tenant-2 accept (other body) status = %d; body %s", foreignConflict.Code, foreignConflict.Body)
	}

	// Same key, same body, different tenant: the tenant's own check, not a
	// replay of the foreign one.
	foreignReplay := modelSurfaceRequestAs(t, apiInst, otherTenant, "POST", "/api/v1/models/test", "shared-key", `{"tier":"deep"}`)
	if foreignReplay.Code != 202 {
		t.Fatalf("tenant-2 replay status = %d; body %s", foreignReplay.Code, foreignReplay.Body)
	}
	var replayBody struct {
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(foreignReplay.Body.Bytes(), &replayBody); err != nil {
		t.Fatalf("decode tenant-2 replay: %v", err)
	}
	if replayBody.CheckID == "" {
		t.Fatal("tenant-2 replay carries no check_id")
	}

	// A foreign check id reads as 404, never as another tenant's results.
	foreignRead := modelSurfaceRequestAs(t, apiInst, otherTenant, "GET", "/api/v1/models/test/"+firstBody.CheckID, "", "")
	if foreignRead.Code != 404 {
		t.Errorf("tenant-2 read of tenant-1's check = %d; want 404 (a foreign id is indistinguishable from an absent one)", foreignRead.Code)
	}
	// And the owner still sees their own check.
	ownerRead := modelSurfaceRequest(t, apiInst, "GET", "/api/v1/models/test/"+firstBody.CheckID, "", "")
	if ownerRead.Code != 200 {
		t.Errorf("owner read of own check = %d; want 200", ownerRead.Code)
	}
}

// TestModelsSurface_QueueDepthIsBounded: acceptance without backpressure is a
// bill — each 202 is a paid call queued behind the others. Past the depth
// cap the answer is 429 naming the cap, replays of already-accepted checks
// still work, and the cap lifts as checks finish.
func TestModelsSurface_QueueDepthIsBounded(t *testing.T) {
	gate := make(chan struct{})
	apiInst, fake := newModelSurfaceAPI(t, gate)

	accepted := make([]string, 0, modelTestMaxQueueDepth)
	for depth := 0; depth < modelTestMaxQueueDepth; depth++ {
		recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test",
			fmt.Sprintf("key-depth-%d", depth), `{"tier":"fast"}`)
		if recorder.Code != 202 {
			t.Fatalf("depth %d: status = %d; body %s", depth, recorder.Code, recorder.Body)
		}
		var acceptedBody struct {
			CheckID string `json:"check_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &acceptedBody); err != nil {
			t.Fatalf("decode depth %d: %v", depth, err)
		}
		accepted = append(accepted, acceptedBody.CheckID)
	}

	// The queue is full: a NEW key is refused with 429 naming the cap…
	refused := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-over-depth", `{"tier":"fast"}`)
	if refused.Code != 429 {
		t.Fatalf("over-depth status = %d; body %s", refused.Code, refused.Body)
	}
	if !strings.Contains(refused.Body.String(), fmt.Sprintf("limit %d", modelTestMaxQueueDepth)) {
		t.Errorf("body = %s; want the cap named", refused.Body)
	}
	// …while a replay of an accepted check is still answered.
	replay := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-depth-0", `{"tier":"fast"}`)
	if replay.Code != 202 {
		t.Fatalf("replay under a full queue status = %d; body %s", replay.Code, replay.Body)
	}

	// Drain: the gated checks finish, and acceptance works again.
	close(gate)
	waitForModelCheckCalls(t, fake, modelTestMaxQueueDepth)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if recorder := modelSurfaceRequest(t, apiInst, "POST", "/api/v1/models/test", "key-after-drain", `{"tier":"fast"}`); recorder.Code == 202 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("acceptance did not recover after the queue drained")
}
