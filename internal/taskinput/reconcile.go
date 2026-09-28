package taskinput

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// ReconcileMode values: how a human resolved uncommitted work the runner found
// in a crashed run's worktree. The vocabulary is closed because the runner
// branches on it and the API echoes it in evidence — a free-form string could
// not carry a guarantee that every mode has a defined handling.
const (
	// ReconcileResumeSession leaves the working tree exactly as the captured
	// session left it and resumes that session: the uncommitted state is part
	// of what the session was doing, so resuming onto it is coherent.
	ReconcileResumeSession = "resume_session"
	// ReconcileKeepAsCheckpoint commits the uncommitted working tree on the
	// run branch as an orchestrator-authored checkpoint before resuming.
	ReconcileKeepAsCheckpoint = "keep_as_checkpoint"
	// ReconcileDiscardToCheckpoint resets the working tree (and removes
	// untracked files) back to the last recorded checkpoint before resuming.
	// Destructive, so it always requires ConfirmUncommittedLoss.
	ReconcileDiscardToCheckpoint = "discard_to_checkpoint"
)

// commitShape matches the two full-length SHA forms git emits (SHA-1 and
// SHA-256). Used to refuse a malformed expected_head before it reaches git.
var commitShape = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// IsFullCommitSHA reports whether sha is a full-length commit hash. Shared by
// the HTTP boundary (a fast refusal of a malformed body) and the reconcile
// decision typing, so the two never disagree about what a HEAD looks like.
func IsFullCommitSHA(sha string) bool {
	return commitShape.MatchString(sha)
}

// ReconcileDecision is the typed body of a worktree-reconcile request: the
// human's explicit choice over a worktree the runner paused on because it held
// uncommitted changes. One contract for both sides of the job queue: the API
// stores it as the reconcile job's payload, and the runner decodes the same
// bytes before touching the tree — the exact shape Continuation uses for
// continue jobs, so the two user-driven job kinds stay symmetrical.
type ReconcileDecision struct {
	// Mode is one of the Reconcile* constants.
	Mode string `json:"mode"`
	// ExpectedHead is the worktree HEAD the human acted on. The runner
	// refuses the decision when HEAD moved in between — applying a choice to
	// a different tree than the one it was made about is how unreviewed work
	// gets destroyed or blessed.
	ExpectedHead string `json:"expected_head"`
	// ConfirmUncommittedLoss is the explicit confirmation for the destructive
	// mode. Required for ReconcileDiscardToCheckpoint; ignored otherwise.
	ConfirmUncommittedLoss bool `json:"confirm_uncommitted_loss"`
}

// ParseReconcileDecision strictly decodes reconcile-request bytes: exactly one
// JSON object, no unknown fields, a known mode, a full-length expected_head
// SHA, and the confirmation flag when the mode destroys uncommitted work. The
// body is small by construction (three fixed fields), so the transport cap the
// HTTP boundary applies elsewhere is the only size check needed.
func ParseReconcileDecision(body []byte) (ReconcileDecision, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ReconcileDecision{}, errors.New("reconcile: a decision body is required")
	}
	var fields struct {
		Mode                   string `json:"mode"`
		ExpectedHead           string `json:"expected_head"`
		ConfirmUncommittedLoss bool   `json:"confirm_uncommitted_loss"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return ReconcileDecision{}, fmt.Errorf("reconcile: %w", err)
	}
	// Exactly one JSON value: a second object after the first must not be
	// silently dropped.
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ReconcileDecision{}, errors.New("reconcile: request body must contain exactly one JSON object")
	}
	switch fields.Mode {
	case ReconcileResumeSession, ReconcileKeepAsCheckpoint, ReconcileDiscardToCheckpoint:
	default:
		return ReconcileDecision{}, fmt.Errorf(
			"reconcile: mode must be one of %s, %s, %s", ReconcileResumeSession, ReconcileKeepAsCheckpoint, ReconcileDiscardToCheckpoint)
	}
	if !IsFullCommitSHA(fields.ExpectedHead) {
		return ReconcileDecision{}, errors.New("reconcile: expected_head must be the full commit SHA the decision applies to")
	}
	if fields.Mode == ReconcileDiscardToCheckpoint && !fields.ConfirmUncommittedLoss {
		return ReconcileDecision{}, errors.New("reconcile: discard_to_checkpoint requires confirm_uncommitted_loss: true")
	}
	return ReconcileDecision{
		Mode:                   fields.Mode,
		ExpectedHead:           fields.ExpectedHead,
		ConfirmUncommittedLoss: fields.ConfirmUncommittedLoss,
	}, nil
}

// Marshal returns the canonical job-payload JSON. Every field is always set
// (parse refuses anything less), so the payload is never the empty object a
// textless continue job carries.
func (decision ReconcileDecision) Marshal() ([]byte, error) {
	return json.Marshal(decision)
}
