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

// writeJSON encodes v as JSON with the given status. Errors are intentionally
// surfaced to the caller (it is the handler's last write).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is the structured error shape for all /api/v1/* failures. `code`
// is a stable machine identifier the UI branches on; `message` is a short
// human-readable explanation.
type errorBody struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Stable error codes. Add as the surface grows.
const (
	codeNotFound          = "not_found"
	codeIllegalTransition = "illegal_transition" // a task-FSM violation
	codeBadInput          = "bad_input"
	codeUnauthorized      = "unauthorized"
	codeForbidden         = "forbidden"
	codeNotImplemented    = "not_implemented"
	codeInternal          = "internal"
)

// writeError emits a structured error response.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorInfo{Code: code, Message: msg}})
}

// notImplemented is the stub response for endpoints declared in the contract
// but not yet built. `epic` names where the implementation lands.
func notImplemented(w http.ResponseWriter, epic, what string) {
	writeJSON(w, http.StatusNotImplemented, errorBody{Error: errorInfo{
		Code:    codeNotImplemented,
		Message: what + " is not implemented yet",
	}})
}

// (notImplemented epic is surfaced in docs/api.md; the epic tag is omitted from
// the JSON body to keep the shape stable — the body names what's missing, the
// contract doc names where it lands.)

// statusForEngine maps an engine.FSM error to (httpStatus, code). Returns
// (0,"") when err is nil or not engine-shaped.
func statusForEngine(err error) (int, string, bool) {
	var ill *engine.ErrIllegalTransition
	if errors.As(err, &ill) {
		return http.StatusConflict, codeIllegalTransition, true
	}
	return 0, "", false
}

// logUnexpected logs errors that are not expected control flow. Used for store
// errors that surface as 500s.
func logUnexpected(log *slog.Logger, err error, where string) {
	if _, _, ok := statusForEngine(err); ok {
		return
	}
	log.Error("unexpected store error", "where", where, "error", err)
}
