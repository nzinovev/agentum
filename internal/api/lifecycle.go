package api

import (
	"context"
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
	), planApproval{}, principal)
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
// Pass a gate → the next stage runs (a fresh invocation). When the task's
// current stage is the pack's approval stage (ADR 0003 D3/D4), advancing IS the
// approval: the task_approvals row is written in the same tx as the transition,
// the enqueue, and the human-decision evidence. Idempotent — a repeat advance
// that matches the recorded decision returns 200 and writes nothing; a
// conflicting decision on an already-decided gate stays a 409.
func (api *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:advance", r.PathValue("id")); !decision.Allowed {
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
	if engine.TaskState(task.State) != engine.StatePausedGate {
		// Idempotency: if this gate was already decided approved, a repeat
		// advance returns the current task without re-transitioning (the task
		// has already moved past the gate). A different decision stays a 409.
		if api.advanceIsIdempotent(w, r, task, principal) {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"advance requires paused_gate; task is "+task.State)
		return
	}
	decision := humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gateAdvance, decisionApproved,
		principal.UserID, time.Now().UTC(),
	)
	// Resolve the pack's approval declaration (if any) for the current stage.
	// When the stage is the approval stage, the task_approvals row joins the tx.
	approvalPlan, _ := api.planStageApproval(r.Context(), task, currentStageOr(task.CurrentStage, ""))
	updated, err := api.applyResume(r, task, engine.EventAdvance, "advance", nil, decision, approvalPlan, principal)
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the advance decision; the task was not advanced")
			return
		}
		statusForTransition(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// advanceIsIdempotent handles a repeat advance on a task that has already left
// paused_gate. When the pack declared an approval on the current stage and the
// recorded decision matches "approved", the repeat returns 200 with the current
// task and writes nothing — a retried POST must be safe. A conflicting decision
// stays a 409. Returns true when the handler has written its response.
func (api *API) advanceIsIdempotent(w http.ResponseWriter, r *http.Request, task sqlc.Task, principal authz.Principal) bool {
	plan, ok := api.planStageApproval(r.Context(), task, currentStageOr(task.CurrentStage, ""))
	if !ok {
		return false
	}
	row, err := api.queries.GetApproval(r.Context(), sqlc.GetApprovalParams{
		TenantID: task.TenantID, TaskID: task.ID, Name: plan.name,
	})
	if err != nil {
		return false // no recorded decision — not idempotent, fall through to 409
	}
	if row.Decision == "approved" {
		writeJSON(w, http.StatusOK, toTaskResponse(task))
		return true
	}
	writeError(w, http.StatusConflict, codeIllegalTransition,
		"gate "+plan.name+" already decided "+row.Decision+"; cannot approve")
	return true
}

// planStageApproval resolves the pack's source_write approval declaration and
// reports whether the given stage is its approval stage. Returns the approval
// and true when the pack declares an approval hosted by currentStage; false
// otherwise (no approval block, or a different stage). Used by the advance
// handler to decide whether to write a task_approvals row in the transition tx.
func (api *API) planStageApproval(ctx context.Context, task sqlc.Task, currentStage string) (planApproval, bool) {
	if api.packs == nil || currentStage == "" {
		return planApproval{}, false
	}
	taskPack, err := api.packs.Resolve(ctx, task.PipelinePack)
	if err != nil {
		return planApproval{}, false
	}
	approval, hasApproval := taskPack.SourceWriteApproval()
	if !hasApproval || approval.Stage != currentStage {
		return planApproval{}, false
	}
	return planApproval{
		name: approval.Name, stage: approval.Stage, artifact: approval.Artifact,
	}, true
}

// planApproval carries the resolved approval declaration into the resume tx, so
// applyResume can write the task_approvals row alongside the transition. The
// revision id is resolved inside the tx (reading the current plan revision).
type planApproval struct {
	name     string
	stage    string
	artifact string
}

// finalDecisionIsIdempotent handles a repeat approve or reject on a task that
// has already left awaiting_final_review (ADR 0003 D4). When the recorded
// final_review decision matches wantDecision, the repeat returns 200 with the
// current task and writes nothing; a conflicting decision stays a 409. Returns
// true when the handler has written its response.
func (api *API) finalDecisionIsIdempotent(w http.ResponseWriter, r *http.Request, task sqlc.Task, wantDecision string) bool {
	row, err := api.queries.GetApproval(r.Context(), sqlc.GetApprovalParams{
		TenantID: task.TenantID, TaskID: task.ID, Name: "final_review",
	})
	if err != nil {
		return false // no recorded decision — not idempotent, fall through to 409
	}
	if row.Decision == wantDecision {
		writeJSON(w, http.StatusOK, toTaskResponse(task))
		return true
	}
	writeError(w, http.StatusConflict, codeIllegalTransition,
		"final_review already decided "+row.Decision+"; cannot "+wantDecision)
	return true
}

// handleRejectTask POST /api/v1/tasks/{id}/reject
// Terminal reject at either human gate (ADR 0003 D4). Reuse of EventCancel
// means the task lands in `cancelled`; a distinct task_approvals row
// (final_review, decision=rejected) and seal reason SealRejected keep the
// sealed record from describing a rejected result as an abort. At the plan gate
// this trivially satisfies "rejecting does not modify source code" — nothing
// ever unlocked source-write, so there is no source change to undo. Reject is
// terminal and preserves everything: worktree torn down, branch retained,
// manifest sealed. Idempotent: a repeat reject matching the recorded decision
// returns 200.
func (api *API) handleRejectTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:reject", r.PathValue("id")); !decision.Allowed {
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
	// Reject is valid at either human gate: awaiting_final_review (final gate)
	// or paused_gate (plan gate). Anywhere else it is illegal unless idempotent.
	atFinalGate := engine.TaskState(task.State) == engine.StateAwaitingFinalReview
	atPlanGate := engine.TaskState(task.State) == engine.StatePausedGate
	if !atFinalGate && !atPlanGate {
		if api.finalDecisionIsIdempotent(w, r, task, "rejected") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"reject requires awaiting_final_review or paused_gate; task is "+task.State)
		return
	}
	if engine.IsTerminal(engine.TaskState(task.State)) {
		if api.finalDecisionIsIdempotent(w, r, task, "rejected") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition, "task is already terminal: "+task.State)
		return
	}
	// Abort the in-flight run first so the worker stops touching the task.
	if api.cancels != nil {
		api.cancels.Cancel(task.ID)
	}
	next, err := engine.Next(engine.TaskState(task.State), engine.EventCancel)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	decision := humanDecisionPatch(
		currentStageOr(task.CurrentStage, ""), gateReject, decisionRejected,
		principal.UserID, time.Now().UTC(),
	)
	approvalName := "final_review"
	if atPlanGate {
		// At the plan gate, record the decision against the pack's approval name
		// when one exists, so the plan-approval audit and the final-review audit
		// are distinguishable.
		if plan, ok := api.planStageApproval(r.Context(), task, currentStageOr(task.CurrentStage, "")); ok {
			approvalName = plan.name
		} else {
			approvalName = "plan"
		}
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
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "teardown", Payload: []byte("{}"),
		}); enqueueErr != nil {
			return enqueueErr
		}
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision, recordLenient); err != nil {
			return err
		}
		// Record the durable reject decision. CreateApproval's ON CONFLICT DO
		// NOTHING makes a repeated reject idempotent.
		if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
			TenantID: task.TenantID, TaskID: task.ID, Name: approvalName,
			Decision: "rejected", ArtifactRevisionID: sql.NullString{}, Actor: principal.UserID,
		}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
			return createErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "RejectTask tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
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
	if engine.TaskState(task.State) != engine.StateAwaitingFinalReview {
		// Idempotency (ADR 0003 D4): a repeat approve on a task that already
		// reached done returns 200 with the current task when the recorded
		// final_review decision matches "approved"; a conflicting decision stays
		// a 409. A retried POST must be safe.
		if api.finalDecisionIsIdempotent(w, r, task, "approved") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"approve requires awaiting_final_review; task is "+task.State)
		return
	}
	next, err := engine.Next(engine.TaskState(task.State), engine.EventApprove)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	// Transactional outbox: the done transition, the teardown-job enqueue, the
	// human-decision evidence, and the final_review approval row commit
	// atomically. result_commit capture happens inside the teardown job (the
	// runner owns the worktree manager) before the worktree is removed. The
	// approval decision rides in the same tx as the transition it gates: a
	// crash between them cannot leave a task that advanced past final approval
	// with no record of who let it through.
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
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision, recordStrict); err != nil {
			return err
		}
		// ADR 0003 D4: record the durable final_review approval row in the same
		// tx. No bound artifact — final_review approves the run, not a document.
		if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
			TenantID: task.TenantID, TaskID: task.ID, Name: "final_review",
			Decision: "approved", ArtifactRevisionID: sql.NullString{}, Actor: principal.UserID,
		}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
			return createErr
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
	// cancel decision rides in the same tx under recordLenient: a sealed or
	// missing manifest (Init is best-effort, and a crash may have sealed the
	// manifest mid-flight) is absorbed so the cancel still lands — cancel is an
	// emergency exit and must be the most tolerant handler. A real write error
	// still fails the tx, which is correct: the transition did not commit.
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
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision, recordLenient); err != nil {
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
// Transactional outbox (F.6.1 AC #6): the transition, the enqueue, the
// human-decision evidence, and (when the stage hosts the pack's approval) the
// task_approvals row commit in one tx, so a resume can never leave the task
// running with no driver job and no record of who resumed it — nor an approved
// plan with no durable approval row. principal is threaded explicitly so this
// helper stays callable from handlers that already required it.
func (api *API) applyResume(r *http.Request, task sqlc.Task, event engine.TaskEvent, kind string, payload []byte, decision manifest.Body, approval planApproval, principal authz.Principal) (sqlc.Task, error) {
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
		if err := api.recordHumanDecisionTx(r.Context(), qtx, principal, task.ID, decision, recordStrict); err != nil {
			return err
		}
		// ADR 0003 D4: when this resume advances past the pack's approval stage,
		// the task_approvals row joins the same tx. CreateApproval's
		// ON CONFLICT DO NOTHING makes a repeated advance idempotent — a retried
		// POST that lost a race returns no rows, which we treat as "already
		// decided" rather than a failure.
		if approval.name != "" {
			revisionID, revErr := api.resolveApprovalRevisionID(r.Context(), qtx, task, approval)
			if revErr != nil {
				return revErr
			}
			if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
				TenantID: task.TenantID, TaskID: task.ID, Name: approval.name,
				Decision: "approved", ArtifactRevisionID: revisionID, Actor: principal.UserID,
			}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
				return createErr
			}
		}
		updated = transitioned
		return nil
	}); err != nil {
		return sqlc.Task{}, err
	}
	return updated, nil
}

// resolveApprovalRevisionID reads the current revision id of the approval
// artifact (e.g. plan/plan.md) inside the resume tx, so the approval row binds
// to exactly the revision the human approved. An empty/missing revision yields
// a NULL revision id — the approval still records the decision; drift detection
// simply cannot fire without a bound revision.
func (api *API) resolveApprovalRevisionID(ctx context.Context, qtx *sqlc.Queries, task sqlc.Task, approval planApproval) (sql.NullString, error) {
	if api.art == nil {
		return sql.NullString{}, nil
	}
	revisionName := approval.stage + "/" + approval.artifact
	revision, err := qtx.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: task.ID, TenantID: task.TenantID, Name: revisionName,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullString{}, nil
		}
		return sql.NullString{}, err
	}
	return sql.NullString{String: revision.ID, Valid: true}, nil
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
