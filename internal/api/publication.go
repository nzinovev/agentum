package api

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// jobKindPublish is the queue kind the publication coordinator serves — the
// same string the server's kind table maps to it.
const jobKindPublish = "publish"

// publicationResponse is the run's delivery state: the publication row's own
// state when a row exists, and otherwise one of the three values below. A run
// exists, so the answer is never a 404.
type publicationResponse struct {
	RunID           string                      `json:"run_id"`
	State           string                      `json:"state"`
	Provider        string                      `json:"provider,omitempty"`
	Target          *publicationTargetView      `json:"target,omitempty"`
	PublishedCommit string                      `json:"published_commit,omitempty"`
	PullRequest     *publicationPullRequestView `json:"pull_request,omitempty"`
	BranchPushedAt  string                      `json:"branch_pushed_at,omitempty"`
	PublishedAt     string                      `json:"published_at,omitempty"`
	Attempts        int32                       `json:"attempts,omitempty"`
	LastError       *publicationErrorView       `json:"last_error,omitempty"`
	CreatedAt       string                      `json:"created_at,omitempty"`
	UpdatedAt       string                      `json:"updated_at,omitempty"`
}

// The states the API adds to the row's own vocabulary, for the cases where no
// row state applies. Named constants because the UI branches on this value the
// same way it branches on an error code, and a closed set is what lets it.
const (
	// publicationStateDisabled: no row, and publication is switched off in
	// configuration. Nothing will be delivered until it is switched on.
	publicationStateDisabled = "disabled"
	// publicationStateNotAttempted: no row, publication is on, and the run
	// has not reached the gate that creates one.
	publicationStateNotAttempted = "not_attempted"
	// publicationStateUnavailable: the row could not be read. Distinct from
	// the two above because "nothing was recorded" and "we could not look"
	// are different claims, and a reader acting on the first one when the
	// second is true acts on a guess.
	publicationStateUnavailable = "unavailable"
)

// publicationTargetView names where the delivery goes. Host and path only —
// no URL a credential could be recovered from.
type publicationTargetView struct {
	Provider     string `json:"provider,omitempty"`
	Host         string `json:"host,omitempty"`
	Owner        string `json:"owner,omitempty"`
	Repository   string `json:"repository,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	RemoteBranch string `json:"remote_branch,omitempty"`
}

// publicationPullRequestView is the pull request the delivery produced.
type publicationPullRequestView struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
}

// publicationErrorView carries the refusal's reason code from the closed
// vocabulary plus the provider's message.
type publicationErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// handleGetPublication GET /api/v1/runs/{id}/publication
// Answers 200 for every existing run: the row's state when a publication
// exists, and otherwise "disabled", "not_attempted", or "unavailable" as the
// constants above define them. The handler reads Postgres only — no provider
// is contacted, so the answer costs one query.
func (api *API) handleGetPublication(w http.ResponseWriter, r *http.Request) {
	_, run, ok := api.requireRunForAction(w, r, authz.ActionRunRead, "GetRun(publication)")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.publicationView(r, run))
}

// handlePublishRun POST /api/v1/runs/{id}/publish
// Requests one publication attempt: 202 with the job enqueued, or 409 naming
// the precondition that failed — publication off in configuration, the run
// not in a deliverable state, or an attempt already in flight under a live
// lease. A recorded failure or blocked outcome is NOT a 409: the explicit
// retry is the action that follows from them.
func (api *API) handlePublishRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunPublish, "GetRun(publish)")
	if !ok {
		return
	}
	if !api.publication.enabled {
		writeError(w, http.StatusConflict, codePublicationDisabled,
			"publication is disabled; set the publication configuration to deliver runs")
		return
	}
	runState := engine.RunState(run.State)
	if runState != engine.StateAwaitingFinalReview && runState != engine.StateDone {
		writeError(w, http.StatusConflict, codePublicationNotReady,
			"publication requires awaiting_final_review or done; run is "+run.State)
		return
	}
	if row, err := api.queries.GetPublicationForRun(r.Context(), sqlc.GetPublicationForRunParams{
		TenantID: principal.TenantID, RunID: run.ID,
	}); err == nil && row.State == "publishing" && row.LeaseExpiresAt.Valid && row.LeaseExpiresAt.Time.After(time.Now()) {
		writeError(w, http.StatusConflict, codeConflict,
			"a publication attempt is already in flight for this run")
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logUnexpected(api.log, err, "GetPublicationForRun(publish)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}

	// The row (created here when missing — the gate may have run while
	// publication was off) and the job land in one transaction, so an enqueue
	// failure cannot leave a pending row no job will ever serve. The row's
	// user_id is the run's author, the same value the gate writes — not the
	// caller's, which is the same person today and stops being it with RBAC.
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		if _, ensureErr := qtx.EnsurePublication(r.Context(), sqlc.EnsurePublicationParams{
			TenantID: principal.TenantID, UserID: run.UserID, RunID: run.ID,
			Provider:        api.publication.provider,
			RemoteBranch:    branchForRun(run.ID),
			PublishedCommit: nullStringOr(run.ResultCommit),
		}); ensureErr != nil && !errors.Is(ensureErr, sql.ErrNoRows) {
			return ensureErr
		}
		_, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID,
			RunID: run.ID, Kind: jobKindPublish, Payload: []byte("{}"),
		})
		return enqueueErr
	}); err != nil {
		logUnexpected(api.log, err, "PublishRun tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, api.publicationView(r, run))
}

// publicationView assembles the response from durable state. The row is read
// FIRST and wins over the configuration: a publication that happened stays
// visible — its pull request, target, and times — after the operator switches
// publication off, because "delivered" is a fact about the run and "off" is a
// fact about what new runs will do.
//
// Only a missing row answers the absence form the configuration names. A read
// that did not complete answers unavailable instead — the same distinction the
// coordinator draws for a manifest it could not read, and for the same reason:
// a reader must be able to tell an absent publication from an unread one. The
// status stays 200 either way, because this block also rides inside the final
// review, and a failed read of the delivery state must not take the review
// down with it.
func (api *API) publicationView(r *http.Request, run sqlc.Run) publicationResponse {
	response := publicationResponse{RunID: run.ID, State: publicationStateNotAttempted}
	row, err := api.queries.GetPublicationForRun(r.Context(), sqlc.GetPublicationForRunParams{
		TenantID: principalTenant(r), RunID: run.ID,
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logUnexpected(api.log, err, "GetPublicationForRun(view)")
			response.State = publicationStateUnavailable
			return response
		}
		if !api.publication.enabled {
			response.State = publicationStateDisabled
		}
		return response
	}
	response.State = row.State
	response.Provider = row.Provider
	response.PublishedCommit = row.PublishedCommit
	response.Attempts = row.Attempts
	response.CreatedAt = row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	response.UpdatedAt = row.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	if row.TargetHost != "" || row.TargetOwner != "" || row.TargetRepository != "" || row.BaseBranch != "" || row.RemoteBranch != "" {
		response.Target = &publicationTargetView{
			Provider:     row.Provider,
			Host:         row.TargetHost,
			Owner:        row.TargetOwner,
			Repository:   row.TargetRepository,
			BaseBranch:   row.BaseBranch,
			RemoteBranch: row.RemoteBranch,
		}
	}
	if row.PrNumber.Valid {
		response.PullRequest = &publicationPullRequestView{
			Number: int(row.PrNumber.Int32),
			URL:    row.PrUrl.String,
			State:  row.PrState.String,
		}
	}
	response.BranchPushedAt = formatPublicationTime(row.BranchPushedAt)
	response.PublishedAt = formatPublicationTime(row.PublishedAt)
	if row.LastErrorCode.Valid {
		response.LastError = &publicationErrorView{
			Code:    row.LastErrorCode.String,
			Message: row.LastErrorMessage.String,
		}
	}
	return response
}

// formatPublicationTime renders a nullable timestamp the way the final-review
// payload renders its times, or "" when NULL.
func formatPublicationTime(when sql.NullTime) string {
	if !when.Valid {
		return ""
	}
	return when.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
}
