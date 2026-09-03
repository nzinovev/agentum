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

// Gate decision values. Centralized so the spelling callers, the
// manifest, and the docs agree on. GateDecision.Decision carries these.
const (
	decisionApproved  = "approved"
	decisionRejected  = "rejected"
	decisionEdited    = "edited"
	decisionContinued = "continued"
)

// Gate identifiers recorded on a GateDecision. These name the kind of gate the
// decision applied to; the diff/audit surface reads them, so they are stable
// across releases. They are GATE LABELS (the audit vocabulary), distinct from
// the task_approvals.name key (the durable approval namespace) — the two must
// not be conflated. gateFinal stays "final" to match the pre-existing gate
// vocabulary; the approval row for the same action is "final_review"
// (approvalNameFinalReview in lifecycle.go).
const (
	gateAdvance       = "advance"
	gateFinal         = "final"
	gateReject        = "reject" // ADR 0003 D4: human reject at a gate
	gateOpenQuestions = "open_questions"
	gateUserStop      = "user_stop"
	gateCancel        = "cancel"
	gateHumanEdit     = "human_edit"
)

// humanDecisionPatch builds the manifest.Body patch that records one human
// gate decision: actor is the shared vocabulary value (a human acted here —
// the system's own gate passes are recorded by the runner with
// authz.ActorSystem), userID is the person whose name the decision was taken
// under. A pure function over its inputs so the gate/decision mapping for each
// lifecycle action is unit-testable without a database or an HTTP harness. The
// caller is responsible for committing it atomically with the state change it
// describes (via manifest.Service.AddEvidenceTx inside the handler's runInTx
// closure).
func humanDecisionPatch(stage, gate, decision, userID string, at time.Time) manifest.Body {
	return manifest.Body{
		GateDecisions: []manifest.GateDecision{{
			Stage:     stage,
			Gate:      gate,
			Decision:  decision,
			Actor:     string(authz.ActorHuman),
			UserID:    userID,
			Timestamp: at,
		}},
	}
}

// gateDecisionPatch is the humanDecisionPatch every lifecycle handler builds:
// the task's current stage, the gate and decision the handler stands for, the
// acting principal, and now. Only the gate and the decision vary between
// handlers, so they are the only things a call site spells out.
func gateDecisionPatch(task sqlc.Task, principal authz.Principal, gate, decision string) manifest.Body {
	return humanDecisionPatch(currentStageOr(task.CurrentStage, ""), gate, decision, principal.UserID, time.Now().UTC())
}

// currentStageOr returns the task's current stage id, or fallback when the task
// has no current stage set (a task that has not entered a stage yet). Used by
// the lifecycle handlers to fill the Stage field of a GateDecision.
func currentStageOr(stage sql.NullString, fallback string) string {
	if stage.Valid && stage.String != "" {
		return stage.String
	}
	return fallback
}

// recordPolicy is how strictly a lifecycle handler treats a manifest write
// failure when recording a human decision. The asymmetry is load-bearing: a
// gate action (advance, approve, continue) must fail the request if the
// decision cannot land on the record — the whole point of the gate is the
// record. Cancel is an emergency exit and must be the most tolerant handler:
// cancelling a task whose manifest sealed during a crash, or was never
// initialized (Init is best-effort), is legitimate, so a sealed or missing
// manifest is absorbed there rather than blocking the cancel.
type recordPolicy int

const (
	// recordStrict fails the transaction on any write error, including
	// sealed/missing. Used by advance/approve/continue.
	recordStrict recordPolicy = iota
	// recordLenient absorbs ErrSealed / ErrNoManifest (returns nil so runInTx
	// commits the transition without the decision); any other write error still
	// fails. Used by cancel.
	recordLenient
)

// recordHumanDecisionTx writes a human-decision patch into the manifest using
// the caller's transaction (the same runInTx the FSM transition + enqueue run
// in), so the decision commits atomically with the state change it describes.
// A nil manifest service (unit tests, a server that did not wire one) is a
// no-op. Under recordLenient, a sealed or missing manifest is absorbed — the
// caller still sees nil and the transition commits without the decision.
func (api *API) recordHumanDecisionTx(
	ctx context.Context,
	qtx *sqlc.Queries,
	principal authz.Principal,
	taskID string,
	decision manifest.Body,
	policy recordPolicy,
) error {
	if api.mfst == nil {
		return nil
	}
	err := api.mfst.AddEvidenceTx(ctx, qtx, principal.TenantID, taskID, decision)
	if err == nil {
		return nil
	}
	if policy == recordLenient && (errors.Is(err, manifest.ErrSealed) || errors.Is(err, manifest.ErrNoManifest)) {
		// A cancel on a task whose manifest sealed during a crash, or whose
		// Init failed (best-effort at task creation), is legitimate. Absorb it
		// so the transition commits; the artifact/revision rows remain the
		// durable record, and evidence_complete will honestly report the gap.
		return nil
	}
	return humanDecisionRecordError{cause: err}
}

// humanDecisionRecordError wraps a manifest write failure so the handlers can
// distinguish "the decision could not be recorded" (which must fail a gate
// action) from a plain store error returned by the transition/enqueue.
type humanDecisionRecordError struct{ cause error }

func (recordErr humanDecisionRecordError) Error() string { return recordErr.cause.Error() }
func (recordErr humanDecisionRecordError) Unwrap() error { return recordErr.cause }

// isHumanDecisionRecordFailure reports whether err came from
// recordHumanDecisionTx rather than the transition/enqueue. Gate actions use it
// to surface a recording failure as "could not record the decision" rather than
// a generic 500, so a reviewer knows the transition did not advance.
func isHumanDecisionRecordFailure(err error) bool {
	var recordErr humanDecisionRecordError
	return errors.As(err, &recordErr)
}
