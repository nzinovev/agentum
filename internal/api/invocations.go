package api

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// invocationResponse is the GET /tasks/{id}/invocations list/get shape. It
// surfaces the per-attempt record (sequence, cycle, stage, stop_reason,
// session_id, resume_of, timestamps) so "each attempt is visible separately"
// is answerable from the API — the cycle column distinguishes retries from
// resumes. result/capability_profile are omitted from the list shape to keep it
// cheap; a client fetches the single-invocation detail for those.
type invocationResponse struct {
	ID         string `json:"id"`
	Stage      string `json:"stage"`
	Sequence   int32  `json:"sequence"`
	Cycle      int32  `json:"cycle"`
	StopReason string `json:"stop_reason,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	ResumeOf   string `json:"resume_of,omitempty"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// handleListInvocations GET /api/v1/tasks/{id}/invocations
// Returns a task's stage invocations ordered by sequence, so each attempt is
// visible in run order. The cycle column distinguishes a retry from a resume.
func (api *API) handleListInvocations(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	// Confirm the task exists (and the tenant can see it) before listing its
	// invocations, so an unknown task returns 404 rather than an empty 200 list
	// that reads as "the task exists but has no invocations."
	if _, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: taskID, TenantID: principal.TenantID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "task not found")
			return
		}
		logUnexpected(api.log, err, "GetTask (invocations)")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	invocations, err := api.queries.ListStageInvocationsForTask(r.Context(), sqlc.ListStageInvocationsForTaskParams{
		TaskID: taskID, TenantID: principal.TenantID,
	})
	if err != nil {
		logUnexpected(api.log, err, "ListStageInvocationsForTask")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	resp := make([]invocationResponse, 0, len(invocations))
	for _, invocation := range invocations {
		resp = append(resp, toInvocationResponse(invocation))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetInvocation GET /api/v1/tasks/{id}/invocations/{iid}
// Returns a single stage invocation by id.
func (api *API) handleGetInvocation(w http.ResponseWriter, r *http.Request) {
	principal, taskID, ok := requireTaskRead(w, r)
	if !ok {
		return
	}
	invocation, err := api.queries.GetStageInvocation(r.Context(), sqlc.GetStageInvocationParams{
		ID: r.PathValue("iid"), TenantID: principal.TenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, "invocation not found")
			return
		}
		logUnexpected(api.log, err, "GetStageInvocation")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	// Belt-and-suspenders: the invocation must belong to the path's task id.
	// The query is tenant-scoped but not task-scoped, so a cross-task id in the
	// path would otherwise surface another task's invocation.
	if invocation.TaskID != taskID {
		writeError(w, http.StatusNotFound, codeNotFound, "invocation not found")
		return
	}
	writeJSON(w, http.StatusOK, toInvocationResponse(invocation))
}

func toInvocationResponse(invocation sqlc.StageInvocation) invocationResponse {
	resp := invocationResponse{
		ID:        invocation.ID,
		Stage:     invocation.Stage,
		Sequence:  invocation.Sequence,
		Cycle:     invocation.Cycle,
		StartedAt: invocation.StartedAt.UTC().Format(time.RFC3339Nano),
	}
	if invocation.StopReason.Valid {
		resp.StopReason = invocation.StopReason.String
	}
	if invocation.SessionID.Valid {
		resp.SessionID = invocation.SessionID.String
	}
	if invocation.ResumeOf.Valid {
		resp.ResumeOf = invocation.ResumeOf.String
	}
	if invocation.FinishedAt.Valid {
		resp.FinishedAt = invocation.FinishedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	return resp
}
