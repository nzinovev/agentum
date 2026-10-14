package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// jobKindRetry is the driving job a retry enqueues. The runner enters it the
// way it enters a run job — at the run's current stage with a fresh session —
// and the separate kind keeps the retry visible in the queue history.
const jobKindRetry = "retry"

// retryJobPayload carries the failure the retry answers. UpdateRunState
// clears runs.error on the way back to running, so the job row is where the
// cause stays attached to the decision that overrode it.
type retryJobPayload struct {
	PreviousError string `json:"previous_error"`
}

// retryIneligibleError carries the retry eligibility verdict out of the
// transaction closure, mapped to a 409 after rollback: writing the response
// inside the closure would speak before the transaction's fate is known.
type retryIneligibleError struct{ reason string }

func (ineligible retryIneligibleError) Error() string { return ineligible.reason }

// handleRetryRun POST /api/v1/runs/{id}/retry sends a failed run back into the
// pipeline. A failure keeps the worktree, branch, and checkpoints, so the run
// resumes from them rather than starting over: the runner re-enters the stage
// it failed in, or the pack entry when it failed before its first stage.
//
// Eligibility is checked under the run's row lock in the same transaction as
// the transition: the run is failed, no job is pending or running for it (a
// worktree discard or branch cleanup would race the resumed run), and its
// branch was not deleted (the checkpoints the retry resumes from live on it).
// The failure seal on the manifest is lifted in that transaction too, so the
// retry decision and every later evidence write land on the same record.
func (api *API) handleRetryRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunRetry, "GetRun(retry)")
	if !ok {
		return
	}
	next, err := engine.Next(engine.RunState(run.State), engine.EventRetry)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}
	var updated sqlc.Run
	err = api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		locked, lockErr := qtx.GetRunForUpdate(r.Context(), sqlc.GetRunForUpdateParams{
			ID: run.ID, TenantID: principal.TenantID,
		})
		if lockErr != nil {
			return lockErr
		}
		if _, nextErr := engine.Next(engine.RunState(locked.State), engine.EventRetry); nextErr != nil {
			return retryIneligibleError{nextErr.Error()}
		}
		if ineligibleErr := retryResourcesEligible(r, qtx, locked); ineligibleErr != nil {
			return ineligibleErr
		}
		// A run can fail before its manifest exists (Init is best-effort at
		// creation); the runner initializes it on the retried pass, so the
		// decision is recorded leniently in that one case only.
		policy := recordStrict
		if api.mfst != nil {
			reopenErr := api.mfst.ReopenFailedTx(r.Context(), qtx, principal.TenantID, locked.ID)
			switch {
			case errors.Is(reopenErr, manifest.ErrNoManifest):
				policy = recordLenient
			case reopenErr != nil:
				return reopenErr
			}
		}
		payload, marshalErr := json.Marshal(retryJobPayload{PreviousError: locked.Error})
		if marshalErr != nil {
			return marshalErr
		}
		transitioned, txErr := api.applyTransition(r.Context(), qtx, principal, lifecycleTransition{
			run: locked, next: next, jobKind: jobKindRetry, jobPayload: payload,
			decision: gateDecisionPatch(locked, principal, gateRetry, decisionRetried), policy: policy,
		})
		if txErr != nil {
			return txErr
		}
		updated = transitioned
		return nil
	})
	if err != nil {
		var ineligible retryIneligibleError
		switch {
		case errors.As(err, &ineligible):
			writeError(w, http.StatusConflict, codeIllegalTransition, ineligible.Error())
		case errors.Is(err, manifest.ErrSealed), errors.Is(err, manifest.ErrCorrected):
			writeError(w, http.StatusConflict, codeConflict, "the run's evidence manifest cannot be reopened: "+err.Error())
		case isHumanDecisionRecordFailure(err):
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the retry decision; the run was not retried")
		default:
			logUnexpected(api.log, err, "RetryRun tx")
			writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// retryResourcesEligible refuses a retry the local resources cannot carry: an
// unfinished job would act on the tree the resumed run is about to use, and a
// branch-cleanup request that was not refused means the branch holding the
// run's checkpoints is gone or going. The read API derives branch_state from
// the same cleanup job, so the UI hides Retry exactly when this refuses.
func retryResourcesEligible(r *http.Request, qtx *sqlc.Queries, run sqlc.Run) error {
	unfinished, countErr := qtx.CountUnfinishedJobsForRunExcluding(r.Context(), sqlc.CountUnfinishedJobsForRunExcludingParams{
		RunID: run.ID, TenantID: run.TenantID, ID: 0,
	})
	if countErr != nil {
		return countErr
	}
	if unfinished > 0 {
		return retryIneligibleError{"the run has unfinished jobs; retry requires a run with no active job"}
	}
	cleanup, cleanupErr := qtx.LatestResourceJobForRun(r.Context(), sqlc.LatestResourceJobForRunParams{
		RunID: run.ID, TenantID: run.TenantID, UserID: run.UserID, Kind: "cleanup",
	})
	if cleanupErr != nil && !errors.Is(cleanupErr, sql.ErrNoRows) {
		return cleanupErr
	}
	if cleanupErr == nil && cleanup.Status != "failed" {
		return retryIneligibleError{"the run branch was deleted; retry has no checkpoints to resume from"}
	}
	return nil
}
