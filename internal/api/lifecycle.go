package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
)

// handleInvocationContinue accepts Continue at an invocation or before the
// first invocation through POST /api/v1/runs/{id}/continue.
// Resume after open_questions or user_stop. The body is an optional
// continuation. Text reaches the resumed session, or the first stage when a
// user_stop occurred before any invocation. An empty body, {}, or null
// continues without new text. Every body
// rule — strict shape, UTF-8, byte budgets, the credentials scan — is checked
// before the FSM transition, so a refused body leaves the run untouched.
func (api *API) handleInvocationContinue(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunContinue, "GetRun(continue)")
	if !ok {
		return
	}
	if r.PathValue("iid") == "" {
		if engine.RunState(run.State) != engine.StatePausedUserStop {
			writeError(w, http.StatusConflict, codeIllegalTransition, "run-level continue requires paused_user_stop before the first invocation")
			return
		}
		_, latestErr := api.queries.LatestStageForRun(r.Context(), sqlc.LatestStageForRunParams{RunID: run.ID, TenantID: run.TenantID})
		if latestErr == nil {
			writeError(w, http.StatusConflict, codeIllegalTransition, "run already has an invocation; use its continue endpoint")
			return
		}
		if !errors.Is(latestErr, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, codeInternal, latestErr.Error())
			return
		}
	}
	// Continue is valid from either open-questions or user-stop pause.
	var event engine.RunEvent
	var gate string
	switch engine.RunState(run.State) {
	case engine.StatePausedOpenQuestions:
		event = engine.EventContinue
		gate = gateOpenQuestions
	case engine.StatePausedUserStop:
		event = engine.EventContinue
		gate = gateUserStop
	default:
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"continue requires paused_open_questions or paused_user_stop; run is "+run.State)
		return
	}
	bodyBytes, read := readRequestBody(w, r, taskinput.MaxContinueBodyBytes)
	if !read {
		return
	}
	continuation, parseErr := parseContinueBody(bodyBytes)
	if parseErr != nil {
		writeRequestBodyError(w, parseErr)
		return
	}
	// A captured session receives text after an invocation. Before the first
	// invocation, a user-stop note reaches the first stage's Task section.
	// A read failure returns 500 because it cannot establish either case.
	if continuation.Text != "" {
		latest, latestErr := api.queries.LatestStageForRun(r.Context(), sqlc.LatestStageForRunParams{
			RunID: run.ID, TenantID: run.TenantID,
		})
		if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
			logUnexpected(api.log, latestErr, "LatestStageForRun(continue)")
			writeError(w, http.StatusInternalServerError, codeInternal, latestErr.Error())
			return
		}
		// A NULL session after an invocation has no delivery target. A user
		// stop before the first invocation uses the first stage instead.
		if (errors.Is(latestErr, sql.ErrNoRows) && engine.RunState(run.State) != engine.StatePausedUserStop) ||
			(latestErr == nil && (!latest.SessionID.Valid || latest.SessionID.String == "")) {
			writeError(w, http.StatusConflict, codeIllegalTransition,
				"continue with text requires a captured session to resume; the latest invocation has none")
			return
		}
	}
	payload, marshalErr := continuation.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape; a 500 keeps an invariant break from
		// being reported as the author's fault.
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}
	updated, err := api.applyResume(r, run, event, "continue", payload,
		gateDecisionPatch(run, principal, gate, decisionContinued), planApproval{}, principal)
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the continue decision; the run was not resumed")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// parseContinueBody turns the raw continue body into the typed, validated
// continuation. Pure (no DB, no HTTP), like parseRunCreate: every boundary
// rule is exercisable without a database. The secret scan runs here so
// credential-shaped text is refused before the job row exists;
// ErrSecretDetected flows out for the handler to map.
func parseContinueBody(body []byte) (taskinput.Continuation, error) {
	continuation, parseErr := taskinput.ParseContinuation(body)
	if parseErr != nil {
		return taskinput.Continuation{}, parseErr
	}
	if scanErr := scanContinuationForCredentials(continuation.Text); scanErr != nil {
		return taskinput.Continuation{}, scanErr
	}
	return continuation, nil
}

// scanContinuationForCredentials is the containment guard for the continue
// text: the same prose scanner and reject policy the run request fields get,
// because the text reaches a model verbatim through the routing block's Task
// section exactly as a description does. An empty text scans nothing.
func scanContinuationForCredentials(text string) error {
	if text == "" {
		return nil
	}
	scanner := artifacts.NewProseScanner(artifacts.PolicyReject)
	_, scanErr := scanner.Scan("text", "continue_request", []byte(text))
	return scanErr
}

// approvalNameFinalReview is the orchestrator-owned approval name for the final
// gate. The plan gate's name is pack-declared (resolved via planApprovalName).
const approvalNameFinalReview = "final_review"

// resolveRunPack resolves the run's effective pack — the project layer at the
// run's pinned base_commit over the builtin source — the same resolution the
// runner executes. base_commit and checkout_path are pinned, so the result is
// deterministic; an error is a real failure and handlers surface it (500),
// never a silent builtin fallback: the gate handlers write run_approvals
// under the pack-declared approval name, and a fallback could write the
// builtin's name while the runner reads the project pack's, leaving
// source-write locked for the rest of the run. A run that never started has
// no pinned commit to read a project layer from, so it resolves the builtin
// of the name — or (nil, nil) when even that fails, the pre-start shape
// callers already understand.
func (api *API) resolveRunPack(ctx context.Context, run sqlc.Run) (*pack.Pack, error) {
	if api.packs == nil {
		return nil, nil
	}
	if !run.BaseCommit.Valid || run.BaseCommit.String == "" || run.CheckoutPath == "" {
		// The pin order guarantees both are set once a run has started; empty
		// values mean the run never started, and without a pinned commit
		// there is no project layer to read — the builtin of the name is the
		// only resolvable pack. A failure here returns (nil, nil), the
		// pre-start shape every helper already understands.
		resolved, err := api.packs.ResolveBuiltin(ctx, run.PipelinePack)
		if err != nil {
			return nil, nil
		}
		return resolved.Pack, nil
	}
	resolved, err := api.packs.ResolveForCommit(ctx, run.PipelinePack, run.CheckoutPath, run.BaseCommit.String)
	if err != nil {
		return nil, fmt.Errorf("resolve pack %q at %s: %w", run.PipelinePack, run.BaseCommit.String, err)
	}
	return resolved.Pack, nil
}

// planApprovalName resolves the pack-declared plan-approval name for a run,
// independent of the run's current stage (the runner may have already advanced
// past the approval stage, so keying on current_stage is a race). Returns ""
// if the pack declares no source_write approval (or the run never started, so
// no pack is pinned). This is the durable key the run_approvals row is written
// under; reject and the plan-gate idempotency check key on it so a reject
// never collides with an approve.
func (api *API) planApprovalName(ctx context.Context, run sqlc.Run) (string, error) {
	runPack, err := api.resolveRunPack(ctx, run)
	if err != nil {
		return "", err
	}
	if runPack == nil {
		return "", nil
	}
	approval, hasApproval := runPack.SourceWriteApproval()
	if !hasApproval {
		return "", nil
	}
	return approval.Name, nil
}

// planApprovalForStage resolves the pack-declared source_write approval when the
// given stage is its approval stage. Used by the advance handler to decide
// whether to write a run_approvals row in the transition tx (only when the
// run is AT the approval stage). Returns the approval and true then; false
// otherwise (no approval block, a different stage, or a run that never
// started).
func (api *API) planApprovalForStage(ctx context.Context, run sqlc.Run, currentStage string) (planApproval, bool, error) {
	if currentStage == "" {
		return planApproval{}, false, nil
	}
	runPack, err := api.resolveRunPack(ctx, run)
	if err != nil {
		return planApproval{}, false, err
	}
	if runPack == nil {
		return planApproval{}, false, nil
	}
	approval, hasApproval := runPack.SourceWriteApproval()
	if !hasApproval || approval.Stage != currentStage {
		return planApproval{}, false, nil
	}
	return planApproval{
		name: approval.Name, stage: approval.Stage, artifact: approval.Artifact,
		withinStage: approval.WithinStage,
	}, true, nil
}

// planApproval carries the resolved approval declaration into the resume tx, so
// applyResume can write the run_approvals row alongside the transition. The
// revision id is resolved inside the tx (reading the current plan revision).
type planApproval struct {
	name        string
	stage       string
	artifact    string
	withinStage bool
	// expectedRevisionID is the plan revision the human's request names. When
	// set, the resume tx refuses unless it is still the current revision: an
	// answer given to one revision must never land on a newer one the human
	// has not read (ask-to-edit produces exactly such a revision).
	expectedRevisionID string
	// verifyOnly checks expectedRevisionID without writing the approval row:
	// ask-to-edit answers the gate but approves nothing.
	verifyOnly bool
}

// staleRevisionError is the in-tx refusal of a gate answer whose
// expected_revision_id is no longer the current plan revision. Mapped to 409
// conflict by statusForTransition; the tx rolls back, so no job is enqueued.
type staleRevisionError struct {
	expected string
	current  string
}

func (stale staleRevisionError) Error() string {
	return "the plan revision changed: the request names " + stale.expected +
		", the current revision is " + stale.current + "; read the current plan and answer again"
}

// revisionPreconditionMissingError is the in-tx form of the 428: the plan
// gained a revision after the handler's read, and the request names none.
type revisionPreconditionMissingError struct {
	current string
}

func (missing revisionPreconditionMissingError) Error() string {
	return "the plan revision is required at the plan gate (expected_revision_id on advance, target_revision_id on ask-to-edit); current: " + missing.current
}

// maxGateAnswerBodyBytes caps the advance body: one revision id. The cap is
// transport hygiene only.
const maxGateAnswerBodyBytes = 4096

// parseAdvanceBody strictly decodes the optional advance body. An empty body
// (or null, or {}) names no revision; anything but a single object carrying at
// most expected_revision_id is refused.
func parseAdvanceBody(body []byte) (string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var fields struct {
		ExpectedRevisionID string `json:"expected_revision_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return "", fmt.Errorf("advance: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", errors.New("advance: request body must contain exactly one JSON object")
	}
	return strings.TrimSpace(fields.ExpectedRevisionID), nil
}

// requirePlanRevisionPrecondition refuses a plan-gate answer that names no
// revision while the plan has one: 428 precondition_missing, the same rule an
// artifact PUT follows. Without it a client showing an older revision could
// approve — or send remarks to — a revision its human never read. The
// authoritative comparison runs again inside the resume tx; this read only
// decides whether the precondition was required. Writes the response itself,
// so false means the handler must return.
func (api *API) requirePlanRevisionPrecondition(w http.ResponseWriter, r *http.Request, run sqlc.Run, approval planApproval) bool {
	if approval.expectedRevisionID != "" {
		return true
	}
	current, err := api.resolveApprovalRevisionID(r.Context(), api.queries, run, approval)
	if err != nil {
		logUnexpected(api.log, err, "CurrentArtifactRevisionForName(plan gate)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return false
	}
	if current.Valid {
		writeError(w, http.StatusPreconditionRequired, codePreconditionMissing,
			revisionPreconditionMissingError{current: current.String}.Error())
		return false
	}
	return true
}

// decisionIsIdempotent handles a repeat gate decision on a run that has already
// left the gate's state. When the recorded decision under name matches
// wantDecision, the repeat returns 200 with the current run and writes nothing
// — a retried POST must be safe. A conflicting decision stays a 409. Returns
// true when the handler has written its response.
//
// Keyed on the durable approval name (tenant, run, name), not on current_stage
// (which the runner has advanced) or a hardcoded constant. The caller resolves
// the name once from the pack / the final_review constant and passes it here.
func (api *API) decisionIsIdempotent(w http.ResponseWriter, r *http.Request, run sqlc.Run, name, wantDecision string) bool {
	row, err := api.queries.GetApproval(r.Context(), sqlc.GetApprovalParams{
		TenantID: run.TenantID, RunID: run.ID, Name: name,
	})
	if err != nil {
		return false // no recorded decision — not idempotent, fall through
	}
	if row.Decision == wantDecision {
		writeJSON(w, http.StatusOK, toRunResponse(run))
		return true
	}
	writeError(w, http.StatusConflict, codeIllegalTransition,
		"gate "+name+" already decided "+row.Decision+"; cannot "+wantDecision)
	return true
}

// hasRejectedApproval finds a prior terminal reject without resolving the pack.
// A plan reject is stored under the pack's approval name, which is unavailable
// once the run leaves that gate and its checkout may no longer be readable.
func (api *API) hasRejectedApproval(ctx context.Context, run sqlc.Run) (bool, error) {
	approvals, err := api.queries.ListApprovalsForRun(ctx, sqlc.ListApprovalsForRunParams{
		TenantID: run.TenantID, RunID: run.ID,
	})
	if err != nil {
		return false, err
	}
	for _, approval := range approvals {
		if approval.Decision == "rejected" {
			return true, nil
		}
	}
	return false, nil
}

// handleInvocationAdvance POST /api/v1/runs/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation). When the run's
// current stage is the pack's approval stage (ADR 0003 D3/D4), advancing IS the
// approval: the run_approvals row is written in the same tx as the transition,
// the enqueue, and the human-decision evidence. Idempotent — a repeat advance
// that matches the recorded decision returns 200 and writes nothing; a
// conflicting decision on an already-decided gate stays a 409.
func (api *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunAdvance, "GetRun(advance)")
	if !ok {
		return
	}
	planName, nameErr := api.planApprovalName(r.Context(), run)
	if engine.RunState(run.State) != engine.StatePausedGate {
		// Off the gate the approval name only keys the idempotency lookup, so
		// a pack that does not resolve — the run failed on that very pack, or
		// its checkout moved — must not turn the answer into a 500: the
		// advance is illegal either way, and that is what the caller is told.
		if nameErr != nil {
			api.log.Warn("resolve run pack for the advance idempotency check; answering without it",
				"run", run.ID, "error", nameErr)
		}
		// Idempotency: if the plan gate was already decided approved, a repeat
		// advance returns the current run without re-transitioning. Keyed on the
		// pack-declared plan name (not current_stage — the runner has advanced
		// past it, so reading current_stage would race the runner).
		if nameErr == nil && planName != "" && api.decisionIsIdempotent(w, r, run, planName, "approved") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"advance requires paused_gate; run is "+run.State)
		return
	}
	if nameErr != nil {
		logUnexpected(api.log, nameErr, "resolveRunPack(advance)")
		writeError(w, http.StatusInternalServerError, codeInternal, nameErr.Error())
		return
	}
	bodyBytes, read := readRequestBody(w, r, maxGateAnswerBodyBytes)
	if !read {
		return
	}
	expectedRevisionID, parseErr := parseAdvanceBody(bodyBytes)
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, parseErr.Error())
		return
	}
	decision := gateDecisionPatch(run, principal, gateAdvance, decisionApproved)
	// Write the run_approvals row only when the run is AT the approval stage;
	// there the advance approves exactly the revision it names.
	approvalPlan, atApproval, approvalErr := api.planApprovalForStage(r.Context(), run, currentStageOr(run.CurrentStage, ""))
	if approvalErr != nil {
		logUnexpected(api.log, approvalErr, "resolveRunPack(advance approval)")
		writeError(w, http.StatusInternalServerError, codeInternal, approvalErr.Error())
		return
	}
	if atApproval {
		approvalPlan.expectedRevisionID = expectedRevisionID
		if approvalPlan.withinStage && expectedRevisionID == "" {
			writeError(w, http.StatusConflict, codeIllegalTransition, "the short plan must have a revision before source_write can be approved")
			return
		}
		if !api.requirePlanRevisionPrecondition(w, r, run, approvalPlan) {
			return
		}
	}
	updated, err := api.applyResume(r, run, engine.EventAdvance, "advance", nil, decision, approvalPlan, principal)
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the advance decision; the run was not advanced")
			return
		}
		statusForTransition(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// handleRejectRun POST /api/v1/runs/{id}/reject
// Terminal reject at either human gate (ADR 0003 D4). Reuse of EventCancel
// means the run lands in `cancelled`; a distinct run_approvals row
// (final_review, decision=rejected) and seal reason SealRejected keep the
// sealed record from describing a rejected result as an abort. At the plan gate
// this trivially satisfies "rejecting does not modify source code" — nothing
// ever unlocked source-write, so there is no source change to undo. Reject is
// terminal and keeps the worktree and branch. The manifest is sealed.
// Idempotent: a repeat reject matching the recorded decision
// returns 200.
func (api *API) handleRejectRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunReject, "GetRun(reject)")
	if !ok {
		return
	}
	// Reject is valid at either human gate: awaiting_final_review (final gate)
	// or paused_gate (plan gate, which also hosts plan_not_approved / drift
	// stops). Anywhere else it is illegal unless idempotent.
	atFinalGate := engine.RunState(run.State) == engine.StateAwaitingFinalReview
	atPlanGate := engine.RunState(run.State) == engine.StatePausedGate
	// Resolve the durable approval name this reject binds to, ONCE, from the
	// run's state and the pack. At the final gate it is the orchestrator-owned
	// "final_review"; at the plan gate it is the pack-declared plan-approval name
	// (resolved from the pack, not hardcoded — a hardcoded "plan" would collide
	// with the recorded approve when the pack names its approval differently, and
	// ON CONFLICT DO NOTHING would discard the reject). A repeat reject reads
	// the durable decisions after the run leaves its gate.
	// The pack is read only at the plan gate, the one place its approval name
	// is used: everywhere else the name is final_review, and a pack that does
	// not resolve (the run failed on that very pack, or its checkout moved)
	// must not turn an illegal-state 409 into a 500.
	rejectName := approvalNameFinalReview
	if atPlanGate {
		planName, nameErr := api.planApprovalName(r.Context(), run)
		if nameErr != nil {
			logUnexpected(api.log, nameErr, "resolveRunPack(reject)")
			writeError(w, http.StatusInternalServerError, codeInternal, nameErr.Error())
			return
		}
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
		alreadyRejected, approvalErr := api.hasRejectedApproval(r.Context(), run)
		if approvalErr != nil {
			logUnexpected(api.log, approvalErr, "ListApprovalsForRun(reject)")
			writeError(w, http.StatusInternalServerError, codeInternal, "could not read the run's decisions")
			return
		}
		if alreadyRejected {
			writeJSON(w, http.StatusOK, toRunResponse(run))
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"reject requires awaiting_final_review or paused_gate; run is "+run.State)
		return
	}
	next, ok := api.beginTerminalAbort(w, run)
	if !ok {
		return
	}
	cancelReason := "rejected_at_plan"
	if atFinalGate {
		cancelReason = "rejected_at_final_review"
	}
	decision := gateDecisionPatch(run, principal, gateReject, decisionRejected)
	var updated sqlc.Run
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			run: run, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordLenient, cancelReason: cancelReason,
		})
		if txErr != nil {
			return txErr
		}
		// Record the durable reject decision under the resolved name. A prior
		// approve under the SAME name (same gate) is a conflicting decision — but
		// CreateApproval's ON CONFLICT DO NOTHING would mask it. So when a row
		// already exists under this name with a different decision, fail the tx
		// with a 409-shape error instead of dropping the reject. This is
		// the case the review flagged: reject at a non-approval paused_gate used
		// to hardcode "plan", collide, and seal "approved".
		if existing, getErr := qtx.GetApproval(r.Context(), sqlc.GetApprovalParams{
			TenantID: run.TenantID, RunID: run.ID, Name: rejectName,
		}); getErr == nil && existing.Decision != "rejected" {
			return conflictingGateDecision{name: rejectName, existing: existing.Decision}
		}
		if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
			TenantID: run.TenantID, UserID: principal.UserID, RunID: run.ID, Name: rejectName,
			Decision: "rejected", ArtifactRevisionID: sql.NullString{}, Actor: string(authz.ActorHuman),
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
		logUnexpected(api.log, err, "RejectRun tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// conflictingGateDecision is returned inside the reject tx when a decision row
// already exists under the resolved gate name with a different decision. The
// handler surfaces it as a 409 rather than letting CreateApproval's
// ON CONFLICT DO NOTHING discard the reject.
type conflictingGateDecision struct {
	name     string
	existing string
}

func (conflict conflictingGateDecision) Error() string {
	return "gate " + conflict.name + " already decided " + conflict.existing
}

// handleInvocationApprove POST /api/v1/runs/{id}/invocations/{iid}/approve
// Final approval moves the run to done. The teardown job records result_commit
// and seals the manifest while retaining both local resources.
func (api *API) handleInvocationApprove(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunApprove, "GetRun(approve)")
	if !ok {
		return
	}
	if engine.RunState(run.State) != engine.StateAwaitingFinalReview {
		// Idempotency (ADR 0003 D4): a repeat approve on a run that already
		// reached done returns 200 with the current run when the recorded
		// final_review decision matches "approved"; a conflicting decision stays
		// a 409. A retried POST must be safe.
		if api.decisionIsIdempotent(w, r, run, approvalNameFinalReview, "approved") {
			return
		}
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"approve requires awaiting_final_review; run is "+run.State)
		return
	}
	next, err := engine.Next(engine.RunState(run.State), engine.EventApprove)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	// Transactional outbox: the done transition, the teardown-job enqueue, the
	// human-decision evidence, and the final_review approval row commit
	// atomically. result_commit capture happens inside the teardown job (the
	// runner owns the worktree manager). The
	// approval decision rides in the same tx as the transition it gates: a
	// crash between them cannot leave a run that advanced past final approval
	// with no record of who let it through.
	decision := gateDecisionPatch(run, principal, gateFinal, decisionApproved)
	var updated sqlc.Run
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			run: run, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordStrict,
		})
		if txErr != nil {
			return txErr
		}
		// ADR 0003 D4: record the durable final_review approval row in the same
		// tx. No bound artifact — final_review approves the run, not a document.
		if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
			TenantID: run.TenantID, UserID: principal.UserID, RunID: run.ID, Name: "final_review",
			Decision: "approved", ArtifactRevisionID: sql.NullString{}, Actor: string(authz.ActorHuman),
		}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
			return createErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the approval decision; the run was not advanced")
			return
		}
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// handleCancelRun POST /api/v1/runs/{id}/cancel
// Terminal abort: any non-terminal run → cancelled. The in-flight run (if any)
// is aborted via the cancel registry, then the FSM transition + teardown-job
// enqueue commit atomically. F.6.1: cancel is a terminal ABORT, distinct from
// pause (non-terminal) and cleanup (explicit branch deletion). Teardown keeps
// the worktree and branch available for review.
func (api *API) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunCancel, "GetRun(cancel)")
	if !ok {
		return
	}
	if engine.IsTerminal(engine.RunState(run.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "run is already terminal: "+run.State)
		return
	}

	next, ok := api.beginTerminalAbort(w, run)
	if !ok {
		return
	}
	// Transactional outbox: abort transition + teardown enqueue in one tx. The
	// cancel decision rides in the same tx under recordLenient: a sealed or
	// missing manifest (Init is best-effort, and a crash may have sealed the
	// manifest mid-flight) is absorbed so the cancel still lands — cancel is an
	// emergency exit and must be the most tolerant handler. A real write error
	// still fails the tx, which is correct: the transition did not commit.
	decision := gateDecisionPatch(run, principal, gateCancel, decisionRejected)
	var updated sqlc.Run
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			run: run, next: next, jobKind: jobKindTeardown,
			decision: decision, policy: recordLenient,
		})
		if txErr != nil {
			return txErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "CancelRun tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// jobKindTeardown is the job every terminal transition enqueues: it captures
// result_commit and seals the manifest without deleting local resources.
const jobKindTeardown = "teardown"

// lifecycleTransition is the shape every lifecycle write shares: the state the
// FSM lands in, the job that drives what happens next, and the human decision
// that authorized it. Grouping them keeps applyTransition's signature readable
// at the call sites, which differ only in these fields.
type lifecycleTransition struct {
	run  sqlc.Run
	next engine.RunState
	// jobKind is the driving job to enqueue ("teardown" for the terminal
	// transitions, the resume kind for continue/advance).
	jobKind string
	// jobPayload is the job body. Empty (or a literal JSON null, which is what
	// an absent request body decodes to) becomes "{}" — a job row always
	// carries a valid JSON object.
	jobPayload   []byte
	decision     manifest.Body
	policy       recordPolicy
	cancelReason string
}

// applyTransition performs the three writes every lifecycle transaction opens
// with — the FSM state update, the driving job enqueue, and the human-decision
// evidence — and returns the updated run row. It runs inside the caller's
// runInTx closure, so the decision commits atomically with the state change it
// describes; callers add whatever else belongs in their own transaction (an
// approval row, a conflicting-decision check) after it returns.
func (api *API) applyTransition(ctx context.Context, qtx *sqlc.Queries, principal authz.Principal, transition lifecycleTransition) (sqlc.Run, error) {
	jobPayload := transition.jobPayload
	if len(jobPayload) == 0 || string(jobPayload) == "null" {
		jobPayload = []byte("{}")
	}
	cancelReason := transition.cancelReason
	if transition.next == engine.StateCancelled && cancelReason == "" {
		cancelReason = "cancelled"
	}
	transitioned, transitionErr := qtx.UpdateRunState(ctx, sqlc.UpdateRunStateParams{
		ID: transition.run.ID, TenantID: principal.TenantID, State: string(transition.next), CancelReason: cancelReason,
	})
	if transitionErr != nil {
		return sqlc.Run{}, transitionErr
	}
	if _, enqueueErr := qtx.EnqueueJob(ctx, sqlc.EnqueueJobParams{
		TenantID: principal.TenantID, UserID: principal.UserID,
		RunID: transition.run.ID, Kind: transition.jobKind, Payload: jobPayload,
	}); enqueueErr != nil {
		return sqlc.Run{}, enqueueErr
	}
	if err := api.recordHumanDecisionTx(ctx, qtx, principal, transition.run.ID, transition.decision, transition.policy); err != nil {
		return sqlc.Run{}, err
	}
	return transitioned, nil
}

// beginTerminalAbort aborts the in-flight run and resolves the state EventCancel
// lands the run in. Shared by reject and cancel — the two terminal aborts,
// which differ in what they record, not in how they stop the run. The in-flight
// run is aborted first so the worker stops touching the run; the cancel
// registry reports false when no run is active (a paused run), which is fine.
// Writes the 409 itself, so ok=false means the handler must return.
func (api *API) beginTerminalAbort(w http.ResponseWriter, run sqlc.Run) (engine.RunState, bool) {
	if api.cancels != nil {
		api.cancels.Cancel(run.ID)
	}
	next, err := engine.Next(engine.RunState(run.State), engine.EventCancel)
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
// run_approvals row commit in one tx, so a resume can never leave the run
// running with no driver job and no record of who resumed it — nor an approved
// plan with no durable approval row. principal is threaded explicitly so this
// helper stays callable from handlers that already required it.
func (api *API) applyResume(r *http.Request, run sqlc.Run, event engine.RunEvent, kind string, payload []byte, decision manifest.Body, approval planApproval, principal authz.Principal) (sqlc.Run, error) {
	next, err := engine.Next(engine.RunState(run.State), event)
	if err != nil {
		return sqlc.Run{}, err
	}
	var updated sqlc.Run
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			run: run, next: next, jobKind: kind, jobPayload: payload,
			decision: decision, policy: recordStrict,
		})
		if txErr != nil {
			return txErr
		}
		// ADR 0003 D4: when this resume advances past the pack's approval stage,
		// the run_approvals row joins the same tx. CreateApproval's
		// ON CONFLICT DO NOTHING makes a repeated advance idempotent — a retried
		// POST that lost a race returns no rows, which we treat as "already
		// decided" rather than a failure.
		if approval.name != "" {
			revisionID, revErr := api.resolveApprovalRevisionID(r.Context(), qtx, run, approval)
			if revErr != nil {
				return revErr
			}
			if approval.withinStage && !revisionID.Valid {
				return staleRevisionError{expected: approval.expectedRevisionID, current: ""}
			}
			// Compared inside the tx, after applyTransition updated the run
			// row: an artifact write takes a share lock on that row, so any
			// plan revision either committed before this read or waits for
			// this tx. The answer therefore binds to the revision current at
			// commit — the handler's earlier read only picked 428 vs. 409.
			if approval.expectedRevisionID == "" && revisionID.Valid {
				return revisionPreconditionMissingError{current: revisionID.String}
			}
			if approval.expectedRevisionID != "" && revisionID.String != approval.expectedRevisionID {
				return staleRevisionError{expected: approval.expectedRevisionID, current: revisionID.String}
			}
			if approval.verifyOnly {
				updated = transitioned
				return nil
			}
			if _, createErr := qtx.CreateApproval(r.Context(), sqlc.CreateApprovalParams{
				TenantID: run.TenantID, UserID: principal.UserID, RunID: run.ID, Name: approval.name,
				Decision: "approved", ArtifactRevisionID: revisionID, Actor: string(authz.ActorHuman),
			}); createErr != nil && !errors.Is(createErr, sql.ErrNoRows) {
				return createErr
			}
		}
		updated = transitioned
		return nil
	}); err != nil {
		return sqlc.Run{}, err
	}
	return updated, nil
}

// resolveApprovalRevisionID reads the current revision id of the approval
// artifact (e.g. plan/plan.md) inside the resume tx, so the approval row binds
// to exactly the revision the human approved. An empty/missing revision yields
// a NULL revision id — the approval still records the decision; drift detection
// simply cannot fire without a bound revision.
func (api *API) resolveApprovalRevisionID(ctx context.Context, qtx *sqlc.Queries, run sqlc.Run, approval planApproval) (sql.NullString, error) {
	if api.art == nil {
		return sql.NullString{}, nil
	}
	revisionName := approval.stage + "/" + approval.artifact
	revision, err := qtx.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		RunID: run.ID, TenantID: run.TenantID, Name: revisionName,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.NullString{}, nil
		}
		return sql.NullString{}, err
	}
	return sql.NullString{String: revision.ID, Valid: true}, nil
}

// handleCleanupRun POST /api/v1/runs/{id}/cleanup
// Explicit branch deletion for a terminal run. A present worktree returns
// 409 conflict. The worker checks again before deleting the branch.
func (api *API) handleCleanupRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunCleanup, "GetRun(cleanup)")
	if !ok {
		return
	}
	// Cleanup is post-terminal only. A running/paused run's branch is live
	// delivery state — deleting it would destroy in-flight work.
	if !engine.IsTerminal(engine.RunState(run.State)) {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"cleanup requires a terminal run; run is "+run.State)
		return
	}
	project, projectErr := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: run.ProjectID, TenantID: run.TenantID})
	if projectErr != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, projectErr.Error())
		return
	}
	checkoutPath := run.CheckoutPath
	if checkoutPath == "" {
		checkoutPath = project.RepoPath
	}
	if _, statErr := os.Stat(worktree.PathFor(checkoutPath, run.ID)); statErr == nil {
		writeError(w, http.StatusConflict, codeConflict, "worktree still exists: delete the worktree first")
		return
	} else if !os.IsNotExist(statErr) {
		writeError(w, http.StatusInternalServerError, codeInternal, statErr.Error())
		return
	}
	if _, err := api.queries.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
		TenantID: principal.TenantID, UserID: principal.UserID, RunID: run.ID, Kind: "cleanup", Payload: []byte("{}"),
	}); err != nil {
		logUnexpected(api.log, err, "EnqueueJob(cleanup)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, toRunResponse(run))
}

// statusForTransition maps an engine/transition error to an HTTP response.
func statusForTransition(w http.ResponseWriter, err error) {
	var stale staleRevisionError
	if errors.As(err, &stale) {
		writeError(w, http.StatusConflict, codeConflict, stale.Error())
		return
	}
	var missing revisionPreconditionMissingError
	if errors.As(err, &missing) {
		writeError(w, http.StatusPreconditionRequired, codePreconditionMissing, missing.Error())
		return
	}
	var illegalErr *engine.ErrIllegalTransition
	if errors.As(err, &illegalErr) {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
}
