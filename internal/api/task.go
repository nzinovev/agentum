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

func toTaskResponse(task sqlc.Task) taskResponse {
	input := task.Input
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return taskResponse{
		ID:           task.ID,
		ProjectID:    task.ProjectID,
		PipelinePack: task.PipelinePack,
		Title:        task.Title,
		Input:        input,
		State:        task.State,
		CreatedAt:    task.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    task.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// requirePrincipal extracts the Principal, writing a structured error on failure.
// Returns false when the caller should return.
func requirePrincipal(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	principal, ok := authz.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "unresolved principal")
		return authz.Principal{}, false
	}
	return principal, true
}

// handleCreateTask POST /api/v1/tasks
// Body: {project_id, pipeline_pack, title, input?}. tenant/user come from the
// Principal, never the body.
func (api *API) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:create", ""); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}

	var req struct {
		ProjectID    string          `json:"project_id"`
		PipelinePack string          `json:"pipeline_pack"`
		Title        string          `json:"title"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "invalid JSON body")
		return
	}
	if req.ProjectID == "" || req.PipelinePack == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "project_id, pipeline_pack, and title are required")
		return
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage("{}")
	}

	task, err := api.queries.CreateTask(r.Context(), sqlc.CreateTaskParams{
		TenantID:     principal.TenantID,
		UserID:       principal.UserID,
		ProjectID:    req.ProjectID,
		PipelinePack: req.PipelinePack,
		Title:        req.Title,
		Input:        req.Input,
	})
	if err != nil {
		logUnexpected(api.log, err, "CreateTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toTaskResponse(task))
}

// handleGetTask GET /api/v1/tasks/{id}
func (api *API) handleGetTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:read", r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}

	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return
		}
		logUnexpected(api.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(task))
}

// handleListTasks GET /api/v1/tasks?project_id=...&limit=...&offset=...
func (api *API) handleListTasks(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:list", ""); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}

	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "project_id query parameter is required")
		return
	}
	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	offset := clampInt(queryInt(r, "offset", 0), 0, 10000)

	tasks, err := api.queries.ListTasksByProject(r.Context(), sqlc.ListTasksByProjectParams{
		TenantID:  principal.TenantID,
		ProjectID: projectID,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		logUnexpected(api.log, err, "ListTasksByProject")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	resp := make([]taskResponse, 0, len(tasks))
	for _, task := range tasks {
		resp = append(resp, toTaskResponse(task))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStartTask POST /api/v1/tasks/{id}/start
// Transitions created -> running through engine.Next and enqueues a run job.
// The worker (not this request) drives the stages; the handler returns as soon
// as the job is queued. An illegal transition is a 409, never a silent write.
func (api *API) handleStartTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:start", r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}

	id := r.PathValue("id")
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: id, TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return
		}
		logUnexpected(api.log, err, "GetTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}

	next, err := engine.Next(engine.TaskState(task.State), engine.EventStart)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}

	updated, err := api.queries.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
		ID:       task.ID,
		TenantID: principal.TenantID,
		State:    string(next),
	})
	if err != nil {
		logUnexpected(api.log, err, "UpdateTaskState")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}

	// Enqueue the run job; the worker picks it up and drives the stages. A
	// failure here leaves the task running with no driver — the recovery pass
	// surfaces it as an interrupted pause on the next boot.
	if _, err := api.queries.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
		TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "run",
		Payload: []byte("{}"),
	}); err != nil {
		logUnexpected(api.log, err, "EnqueueJob")
		writeError(w, http.StatusInternalServerError, codeInternal, "task started but run job could not be enqueued")
		return
	}
	writeJSON(w, http.StatusOK, toTaskResponse(updated))
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return parsed
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}
