package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
)

// maxAskToEditBodyBytes shares the continue body budget: both carry prose a
// human wrote for the next model turn through the same Task-section channel.
const maxAskToEditBodyBytes = taskinput.MaxContinueBodyBytes

// The plan-gate Request-changes action. A remark on a plan has nowhere to go
// today: continue refuses paused_gate, so the only answers were approve or
// reject. ask-to-edit is the third answer — the planner's session resumes
// with the remarks rendered into the routing block's Task section, the
// revised plan becomes a new revision, and THAT revision needs its own
// approval. Nothing else about the run's request changes: title,
// description, and overrides are untouched, and the remarks never reach a
// stage other than the planner (the continuation text applies to the first
// resumed invocation alone, exactly as a continue's answer).

// gateAskToEdit and decisionRequestedChanges are the decision-vocabulary
// values the action records, same family as the lifecycle gates.
const (
	gateAskToEdit            = "ask_to_edit"
	decisionRequestedChanges = "requested_changes"
)

// jobKindAskToEdit is the driving job the action enqueues.
const jobKindAskToEdit = "ask_to_edit"

// handleInvocationAskToEdit POST /api/v1/runs/{id}/invocations/{iid}/ask-to-edit
// Body: {"text": "remarks on this plan revision", "target_revision_id":
// "<plan revision>"} — text required, non-blank, ≤ 32 KiB, credential-scanned
// (422 on a match), like every text that reaches a model; the revision id is
// required while the plan has a revision (428 without it, 409 when it is no
// longer current). Valid only at the pack's source_write approval gate before the
// first grant: after implementation unlocked, remarks are a rework decision
// this action does not make. The transition, the driving job, and the
// human-decision evidence commit atomically; a repeat POST after the
// transition sees running and 409s, so a retried click cannot double-fire.
func (api *API) handleInvocationAskToEdit(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunAskToEdit, "GetRun(ask-to-edit)")
	if !ok {
		return
	}
	if engine.RunState(run.State) != engine.StatePausedGate {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"ask-to-edit requires the plan gate (paused_gate); run is "+run.State)
		return
	}
	// Only the pack's source_write approval stage hosts the action, and only
	// before the first grant: an approval row with decision "approved" means
	// implementation unlocked, and remarks after that are a different
	// (future) rework decision, not a plan revision.
	approvalPlan, atApproval, approvalErr := api.planApprovalForStage(r.Context(), run, currentStageOr(run.CurrentStage, ""))
	if approvalErr != nil {
		logUnexpected(api.log, approvalErr, "resolveRunPack(ask-to-edit)")
		writeError(w, http.StatusInternalServerError, codeInternal, approvalErr.Error())
		return
	}
	if !atApproval {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"ask-to-edit applies to the pack's plan approval stage; current stage is "+currentStageOr(run.CurrentStage, ""))
		return
	}
	if existing, getErr := api.queries.GetApproval(r.Context(), sqlc.GetApprovalParams{
		TenantID: run.TenantID, RunID: run.ID, Name: approvalPlan.name,
	}); getErr == nil && existing.Decision == "approved" {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"the plan was already approved and source-write unlocked; remarks after implementation are a rework decision this action does not make")
		return
	} else if getErr != nil && !errors.Is(getErr, sql.ErrNoRows) {
		logUnexpected(api.log, getErr, "GetApproval(ask-to-edit)")
		writeError(w, http.StatusInternalServerError, codeInternal, getErr.Error())
		return
	}
	budget, budgetResolveErr := api.packAskToEditBudget(r, run)
	if budgetResolveErr != nil {
		logUnexpected(api.log, budgetResolveErr, "resolveRunPack(ask-to-edit budget)")
		writeError(w, http.StatusInternalServerError, codeInternal, budgetResolveErr.Error())
		return
	}
	spent, budgetErr := api.queries.CountJobsOfKindForRun(r.Context(), sqlc.CountJobsOfKindForRunParams{
		RunID: run.ID, TenantID: run.TenantID, Kind: jobKindAskToEdit,
	})
	if budgetErr != nil {
		logUnexpected(api.log, budgetErr, "CountJobsOfKindForRun(ask-to-edit)")
		writeError(w, http.StatusInternalServerError, codeInternal, budgetErr.Error())
		return
	}
	if budget <= 0 || int(spent) >= budget {
		writeError(w, http.StatusConflict, codeEditBudgetExhausted,
			"the run's plan revision budget is exhausted (ask_to_edit)")
		return
	}

	bodyBytes, read := readRequestBody(w, r, maxAskToEditBodyBytes)
	if !read {
		return
	}
	remarks, targetRevisionID, parseErr := parseAskToEditBody(bodyBytes)
	if parseErr != nil {
		writeRequestBodyError(w, parseErr)
		return
	}
	if remarks.Text == "" {
		writeError(w, http.StatusBadRequest, codeBadInput,
			"ask-to-edit requires non-empty text: the remarks the planner must address")
		return
	}
	payload, marshalErr := remarks.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape; a 500 keeps an invariant break from
		// being reported as the author's fault.
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}

	// The remarks answer one plan revision. Naming it is required while the
	// plan has one, and the resume tx re-checks it, so remarks written against
	// an older revision never re-run the planner over a newer one.
	approvalPlan.expectedRevisionID = targetRevisionID
	approvalPlan.verifyOnly = true
	if !api.requirePlanRevisionPrecondition(w, r, run, approvalPlan) {
		return
	}
	decision := gateDecisionPatch(run, principal, gateAskToEdit, decisionRequestedChanges)
	updated, err := api.applyResume(r, run, engine.EventAskToEdit, jobKindAskToEdit, payload, decision, approvalPlan, principal)
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the request-changes decision; the plan gate was not reopened")
			return
		}
		statusForTransition(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// parseAskToEditBody strictly decodes {"text", "target_revision_id"}: one
// JSON object, no other fields. The text goes through the continuation parser
// unchanged (raw bytes, so its UTF-8, budget, and credential checks see what
// the client sent), and becomes the job payload a continue would carry.
func parseAskToEditBody(body []byte) (taskinput.Continuation, string, error) {
	var fields struct {
		Text             json.RawMessage `json:"text"`
		TargetRevisionID string          `json:"target_revision_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return taskinput.Continuation{}, "", fmt.Errorf("ask-to-edit: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return taskinput.Continuation{}, "", errors.New("ask-to-edit: request body must contain exactly one JSON object")
	}
	textBody := []byte("{}")
	if len(fields.Text) != 0 {
		textBody = append(append([]byte(`{"text":`), fields.Text...), '}')
	}
	remarks, parseErr := parseContinueBody(textBody)
	if parseErr != nil {
		return taskinput.Continuation{}, "", parseErr
	}
	return remarks, strings.TrimSpace(fields.TargetRevisionID), nil
}

// packAskToEditBudget resolves the pack's declared revision budget for the
// run. A run that never started (no pinned pack) reports 0; a resolution
// failure is the caller's 500 — the budget decides whether an edit is still
// accepted, and a silent 0 would refuse every edit over a readable error.
func (api *API) packAskToEditBudget(r *http.Request, run sqlc.Run) (int, error) {
	runPack, err := api.resolveRunPack(r.Context(), run)
	if err != nil {
		return 0, err
	}
	if runPack == nil {
		return 0, nil
	}
	return runPack.Budgets.AskToEdit, nil
}
