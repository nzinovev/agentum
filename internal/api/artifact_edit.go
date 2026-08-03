package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Repeated artifact-edit messages, as constants so the wording callers and
// logs match on cannot drift.
const (
	msgArtifactEditStoreNotConfigured = "artifact store not configured"
	msgArtifactEditRequiresContent    = "content is required"
	msgArtifactEditPrecondition       = "expected_revision_id is required when the artifact already has a revision; resend with the current revision id from GET"
)

// artifactEditRequest is the PUT body for a human artifact edit. kind is
// optional (defaults to the prior revision's kind, or "file" for a create).
// expected_revision_id is the optimistic-concurrency precondition: when the
// artifact already has a current revision, it must name that revision, so two
// editors racing produce a 409 for the loser rather than a silent lost update.
// It may be empty only on a first create, when there is no current revision.
type artifactEditRequest struct {
	Content            string `json:"content"`
	Kind               string `json:"kind,omitempty"`
	ExpectedRevisionID string `json:"expected_revision_id,omitempty"`
}

// handleArtifactGet GET /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}
// Returns the current revision of (task, name) plus its content. The revision
// id is surfaced in the X-Revision-Id response header so a client can use it as
// the expected_revision_id precondition for a subsequent PUT.
func (api *API) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	taskID := r.PathValue("id")
	if decision := authz.Can(r.Context(), principal, authz.ActionTaskRead, taskID); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	if api.art == nil {
		writeError(w, http.StatusNotFound, codeNotFound, msgArtifactEditStoreNotConfigured)
		return
	}
	name := r.PathValue("name")
	revision, err := api.art.Current(r.Context(), principal.TenantID, taskID, name)
	if err != nil {
		writeError(w, statusForArtifactStoreErr(err), codeForArtifactStoreErr(err), errForCaller(err))
		return
	}
	w.Header().Set("Content-Type", contentTypeForKind(revision.Kind))
	w.Header().Set("X-Revision-Id", revision.ID)
	w.Header().Set("X-Content-Hash", revision.ContentHash)
	w.Header().Set("ETag", `"`+revision.ContentHash+`"`)
	if _, copyErr := api.art.CopyTo(r.Context(), principal.TenantID, revision.ID, w); copyErr != nil {
		// Header already written; the best we can do is log.
		api.log.Warn("stream artifact edit content", "revision", revision.ID, "error", copyErr)
		return
	}
}

// handleArtifactPut PUT /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}
// Creates a new revision from the request body. A human edit has no source
// invocation (actor = human), unlike a stage capture. Because the edit IS the
// approval at a human_edit gate, a successful PUT also records a
// HumanDecision{decision: "edited"} on the manifest — a plain AddEvidence, since
// this handler performs no FSM transition and needs no shared transaction.
//
// Precondition policy: when the artifact already has a current revision, the
// request MUST carry expected_revision_id naming it. This prevents a blind
// overwrite of an existing artifact by a PUT that did not first GET the
// revision it is replacing. A create (no current revision yet) needs no
// precondition. The two race outcomes are therefore: two creates collide → the
// store's unique index rejects the loser with ErrRevisionConflict (409); two
// edits collide → the loser's expected_revision_id no longer names the current
// revision → ErrRevisionConflict (409).
func (api *API) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	taskID := r.PathValue("id")
	if decision := authz.Can(r.Context(), principal, authz.ActionTaskRead, taskID); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	if api.art == nil {
		writeError(w, http.StatusNotFound, codeNotFound, msgArtifactEditStoreNotConfigured)
		return
	}
	name := r.PathValue("name")

	// Limit the read so a huge body cannot exhaust memory before the store's
	// own limits run. The store sizes its own rows; this is just the transport.
	bodyBytes, readErr := io.ReadAll(io.LimitReader(r.Body, maxArtifactEditBytes))
	if readErr != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "could not read request body: "+readErr.Error())
		return
	}
	var req artifactEditRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Content) == 0 {
		writeError(w, http.StatusBadRequest, codeBadInput, msgArtifactEditRequiresContent)
		return
	}

	// Determine the kind: the request's kind, else the prior revision's kind,
	// else the default "file" for a create.
	kind := req.Kind
	current, currentErr := api.art.Current(r.Context(), principal.TenantID, taskID, name)
	hasCurrent := currentErr == nil
	if hasCurrent && kind == "" {
		kind = current.Kind
	}
	if kind == "" {
		kind = "file"
	}
	// Enforce the precondition. When a current revision exists, the caller must
	// pin it; a missing precondition would chain the new revision onto the
	// current one without the caller having seen it — a blind overwrite. The
	// first-create case (no current revision) needs no precondition.
	if hasCurrent && req.ExpectedRevisionID == "" {
		writeError(w, http.StatusPreconditionRequired, codePreconditionMissing, msgArtifactEditPrecondition)
		return
	}
	// When the caller pins a revision but the artifact has none, that is a
	// conflict (the caller expected a revision that does not exist), not a
	// create — the store's planRevision would reject it as a conflict anyway,
	// but surfacing it here gives a clearer message.
	if !hasCurrent && req.ExpectedRevisionID != "" {
		if !errors.Is(currentErr, artifacts.ErrNoCurrentRevision) {
			writeError(w, http.StatusInternalServerError, codeInternal, errForCaller(currentErr))
			return
		}
		writeError(w, http.StatusConflict, codeConflict,
			"expected_revision_id was set but the artifact has no current revision; resend without it to create")
		return
	}

	revision, err := api.art.Put(r.Context(), artifacts.PutParams{
		TenantID:                principal.TenantID,
		UserID:                  principal.UserID,
		TaskID:                  taskID,
		Name:                    name,
		Kind:                    kind,
		Bytes:                   []byte(req.Content),
		Actor:                   artifacts.ActorHuman, // no Source: a human edit has no invocation
		ExpectedCurrentRevision: req.ExpectedRevisionID,
	})
	if err != nil {
		writeError(w, statusForArtifactStoreErr(err), codeForArtifactStoreErr(err), errForCaller(err))
		return
	}
	// The edit IS the approval at a human_edit gate. Record it on the manifest
	// so the human decision trail carries who edited what and when. A plain
	// AddEvidence is fine here: no FSM transition, so no shared transaction.
	api.recordHumanEditDecision(r.Context(), principal, taskID)
	writeJSON(w, http.StatusOK, toArtifactRevisionResponse(revision))
}

// maxArtifactEditBytes caps a PUT body. Larger than the store's own cap so the
// store's limit governs the rejection message rather than a transport error.
const maxArtifactEditBytes = 16 << 20 // 16 MiB

// recordHumanEditDecision records a HumanDecision{decision: edited} for a
// successful human artifact edit. The stage is resolved from the task's
// current_stage so the decision is placeable in the audit trail. Best-effort:
// a nil manifest service (tests, a server that did not wire one) is a no-op,
// and a sealed / missing manifest is dropped — the edit itself landed, and the
// artifact revision row is its own durable record.
func (api *API) recordHumanEditDecision(ctx context.Context, principal authz.Principal, taskID string) {
	if api.mfst == nil {
		return
	}
	stage := ""
	if task, err := api.queries.GetTask(ctx, sqlc.GetTaskParams{ID: taskID, TenantID: principal.TenantID}); err == nil {
		stage = currentStageOr(task.CurrentStage, "")
	}
	patch := humanDecisionPatch(stage, gateHumanEdit, decisionEdited, principal.UserID, time.Now().UTC())
	if err := api.mfst.AddEvidence(ctx, principal.TenantID, taskID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) || errors.Is(err, manifest.ErrNoManifest) {
			return
		}
		api.log.Warn("record human-edit decision", "task", taskID, "error", err)
	}
}

// statusForArtifactStoreErr maps an artifacts.Store error to an HTTP status.
// ErrRevisionConflict → 409 (the chain moved under the caller); ErrSecretDetected
// → 422 (the caller sent credential-shaped content the policy rejects);
// ErrNoCurrentRevision → 404.
func statusForArtifactStoreErr(err error) int {
	switch {
	case errors.Is(err, artifacts.ErrRevisionConflict):
		return http.StatusConflict
	case errors.Is(err, artifacts.ErrSecretDetected):
		return http.StatusUnprocessableEntity
	case errors.Is(err, artifacts.ErrNoCurrentRevision):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// codeForArtifactStoreErr is the stable machine code matching
// statusForArtifactStoreErr.
func codeForArtifactStoreErr(err error) string {
	switch {
	case errors.Is(err, artifacts.ErrRevisionConflict):
		return codeConflict
	case errors.Is(err, artifacts.ErrSecretDetected):
		return codeBadInput
	case errors.Is(err, artifacts.ErrNoCurrentRevision):
		return codeNotFound
	default:
		return codeInternal
	}
}

// errForCaller reduces a store error to a short caller-facing message. The
// underlying error already carries the cause; keep it rather than inventing a
// vaguer one.
func errForCaller(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
