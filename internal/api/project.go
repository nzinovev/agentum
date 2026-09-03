package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/repoid"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// projectResponse is the public project shape. tenant_id and user_id are
// intentionally absent: identity is implicit in the Principal, not echoed back.
// repo_identity is read-only: it is computed at the registration boundary from
// the repository itself and is never accepted from a request body.
type projectResponse struct {
	ID              string   `json:"id"`
	RepoIdentity    string   `json:"repo_identity"`
	RepoPath        string   `json:"repo_path"`
	Name            string   `json:"name"`
	RelatedProjects []string `json:"related_projects"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`

	// The three fields below appear only on a registration that changed the
	// project's working copy. RunsReboundToNewCheckout is how many unfinished
	// runs moved with the copy (the relocation branch);
	// RunsAwaitingPreviousCheckout is how many stay in the previous copy —
	// the two-copies branch, and the could-not-tell branch, which keeps runs
	// where they are because rebinding on a guess is irreversible while
	// staying is recoverable (a run whose copy really is gone pauses itself
	// with checkout_unavailable and resumes when the path returns).
	PreviousRepoPath             string `json:"previous_repo_path,omitempty"`
	RunsReboundToNewCheckout     int64  `json:"runs_rebound_to_new_checkout,omitempty"`
	RunsAwaitingPreviousCheckout int64  `json:"runs_awaiting_previous_checkout,omitempty"`
}

func toProjectResponse(project sqlc.Project) projectResponse {
	related := project.RelatedProjects
	if related == nil {
		related = []string{}
	}
	return projectResponse{
		ID:              project.ID,
		RepoIdentity:    project.RepoIdentity,
		RepoPath:        project.RepoPath,
		Name:            project.Name,
		RelatedProjects: related,
		CreatedAt:       project.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       project.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// handleCreateProject POST /api/v1/projects
// Body: {repo_path, name, related_projects?}. tenant/user come from the
// Principal; repo_identity is computed from the repository itself and never
// taken from the body. Idempotent: registering the same repository (same
// identity — the same copy or a clone of the same history) updates repo_path,
// name and the related set rather than failing, so a moved directory keeps its
// run history.
//
// When the registration moves the project's working copy, the handler resolves
// the PREVIOUS path to tell the two possible worlds apart:
//
//   - the previous path no longer holds this repository (it moved, or
//     something else lives there now): the working copy is one and it
//     relocated — unfinished runs are rebound to the new path in the same
//     transaction;
//   - the previous path still resolves to the same identity: there are now two
//     working copies. Unfinished runs stay where they started (their pinned
//     checkout keeps them there), and the response names the previous path and
//     how many runs remain in it.
func (api *API) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionProjectCreate, "")
	if !ok {
		return
	}

	var req struct {
		RepoPath        string   `json:"repo_path"`
		Name            string   `json:"name"`
		RelatedProjects []string `json:"related_projects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "invalid JSON body")
		return
	}
	if req.RepoPath == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "repo_path and name are required")
		return
	}
	// related_projects is NOT NULL DEFAULT '{}' in Postgres, but pq.Array(nil)
	// sends NULL; coerce nil to an empty slice so an omitted field inserts cleanly.
	if req.RelatedProjects == nil {
		req.RelatedProjects = []string{}
	}

	// The single git gate of registration: identity, normalized working-tree
	// root, and every refusal that says "this cannot be a project's working
	// copy" (no commits, shallow, linked worktree) come from one probe.
	identity, err := repoid.Resolve(r.Context(), req.RepoPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "repo_path: "+err.Error())
		return
	}

	// The previous row under this identity must be read before the upsert
	// overwrites its path — it decides what happens to unfinished runs. A
	// read failure is not "no previous row": treating it as one would skip
	// the rebind decision silently, so it fails the request instead.
	existing, err := api.queries.GetProjectByIdentity(r.Context(), sqlc.GetProjectByIdentityParams{
		TenantID: principal.TenantID, RepoIdentity: identity.Value,
	})
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// First registration of this repository: there is no previous path to
		// resolve.
	default:
		logUnexpected(api.log, err, "GetProjectByIdentity")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	previousRowFound := err == nil
	pathChanged := previousRowFound && existing.RepoPath != identity.TopLevel
	previousState := previousCopyUnknown
	if pathChanged {
		previousState = classifyPreviousCopy(r.Context(), existing.RepoPath, existing.RepoRootCommits)
	}

	var reboundCount int64
	var proj sqlc.Project
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		created, createErr := qtx.CreateProject(r.Context(), sqlc.CreateProjectParams{
			TenantID:        principal.TenantID,
			UserID:          principal.UserID,
			RepoIdentity:    identity.Value,
			RepoRootCommits: identity.Roots,
			RepoPath:        identity.TopLevel,
			Name:            req.Name,
			RelatedProjects: req.RelatedProjects,
		})
		if createErr != nil {
			return createErr
		}
		proj = created
		if previousState != previousCopyGone {
			// Either nothing moved, or the previous copy still holds the
			// repository, or the probe could not tell: unfinished runs keep
			// their pinned checkout, and the count is reported below so the
			// split is visible at registration time.
			return nil
		}
		// The previous copy is gone (moved, or replaced by another
		// repository): the one working copy relocated, and unfinished runs
		// move with it — reported, because a run that silently changes its
		// working copy is a fact the operator must see. Terminal runs keep
		// their historical checkout_path.
		rebound, rebindErr := qtx.RebindActiveCheckouts(r.Context(), sqlc.RebindActiveCheckoutsParams{
			TenantID: principal.TenantID, ProjectID: proj.ID,
			CheckoutPath: existing.RepoPath, CheckoutPath_2: identity.TopLevel,
		})
		if rebindErr != nil {
			return rebindErr
		}
		reboundCount = int64(len(rebound))
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "CreateProject tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}

	response := toProjectResponse(proj)
	response.RunsReboundToNewCheckout = reboundCount
	if pathChanged {
		response.PreviousRepoPath = existing.RepoPath
		if previousState != previousCopyGone {
			awaiting, countErr := api.queries.CountActiveTasksOnCheckout(r.Context(), sqlc.CountActiveTasksOnCheckoutParams{
				TenantID: principal.TenantID, ProjectID: proj.ID, CheckoutPath: existing.RepoPath,
			})
			if countErr != nil {
				logUnexpected(api.log, countErr, "CountActiveTasksOnCheckout")
			} else {
				response.RunsAwaitingPreviousCheckout = awaiting
			}
		}
	}
	writeJSON(w, http.StatusCreated, response)
}

// previousCopyState classifies what the project's previous working-copy path
// holds; the classification decides what happens to unfinished runs.
type previousCopyState int

const (
	// previousCopyHolds: the previous path still holds the same repository —
	// there are two working copies now, and runs stay where they started.
	previousCopyHolds previousCopyState = iota
	// previousCopyGone: the previous path is absent from disk, or holds a
	// different repository — the working copy is one and it moved, so
	// unfinished runs follow it (the only branch that rebinds).
	previousCopyGone
	// previousCopyUnknown: the path is present but the probe could not
	// answer — git failed, the directory is unreadable, a mount is down. An
	// unreadable directory is not a moved one, and the rebind is
	// irreversible: after it, the runs' branches and checkpoints live in a
	// copy nothing points to, and re-registering the old path would only
	// create a "second copy" and never rebind back. Runs therefore stay on
	// their pin, which IS recoverable: a run whose copy is really gone
	// pauses itself with checkout_unavailable and resumes when the path
	// returns. Unknown behaves like holds everywhere.
	previousCopyUnknown
)

// classifyPreviousCopy probes the previous path with the recorded roots.
// Confirming the roots is an object lookup, not a history walk, so the probe
// stays cheap at registration time.
func classifyPreviousCopy(ctx context.Context, previousPath string, roots []string) previousCopyState {
	if _, statErr := os.Stat(previousPath); statErr != nil && errors.Is(statErr, os.ErrNotExist) {
		return previousCopyGone
	}
	_, verifyErr := repoid.Verify(ctx, previousPath, roots)
	switch {
	case verifyErr == nil:
		return previousCopyHolds
	case errors.Is(verifyErr, repoid.ErrForeignRepository):
		// Another repository lives at the previous path now — ours cannot
		// come back to it, so it moved.
		return previousCopyGone
	default:
		return previousCopyUnknown
	}
}

// handleGetProject GET /api/v1/projects/{id}
func (api *API) handleGetProject(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionProjectRead, r.PathValue("id"))
	if !ok {
		return
	}

	proj, err := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: r.PathValue("id"), TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "project not found")
			return
		}
		logUnexpected(api.log, err, "GetProject")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toProjectResponse(proj))
}

// handleListProjects GET /api/v1/projects?limit=...&offset=...
func (api *API) handleListProjects(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionProjectList, "")
	if !ok {
		return
	}

	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	offset := clampInt(queryInt(r, "offset", 0), 0, 10000)

	projs, err := api.queries.ListProjects(r.Context(), sqlc.ListProjectsParams{
		TenantID: principal.TenantID,
		Limit:    int32(limit),
		Offset:   int32(offset),
	})
	if err != nil {
		logUnexpected(api.log, err, "ListProjects")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	resp := make([]projectResponse, 0, len(projs))
	for _, project := range projs {
		resp = append(resp, toProjectResponse(project))
	}
	writeJSON(w, http.StatusOK, resp)
}
