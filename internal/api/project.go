package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// projectResponse is the public project shape. tenant_id and user_id are
// intentionally absent: identity is implicit in the Principal, not echoed back.
type projectResponse struct {
	ID              string   `json:"id"`
	RepoPath        string   `json:"repo_path"`
	Name            string   `json:"name"`
	RelatedProjects []string `json:"related_projects"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

func toProjectResponse(p sqlc.Project) projectResponse {
	rel := p.RelatedProjects
	if rel == nil {
		rel = []string{}
	}
	return projectResponse{
		ID:              p.ID,
		RepoPath:        p.RepoPath,
		Name:            p.Name,
		RelatedProjects: rel,
		CreatedAt:       p.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// validateGitRepo confirms path is inside a real git work tree. This is the
// project-registration gate (04 §7.1.1): a project must point at a real repo,
// since the runner creates worktrees off it. Returns a user-facing message on
// failure.
func validateGitRepo(path string) error {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		// Surface git's own stderr when present — it usually names the real
		// problem (not a repo, no such path). Trim so the API message stays tidy.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return errors.New(msg)
			}
		}
		return errors.New("not a git repository")
	}
	if strings.TrimSpace(string(out)) != "true" {
		// Bare repos and the .git dir itself report "false"; a project repo must
		// be a work tree the agent can operate in.
		return errors.New("path is not inside a git work tree")
	}
	return nil
}

// handleCreateProject POST /api/v1/projects
// Body: {repo_path, name, related_projects?}. tenant/user come from the
// Principal. Idempotent: re-registering an existing repo_path updates name and
// the related set rather than failing.
func (a *API) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if d := authz.Can(r.Context(), p, "project:create", ""); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
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

	if err := validateGitRepo(req.RepoPath); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "repo_path: "+err.Error())
		return
	}

	proj, err := a.q.CreateProject(r.Context(), sqlc.CreateProjectParams{
		TenantID:        p.TenantID,
		UserID:          p.UserID,
		RepoPath:        req.RepoPath,
		Name:            req.Name,
		RelatedProjects: req.RelatedProjects,
	})
	if err != nil {
		logUnexpected(a.log, err, "CreateProject")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toProjectResponse(proj))
}

// handleGetProject GET /api/v1/projects/{id}
func (a *API) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if d := authz.Can(r.Context(), p, "project:read", r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return
	}

	proj, err := a.q.GetProject(r.Context(), sqlc.GetProjectParams{ID: r.PathValue("id"), TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "project not found")
			return
		}
		logUnexpected(a.log, err, "GetProject")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toProjectResponse(proj))
}

// handleListProjects GET /api/v1/projects?limit=...&offset=...
func (a *API) handleListProjects(w http.ResponseWriter, r *http.Request) {
	p, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if d := authz.Can(r.Context(), p, "project:list", ""); !d.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, d.Reason)
		return
	}

	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	offset := clampInt(queryInt(r, "offset", 0), 0, 10000)

	projs, err := a.q.ListProjects(r.Context(), sqlc.ListProjectsParams{
		TenantID: p.TenantID,
		Limit:    int32(limit),
		Offset:   int32(offset),
	})
	if err != nil {
		logUnexpected(a.log, err, "ListProjects")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	resp := make([]projectResponse, 0, len(projs))
	for _, pr := range projs {
		resp = append(resp, toProjectResponse(pr))
	}
	writeJSON(w, http.StatusOK, resp)
}
