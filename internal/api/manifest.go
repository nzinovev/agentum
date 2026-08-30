package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/manifest"
)

// Repeated manifest-handler messages, as constants so the wording callers and
// logs match on cannot drift.
const (
	msgManifestServiceNotConfigured = "manifest service not configured"
	msgManifestNotInitialized       = "manifest not initialized"
)

// manifestResponse is the public shape of a manifest. body is the merged
// manifest body (sealed body with the latest correction applied); seal_info
// carries the seal metadata; corrections lists post-seal amendments in order.
type manifestResponse struct {
	TaskID      string           `json:"task_id"`
	Body        manifest.Body    `json:"body"`
	Seal        sealInfoResponse `json:"seal"`
	Corrections []correctionResp `json:"corrections"`
}

type sealInfoResponse struct {
	Sealed    bool   `json:"sealed"`
	Reason    string `json:"reason,omitempty"`
	SealedBy  string `json:"sealed_by,omitempty"`
	SealedAt  string `json:"sealed_at,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type correctionResp struct {
	ID        string        `json:"id"`
	Reason    string        `json:"reason"`
	Body      manifest.Body `json:"body"`
	CreatedAt string        `json:"created_at"`
}

func toManifestResponse(taskID string, body manifest.Body, seal manifest.SealInfo, corrections []manifest.Correction) manifestResponse {
	resp := manifestResponse{
		TaskID: taskID,
		Body:   body,
		Seal: sealInfoResponse{
			Sealed:    seal.Sealed,
			Reason:    seal.Reason,
			SealedBy:  seal.SealedBy,
			CreatedAt: seal.CreatedAt.UTC().Format(time.RFC3339Nano),
			UpdatedAt: seal.UpdatedAt.UTC().Format(time.RFC3339Nano),
		},
		Corrections: make([]correctionResp, 0, len(corrections)),
	}
	if seal.SealedAt.Valid {
		resp.Seal.SealedAt = seal.SealedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	for _, correction := range corrections {
		resp.Corrections = append(resp.Corrections, correctionResp{
			ID:        correction.ID,
			Reason:    correction.Reason,
			Body:      correction.Body,
			CreatedAt: correction.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return resp
}

// handleGetManifest GET /api/v1/tasks/{id}/manifest
// Returns the manifest body, seal metadata, and any corrections.
func (api *API) handleGetManifest(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	if !api.requireManifestService(w) {
		return
	}
	body, seal, corrections, err := api.mfst.Get(r.Context(), principal.TenantID, taskID)
	if err != nil {
		if errors.Is(err, manifest.ErrNoManifest) {
			writeError(w, http.StatusNotFound, codeNotFound, msgManifestNotInitialized)
			return
		}
		logUnexpected(api.log, err, "GetManifest")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toManifestResponse(taskID, body, seal, corrections))
}

// handleDiffManifest GET /api/v1/tasks/{id}/manifest/diff?other=<task-id>
// Compares this task's sealed manifest body with another task's sealed body
// and returns the input-level differences. Outputs (artifacts produced) and
// human decisions are NOT compared — those are results, not inputs. The
// response is empty when the two manifests are equivalent on the input axes.
func (api *API) handleDiffManifest(w http.ResponseWriter, r *http.Request) {
	principal, leftID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	rightID := r.URL.Query().Get("other")
	if rightID == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "other query parameter (task id) is required")
		return
	}
	// The comparison reads a second task, so it needs its own read decision —
	// being allowed to read the left task says nothing about the right one.
	if !authorizeTaskRead(w, r, principal, rightID) {
		return
	}
	if !api.requireManifestService(w) {
		return
	}
	leftBody, _, _, err := api.mfst.Get(r.Context(), principal.TenantID, leftID)
	if err != nil {
		if errors.Is(err, manifest.ErrNoManifest) {
			writeError(w, http.StatusNotFound, codeNotFound, "manifest for "+leftID+" not initialized")
			return
		}
		logUnexpected(api.log, err, "DiffManifest left")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	rightBody, _, _, err := api.mfst.Get(r.Context(), principal.TenantID, rightID)
	if err != nil {
		if errors.Is(err, manifest.ErrNoManifest) {
			writeError(w, http.StatusNotFound, codeNotFound, "manifest for "+rightID+" not initialized")
			return
		}
		logUnexpected(api.log, err, "DiffManifest right")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, manifest.DiffManifests(leftBody, rightBody))
}

// handleCorrectManifest POST /api/v1/tasks/{id}/manifest/corrections
// Body: {reason: string, body?: partial manifest body}. Adds a post-seal
// correction to the manifest. The body is merged into the sealed body; fields
// the caller does not include are left unchanged. 409 when the manifest is not
// yet sealed (the caller should use AddEvidence via the runner — there is no
// public AddEvidence path yet).
func (api *API) handleCorrectManifest(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	if !api.requireManifestService(w) {
		return
	}
	var req struct {
		Reason string          `json:"reason"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "invalid JSON body")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "reason is required")
		return
	}
	patch := manifest.Body{}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &patch); err != nil {
			writeError(w, http.StatusBadRequest, codeBadInput, "invalid manifest body: "+err.Error())
			return
		}
	}
	// The write path speaks one schema: a correction may not re-introduce the
	// schema-1 sections schema 2 replaced. The refusal is a 400 naming what to
	// drop, not a silent drop.
	if patch.CarriesLegacySections() {
		writeError(w, http.StatusBadRequest, codeBadInput,
			"patch carries schema-1 sections (prompts, model, capabilities.effective, adapter.name/version) removed in schema 2; correct the invocations section instead")
		return
	}
	if err := api.mfst.Correct(r.Context(), principal.TenantID, principal.UserID, taskID, req.Reason, patch); err != nil {
		if errors.Is(err, manifest.ErrNoManifest) {
			writeError(w, http.StatusNotFound, codeNotFound, msgManifestNotInitialized)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgManifestNotInitialized)
			return
		}
		logUnexpected(api.log, err, "CorrectManifest")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
}

// requireManifestService guards the handlers that cannot work without the
// manifest service, writing the 404 itself when the server was built without
// one.
func (api *API) requireManifestService(w http.ResponseWriter) bool {
	if api.mfst == nil {
		writeError(w, http.StatusNotFound, codeNotFound, msgManifestServiceNotConfigured)
		return false
	}
	return true
}
