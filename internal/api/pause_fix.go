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
	"strings"
	"unicode/utf8"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
)

// handlePauseRun records a durable stop request while the current invocation finishes.
func (api *API) handlePauseRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunPause, "GetRun(pause)")
	if !ok {
		return
	}
	if engine.RunState(run.State) != engine.StateRunning {
		writeError(w, http.StatusConflict, codeIllegalTransition, "pause requires running; run is "+run.State)
		return
	}
	updated, err := api.queries.RequestRunPause(r.Context(), sqlc.RequestRunPauseParams{ID: run.ID, TenantID: principal.TenantID})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, codeIllegalTransition, "run is no longer running")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

type fixRequestBody struct {
	Text         string `json:"text"`
	ResultCommit string `json:"result_commit"`
}

// handleFixRequest stores a human finding and queues a new fixer invocation.
func (api *API) handleFixRequest(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunRequestFix, "GetRun(fix-request)")
	if !ok {
		return
	}
	if engine.RunState(run.State) != engine.StateAwaitingFinalReview {
		writeError(w, http.StatusConflict, codeIllegalTransition, "fix-request requires awaiting_final_review; run is "+run.State)
		return
	}
	if !api.requireArtifactStore(w) {
		return
	}
	bodyBytes, read := readRequestBody(w, r, taskinput.MaxContinueBodyBytes)
	if !read {
		return
	}
	if !utf8.Valid(bodyBytes) {
		writeError(w, http.StatusBadRequest, codeBadInput, "request body must be valid UTF-8")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	decoder.DisallowUnknownFields()
	var request fixRequestBody
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, codeBadInput, "request body must contain one JSON object")
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "text is required")
		return
	}
	if len(request.Text) > taskinput.MaxContinuationTextBytes {
		writeError(w, http.StatusBadRequest, codeBadInput,
			fmt.Sprintf("text exceeds %d bytes", taskinput.MaxContinuationTextBytes))
		return
	}
	if request.ResultCommit == "" || request.ResultCommit != nullStringOr(run.ResultCommit) {
		writeError(w, http.StatusConflict, codeConflict, "result_commit changed; review the current result")
		return
	}
	if scanErr := scanContinuationForCredentials(request.Text); scanErr != nil {
		writeRequestBodyError(w, scanErr)
		return
	}
	runPack, err := api.resolveRunPack(r.Context(), run)
	if err != nil || runPack == nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "could not resolve the run pack")
		return
	}
	fixerStage, found := firstFixerStage(runPack)
	if !found {
		writeError(w, http.StatusConflict, codeIllegalTransition, "run pack has no fixer stage")
		return
	}
	const artifactName = "final/fix-request.md"
	current, currentErr := api.art.Current(r.Context(), principal.TenantID, run.ID, artifactName)
	if currentErr != nil && !errors.Is(currentErr, artifacts.ErrNoCurrentRevision) {
		writeError(w, http.StatusInternalServerError, codeInternal, currentErr.Error())
		return
	}
	expected := ""
	if currentErr == nil {
		expected = current.ID
	}
	nextState, transitionErr := engine.Next(engine.RunState(run.State), engine.EventRequestFix)
	if transitionErr != nil {
		statusForTransition(w, transitionErr)
		return
	}
	decision := gateDecisionPatch(run, principal, gateFinal, decisionRequestedChanges)
	var updated sqlc.Run
	_, err = api.art.Put(r.Context(), artifacts.PutParams{
		TenantID: principal.TenantID, UserID: principal.UserID, RunID: run.ID,
		Name: artifactName, Kind: "human_fix_request", Bytes: []byte(request.Text),
		Actor: artifacts.ActorHuman, ExpectedCurrentRevision: expected,
		RequiredRunState: string(engine.StateAwaitingFinalReview),
		AfterRevision: func(ctx context.Context, queries *sqlc.Queries, revision artifacts.Revision) error {
			var reopenErr error
			updated, reopenErr = queries.ReopenFinalReviewForFix(ctx, sqlc.ReopenFinalReviewForFixParams{
				ID: run.ID, TenantID: principal.TenantID, CurrentStage: sql.NullString{String: fixerStage, Valid: true},
				ResultCommit: run.ResultCommit,
				FixRevisionID: sql.NullString{String: revision.ID, Valid: true}, NextState: string(nextState),
			})
			if reopenErr != nil {
				return reopenErr
			}
			_, enqueueErr := queries.EnqueueJob(ctx, sqlc.EnqueueJobParams{
				TenantID: principal.TenantID, UserID: principal.UserID, RunID: run.ID,
				Kind: "fix_request", Payload: []byte("{}"),
			})
			if enqueueErr != nil {
				return enqueueErr
			}
			return api.recordHumanDecisionTx(ctx, queries, principal, run.ID, decision, recordStrict)
		},
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusConflict, codeConflict, "result changed; review the current result")
		return
	}
	if err != nil {
		writeError(w, statusForArtifactStoreErr(err), codeForArtifactStoreErr(err), errForCaller(err))
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
}

func firstFixerStage(runPack *pack.Pack) (string, bool) {
	fixers := runPack.FixerStages()
	if len(fixers) == 0 {
		return "", false
	}
	return fixers[0], true
}
