package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Model-check event types, framed by the existing tenant stream
// (GET /api/v1/events, replay by Last-Event-ID). One event per model keeps a
// live consumer honest about progress — "stuck on the third minute" is visible
// immediately, not after the whole check finishes.
const (
	EvModelsTestStarted      = "models.test_started"
	EvModelsTestModelChecked = "models.test_model_checked"
	EvModelsTestFinished     = "models.test_finished"
)

// Model-check states.
const (
	modelCheckRunning  = "running"
	modelCheckFinished = "finished"
)

// modelTestDefaults apply when the server wired no explicit limits.
const (
	defaultModelTestMaxSeconds       = 120
	defaultModelTestRetentionMinutes = 60
	// defaultModelTestTimeoutSeconds is the per-request deadline when the
	// caller sends none: the documented patience of one check.
	defaultModelTestTimeoutSeconds = 60
)

// maxModelTestRequestBytes bounds the request body. It carries a tier name or
// a model string, not content.
const maxModelTestRequestBytes = 4 * 1024

// modelTestLimits bounds the on-demand check surface: the ceiling a request's
// timeout_seconds may name, and how long accepted checks and their
// idempotency keys stay remembered.
type modelTestLimits struct {
	maxSeconds       int
	retentionMinutes int
}

func (limits modelTestLimits) ceilingSeconds() int {
	if limits.maxSeconds > 0 {
		return limits.maxSeconds
	}
	return defaultModelTestMaxSeconds
}

func (limits modelTestLimits) retention() time.Duration {
	if limits.retentionMinutes > 0 {
		return time.Duration(limits.retentionMinutes) * time.Minute
	}
	return time.Duration(defaultModelTestRetentionMinutes) * time.Minute
}

// modelCheckTarget is one (model, variant) pair a check will invoke, with the
// tier that asked for it. Variant is carried for the surface's shape: tiers
// today declare models only, and the value arrives empty until they grow a
// variant of their own.
type modelCheckTarget struct {
	Tier    string `json:"tier,omitempty"`
	Model   string `json:"model"`
	Variant string `json:"variant,omitempty"`
}

// modelCheckResult is one target's outcome as the API renders it.
type modelCheckResult struct {
	Tier      string `json:"tier,omitempty"`
	Model     string `json:"model"`
	Variant   string `json:"variant,omitempty"`
	Outcome   string `json:"outcome"`
	LatencyMs int64  `json:"latency_ms"`
	Reason    string `json:"reason,omitempty"`
}

// modelCheck is one accepted check: identity, targets, and the results
// gathered so far. Owned by the registry; mutated under its mutex.
type modelCheck struct {
	checkID    string
	targets    []modelCheckTarget
	state      string
	results    []modelCheckResult
	acceptedAt time.Time
}

// keyedCheck pairs an idempotency key with the fingerprint of the request it
// served, so a replay can be told apart from a key reused for another body.
type keyedCheck struct {
	fingerprint string
	check       *modelCheck
}

// modelCheckRegistry is the in-process memory of accepted checks: by
// idempotency key (dedupe) and by check id (state reads). Diagnostics carry
// no durable state — no table, no migration — and the TTL is the honest
// consequence: a restart forgets keys and unfinished checks, the finished
// results stay in the event stream, and a client retries.
type modelCheckRegistry struct {
	mu        sync.Mutex
	byKey     map[string]keyedCheck
	byID      map[string]*modelCheck
	retention time.Duration
	// runMu serializes check execution: requests with different keys are
	// different intentions and are all accepted, but they do not run
	// concurrently — a fan-out of paid calls is never the answer to a slow
	// model.
	runMu sync.Mutex
}

func newModelCheckRegistry(retention time.Duration) *modelCheckRegistry {
	return &modelCheckRegistry{
		byKey:     make(map[string]keyedCheck),
		byID:      make(map[string]*modelCheck),
		retention: retention,
	}
}

// sweepLocked drops entries past their TTL. Caller holds mu.
func (registry *modelCheckRegistry) sweepLocked(now time.Time) {
	for key, entry := range registry.byKey {
		if now.Sub(entry.check.acceptedAt) > registry.retention {
			delete(registry.byKey, key)
			delete(registry.byID, entry.check.checkID)
		}
	}
}

// accept registers a check under an idempotency key, or recognizes a replay:
// the same key with the same request fingerprint returns the existing check;
// the same key with a different fingerprint is a conflict — silently
// substituting what a key means is how a client pays twice for one intent.
func (registry *modelCheckRegistry) accept(key, fingerprint string, targets []modelCheckTarget) (check *modelCheck, replay bool, conflict bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.sweepLocked(time.Now())
	if existing, found := registry.byKey[key]; found {
		if existing.fingerprint != fingerprint {
			return nil, false, true
		}
		return existing.check, true, false
	}
	check = &modelCheck{
		checkID:    newCheckID(),
		targets:    targets,
		state:      modelCheckRunning,
		acceptedAt: time.Now(),
	}
	registry.byKey[key] = keyedCheck{fingerprint: fingerprint, check: check}
	registry.byID[check.checkID] = check
	return check, false, false
}

// lookup returns the check by id, or nil when unknown or past its TTL.
func (registry *modelCheckRegistry) lookup(checkID string) *modelCheck {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.sweepLocked(time.Now())
	return registry.byID[checkID]
}

// recordResult appends one target's outcome.
func (registry *modelCheckRegistry) recordResult(check *modelCheck, result modelCheckResult) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	check.results = append(check.results, result)
}

// finish marks the check complete.
func (registry *modelCheckRegistry) finish(check *modelCheck) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	check.state = modelCheckFinished
}

// newCheckID mints an unguessable check id.
func newCheckID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand does not fail on the platforms this runs on; if it ever
		// does, fall back to a time-derived id rather than refusing a
		// diagnostic.
		return fmt.Sprintf("chk_%d", time.Now().UnixNano())
	}
	return "chk_" + hex.EncodeToString(raw[:])
}

// modelsCatalogResponse is the catalog half of GET /models. Status uses the
// probe label vocabulary; "unsupported" marks an adapter that cannot be asked.
type modelsCatalogResponse struct {
	Status    string `json:"status"`
	Models    int    `json:"models,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// modelsTierResponse is one resolved tier in GET /models. InCatalog is false
// whenever the question could not be answered (catalog unavailable or
// unsupported) — the catalog status says which of the two it was.
type modelsTierResponse struct {
	Tier      string `json:"tier"`
	Model     string `json:"model"`
	Variant   string `json:"variant"`
	InCatalog bool   `json:"in_catalog"`
}

// modelsResponse is GET /models: what the process runs on and what the
// runtime says it can run. A remote client cannot read models.yaml off the
// server's disk — without this handle it has nothing to put in a test request
// and nothing to show on a screen.
type modelsResponse struct {
	Adapter     string                `json:"adapter"`
	DefaultTier string                `json:"default_tier"`
	Catalog     modelsCatalogResponse `json:"catalog"`
	Tiers       []modelsTierResponse  `json:"tiers"`
}

// handleListModels GET /api/v1/models — the process's resolved tiers, the
// default tier, the adapter id, and the catalog probe's outcome. The tiers
// are the values the server resolved at boot — the same ones runs resolve
// against — not a re-read of the file, which would be a second source of
// truth free to disagree with the runs.
func (api *API) handleListModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAccess(w, r, authz.ActionModelRead, ""); !ok {
		return
	}
	if api.execAdapter == nil {
		writeError(w, http.StatusServiceUnavailable, codeInternal, "execution adapter is not wired")
		return
	}
	descriptor := api.execAdapter.Describe()

	catalogStatus := modelsCatalogResponse{Status: models.CatalogUnsupported}
	var catalog models.Catalog
	if descriptor.EnumeratesModels {
		catalog = api.execAdapter.Catalog(r.Context())
		catalogStatus.Status = catalog.Label()
		catalogStatus.Models = len(catalog.Models)
		catalogStatus.CheckedAt = catalog.CheckedAt.UTC().Format(time.RFC3339)
	}

	tierNames := make([]string, 0, len(api.resolvedTiers.Tiers))
	for tierName := range api.resolvedTiers.Tiers {
		tierNames = append(tierNames, tierName)
	}
	sort.Strings(tierNames)
	tiers := make([]modelsTierResponse, 0, len(tierNames))
	for _, tierName := range tierNames {
		modelName := api.resolvedTiers.Tiers[tierName]
		_, inCatalog := catalog.Lookup(modelName)
		tiers = append(tiers, modelsTierResponse{
			Tier: tierName, Model: modelName, Variant: "", InCatalog: inCatalog,
		})
	}
	writeJSON(w, http.StatusOK, modelsResponse{
		Adapter:     string(descriptor.ID),
		DefaultTier: api.resolvedTiers.Default,
		Catalog:     catalogStatus,
		Tiers:       tiers,
	})
}

// modelTestRequestBody is what POST /models/test accepts: a tier name, or an
// explicit model (+ variant), or neither — an empty body checks every
// configured tier. Strict decoding: the request is ours, and a typo'd field
// must not silently select "all tiers".
type modelTestRequestBody struct {
	Tier        string `json:"tier"`
	Model       string `json:"model"`
	Variant     string `json:"variant"`
	TimeoutSecs int    `json:"timeout_seconds"`
}

// modelTestRequest is the parsed and resolved request: the deduplicated
// targets and the per-check deadline in seconds.
type modelTestRequest struct {
	targets     []modelCheckTarget
	timeoutSecs int
}

// parseModelTestRequest validates the body against the process's resolved
// tiers and deduplicates (model, variant) pairs — three tiers naming one
// model are one paid call, attributed to the first tier (sorted) that named
// it.
func parseModelTestRequest(request modelTestRequestBody, resolvedTiers models.Config, ceilingSeconds int) (modelTestRequest, error) {
	if request.Tier != "" && request.Model != "" {
		return modelTestRequest{}, fmt.Errorf("tier and model are mutually exclusive: name one")
	}
	timeoutSecs := request.TimeoutSecs
	if timeoutSecs == 0 {
		timeoutSecs = defaultModelTestTimeoutSeconds
	}
	if timeoutSecs < 0 || timeoutSecs > ceilingSeconds {
		return modelTestRequest{}, fmt.Errorf("timeout_seconds must be between 1 and %d", ceilingSeconds)
	}

	var targets []modelCheckTarget
	seen := make(map[string]bool)
	addTarget := func(tier, modelName, variant string) {
		key := modelName + "\x00" + variant
		if seen[key] {
			return
		}
		seen[key] = true
		targets = append(targets, modelCheckTarget{Tier: tier, Model: modelName, Variant: variant})
	}
	switch {
	case request.Tier != "":
		modelName, found := resolvedTiers.Tiers[request.Tier]
		if !found {
			known := make([]string, 0, len(resolvedTiers.Tiers))
			for tierName := range resolvedTiers.Tiers {
				known = append(known, tierName)
			}
			sort.Strings(known)
			return modelTestRequest{}, fmt.Errorf("unknown tier %q (known: %s)", request.Tier, strings.Join(known, ", "))
		}
		addTarget(request.Tier, modelName, "")
	case request.Model != "":
		addTarget("", request.Model, request.Variant)
	default:
		// Empty body: every configured tier, deduplicated by pair.
		tierNames := make([]string, 0, len(resolvedTiers.Tiers))
		for tierName := range resolvedTiers.Tiers {
			tierNames = append(tierNames, tierName)
		}
		sort.Strings(tierNames)
		for _, tierName := range tierNames {
			addTarget(tierName, resolvedTiers.Tiers[tierName], "")
		}
	}
	if len(targets) == 0 {
		return modelTestRequest{}, fmt.Errorf("no tiers are configured; name a model explicitly")
	}
	return modelTestRequest{targets: targets, timeoutSecs: timeoutSecs}, nil
}

// fingerprintRequest canonicalizes a parsed request so an idempotent replay
// (same intent, possibly different whitespace) matches while a different body
// does not.
func fingerprintRequest(request modelTestRequest) string {
	hasher := sha256.New()
	_ = json.NewEncoder(hasher).Encode(request.targets)
	fmt.Fprintf(hasher, "timeout=%d", request.timeoutSecs)
	return hex.EncodeToString(hasher.Sum(nil))
}

// handleStartModelTest POST /api/v1/models/test — accepts a check for
// execution and returns 202 immediately: the check runs in the background and
// its results arrive as events on the tenant stream, because holding an HTTP
// session for up to two minutes is a wedged goroutine, a socket through every
// proxy, and a user glued to a screen for an answer that comes on its own.
//
// Idempotency-Key is required, not advisory: an optional guard against a
// double click is no guard — the client that omits it pays for the call
// twice and learns it from the bill. A repeated key with the same body
// returns the same check; with a different body it is a 409, because silently
// redefining what a key means is the same theft.
func (api *API) handleStartModelTest(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionModelTest, "")
	if !ok {
		return
	}
	if api.execAdapter == nil {
		writeError(w, http.StatusServiceUnavailable, codeInternal, "execution adapter is not wired")
		return
	}
	if _, canTest := api.execAdapter.(agent.ModelTester); !canTest {
		writeError(w, http.StatusNotImplemented, codeNotImplemented, "the execution adapter cannot test models")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, codeBadInput,
			"the Idempotency-Key header is required: the check calls the model and a retry must not double-bill it")
		return
	}

	var body modelTestRequestBody
	if r.Body != nil {
		raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxModelTestRequestBytes))
		if readErr != nil {
			writeError(w, http.StatusBadRequest, codeBadInput, fmt.Sprintf("read body: %v", readErr))
			return
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed != "" {
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&body); decodeErr != nil {
				writeError(w, http.StatusBadRequest, codeBadInput, fmt.Sprintf("parse body: %v", decodeErr))
				return
			}
		}
	}
	request, parseErr := parseModelTestRequest(body, api.resolvedTiers, api.modelTest.ceilingSeconds())
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, parseErr.Error())
		return
	}

	check, replay, conflict := api.modelChecks.accept(idempotencyKey, fingerprintRequest(request), request.targets)
	if conflict {
		writeError(w, http.StatusConflict, codeConflict,
			"this Idempotency-Key was used for a different request; a key names one intent")
		return
	}
	if !replay {
		// The accepted context keeps the principal's values (tenant, user)
		// but not the request's cancellation: the check outlives the POST
		// response by design.
		acceptedCtx := context.WithoutCancel(r.Context())
		api.emitModelTestEvent(acceptedCtx, principal, EvModelsTestStarted, map[string]any{
			"check_id": check.checkID,
			"targets":  check.targets,
		})
		go api.runModelCheck(acceptedCtx, principal, check, request)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"check_id": check.checkID,
		"targets":  check.targets,
	})
}

// handleGetModelTest GET /api/v1/models/test/{id} — the state and results of
// one check, for a reconnecting client or one that does not listen to the
// event stream. Unknown and TTL-expired ids are 404; a finished result
// survives in the event stream regardless.
func (api *API) handleGetModelTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAccess(w, r, authz.ActionModelRead, ""); !ok {
		return
	}
	checkID := r.PathValue("id")
	check := api.modelChecks.lookup(checkID)
	if check == nil {
		writeError(w, http.StatusNotFound, codeNotFound,
			"model check not found (unknown, or past its retention; finished results remain in the event stream)")
		return
	}
	api.modelChecks.mu.Lock()
	results := append([]modelCheckResult(nil), check.results...)
	state := check.state
	api.modelChecks.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"check_id": check.checkID,
		"state":    state,
		"results":  results,
	})
}

// runModelCheck executes one accepted check in the background: serialized
// with any other accepted check, one runtime invocation per target, one event
// per result, one finishing event carrying the whole table. The only trace a
// check leaves in the database is these diagnostic events; no run, no
// invocation rows, no manifest.
func (api *API) runModelCheck(ctx context.Context, principal authz.Principal, check *modelCheck, request modelTestRequest) {
	// Serial execution: an accepted check waits for the one running, which is
	// what keeps a slow model from turning into a fan-out of paid calls.
	api.modelChecks.runMu.Lock()
	defer api.modelChecks.runMu.Unlock()

	tester := api.execAdapter.(agent.ModelTester)
	deadline := time.Duration(request.timeoutSecs) * time.Second
	startedAt := time.Now()
	for _, target := range request.targets {
		selection := models.Selection{
			Tier:     target.Tier,
			Provider: models.SplitProvider(target.Model),
			Options:  models.Options{Model: target.Model},
		}
		outcome := tester.TestModel(ctx, selection, deadline)
		result := modelCheckResult{
			Tier:      target.Tier,
			Model:     target.Model,
			Variant:   target.Variant,
			Outcome:   outcome.Outcome,
			LatencyMs: outcome.Latency.Milliseconds(),
			Reason:    outcome.Reason,
		}
		api.modelChecks.recordResult(check, result)
		api.emitModelTestEvent(ctx, principal, EvModelsTestModelChecked, map[string]any{
			"check_id":   check.checkID,
			"tier":       result.Tier,
			"model":      result.Model,
			"variant":    result.Variant,
			"outcome":    result.Outcome,
			"latency_ms": result.LatencyMs,
			"reason":     result.Reason,
		})
	}
	api.modelChecks.finish(check)
	api.emitModelTestEvent(ctx, principal, EvModelsTestFinished, map[string]any{
		"check_id":    check.checkID,
		"results":     check.results,
		"duration_ms": time.Since(startedAt).Milliseconds(),
	})
}

// emitModelTestEvent appends one model-check event to the tenant stream.
// RunID is NULL — a check is not a run — and the actor is human: the check
// exists because a person asked for it through this API. A write failure is
// logged, not fatal: the GET handle and the finishing event both still
// deliver the result, and a diagnostic must not error a request it already
// accepted. A nil querier (unit tests without a database) skips the write —
// the production constructor always has one.
func (api *API) emitModelTestEvent(ctx context.Context, principal authz.Principal, eventType string, payload any) {
	if api.queries == nil {
		return
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		api.log.Warn("emit model test event: marshal", "type", eventType, "error", marshalErr)
		return
	}
	if _, appendErr := api.queries.AppendEvent(ctx, sqlc.AppendEventParams{
		TenantID: principal.TenantID,
		UserID:   principal.UserID,
		RunID:    sql.NullString{}, // a check is not a run; the tenant stream carries it
		Type:     eventType,
		Payload:  raw,
		Actor:    string(authz.ActorHuman),
	}); appendErr != nil {
		api.log.Warn("emit model test event", "type", eventType, "error", appendErr)
	}
}
