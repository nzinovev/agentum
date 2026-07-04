// Package api holds the HTTP handlers behind the single front door. Handlers
// resolve the Principal from the context (injected by the server boundary),
// call authz.Can, then talk to the store via the sqlc-generated querier. They
// never read tenant_id or user_id from the request body — identity is implicit
// in the Principal.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nzinovev/agentum/internal/engine"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// isConflict reports whether err is an FSM illegal-transition error.
func isConflict(err error) bool {
	var ill *engine.ErrIllegalTransition
	return errors.As(err, &ill)
}

// logUnexpected logs errors that are not expected control flow (misses and
// conflicts are silent). Used for store errors that surface as 500s.
func logUnexpected(log *slog.Logger, err error, where string) {
	if isConflict(err) {
		return
	}
	log.Error("unexpected store error", "where", where, "error", err)
}
