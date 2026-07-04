package api

import (
	"log/slog"
	"net/http"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// API wires the sqlc querier behind the HTTP handlers. It is constructed once
// per process and mounted on the server's mux via Register.
type API struct {
	q   *sqlc.Queries
	log *slog.Logger
}

func New(q *sqlc.Queries, log *slog.Logger) *API {
	return &API{q: q, log: log}
}

// Register attaches the v1 task surface to the mux. The server has already
// applied the boundary middleware, so every call below carries a Principal.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tasks", a.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks", a.handleCreateTask)
	mux.HandleFunc("GET /api/v1/tasks/{id}", a.handleGetTask)
	mux.HandleFunc("POST /api/v1/tasks/{id}/start", a.handleStartTask)
}
