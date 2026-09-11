package api

import (
	"context"
	"encoding/json"
	"errors"
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
// descriptor + catalog, and a ModelTester that counts invocations and can be
// held mid-check so "running" is observable deterministically.
type modelSurfaceAdapter struct {
	descriptor agent.Descriptor
	catalog    models.Catalog
	// testCalls counts TestModel invocations.
	testCalls int
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
		Model: selection.Options.Model, Tier: selection.Tier,
		Outcome: agent.ModelCheckOK, Latency: latency, CheckedAt: time.Now().UTC(),
	}
}

func (fake *modelSurfaceAdapter) calls() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.testCalls
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
			Tiers: map[string]string{
				"fast":   "prov/one-model",
				"strong": "prov/one-model",
				"deep":   "prov/other-model",
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
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	request = request.WithContext(authz.WithPrincipal(request.Context(), authzPrincipal()))
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
	for _, expected := range []struct{ tier, model string }{
		{"deep", "prov/other-model"}, {"fast", "prov/one-model"}, {"strong", "prov/one-model"},
	} {
		matched := false
		for _, tier := range response.Tiers {
			if tier.Tier == expected.tier && tier.Model == expected.model && tier.Variant == "" && tier.InCatalog {
				matched = true
			}
		}
		if !matched {
			t.Errorf("tiers = %+v; want %s -> %s, in catalog, no variant", response.Tiers, expected.tier, expected.model)
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
// intent; reusing it for another request is a 409, never a silent
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
// not advisory — the client that omits it pays for the call twice and learns
// it from the bill. The 400 names the header.
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
// and tier+model together.
func TestModelsSurface_BadRequests(t *testing.T) {
	apiInst, _ := newModelSurfaceAPI(t, nil)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"unknown tier", `{"tier":"turbo"}`, "unknown tier"},
		{"timeout above ceiling", `{"tier":"fast","timeout_seconds":9999}`, "between 1 and 120"},
		{"unknown body field", `{"tiers":"fast"}`, "parse body"},
		{"tier and model together", `{"tier":"fast","model":"prov/one-model"}`, "mutually exclusive"},
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

// TestModelCheckRegistry_RetentionForgets: entries past the TTL are gone from
// both maps — an idempotency key forgotten by age behaves like a key never
// seen (a new check), and a check id forgotten by age reads as 404 upstream.
func TestModelCheckRegistry_RetentionForgets(t *testing.T) {
	t.Parallel()
	registry := newModelCheckRegistry(10 * time.Millisecond)
	check, replay, conflict := registry.accept("key-ttl", "print", []modelCheckTarget{{Model: "prov/one-model"}})
	if replay || conflict {
		t.Fatalf("accept = replay:%v conflict:%v; want a fresh registration", replay, conflict)
	}
	if registry.lookup(check.checkID) == nil {
		t.Fatal("fresh check not readable")
	}
	time.Sleep(20 * time.Millisecond)
	if registry.lookup(check.checkID) != nil {
		t.Error("check survived its TTL; the registry must forget")
	}
	again, replay, conflict := registry.accept("key-ttl", "print", []modelCheckTarget{{Model: "prov/one-model"}})
	if replay || conflict {
		t.Errorf("expired key: replay=%v conflict=%v; an expired key must behave like a never-seen key", replay, conflict)
	}
	if again.checkID == check.checkID {
		t.Error("re-accept after TTL returned the dead check's id")
	}
}
