package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
)

// Worktree recovery and disposal actions. Both exist because the runner no
// longer destroys work on its own: a crashed run's dirty tree pauses for an
// explicit human decision (reconcile), and a stopped or terminal run's working
// tree is removed only by an audited, confirmed request (discard). Neither
// action deletes the agentum/<run-id> branch — cleanup keeps that verb.

// Gate labels and decision values the two handlers record. Same vocabulary
// family as the lifecycle gates in human_decision.go.
const (
	gateWorktreeReconcile = "worktree_reconcile"
	gateWorktreeDiscard   = "worktree_discard"
)

// maxWorktreeBodyBytes caps the reconcile/discard request bodies. Both carry
// three fixed-size fields; the cap is transport hygiene only.
const maxWorktreeBodyBytes = 4096

// handleWorktreeReconcile POST /api/v1/runs/{id}/worktree/reconcile
// Resolve a worktree_uncommitted_changes pause: the body names the mode
// (resume_session | keep_as_checkpoint | discard_to_checkpoint) and the HEAD
// the decision applies to. The transition (paused_user_stop → running), the
// driving job, and the human-decision evidence commit atomically, exactly as a
// continue does; the runner then verifies the HEAD and applies the mode before
// any invocation runs. A malformed or unconfirmed body is refused before the
// transition, so the pause survives a bad request untouched.
func (api *API) handleWorktreeReconcile(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunReconcile, "GetRun(reconcile)")
	if !ok {
		return
	}
	if engine.RunState(run.State) != engine.StatePausedUserStop {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"reconcile requires paused_user_stop (the worktree_uncommitted_changes stop); run is "+run.State)
		return
	}
	if run.StopReason != "worktree_uncommitted_changes" {
		writeError(w, http.StatusConflict, codeIllegalTransition, "reconcile requires worktree_uncommitted_changes; stop reason is "+run.StopReason)
		return
	}
	bodyBytes, read := readRequestBody(w, r, maxWorktreeBodyBytes)
	if !read {
		return
	}
	decision, parseErr := taskinput.ParseReconcileDecision(bodyBytes)
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, parseErr.Error())
		return
	}
	if !api.requireCurrentWorktree(w, r, run, decision.ExpectedHead, false, false) {
		return
	}
	payload, marshalErr := decision.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape; a 500 keeps an invariant break from
		// being reported as the author's fault.
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}
	decisionPatch := gateDecisionPatch(run, principal, gateWorktreeReconcile, decision.Mode)
	updated, err := api.applyResume(r, run, engine.EventContinue, "reconcile", payload, decisionPatch, planApproval{}, principal)
	if err != nil {
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the reconcile decision; the run was not resumed")
			return
		}
		statusForTransition(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

// discardWorktreeRequest is the typed body of the discard-worktree endpoint.
// ExpectedHead is the worktree HEAD the human confirmed: the runner refuses to
// remove a tree that moved since. DiscardUncommitted is the explicit
// confirmation that uncommitted files may be destroyed; the runner demands it
// whenever the tree is actually dirty.
type discardWorktreeRequest struct {
	ExpectedHead       string `json:"expected_head"`
	DiscardUncommitted bool   `json:"discard_uncommitted"`
	DiscardUnreadable  bool   `json:"discard_unreadable"`
}

// parseDiscardWorktreeBody strictly decodes the discard request: exactly one
// JSON object, no unknown fields, a full-length expected_head SHA. The
// uncommitted-loss flag is validated against the real tree state by the
// runner, not here — dirtiness is a git fact the API boundary cannot see.
func parseDiscardWorktreeBody(body []byte) (discardWorktreeRequest, error) {
	var req discardWorktreeRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return discardWorktreeRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return discardWorktreeRequest{}, errors.New("request body must contain exactly one JSON object")
	}
	if req.DiscardUnreadable {
		if req.ExpectedHead != "" || !req.DiscardUncommitted {
			return discardWorktreeRequest{}, errors.New("discard_unreadable requires an empty expected_head and discard_uncommitted: true")
		}
		return req, nil
	}
	if !taskinput.IsFullCommitSHA(req.ExpectedHead) {
		return discardWorktreeRequest{}, errors.New("expected_head must be the full commit SHA of the worktree being discarded")
	}
	return req, nil
}

// handleWorktreeDiscard POST /api/v1/runs/{id}/worktree/discard
// Remove a run's working tree — the tree only, never the branch. Valid for a
// terminal run, or for an explicitly stopped run with no other unfinished job
// (the state/jobs check runs under a row lock in the same transaction as the
// enqueue, so a lifecycle action racing this request cannot interleave). The
// runner re-checks every precondition at execution time and additionally
// verifies the HEAD and the uncommitted-loss confirmation before removing
// anything. Audited through the recorded human decision and the
// run.worktree_discarded event.
func (api *API) handleWorktreeDiscard(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunDiscardWorktree, "GetRun(discard-worktree)")
	if !ok {
		return
	}
	state := engine.RunState(run.State)
	if !engine.IsTerminal(state) && !engine.IsPaused(state) {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"discard requires a terminal or paused run without an active job; run is "+run.State)
		return
	}
	bodyBytes, read := readRequestBody(w, r, maxWorktreeBodyBytes)
	if !read {
		return
	}
	request, parseErr := parseDiscardWorktreeBody(bodyBytes)
	if parseErr != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, parseErr.Error())
		return
	}
	if request.DiscardUnreadable && !engine.IsTerminal(state) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "discard_unreadable requires a terminal run")
		return
	}
	if request.DiscardUnreadable {
		if !api.requireUnreadableWorktree(w, r, run) {
			return
		}
	} else {
		if !api.requireCurrentWorktree(w, r, run, request.ExpectedHead, true, request.DiscardUncommitted) {
			return
		}
	}
	payload, marshalErr := json.Marshal(request)
	if marshalErr != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}
	var enqueued bool
	err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		// Lock the run row so a concurrent lifecycle write (continue, cancel,
		// advance) serializes against this eligibility check instead of
		// interleaving: the FSM guards those transitions, but this endpoint
		// changes no run state, so without the lock its read could race a
		// transition that makes the tree live again.
		locked, lockErr := qtx.GetRunForUpdate(r.Context(), sqlc.GetRunForUpdateParams{
			ID: run.ID, TenantID: principal.TenantID,
		})
		if lockErr != nil {
			return lockErr
		}
		lockedState := engine.RunState(locked.State)
		if !engine.IsTerminal(lockedState) && !engine.IsPaused(lockedState) {
			return discardIneligibleError{"discard requires a terminal or paused run without an active job; run is " + locked.State}
		}
		unfinished, countErr := qtx.CountUnfinishedJobsForRunExcluding(r.Context(), sqlc.CountUnfinishedJobsForRunExcludingParams{
			RunID: run.ID, TenantID: principal.TenantID, ID: 0,
		})
		if countErr != nil {
			return countErr
		}
		if unfinished > 0 {
			return discardIneligibleError{"the run has unfinished jobs; discard requires a run with no active job"}
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID,
			RunID: run.ID, Kind: "discard_worktree", Payload: payload,
		}); enqueueErr != nil {
			return enqueueErr
		}
		if recordErr := api.recordHumanDecisionTx(r.Context(), qtx, principal, run.ID,
			gateDecisionPatch(run, principal, gateWorktreeDiscard, "discarded"), recordStrict); recordErr != nil {
			return recordErr
		}
		enqueued = true
		return nil
	})
	if err != nil {
		var ineligible discardIneligibleError
		if errors.As(err, &ineligible) {
			writeError(w, http.StatusConflict, codeIllegalTransition, ineligible.Error())
			return
		}
		if isHumanDecisionRecordFailure(err) {
			writeError(w, http.StatusInternalServerError, codeInternal,
				"could not record the discard decision; nothing was enqueued")
			return
		}
		logUnexpected(api.log, err, "DiscardWorktree tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	if !enqueued {
		writeError(w, http.StatusInternalServerError, codeInternal, "discard was not enqueued")
		return
	}
	writeJSON(w, http.StatusAccepted, toRunResponse(run))
}

// discardIneligibleError carries the discard eligibility verdict out of the
// transaction closure, mapped to a 409 after commit-or-rollback: writing the
// HTTP response inside the closure would speak before the tx's fate is known.
type discardIneligibleError struct{ reason string }

func (ineligible discardIneligibleError) Error() string { return ineligible.reason }

// requireCurrentWorktree checks the HEAD and dirty-file confirmation before
// queueing a destructive action. The worker checks again before applying it.
func (api *API) requireCurrentWorktree(w http.ResponseWriter, r *http.Request, run sqlc.Run, expectedHead string, checkDirty, confirmDirty bool) bool {
	project, err := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: run.ProjectID, TenantID: run.TenantID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return false
	}
	checkoutPath := run.CheckoutPath
	if checkoutPath == "" {
		checkoutPath = project.RepoPath
	}
	wtRoot := worktree.PathFor(checkoutPath, run.ID)
	if !worktree.DirPresent(wtRoot) {
		writeError(w, http.StatusConflict, codeConflict, "worktree is no longer present; reload the run")
		return false
	}
	manager := worktree.New()
	head, headErr := manager.HeadCommit(r.Context(), wtRoot)
	if headErr != nil {
		writeError(w, http.StatusConflict, codeConflict, "worktree HEAD could not be read; reload the run")
		return false
	}
	if head != expectedHead {
		writeError(w, http.StatusConflict, codeConflict, "worktree HEAD changed; reload the run before deciding")
		return false
	}
	if !checkDirty {
		return true
	}
	changes, changesErr := manager.WorktreeChanges(r.Context(), wtRoot)
	if changesErr != nil {
		writeError(w, http.StatusConflict, codeConflict, "worktree changes could not be read; reload the run")
		return false
	}
	if len(changes) > 0 && !confirmDirty {
		writeError(w, http.StatusBadRequest, codeBadInput, "worktree holds uncommitted paths; confirm their loss")
		return false
	}
	return true
}

// requireUnreadableWorktree accepts the separately confirmed recovery path only
// while the run's worktree directory exists and its HEAD cannot be read.
func (api *API) requireUnreadableWorktree(w http.ResponseWriter, r *http.Request, run sqlc.Run) bool {
	project, err := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: run.ProjectID, TenantID: run.TenantID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return false
	}
	checkoutPath := run.CheckoutPath
	if checkoutPath == "" {
		checkoutPath = project.RepoPath
	}
	wtRoot := worktree.PathFor(checkoutPath, run.ID)
	info, statErr := os.Lstat(wtRoot)
	if statErr != nil || !info.IsDir() {
		writeError(w, http.StatusConflict, codeConflict, "worktree directory is no longer present; reload the run")
		return false
	}
	if worktree.DirPresent(wtRoot) {
		if _, headErr := worktree.New().HeadCommit(r.Context(), wtRoot); headErr == nil {
			writeError(w, http.StatusConflict, codeConflict, "worktree HEAD is readable; reload the run and confirm its current HEAD")
			return false
		}
	}
	return true
}
