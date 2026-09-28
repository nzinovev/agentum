package api

import (
	"database/sql"
	"errors"
	"net/http"

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
// Body: {"text": "remarks on this plan revision"} — required, non-blank,
// ≤ 32 KiB, credential-scanned (422 on a match), like every text that reaches
// a model. Valid only at the pack's source_write approval gate before the
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
	approvalPlan, atApproval := api.planApprovalForStage(r.Context(), run, currentStageOr(run.CurrentStage, ""))
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
	budget := api.packAskToEditBudget(r, run)
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
	remarks, parseErr := parseContinueBody(bodyBytes)
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

	decision := gateDecisionPatch(run, principal, gateAskToEdit, decisionRequestedChanges)
	updated, err := api.applyResume(r, run, engine.EventAskToEdit, jobKindAskToEdit, payload, decision, planApproval{}, principal)
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

// packAskToEditBudget resolves the pack's declared revision budget for the
// run. A pack that cannot be resolved reports 0 — the same fail-closed
// default planApprovalName uses for its name.
func (api *API) packAskToEditBudget(r *http.Request, run sqlc.Run) int {
	if api.packs == nil {
		return 0
	}
	runPack, err := api.packs.Resolve(r.Context(), run.PipelinePack)
	if err != nil {
		return 0
	}
	return runPack.Budgets.AskToEdit
}
