package server

import (
	"encoding/json"
	"net/http"
)

// registerRoutes wires the single external surface. Real handlers move to
// internal/api and are attached here as they're built; stubs return 501 until
// the engine + sqlc-generated querier are connected.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks", s.handleCreateTask)

	// SSE: the live agent/event stream. Backed by the durable events table,
	// replayable via Last-Event-ID.
	mux.HandleFunc("GET /api/v1/events", s.handleEventStream)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	_ = principalFrom(r) // available to handlers once the querier is wired
	writeJSON(w, http.StatusNotImplemented, map[string]any{"todo": "list tasks via sqlc-generated querier"})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]any{"todo": "create task via engine + FSM"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
