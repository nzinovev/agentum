package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

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

	updated, err := api.applyResume(r, task, event, "continue", payload,
		gateDecisionPatch(task, principal, gate, decisionContinued), planApproval{}, principal)
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

// approvalNameFinalReview is the orchestrator-owned approval name for the final
// gate. The plan gate's name is pack-declared (resolved via planApprovalName).
const approvalNameFinalReview = "final_review"

// planApprovalName resolves the pack-declared plan-approval name for a task,
// independent of the task's current stage (the runner may have already advanced
// past the approval stage, so keying on current_stage is a race). Returns "" if
// the pack declares no source_write approval. This is the durable key the
// task_approvals row is written under; reject and the plan-gate idempotency
// check key on it so a reject never collides with an approve.
func (api *API) planApprovalName(ctx context.Context, task sqlc.Task) string {
	if api.packs == nil {
		return ""
	}
	taskPack, err := api.packs.Resolve(ctx, task.PipelinePack)
	if err != nil {
		return ""
	}
	approval, hasApproval := taskPack.SourceWriteApproval()
	if !hasApproval {
		return ""
	}
	return approval.Name
}

// planApprovalForStage resolves the pack-declared source_write approval when the
// given stage is its approval stage. Used by the advance handler to decide
// whether to write a task_approvals row in the transition tx (only when the
// task is AT the approval stage). Returns the approval and true then; false
// otherwise (no approval block, or a different stage).
func (api *API) planApprovalForStage(ctx context.Context, task sqlc.Task, currentStage string) (planApproval, bool) {
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

// decisionIsIdempotent handles a repeat gate decision on a task that has already
// left the gate's state. When the recorded decision under name matches
// wantDecision, the repeat returns 200 with the current task and writes nothing
// — a retried POST must be safe. A conflicting decision stays a 409. Returns
// true when the handler has written its response.
//
// Keyed on the durable approval name (tenant, task, name), not on current_stage
// (which the runner has advanced) or a hardcoded constant. The caller resolves
// the name once from the pack / the final_review constant and passes it here.
func (api *API) decisionIsIdempotent(w http.ResponseWriter, r *http.Request, task sqlc.Task, name, wantDecision string) bool {
	row, err := api.queries.GetApproval(r.Context(), sqlc.GetApprovalParams{
		TenantID: task.TenantID, TaskID: task.ID, Name: name,
	})
	if err != nil {
		return false // no recorded decision — not idempotent, fall through
	}
	if row.Decision == wantDecision {
		writeJSON(w, http.StatusOK, toTaskResponse(task))
		return true
	}
	writeError(w, http.StatusConflict, codeIllegalTransition,
		"gate "+name+" already decided "+row.Decision+"; cannot "+wantDecision)
	return true
}

// handleInvocationAdvance POST /api/v1/tasks/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation). When the task's
// current stage is the pack's approval stage (ADR 0003 D3/D4), advancing IS the
// approval: the task_approvals row is written in the same tx as the transition,
// the enqueue, and the human-decision evidence. Idempotent — a repeat advance
// that matches the recorded decision returns 200 and writes nothing; a
// conflicting decision on an already-decided gate stays a 409.
func (api *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	principal, task, ok := api.requireTaskForAction(w, r, authz.ActionTaskAdvance, "GetTask(advance)")
	if !ok {
		return
	}
	planName := api.planApprovalName(r.Context(), task)
	if engine.TaskState(task.State) != engine.StatePausedGate {
		// Idempotency: if the plan gate was already decided approved, a repeat
		// advance returns the current task without re-transitioning. Keyed on the
		// pack-declared plan name (not current_stage — the runner has advanced
		// past it, so reading current_stage would race the runner).
		if planName != "" && api.decisionIsIdempotent(w, r, task, planName, "approved") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"advance requires paused_gate; task is "+task.State)
		return
	}
	decision := gateDecisionPatch(task, principal, gateAdvance, decisionApproved)
	// Write the task_approvals row only when the task is AT the approval stage.
	approvalPlan, _ := api.planApprovalForStage(r.Context(), task, currentStageOr(task.CurrentStage, ""))
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
	principal, task, ok := api.requireTaskForAction(w, r, authz.ActionTaskReject, "GetTask(reject)")
	if !ok {
		return
	}
	// Reject is valid at either human gate: awaiting_final_review (final gate)
	// or paused_gate (plan gate, which also hosts plan_not_approved / drift
	// stops). Anywhere else it is illegal unless idempotent.
	atFinalGate := engine.TaskState(task.State) == engine.StateAwaitingFinalReview
	atPlanGate := engine.TaskState(task.State) == engine.StatePausedGate
	// Resolve the durable approval name this reject binds to, ONCE, from the
	// task's state and the pack. At the final gate it is the orchestrator-owned
	// "final_review"; at the plan gate it is the pack-declared plan-approval name
	// (resolved from the pack, not hardcoded — a hardcoded "plan" would collide
	// with the recorded approve when the pack names its approval differently, and
	// ON CONFLICT DO NOTHING would silently discard the reject). The same name
	// keys the idempotency check, so a repeat reject after any gate reject
	// returns 200 regardless of which gate fired first.
	planName := api.planApprovalName(r.Context(), task)
	rejectName := approvalNameFinalReview
	if atPlanGate {
		rejectName = planName
		if rejectName == "" {
			// A pack with no source_write approval has no plan gate to reject at;
			// a paused_gate stop there is not an approval decision. Treat it as
			// final_review so the reject still records under a stable name rather
			// than guessing. (Should not happen for the shipped pack, but a pack
			// author may pause at a non-approval gate.)
			rejectName = approvalNameFinalReview
		}
	}
	if !atFinalGate && !atPlanGate {
		if api.decisionIsIdempotent(w, r, task, rejectName, "rejected") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"reject requires awaiting_final_review or paused_gate; task is "+task.State)
		return
	}
	if engine.IsTerminal(engine.TaskState(task.State)) {
		if api.decisionIsIdempotent(w, r, task, rejectName, "rejected") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition, "task is already terminal: "+task.State)
		return
	}
	next, ok := api.beginTerminalAbort(w, task)
	if !ok {
		return
	}
	decision := gateDecisionPatch(task, principal, gateReject, decisionRejected)
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			task: task, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordLenient,
		})
		if txErr != nil {
			return txErr
		}
		// Record the durable reject decision under the resolved name. A prior
		// approve under the SAME name (same gate) is a conflicting decision — but
		// CreateApproval's ON CONFLICT DO NOTHING would mask it. So when a row
		// already exists under this name with a different decision, fail the tx
		// with a 409-shape error instead of silently dropping the reject. This is
		// the case the review flagged: reject at a non-approval paused_gate used
		// to hardcode "plan", collide, and seal "approved".
		if existing, getErr := qtx.GetApproval(r.Context(), sqlc.GetApprovalParams{
			TenantID: task.TenantID, TaskID: task.ID, Name: rejectName,
		}); getErr == nil && existing.Decision != "rejected" {
			return conflictingGateDecision{name: rejectName, existing: existing.Decision}
		}
		if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
			TenantID: task.TenantID, TaskID: task.ID, Name: rejectName,
			Decision: "rejected", ArtifactRevisionID: sql.NullString{}, Actor: principal.UserID,
		}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
			return createErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		var conflict conflictingGateDecision
		if errors.As(err, &conflict) {
			writeError(w, http.StatusConflict, codeIllegalTransition,
				"gate "+conflict.name+" already decided "+conflict.existing+"; cannot reject")
			return
		}
		logUnexpected(api.log, err, "RejectTask tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

// conflictingGateDecision is returned inside the reject tx when a decision row
// already exists under the resolved gate name with a different decision. The
// handler surfaces it as a 409 rather than letting CreateApproval's
// ON CONFLICT DO NOTHING silently discard the reject.
type conflictingGateDecision struct {
	name     string
	existing string
}

func (conflict conflictingGateDecision) Error() string {
	return "gate " + conflict.name + " already decided " + conflict.existing
}

// handleInvocationApprove POST /api/v1/tasks/{id}/invocations/{iid}/approve
// Final approval → task done. Memory commit (Epic 1) is deferred. The teardown
// job records result_commit (the agentum/<task-id> tip) and removes the worktree
// only — the branch + result_commit remain resolvable for review (F.6.1 AC #3).
func (api *API) handleInvocationApprove(w http.ResponseWriter, r *http.Request) {
	principal, task, ok := api.requireTaskForAction(w, r, authz.ActionTaskApprove, "GetTask(approve)")
	if !ok {
		return
	}
	if engine.TaskState(task.State) != engine.StateAwaitingFinalReview {
		// Idempotency (ADR 0003 D4): a repeat approve on a task that already
		// reached done returns 200 with the current task when the recorded
		// final_review decision matches "approved"; a conflicting decision stays
		// a 409. A retried POST must be safe.
		if api.decisionIsIdempotent(w, r, task, approvalNameFinalReview, "approved") {
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
	decision := gateDecisionPatch(task, principal, gateFinal, decisionApproved)
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			task: task, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordStrict,
		})
		if txErr != nil {
			return txErr
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
	principal, task, ok := api.requireTaskForAction(w, r, authz.ActionTaskCancel, "GetTask(cancel)")
	if !ok {
		return
	}
	if engine.IsTerminal(engine.TaskState(task.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "task is already terminal: "+task.State)
		return
	}

	next, ok := api.beginTerminalAbort(w, task)
	if !ok {
		return
	}
	// Transactional outbox: abort transition + teardown enqueue in one tx. The
	// cancel decision rides in the same tx under recordLenient: a sealed or
	// missing manifest (Init is best-effort, and a crash may have sealed the
	// manifest mid-flight) is absorbed so the cancel still lands — cancel is an
	// emergency exit and must be the most tolerant handler. A real write error
	// still fails the tx, which is correct: the transition did not commit.
	decision := gateDecisionPatch(task, principal, gateCancel, decisionRejected)
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			task: task, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordLenient,
		})
		if txErr != nil {
			return txErr
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

// jobKindTeardown is the job every terminal transition enqueues: it captures
// result_commit and removes the worktree, leaving the branch resolvable.
const jobKindTeardown = "teardown"

// lifecycleTransition is the shape every lifecycle write shares: the state the
// FSM lands in, the job that drives what happens next, and the human decision
// that authorized it. Grouping them keeps applyTransition's signature readable
// at the call sites, which differ only in these fields.
type lifecycleTransition struct {
	task sqlc.Task
	next engine.TaskState
	// jobKind is the driving job to enqueue ("teardown" for the terminal
	// transitions, the resume kind for continue/advance).
	jobKind string
	// jobPayload is the job body. Empty (or a literal JSON null, which is what
	// an absent request body decodes to) becomes "{}" — a job row always
	// carries a valid JSON object.
	jobPayload []byte
	decision   manifest.Body
	policy     recordPolicy
}

// applyTransition performs the three writes every lifecycle transaction opens
// with — the FSM state update, the driving job enqueue, and the human-decision
// evidence — and returns the updated task row. It runs inside the caller's
// runInTx closure, so the decision commits atomically with the state change it
// describes; callers add whatever else belongs in their own transaction (an
// approval row, a conflicting-decision check) after it returns.
func (api *API) applyTransition(ctx context.Context, qtx *sqlc.Queries, principal authz.Principal, transition lifecycleTransition) (sqlc.Task, error) {
	jobPayload := transition.jobPayload
	if len(jobPayload) == 0 || string(jobPayload) == "null" {
		jobPayload = []byte("{}")
	}
	transitioned, transitionErr := qtx.UpdateTaskState(ctx, sqlc.UpdateTaskStateParams{
		ID: transition.task.ID, TenantID: principal.TenantID, State: string(transition.next),
	})
	if transitionErr != nil {
		return sqlc.Task{}, transitionErr
	}
	if _, enqueueErr := qtx.EnqueueJob(ctx, sqlc.EnqueueJobParams{
		TenantID: principal.TenantID, UserID: principal.UserID,
		TaskID: transition.task.ID, Kind: transition.jobKind, Payload: jobPayload,
	}); enqueueErr != nil {
		return sqlc.Task{}, enqueueErr
	}
	if err := api.recordHumanDecisionTx(ctx, qtx, principal, transition.task.ID, transition.decision, transition.policy); err != nil {
		return sqlc.Task{}, err
	}
	return transitioned, nil
}

// beginTerminalAbort aborts the in-flight run and resolves the state EventCancel
// lands the task in. Shared by reject and cancel — the two terminal aborts,
// which differ in what they record, not in how they stop the run. The in-flight
// run is aborted first so the worker stops touching the task; the cancel
// registry reports false when no run is active (a paused task), which is fine.
// Writes the 409 itself, so ok=false means the handler must return.
func (api *API) beginTerminalAbort(w http.ResponseWriter, task sqlc.Task) (engine.TaskState, bool) {
	if api.cancels != nil {
		api.cancels.Cancel(task.ID)
	}
	next, err := engine.Next(engine.TaskState(task.State), engine.EventCancel)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return "", false
	}
	return next, true
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
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			task: task, next: next, jobKind: kind, jobPayload: payload,
			decision: decision, policy: recordStrict,
		})
		if txErr != nil {
			return txErr
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
	principal, task, ok := api.requireTaskForAction(w, r, authz.ActionTaskCleanup, "GetTask(cleanup)")
	if !ok {
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
