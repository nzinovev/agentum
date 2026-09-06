package api

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// RunCanceler aborts an in-flight run by run id. Implemented by the runner's
// CancelRegistry; declared here so the API does not import the runner package.
type RunCanceler interface {
	Cancel(runID string) bool
}

// API wires the sqlc querier behind the HTTP handlers. It is constructed once
// per process and mounts the v1 surface on the server's mux via Register. The
// db handle backs the transactional outbox: every FSM transition that carries a
// runnable-job intent enqueues inside the same tx, so a handler that cannot
// enqueue rolls back the transition (F.6.1 AC #6).
type API struct {
	db      *sql.DB
	queries *sqlc.Queries
	log     *slog.Logger
	cancels RunCanceler

	// art is the immutable artifact revisions store. Nil when the server did
	// not wire one (e.g. tests); the artifact read handlers 404 then.
	art artifacts.Store
	// mfst is the evidence manifest service. Nil when the server did not wire
	// one; the manifest read handlers 404 then.
	mfst *manifest.Service
	// packs resolves pipeline packs by ref. Required for the gate handlers to
	// detect a pack-declared approval and write the run_approvals row in the
	// same tx as the advance transition (ADR 0003 D4). Nil when the server did
	// not wire one; the approval write is skipped then.
	packs pack.Source
}

// Option configures an API at construction. Used for the artifact store +
// manifest service — both are optional so unit tests can build a minimal API.
type Option func(*API)

// WithArtifactStore attaches an immutable artifact revisions store to the API.
// Required for the artifact read handlers to do anything useful.
func WithArtifactStore(store artifacts.Store) Option {
	return func(apiInst *API) { apiInst.art = store }
}

// WithManifestService attaches an evidence manifest service to the API.
// Required for the manifest read handlers to do anything useful.
func WithManifestService(service *manifest.Service) Option {
	return func(apiInst *API) { apiInst.mfst = service }
}

// WithPackSource attaches a pipeline-pack resolver to the API (ADR 0003 D4).
// Required for the gate handlers to detect a pack-declared approval and write
// the matching run_approvals row in the same tx as the advance transition.
func WithPackSource(source pack.Source) Option {
	return func(apiInst *API) { apiInst.packs = source }
}

// New builds the API. db backs the transactional outbox; cancels lets the cancel
// handler abort an in-flight run (nil leaves cancel as a no-op — the FSM
// transition still applies). Options wire the artifact store + manifest service.
func New(db *sql.DB, queries *sqlc.Queries, log *slog.Logger, cancels RunCanceler, options ...Option) *API {
	apiInst := &API{db: db, queries: queries, log: log, cancels: cancels}
	for _, option := range options {
		option(apiInst)
	}
	return apiInst
}

// runInTx executes fn against a fresh transaction-scoped Queries. Commit on nil,
// rollback (and error propagation) otherwise. This is the transactional-outbox
// primitive: a handler composes its FSM transition + EnqueueJob inside fn and
// they land atomically — a post-transition enqueue failure can never leave the
// run in a state whose runnable intent was lost.
func (api *API) runInTx(ctx context.Context, fn func(qtx *sqlc.Queries) error) error {
	tx, err := api.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(api.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// Register attaches the full v1 surface to the mux. Implemented endpoints live
// here; unimplemented contract endpoints are declared in stubs.go. The server
// has already applied the boundary middleware, so every call below carries a
// Principal.
//
// The mux parameter is an interface *http.ServeMux satisfies (its HandleFunc
// takes the plain func type — an interface naming http.HandlerFunc here would
// not match ServeMux's method set): ServeMux offers no way to enumerate
// registered patterns, so the route-vocabulary test passes a recording stub to
// read the surface back. Behavior for real callers is unchanged.
func (api *API) Register(mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}) {
	// Projects (registration: one repo = one project).
	mux.HandleFunc("GET /api/v1/projects", api.handleListProjects)
	mux.HandleFunc("POST /api/v1/projects", api.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", api.handleGetProject)

	// Runs (lifecycle).
	mux.HandleFunc("GET /api/v1/runs", api.handleListRuns)
	mux.HandleFunc("POST /api/v1/runs", api.handleCreateRun)
	mux.HandleFunc("GET /api/v1/runs/{id}", api.handleGetRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/start", api.handleStartRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", api.handleCancelRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/reject", api.handleRejectRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/cleanup", api.handleCleanupRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/final-review", api.handleFinalReview)

	// Stage invocations (read-only for now).
	mux.HandleFunc("GET /api/v1/runs/{id}/invocations", api.handleListInvocations)
	mux.HandleFunc("GET /api/v1/runs/{id}/invocations/{iid}", api.handleGetInvocation)

	// Artifacts (revisions store): list revisions + stream content. These are
	// the read surface over the immutable, worktree-independent revisions store
	// (F.7). A missing revision is a 404; the handler never falls back to the
	// disposable worktree path.
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts", api.handleListArtifacts)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts/revisions/{rid}", api.handleGetArtifactRevision)
	mux.HandleFunc("GET /api/v1/runs/{id}/artifacts/revisions/{rid}/content", api.handleGetArtifactContent)

	// Evidence manifest (read-only). GET returns the manifest body + seal
	// metadata + corrections; GET .../diff compares two sealed manifests.
	mux.HandleFunc("GET /api/v1/runs/{id}/manifest", api.handleGetManifest)
	mux.HandleFunc("GET /api/v1/runs/{id}/manifest/diff", api.handleDiffManifest)
	mux.HandleFunc("POST /api/v1/runs/{id}/manifest/corrections", api.handleCorrectManifest)

	// Gate actions (§3.2 stop conditions → continue semantics).
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/continue", api.handleInvocationContinue)
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/advance", api.handleInvocationAdvance)
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/approve", api.handleInvocationApprove)
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/edit", api.handleInvocationEdit)
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/ask-to-edit", api.handleInvocationAskToEdit)
	mux.HandleFunc("POST /api/v1/runs/{id}/invocations/{iid}/add-context", api.handleInvocationAddContext)

	// Artifacts. {name...} matches a multi-segment path so orchestrator-built
	// names like "plan/plan.md", "review/verdict.json", and
	// "<stage>/result.json" are addressable (ADR 0003 D2). Purely additive:
	// single-segment names keep resolving identically.
	mux.HandleFunc("GET /api/v1/runs/{id}/invocations/{iid}/artifacts/{name...}", api.handleArtifactGet)
	mux.HandleFunc("PUT /api/v1/runs/{id}/invocations/{iid}/artifacts/{name...}", api.handleArtifactPut)

	// Memory keyword-pull handle.
	mux.HandleFunc("GET /api/v1/projects/{id}/memory", api.handleMemorySearch)

	// Packs.
	mux.HandleFunc("GET /api/v1/packs", api.handleListPacks)
	mux.HandleFunc("GET /api/v1/packs/{name}", api.handleGetPack)

	// SSE event streams.
	mux.HandleFunc("GET /api/v1/events", api.handleEventStream)
	mux.HandleFunc("GET /api/v1/runs/{id}/events", api.handleRunEventStream)
}
