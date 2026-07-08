package api

import (
	"log/slog"
	"net/http"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// API wires the sqlc querier behind the HTTP handlers. It is constructed once
// per process and mounts the v1 surface on the server's mux via Register.
type API struct {
	q   *sqlc.Queries
	log *slog.Logger
}

func New(q *sqlc.Queries, log *slog.Logger) *API {
	return &API{q: q, log: log}
}

// Register attaches the full v1 surface to the mux. Implemented endpoints live
// here; unimplemented contract endpoints are declared in stubs.go. The server
// has already applied the boundary middleware, so every call below carries a
// Principal.
func (a *API) Register(mux *http.ServeMux) {
	// Projects (registration: one repo = one project).
	mux.HandleFunc("GET /api/v1/projects", a.handleListProjects)
	mux.HandleFunc("POST /api/v1/projects", a.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", a.handleGetProject)

	// Tasks (lifecycle).
	mux.HandleFunc("GET /api/v1/tasks", a.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks", a.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", a.handleGetTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/start", a.handleStartTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", a.handleCancelTask)

	// Stage invocations (read-only for now).
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations", a.handleListInvocations)
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations/{iid}", a.handleGetInvocation)

	// Gate actions (§3.2 stop conditions → continue semantics).
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/continue", a.handleInvocationContinue)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/advance", a.handleInvocationAdvance)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/approve", a.handleInvocationApprove)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/edit", a.handleInvocationEdit)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/ask-to-edit", a.handleInvocationAskToEdit)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/add-context", a.handleInvocationAddContext)

	// Artifacts.
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}", a.handleArtifactGet)
	mux.HandleFunc("PUT /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}", a.handleArtifactPut)

	// Memory keyword-pull handle.
	mux.HandleFunc("GET /api/v1/projects/{id}/memory", a.handleMemorySearch)

	// Packs.
	mux.HandleFunc("GET /api/v1/packs", a.handleListPacks)
	mux.HandleFunc("GET /api/v1/packs/{name}", a.handleGetPack)

	// SSE event streams.
	mux.HandleFunc("GET /api/v1/events", a.handleEventStream)
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", a.handleTaskEventStream)
}
