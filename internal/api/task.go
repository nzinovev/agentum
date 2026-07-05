package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// taskResponse is the public task shape. tenant_id and user_id are intentionally
// absent: identity is implicit in the Principal, not echoed back.
type taskResponse struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	PipelinePack string          `json:"pipeline_pack"`
	Title        string          `json:"title"`
	Input        json.RawMessage `json:"input"`
	State        string          `json:"state"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toTaskResponse(t sqlc.Task) taskResponse {
	in := t.Input
	if len(in) == 0 {
		in = json.RawMessage("{}")
	}
	return taskResponse{
		ID:           t.ID,
		ProjectID:    t.ProjectID,
		PipelinePack: t.PipelinePack,
		Title:        t.Title,
		Input:        in,
		State:        t.State,
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// handleCreateTask POST /api/v1/tasks
// Body: {project_id, pipeline_pack, title, input?}. tenant/user come from the
// Principal, never the body.
func (a *API) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	p, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unresolved principal")
		return
	}
	if d := authz.Can(r.Context(), p, "task:create", ""); !d.Allowed {
		writeError(w, http.StatusForbidden, d.Reason)
		return
	}

	var req struct {
		ProjectID    string          `json:"project_id"`
		PipelinePack string          `json:"pipeline_pack"`
		Title        string          `json:"title"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ProjectID == "" || req.PipelinePack == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, "project_id, pipeline_pack, and title are required")
		return
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}

	task, err := a.q.CreateTask(r.Context(), sqlc.CreateTaskParams{
		TenantID:     p.TenantID,
		UserID:       p.UserID,
		ProjectID:    req.ProjectID,
		PipelinePack: req.PipelinePack,
		Title:        req.Title,
		Input:        req.Input,
	})
	if err != nil {
		// Most CreateTask failures are bad input (invalid uuid, shape); the rest
		// are unexpected and logged. Treat as 400 for the single-owner MVP.
		logUnexpected(a.log, err, "CreateTask")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toTaskResponse(task))
}

// handleGetTask GET /api/v1/tasks/{id}
func (a *API) handleGetTask(w http.ResponseWriter, r *http.Request) {
	p, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unresolved principal")
		return
	}
	if d := authz.Can(r.Context(), p, "task:read", r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, d.Reason)
		return
	}

	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		logUnexpected(a.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(task))
}

// handleListTasks GET /api/v1/tasks?project_id=...&limit=...&offset=...
func (a *API) handleListTasks(w http.ResponseWriter, r *http.Request) {
	p, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unresolved principal")
		return
	}
	if d := authz.Can(r.Context(), p, "task:list", ""); !d.Allowed {
		writeError(w, http.StatusForbidden, d.Reason)
		return
	}

	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "project_id query parameter is required")
		return
	}
	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	offset := clampInt(queryInt(r, "offset", 0), 0, 10000)

	tasks, err := a.q.ListTasksByProject(r.Context(), sqlc.ListTasksByProjectParams{
		TenantID:  p.TenantID,
		ProjectID: projectID,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		logUnexpected(a.log, err, "ListTasksByProject")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := make([]taskResponse, 0, len(tasks))
	for _, t := range tasks {
		resp = append(resp, toTaskResponse(t))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStartTask POST /api/v1/tasks/{id}/start
// Transitions created -> running through engine.Next. This is the proof point
// that the FSM gates every state change: an illegal transition is a 409, never
// a silent write.
func (a *API) handleStartTask(w http.ResponseWriter, r *http.Request) {
	p, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unresolved principal")
		return
	}
	if d := authz.Can(r.Context(), p, "task:start", r.PathValue("id")); !d.Allowed {
		writeError(w, http.StatusForbidden, d.Reason)
		return
	}

	id := r.PathValue("id")
	task, err := a.q.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: p.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		logUnexpected(a.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	next, err := engine.Next(engine.TaskState(task.State), engine.EventStart)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	updated, err := a.q.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
		ID:       task.ID,
		TenantID: p.TenantID,
		State:    string(next),
	})
	if err != nil {
		logUnexpected(a.log, err, "UpdateTaskState")
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
