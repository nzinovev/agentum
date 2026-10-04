package server

import (
	"encoding/json"
	"net/http"
)

// registerRoutes wires health, readiness, the API, and bundled frontend routes.
// The API registers first so its specific patterns take precedence over the SPA root.
func (server *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("GET /readyz", server.handleReady)
	server.api.Register(mux)
	mux.HandleFunc("GET /", serveSPA)
}

func (server *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (server *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := server.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
