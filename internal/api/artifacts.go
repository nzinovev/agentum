package api

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nzinovev/agentum/internal/artifacts"
)

// Repeated artifact-handler messages, as constants so the wording callers and
// logs match on cannot drift.
const (
	msgArtifactStoreNotConfigured = "artifact store not configured"
	msgRevisionNotFound           = "revision not found"
)

// artifactRevisionResponse is the public shape of an artifact revision. The
// content_hash is the worktree-independent content address — the bytes the
// agent wrote can be reconstructed from it via the blob store. prev_revision_id
// chains edits so the audit trail reads in order.
type artifactRevisionResponse struct {
	ID               string `json:"id"`
	TaskID           string `json:"task_id"`
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	ContentHash      string `json:"content_hash"`
	ContentSize      int64  `json:"content_size"`
	ActionType       string `json:"action_type"`
	PrevRevisionID   string `json:"prev_revision_id,omitempty"`
	SourceInvocation string `json:"source_invocation_id,omitempty"`
	DeliveryStep     string `json:"delivery_step,omitempty"`
	ExecutionUnit    string `json:"execution_unit,omitempty"`
	Phase            string `json:"phase,omitempty"`
	Actor            string `json:"actor"`
	IsCurrent        bool   `json:"is_current"`
	CreatedAt        string `json:"created_at"`
}

func toArtifactRevisionResponse(revision artifacts.Revision) artifactRevisionResponse {
	return artifactRevisionResponse{
		ID:               revision.ID,
		TaskID:           revision.TaskID,
		Name:             revision.Name,
		Kind:             revision.Kind,
		ContentHash:      revision.ContentHash,
		ContentSize:      revision.ContentSize,
		ActionType:       string(revision.ActionType),
		PrevRevisionID:   revision.Prev,
		SourceInvocation: revision.Source,
		DeliveryStep:     revision.ExecutionCoordinate.DeliveryStep,
		ExecutionUnit:    revision.ExecutionCoordinate.ExecutionUnit,
		Phase:            revision.ExecutionCoordinate.Phase,
		Actor:            string(revision.Actor),
		IsCurrent:        revision.IsCurrent,
		CreatedAt:        revision.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// handleListArtifacts GET /api/v1/tasks/{id}/artifacts?current=true
// Returns artifact revisions for a task. ?current=true narrows to current
// revisions only (the snapshot a resume / comparison reads). Default: all
// revisions, including superseded.
func (api *API) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	if !api.requireArtifactStore(w) {
		return
	}
	currentOnly, _ := strconv.ParseBool(r.URL.Query().Get("current"))
	var revisions []artifacts.Revision
	var err error
	if currentOnly {
		revisions, err = api.art.ListCurrent(r.Context(), principal.TenantID, taskID)
	} else {
		revisions, err = api.art.ListForTask(r.Context(), principal.TenantID, taskID)
	}
	if err != nil {
		logUnexpected(api.log, err, "ListArtifacts")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	resp := make([]artifactRevisionResponse, 0, len(revisions))
	for _, revision := range revisions {
		resp = append(resp, toArtifactRevisionResponse(revision))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetArtifactRevision GET /api/v1/tasks/{id}/artifacts/revisions/{rid}
// Returns the revision metadata (no bytes). The content lives at the
// sibling /content path; splitting keeps this handler cheap and lets a client
// inspect a revision without buffering the bytes.
func (api *API) handleGetArtifactRevision(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	if !api.requireArtifactStore(w) {
		return
	}
	revision, found := api.revisionForTask(w, r, principal.TenantID, taskID, "GetArtifactRevision")
	if !found {
		return
	}
	writeJSON(w, http.StatusOK, toArtifactRevisionResponse(revision))
}

// handleGetArtifactContent GET /api/v1/tasks/{id}/artifacts/revisions/{rid}/content
// Streams the blob bytes for a revision. Sets Content-Type from the revision
// kind when known, else application/octet-stream. Streams via CopyTo so large
// blobs do not buffer in memory.
func (api *API) handleGetArtifactContent(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	if !api.requireArtifactStore(w) {
		return
	}
	revision, found := api.revisionForTask(w, r, principal.TenantID, taskID, "GetArtifactContent")
	if !found {
		return
	}
	w.Header().Set("Content-Type", contentTypeForKind(revision.Kind))
	w.Header().Set("X-Content-Hash", revision.ContentHash)
	w.Header().Set("ETag", `"`+revision.ContentHash+`"`)
	if _, copyErr := api.art.CopyTo(r.Context(), principal.TenantID, revision.ID, w); copyErr != nil {
		// The header is already written; the best we can do is log.
		api.log.Warn("stream artifact content", "revision", revision.ID, "error", copyErr)
		return
	}
}

// requireArtifactStore guards the handlers that cannot work without the
// artifact store, writing the 404 itself when the server was built without one.
func (api *API) requireArtifactStore(w http.ResponseWriter) bool {
	if api.art == nil {
		writeError(w, http.StatusNotFound, codeNotFound, msgArtifactStoreNotConfigured)
		return false
	}
	return true
}

// revisionForTask loads the {rid} revision and confirms it belongs to taskID.
// A revision the tenant may read but that hangs off a DIFFERENT task is
// reported as not-found rather than acknowledged, so the response never leaks
// its cross-task existence. Writes the error response itself; found=false means
// the handler must return. where names the caller for the unexpected-error log.
func (api *API) revisionForTask(w http.ResponseWriter, r *http.Request, tenantID, taskID, where string) (artifacts.Revision, bool) {
	revision, err := api.art.Get(r.Context(), tenantID, r.PathValue("rid"))
	if err != nil {
		if errors.Is(err, artifacts.ErrNoCurrentRevision) || errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgRevisionNotFound)
			return artifacts.Revision{}, false
		}
		logUnexpected(api.log, err, where)
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return artifacts.Revision{}, false
	}
	if revision.TaskID != taskID {
		writeError(w, http.StatusNotFound, codeNotFound, msgRevisionNotFound)
		return artifacts.Revision{}, false
	}
	return revision, true
}

// contentTypeForKind maps a revision kind to a Content-Type. The map covers
// the kinds the agent contract produces today; unknown kinds fall back to
// octet-stream (downloadable, not rendered).
func contentTypeForKind(kind string) string {
	switch kind {
	case "result_json", "json":
		return "application/json; charset=utf-8"
	case "spec", "adr", "markdown", "md":
		return "text/markdown; charset=utf-8"
	case "code":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}
