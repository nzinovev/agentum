package api

import (
	"log/slog"
	"net/http"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TaskCanceler aborts an in-flight run by task id. Implemented by the runner's
// CancelRegistry; declared here so the API does not import the runner package.
type TaskCanceler interface {
	Cancel(taskID string) bool
}

// API wires the sqlc querier behind the HTTP handlers. It is constructed once
// per process and mounts the v1 surface on the server's mux via Register.
type API struct {
	queries *sqlc.Queries
	log     *slog.Logger
	cancels TaskCanceler
}

// New builds the API. cancels lets the cancel handler abort an in-flight run;
// nil leaves cancel as a no-op (the FSM transition still applies).
func New(queries *sqlc.Queries, log *slog.Logger, cancels TaskCanceler) *API {
	return &API{queries: queries, log: log, cancels: cancels}
}

// Register attaches the full v1 surface to the mux. Implemented endpoints live
// here; unimplemented contract endpoints are declared in stubs.go. The server
// has already applied the boundary middleware, so every call below carries a
// Principal.
func (api *API) Register(mux *http.ServeMux) {
	// Projects (registration: one repo = one project).
	mux.HandleFunc("GET /api/v1/projects", api.handleListProjects)
	mux.HandleFunc("POST /api/v1/projects", api.handleCreateProject)
	mux.HandleFunc("GET /api/v1/projects/{id}", api.handleGetProject)

	// Tasks (lifecycle).
	mux.HandleFunc("GET /api/v1/tasks", api.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks", api.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", api.handleGetTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/start", api.handleStartTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", api.handleCancelTask)

	// Stage invocations (read-only for now).
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations", api.handleListInvocations)
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations/{iid}", api.handleGetInvocation)

	// Gate actions (§3.2 stop conditions → continue semantics).
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/continue", api.handleInvocationContinue)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/advance", api.handleInvocationAdvance)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/approve", api.handleInvocationApprove)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/edit", api.handleInvocationEdit)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/ask-to-edit", api.handleInvocationAskToEdit)
	mux.HandleFunc("POST /api/v1/tasks/{id}/invocations/{iid}/add-context", api.handleInvocationAddContext)

	// Artifacts.
	mux.HandleFunc("GET /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}", api.handleArtifactGet)
	mux.HandleFunc("PUT /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}", api.handleArtifactPut)

	// Memory keyword-pull handle.
	mux.HandleFunc("GET /api/v1/projects/{id}/memory", api.handleMemorySearch)

	// Packs.
	mux.HandleFunc("GET /api/v1/packs", api.handleListPacks)
	mux.HandleFunc("GET /api/v1/packs/{name}", api.handleGetPack)

	// SSE event streams.
	mux.HandleFunc("GET /api/v1/events", api.handleEventStream)
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", api.handleTaskEventStream)
}
