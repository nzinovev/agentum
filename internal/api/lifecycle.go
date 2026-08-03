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
	principal, ok := requirePrincipal(w, r)
	if !ok {
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
	var gate string
	switch engine.TaskState(task.State) {
	case engine.StatePausedOpenQuestions:
		event = engine.EventContinue
		gate = gateOpenQuestions
	case engine.StatePausedUserStop:
		event = engine.EventContinue
		gate = gateUserStop
	default:
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"continue requires paused_open_questions or paused_user_stop; task is "+task.State)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body) // optional; ignored at MVP
	payload, _ := json.Marshal(body)

	updated, err := api.applyResume(r, task, event, "continue", payload, humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gate, decisionContinued,
		principal.UserID, time.Now().UTC(),
	))
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the continue decision; the task was not resumed")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// handleInvocationAdvance POST /api/v1/tasks/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation).
func (api *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePrincipal(w, r); !ok {
		return
	}
	task, ok := api.resumeFromPauseRead(w, r, engine.StatePausedGate, engine.EventAdvance, "advance")
	if !ok {
		return
	}
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
	// Transactional outbox: the done transition, the teardown-job enqueue, and
	// the human-decision evidence commit atomically. result_commit capture
	// happens inside the teardown job (the runner owns the worktree manager)
	// before the worktree is removed. The approval decision rides in the same
	// tx as the transition it gates: a crash between them cannot leave a task
	// that advanced past final approval with no record of who let it through.
	decision := humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gateFinal, decisionApproved,
		principal.UserID, time.Now().UTC(),
	)
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, transitionErr := qtx.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
			ID: task.ID, TenantID: principal.TenantID, State: string(next),
		})
		if transitionErr != nil {
			return transitionErr
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
		}); enqueueErr != nil {
			return enqueueErr
		}
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision); err != nil {
			return err
		}
		updated = transitioned
		return nil
	}); err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the approval decision; the task was not advanced")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
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
	// Transactional outbox: abort transition + teardown enqueue in one tx. The
	// cancel decision rides in the same tx, but a cancel on a task whose
	// manifest sealed during a crash is legitimate — a sealed/missing manifest
	// is non-fatal here (unlike approve/advance, where failing to record the
	// decision must fail the request).
	decision := humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gateCancel, decisionRejected,
		principal.UserID, time.Now().UTC(),
	)
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, transitionErr := qtx.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
			ID: task.ID, TenantID: principal.TenantID, State: string(next),
		})
		if transitionErr != nil {
			return transitionErr
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
		}); enqueueErr != nil {
			return enqueueErr
		}
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision); err != nil {
			return err
		}
		updated = transitioned
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "CancelTask tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// applyResume runs the FSM transition and enqueues the driving job, carrying an
// optional payload (continue's answers/context). Shared by continue/advance.
// Transactional outbox (F.6.1 AC #6): the transition, the enqueue, and the
// human-decision evidence commit in one tx, so a resume can never leave the
// task running with no driver job and no record of who resumed it.
func (api *API) applyResume(r *http.Request, task sqlc.Task, event engine.TaskEvent, kind string, payload []byte, decision manifest.Body) (sqlc.Task, error) {
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
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision); err != nil {
			return err
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
	updated, err := api.applyResume(r, task, event, kind, nil, humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gateAdvance, decisionApproved,
		principal.UserID, time.Now().UTC(),
	))
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the advance decision; the task was not advanced")
			return sqlc.Task{}, false
		}
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

// principalTenant returns the tenant id from the request's principal.
func principalTenant(r *http.Request) string {
	principal, _ := authz.PrincipalFrom(r.Context())
	return principal.TenantID
}
