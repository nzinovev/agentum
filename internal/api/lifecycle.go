package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// handleInvocationContinue POST /api/v1/tasks/{id}/invocations/{iid}/continue
// Resume after open_questions / user_stop (session-id resume). The body carries
// optional answers/context appended to the resumed session.
func (a *API) handleInvocationContinue(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	// Continue is valid from either open-questions or user-stop pause.
	id := r.PathValue("id")
	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: principalTenant(r)})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, codeBadInput, err.Error())
		return
	}
	var event engine.TaskEvent
	switch engine.TaskState(task.State) {
	case engine.StatePausedOpenQuestions, engine.StatePausedUserStop:
		event = engine.EventContinue
	default:
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"continue requires paused_open_questions or paused_user_stop; task is "+task.State)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // optional; ignored at MVP
	payload, _ := json.Marshal(body)

	updated, err := a.applyResume(r, task, event, "continue", payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// handleInvocationAdvance POST /api/v1/tasks/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation).
func (a *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	task, ok := a.resumeFromPauseRead(w, r, engine.StatePausedGate, engine.EventAdvance, "advance")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(task))
}

// handleInvocationApprove POST /api/v1/tasks/{id}/invocations/{iid}/approve
// Final approval → task done. Memory commit (Epic 1) is deferred; for now this
// completes the task and schedules worktree teardown.
func (a *API) handleInvocationApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if d := authz.Can(r.Context(), p, "task:approve", r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return
	}
	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return
		}
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	if engine.TaskState(task.State) != engine.StateAwaitingMemoryCommit {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"approve requires awaiting_memory_commit; task is "+task.State)
		return
	}
	next, err := engine.Next(engine.TaskState(task.State), engine.EventApprove)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	updated, err := a.q.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: p.TenantID, State: string(next),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	// Schedule teardown; the worker removes the worktree once the task is done.
	if _, err := a.q.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
		TenantID: p.TenantID, UserID: p.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
	}); err != nil {
		logUnexpected(a.log, err, "EnqueueJob(teardown)")
		// Non-fatal: the task is done; teardown can be retried/forced manually.
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// handleCancelTask POST /api/v1/tasks/{id}/cancel
// Cancel any non-terminal task: abort the in-flight run (if any) via the cancel
// registry, then transition to cancelled. Non-destructive — session-id resume
// and the worktree survive until teardown (PR4).
func (a *API) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if d := authz.Can(r.Context(), p, "task:cancel", r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return
	}
	id := r.PathValue("id")
	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return
		}
		logUnexpected(a.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	if engine.IsTerminal(engine.TaskState(task.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "task is already terminal: "+task.State)
		return
	}

	// Abort the in-flight run first so the worker stops touching the task. The
	// registry returns false when no run is active (a paused task) — that's fine.
	if a.cancels != nil {
		a.cancels.Cancel(task.ID)
	}

	next, err := engine.Next(engine.TaskState(task.State), engine.EventCancel)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	updated, err := a.q.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: p.TenantID, State: string(next),
	})
	if err != nil {
		logUnexpected(a.log, err, "UpdateTaskState")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	// Schedule teardown; the worker removes the worktree once the run has aborted.
	if _, err := a.q.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
		TenantID: p.TenantID, UserID: p.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
	}); err != nil {
		logUnexpected(a.log, err, "EnqueueJob(teardown)")
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// applyResume runs the FSM transition and enqueues the driving job, carrying an
// optional payload (continue's answers/context). Shared by continue/advance.
func (a *API) applyResume(r *http.Request, task sqlc.Task, event engine.TaskEvent, kind string, payload []byte) (sqlc.Task, error) {
	p, _ := authz.PrincipalFrom(r.Context())
	next, err := engine.Next(engine.TaskState(task.State), event)
	if err != nil {
		return sqlc.Task{}, err
	}
	updated, err := a.q.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
		ID: task.ID, TenantID: p.TenantID, State: string(next),
	})
	if err != nil {
		return sqlc.Task{}, err
	}
	arg := sqlc.EnqueueJobParams{
		TenantID: p.TenantID, UserID: p.UserID, TaskID: task.ID, Kind: kind,
		Payload: []byte("{}"),
	}
	if len(payload) > 0 && string(payload) != "null" {
		arg.Payload = payload
	}
	if _, err := a.q.EnqueueJob(r.Context(), arg); err != nil {
		return sqlc.Task{}, err
	}
	return updated, nil
}

// resumeFromPauseRead wraps resumeFromPause for the simple no-body case (advance).
func (a *API) resumeFromPauseRead(w http.ResponseWriter, r *http.Request, want engine.TaskState, event engine.TaskEvent, kind string) (sqlc.Task, bool) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return sqlc.Task{}, false
	}
	if d := authz.Can(r.Context(), p, "task:"+string(event), r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return sqlc.Task{}, false
	}
	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return sqlc.Task{}, false
		}
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return sqlc.Task{}, false
	}
	if engine.TaskState(task.State) != want {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			string(event)+" requires "+string(want)+"; task is "+task.State)
		return sqlc.Task{}, false
	}
	updated, err := a.applyResume(r, task, event, kind, nil)
	if err != nil {
		statusForTransition(w, err)
		return sqlc.Task{}, false
	}
	return updated, true
}

// statusForTransition maps an engine/transition error to an HTTP response.
func statusForTransition(w http.ResponseWriter, err error) {
	var ill *engine.ErrIllegalTransition
	if errors.As(err, &ill) {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
}

// principalTenant returns the tenant id from the request's principal.
func principalTenant(r *http.Request) string {
	p, _ := authz.PrincipalFrom(r.Context())
	return p.TenantID
}
