package api

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Human gate decision values. Centralized so the spelling callers, the
// manifest, and the docs agree on. HumanDecision.Decision carries these.
const (
	decisionApproved  = "approved"
	decisionRejected  = "rejected"
	decisionEdited    = "edited"
	decisionContinued = "continued"
)

// Gate identifiers recorded on a HumanDecision. These name the kind of gate the
// decision applied to; the diff/audit surface reads them, so they are stable.
const (
	gateAdvance       = "advance"
	gateFinal         = "final"
	gateOpenQuestions = "open_questions"
	gateUserStop      = "user_stop"
	gateCancel        = "cancel"
	gateHumanEdit     = "human_edit"
)

// humanDecisionPatch builds the manifest.Body patch that records one human
// gate decision. It is a pure function over its inputs so the gate/decision
// mapping for each lifecycle action is unit-testable without a database or an
// HTTP harness. The caller is responsible for committing it atomically with
// the state change it describes (via manifest.Service.AddEvidenceTx inside the
// handler's runInTx closure).
func humanDecisionPatch(stage, gate, decision, actor string, at time.Time) manifest.Body {
	return manifest.Body{
		HumanGates: []manifest.HumanDecision{{
			Stage:     stage,
			Gate:      gate,
			Decision:  decision,
			Actor:     actor,
			Timestamp: at,
		}},
	}
}

// currentStageOr returns the task's current stage id, or fallback when the task
// has no current stage set (a task that has not entered a stage yet). Used by
// the lifecycle handlers to fill the Stage field of a HumanDecision.
func currentStageOr(stage sql.NullString, fallback string) string {
	if stage.Valid && stage.String != "" {
		return stage.String
	}
	return fallback
}

// recordHumanDecisionTx writes a human-decision patch into the manifest using
// the caller's transaction (the same runInTx the FSM transition + enqueue run
// in), so the decision commits atomically with the state change it describes. A
// nil manifest service (unit tests, or a server that did not wire one) is a
// no-op — the lifecycle action still proceeds, matching the read handlers that
// also tolerate a nil manifest service.
//
// The cancel path treats ErrSealed / ErrNoManifest as non-fatal (a cancel on a
// task whose manifest sealed during a crash is legitimate); the advance/approve
// paths must fail the whole request on any non-fatal-class error, because a
// gate passed with no recorded decision defeats the gate's purpose. The caller
// distinguishes the two via isHumanDecisionRecordFailure.
func (api *API) recordHumanDecisionTx(
	ctx context.Context,
	qtx *sqlc.Queries,
	principal authz.Principal,
	taskID string,
	decision manifest.Body,
) error {
	if api.mfst == nil {
		return nil
	}
	if err := api.mfst.AddEvidenceTx(ctx, qtx, principal.TenantID, taskID, decision); err != nil {
		return humanDecisionRecordError{cause: err}
	}
	return nil
}

// humanDecisionRecordError wraps any failure from recordHumanDecisionTx so the
// handlers can distinguish "the decision could not be recorded" (which must
// fail the request for gate actions) from a plain store error. ErrSealed and
// ErrNoManifest are unpacked separately by the handlers via errors.Is.
type humanDecisionRecordError struct{ cause error }

func (recordErr humanDecisionRecordError) Error() string { return recordErr.cause.Error() }
func (recordErr humanDecisionRecordError) Unwrap() error { return recordErr.cause }

// isHumanDecisionRecordFailure reports whether err is a record failure that is
// NOT one of the cancel-acceptable cases (sealed / no manifest). Gate actions
// (advance, approve, continue) use this to decide whether to fail the request.
func isHumanDecisionRecordFailure(err error) bool {
	var recordErr humanDecisionRecordError
	if !errors.As(err, &recordErr) {
		return false
	}
	// Sealed / no-manifest are acceptable for cancel; a gate action treats
	// them as failures too (the decision must be on the record), but they are
	// not internal errors — surface them as the same internal status the
	// caller already uses for "could not record the decision".
	return true
}
