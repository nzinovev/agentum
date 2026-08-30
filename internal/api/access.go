// Access guards for the HTTP layer.
//
// The permission DECISION lives in internal/authz — this file is only the
// HTTP adaptation of it: turn "no principal" into a 401, turn a denied
// decision into a 403, and (for the handlers that act on a task) load the row
// behind the {id} path value. That adaptation needs this package's unexported
// response helpers and the *API dependencies, so it stays in package api
// rather than becoming a package of its own; what it must NOT do is live
// inside a resource's handler file, where a guard used by every route looks
// like it belongs to one of them.
//
// Every handler enters through requireAccess or a guard built on it.
// authz.Can is reached from exactly two places in the codebase: authorize
// below, and the route gate in internal/server.

package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// msgTaskNotFound is the user-facing message for a missing task. Shared by the
// guard and the lifecycle handlers so the wording stays stable.
const msgTaskNotFound = "task not found"

// requirePrincipal extracts the Principal, writing a structured error on failure.
// Returns false when the caller should return.
func requirePrincipal(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	principal, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "unresolved principal")
		return authz.Principal{}, false
	}
	return principal, true
}

// requireAccess is THE permission preamble: resolve the request principal, then
// authorize action on resource. Every handler starts here — directly, or
// through one of the guards below that build on it — so an unauthenticated or
// forbidden caller gets the same 401 / 403 on every route, and no handler
// hand-rolls the pair. ok=false means the response is already written and the
// handler must return.
func requireAccess(w http.ResponseWriter, r *http.Request, action, resource string) (authz.Principal, bool) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return authz.Principal{}, false
	}
	if !authorize(w, r, principal, action, resource) {
		return authz.Principal{}, false
	}
	return principal, true
}

// authorize is requireAccess's second half, for the handlers that already
// hold a principal and must authorize a SECOND resource (the manifest diff's
// ?other= task). Writes the 403 itself.
func authorize(w http.ResponseWriter, r *http.Request, principal authz.Principal, action, resource string) bool {
	if decision := authz.Can(r.Context(), principal, action, resource); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return false
	}
	return true
}

// requireTaskRead is requireAccess for the commonest case: task:read on the
// {id} path value, whose id the caller then needs. Used by the read-side
// handlers that address a task's sub-resources (artifacts, manifest,
// invocations).
func requireTaskRead(w http.ResponseWriter, r *http.Request) (authz.Principal, string, bool) {
	taskID := r.PathValue("id")
	principal, ok := requireAccess(w, r, authz.ActionTaskRead, taskID)
	if !ok {
		return authz.Principal{}, "", false
	}
	return principal, taskID, true
}

// requireTaskForAction is requireTaskRead's write-side sibling: requireAccess
// for an arbitrary action on the {id} path value, plus the task row itself,
// which every handler that acts ON a task needs before it can decide anything.
// It writes the 401 / 403 / 404 / 400 itself, so ok=false means the response
// is already written and the handler must return.
//
// A store error that is not a missing row is logged under where before the 400,
// because nothing downstream reports it: the caller sees only the status.
// handleInvocationContinue deliberately keeps its own preamble — it checks no
// action and answers a missing task with a different code.
func (api *API) requireTaskForAction(w http.ResponseWriter, r *http.Request, action, where string) (authz.Principal, sqlc.Task, bool) {
	taskID := r.PathValue("id")
	principal, ok := requireAccess(w, r, action, taskID)
	if !ok {
		return authz.Principal{}, sqlc.Task{}, false
	}
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: taskID, TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
			return authz.Principal{}, sqlc.Task{}, false
		}
		logUnexpected(api.log, err, where)
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return authz.Principal{}, sqlc.Task{}, false
	}
	return principal, task, true
}

// principalTenant returns the tenant id from the request's principal. For the
// callers that have already passed a guard and need only the tenant.
func principalTenant(r *http.Request) string {
	principal, _ := authz.PrincipalFrom(r.Context())
	return principal.TenantID
}
