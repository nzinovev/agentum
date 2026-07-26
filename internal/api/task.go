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
	"github.com/nzinovev/agentum/internal/worktree"
)

// taskResponse is the public task shape. tenant_id and user_id are intentionally
// absent: identity is implicit in the Principal, not echoed back. base_commit /
// result_commit / branch expose the git egress surface (F.6.1 AC #7): base_ref
// is the user-supplied input, base_commit the once-resolved immutable lineage
// anchor, result_commit the recorded tip at terminal teardown, and branch the
// resolvable delivery ref that survives worktree teardown.
type taskResponse struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	PipelinePack string          `json:"pipeline_pack"`
	Title        string          `json:"title"`
	Input        json.RawMessage `json:"input"`
	State        string          `json:"state"`
	BaseRef      string          `json:"base_ref"`
	BaseCommit   string          `json:"base_commit"`
	ResultCommit string          `json:"result_commit"`
	Branch       string          `json:"branch"`
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
		BaseRef:      task.BaseRef,
		BaseCommit:   nullStringOr(task.BaseCommit),
		ResultCommit: nullStringOr(task.ResultCommit),
		Branch:       worktree.BranchFor(task.ID),
		CreatedAt:    task.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    task.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// nullStringOr returns the String value when Valid, else "". Keeps the response
// shape a plain string for nullable commit columns (unset reads as "").
func nullStringOr(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
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
		BaseRef      string          `json:"base_ref"`
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
	// base_ref defaults to HEAD at the call site so the SQL stays
	// inference-clean (sqlc emits a plain string param). The column default is
	// the backstop; this makes the intent explicit and the recorded input
	// reproducible.
	if req.BaseRef == "" {
		req.BaseRef = "HEAD"
	}

	task, err := api.queries.CreateTask(r.Context(), sqlc.CreateTaskParams{
		TenantID:     principal.TenantID,
		UserID:       principal.UserID,
		ProjectID:    req.ProjectID,
		PipelinePack: req.PipelinePack,
		Title:        req.Title,
		Input:        req.Input,
		BaseRef:      req.BaseRef,
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

	// Transactional outbox (F.6.1 AC #6): the FSM transition and the run-job
	// enqueue commit in one tx. A failed enqueue rolls back the transition, so
	// the task can never be left running with no driver intent.
	var updated sqlc.Task
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, transitionErr := qtx.UpdateTaskState(r.Context(), sqlc.UpdateTaskStateParams{
			ID:       task.ID,
			TenantID: principal.TenantID,
			State:    string(next),
		})
		if transitionErr != nil {
			return transitionErr
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, TaskID: task.ID, Kind: "run",
			Payload: []byte("{}"),
		}); enqueueErr != nil {
			return enqueueErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "StartTask tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
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
