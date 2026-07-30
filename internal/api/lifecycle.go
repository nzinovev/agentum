package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// msgTaskNotFound is the user-facing message for a missing task. Shared across
// the lifecycle handlers so the wording stays stable.
const msgTaskNotFound = "task not found"

// handleInvocationContinue POST /api/v1/tasks/{id}/invocations/{iid}/continue
// Resume after open_questions / user_stop (session-id resume). The body carries
// optional answers/context appended to the resumed session.
func (api *API) handleInvocationContinue(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	// Continue is valid from either open-questions or user-stop pause.
	id := r.PathValue("id")
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: principalTenant(r)})
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

	updated, err := api.applyResume(r, task, event, "continue", payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// handleInvocationAdvance POST /api/v1/tasks/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation). For the backend-dev
// pack this is the plan-approval human gate: the decision is recorded as audit
// evidence before the source-writing stages are allowed to run.
func (api *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	task, ok := api.resumeFromPauseRead(w, r, engine.StatePausedGate, engine.EventAdvance, "advance")
	if !ok {
		return
	}
	api.recordHumanGate(r, task, "approval", "approved")
	writeJSON(w, http.StatusOK, toTaskResponse(task))
}

// handleInvocationApprove POST /api/v1/tasks/{id}/invocations/{iid}/approve
// Final approval → task done. Memory commit (Epic 1) is deferred. The teardown
// job records result_commit (the agentum/<task-id> tip) and removes the worktree
// only — the branch + result_commit remain resolvable for review (F.6.1 AC #3).
func (api *API) handleInvocationApprove(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:approve", r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
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
	// Transactional outbox: the done transition and the teardown-job enqueue
	// commit atomically. result_commit capture happens inside the teardown job
	// (the runner owns the worktree manager) before the worktree is removed.
	updated, err := api.transitionAndEnqueueTeardown(r, task, next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	api.recordHumanGate(r, updated, "final", "approved")
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// handleCancelTask POST /api/v1/tasks/{id}/cancel
// Terminal abort: any non-terminal task → cancelled. The in-flight run (if any)
// is aborted via the cancel registry, then the FSM transition + teardown-job
// enqueue commit atomically. F.6.1: cancel is a terminal ABORT, distinct from
// pause (non-terminal) and cleanup (explicit branch deletion). The teardown job
// removes the worktree only — the agentum/<task-id> branch and any committed
// recovery work survive for review (AC #4).
func (api *API) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:cancel", r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	id := r.PathValue("id")
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
			return
		}
		logUnexpected(api.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	if engine.IsTerminal(engine.TaskState(task.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "task is already terminal: "+task.State)
		return
	}

	// Abort the in-flight run first so the worker stops touching the task. The
	// registry returns false when no run is active (a paused task) — that's fine.
	if api.cancels != nil {
		api.cancels.Cancel(task.ID)
	}

	next, err := engine.Next(engine.TaskState(task.State), engine.EventCancel)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	// Transactional outbox: abort transition + teardown enqueue in one tx.
	updated, err := api.transitionAndEnqueueTeardown(r, task, next)
	if err != nil {
		logUnexpected(api.log, err, "CancelTask tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	// A cancel at the final-review gate is a result rejection; anywhere else it
	// is a mid-run abort. Both are terminal and preserve the branch + evidence —
	// the decision is recorded so the audit trail distinguishes the two.
	cancelDecision := "cancelled"
	if engine.TaskState(task.State) == engine.StateAwaitingMemoryCommit {
		cancelDecision = "rejected"
	}
	api.recordHumanGate(r, updated, "cancel", cancelDecision)
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// applyResume runs the FSM transition and enqueues the driving job, carrying an
// optional payload (continue's answers/context). Shared by continue/advance.
// Transactional outbox (F.6.1 AC #6): the transition and the enqueue commit in
// one tx, so a resume can never leave the task running with no driver job.
func (api *API) applyResume(r *http.Request, task sqlc.Task, event engine.TaskEvent, kind string, payload []byte) (sqlc.Task, error) {
	principal, _ := authz.PrincipalFrom(r.Context())
	next, err := engine.Next(engine.TaskState(task.State), event)
	if err != nil {
		return sqlc.Task{}, err
	}
	jobPayload := []byte("{}")
	if len(payload) > 0 && string(payload) != "null" {
		jobPayload = payload
	}
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, transitionErr := qtx.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
			ID: task.ID, TenantID: principal.TenantID, State: string(next),
		})
		if transitionErr != nil {
			return transitionErr
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: kind, Payload: jobPayload,
		}); enqueueErr != nil {
			return enqueueErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		return sqlc.Task{}, err
	}
	return updated, nil
}

// resumeFromPauseRead wraps resumeFromPause for the simple no-body case (advance).
func (api *API) resumeFromPauseRead(w http.ResponseWriter, r *http.Request, want engine.TaskState, event engine.TaskEvent, kind string) (sqlc.Task, bool) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return sqlc.Task{}, false
	}
	if decision := authz.Can(r.Context(), principal, "task:"+string(event), r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return sqlc.Task{}, false
	}
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
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
	updated, err := api.applyResume(r, task, event, kind, nil)
	if err != nil {
		statusForTransition(w, err)
		return sqlc.Task{}, false
	}
	return updated, true
}

// handleCleanupTask POST /api/v1/tasks/{id}/cleanup
// Explicit, idempotent branch deletion (F.6.1 AC #4). Distinct verb from
// cancel (terminal abort) and pause: cleanup operates on an ALREADY-terminal
// task and removes its delivery artifacts. A generic cancel cannot ambiguously
// mean all three. Enqueues a cleanup job (the runner owns the worktree manager
// that performs the git branch deletion); the job is idempotent, so re-posting
// is safe. Audited via the task.cleanup_done event.
func (api *API) handleCleanupTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:cleanup", r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	id := r.PathValue("id")
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
			return
		}
		logUnexpected(api.log, err, "GetTask(cleanup)")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	// Cleanup is post-terminal only. A running/paused task's branch is live
	// delivery state — deleting it would destroy in-flight work.
	if !engine.IsTerminal(engine.TaskState(task.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"cleanup requires a terminal task; task is "+task.State)
		return
	}
	if _, err := api.queries.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
		TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "cleanup", Payload: []byte("{}"),
	}); err != nil {
		logUnexpected(api.log, err, "EnqueueJob(cleanup)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, toTaskResponse(task))
}

// statusForTransition maps an engine/transition error to an HTTP response.
func statusForTransition(w http.ResponseWriter, err error) {
	var illegalErr *engine.ErrIllegalTransition
	if errors.As(err, &illegalErr) {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
}

// transitionAndEnqueueTeardown applies a terminal transition and enqueues the
// worktree teardown job inside one transaction — the transactional outbox: a
// handler that cannot enqueue rolls back the transition, so a task is never
// left terminal with no teardown intent. Shared by approve and cancel, both of
// which reach a terminal state whose worktree disposal is the teardown job.
// Identity (tenant_id, user_id) is read from the request's principal, never the
// body.
func (api *API) transitionAndEnqueueTeardown(r *http.Request, task sqlc.Task, nextState engine.TaskState) (sqlc.Task, error) {
	principal, _ := authz.PrincipalFrom(r.Context())
	var updated sqlc.Task
	transitionErr := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, err := qtx.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
			ID: task.ID, TenantID: principal.TenantID, State: string(nextState),
		})
		if err != nil {
			return err
		}
		if _, err := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
		}); err != nil {
			return err
		}
		updated = transitioned
		return nil
	})
	return updated, transitionErr
}

// principalTenant returns the tenant id from the request's principal.
func principalTenant(r *http.Request) string {
	principal, _ := authz.PrincipalFrom(r.Context())
	return principal.TenantID
}

// recordHumanGate writes one human gate decision to the evidence manifest. The
// FSM already makes the underlying action idempotent (a second advance/approve/
// cancel cannot apply from the resulting state), so the recording runs at most
// once per decision; after the task reaches a terminal state the manifest is
// sealed and AddEvidence is a logged no-op. Best-effort: a manifest write
// failure never blocks the lifecycle transition it accompanies.
func (api *API) recordHumanGate(r *http.Request, task sqlc.Task, gate, decision string) {
	if api.mfst == nil {
		return
	}
	principal, _ := authz.PrincipalFrom(r.Context())
	stage := ""
	if task.CurrentStage.Valid {
		stage = task.CurrentStage.String
	}
	patch := manifest.Body{HumanGates: []manifest.HumanDecision{{
		Stage: stage, Gate: gate, Decision: decision,
		Actor: principal.UserID, Timestamp: time.Now().UTC(),
	}}}
	if err := api.mfst.AddEvidence(r.Context(), task.TenantID, task.ID, patch); err != nil {
		api.log.Warn("record human gate", "task", task.ID, "gate", gate, "error", err)
	}
}
