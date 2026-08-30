package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
)

// taskResponse is the public task shape. tenant_id and user_id are
// intentionally absent: identity is implicit in the Principal, not echoed
// back. description is the request; overrides is how this run differs from the
// project defaults — orchestrator-facing, echoed for the author but never
// delivered to the agent. base_commit / result_commit / branch expose the git
// egress surface (F.6.1 AC #7): base_ref is the user-supplied input,
// base_commit the once-resolved immutable lineage anchor, result_commit the
// recorded tip at terminal teardown, and branch the resolvable delivery ref
// that survives worktree teardown.
type taskResponse struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	PipelinePack string          `json:"pipeline_pack"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Overrides    json.RawMessage `json:"overrides"`
	State        string          `json:"state"`
	BaseRef      string          `json:"base_ref"`
	BaseCommit   string          `json:"base_commit"`
	ResultCommit string          `json:"result_commit"`
	Branch       string          `json:"branch"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toTaskResponse(task sqlc.Task) taskResponse {
	overrides := task.Overrides
	if len(overrides) == 0 {
		overrides = json.RawMessage("{}")
	}
	return taskResponse{
		ID:           task.ID,
		ProjectID:    task.ProjectID,
		PipelinePack: task.PipelinePack,
		Title:        task.Title,
		Description:  task.Description,
		Overrides:    overrides,
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

// taskCreateRequest is the POST /tasks body. The request half — title +
// description — reaches the model; the overrides half configures the run and
// is orchestrator-only. Decoded with DisallowUnknownFields: a typo'd or legacy
// `input` blob is a loud 400, not a silently dropped key that weakens the run.
type taskCreateRequest struct {
	ProjectID    string          `json:"project_id"`
	PipelinePack string          `json:"pipeline_pack"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Overrides    json.RawMessage `json:"overrides"`
	BaseRef      string          `json:"base_ref"`
}

// maxTaskCreateBytes caps the create body on the transport. It must be sized
// for the ENCODED body while the field budgets are measured on the DECODED
// string, and those differ by more than a rounding error: a client that
// ASCII-escapes non-ASCII (the default for Python's json.dumps and Jackson)
// sends a Cyrillic description at ~2.5x its UTF-8 size, and a per-character
// worst case (an escaped control char, 1 byte -> 6) is 6x. Sizing the cap at
// the budget plus a few KiB would reject an in-budget Russian description as
// malformed JSON — and the repo's own example task is Russian.
//
// The cap is memory hygiene, not the contract: taskinput.Validate owns the
// real limit, on the decoded string, where the author can be told which field
// is too long.
const maxTaskCreateBytes = 6*taskinput.MaxDescriptionBytes + (16 << 10)

// parseTaskCreate turns the raw body into the typed, validated request. Pure
// (no DB, no HTTP): every validation rule of the boundary is exercisable
// without a database. The secret scan runs here so a credential-shaped
// description is refused before any row exists; ErrSecretDetected flows out
// for the handler to map.
func parseTaskCreate(body []byte) (taskCreateRequest, taskinput.Request, error) {
	var req taskCreateRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return taskCreateRequest{}, taskinput.Request{}, err
	}
	overrides, err := taskinput.ParseOverrides(req.Overrides)
	if err != nil {
		return taskCreateRequest{}, taskinput.Request{}, err
	}
	typed := taskinput.Request{
		Title:       req.Title,
		Description: req.Description,
		Overrides:   overrides,
	}
	if err := typed.Validate(); err != nil {
		return taskCreateRequest{}, taskinput.Request{}, err
	}
	if scanErr := scanRequestForCredentials(typed); scanErr != nil {
		return taskCreateRequest{}, taskinput.Request{}, scanErr
	}
	return req, typed, nil
}

// scanRequestForCredentials is the containment guard for the request fields.
// BOTH title and description are scanned: each is delivered verbatim to a
// model through the routing block's Task section and recorded verbatim in the
// evidence manifest, which is the whole justification for scanning either.
// Scanning only the description would leave the same leak one field to the
// left.
//
// NewProseScanner, not NewDefaultScanner: these are sentences a human wrote,
// so only the credential-shape rules apply. The label-context rules reject
// ordinary task descriptions ("Add Bearer authentication to /settings"), and
// an author who cannot create the task has no way to override the refusal.
//
// PolicyReject, not redact: a redacted task request is a corrupted task
// request, and the author is present to fix it.
func scanRequestForCredentials(request taskinput.Request) error {
	scanner := artifacts.NewProseScanner(artifacts.PolicyReject)
	for _, field := range []struct {
		name string
		text string
	}{
		{name: "title", text: request.Title},
		{name: "description", text: request.Description},
	} {
		if _, scanErr := scanner.Scan(field.name, "task_request", []byte(field.text)); scanErr != nil {
			return scanErr
		}
	}
	return nil
}

// writeTaskCreateError maps parseTaskCreate failures onto the boundary's HTTP
// contract: everything malformed or over-budget is a 400; a detected
// credential is a 422 bad_input, the same mapping artifact_edit.go uses for
// ErrSecretDetected (do not invent a new code).
func writeTaskCreateError(w http.ResponseWriter, err error) {
	if errors.Is(err, artifacts.ErrSecretDetected) {
		writeError(w, http.StatusUnprocessableEntity, codeBadInput, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
}

// handleCreateTask POST /api/v1/tasks
// Body: {project_id, pipeline_pack, title, description, overrides?, base_ref?}.
// tenant/user come from the Principal, never the body. The stored overrides
// are the canonical serialization, so two identically-valued requests produce
// identical rows and identical revisions.
func (api *API) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, "task:create", ""); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}

	// MaxBytesReader, not io.LimitReader: LimitReader reports EOF at the cap
	// with no error, so an oversized body arrives silently truncated and fails
	// as "invalid JSON" — a message that sends the author looking for a syntax
	// error that is not there. MaxBytesReader returns a real error instead.
	bodyBytes, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTaskCreateBytes))
	if readErr != nil {
		var toolarge *http.MaxBytesError
		if errors.As(readErr, &toolarge) {
			writeError(w, http.StatusBadRequest, codeBadInput,
				fmt.Sprintf("request body exceeds %d bytes", toolarge.Limit))
			return
		}
		writeError(w, http.StatusBadRequest, codeBadInput, "could not read request body: "+readErr.Error())
		return
	}
	req, typed, parseErr := parseTaskCreate(bodyBytes)
	if parseErr != nil {
		writeTaskCreateError(w, parseErr)
		return
	}
	// title is deliberately absent here: parseTaskCreate already rejected a
	// blank one through taskinput.Validate, with a message naming the field.
	if req.ProjectID == "" || req.PipelinePack == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "project_id and pipeline_pack are required")
		return
	}
	// base_ref defaults to HEAD at the call site so the SQL stays
	// inference-clean (sqlc emits a plain string param). The column default is
	// the backstop; this makes the intent explicit and the recorded input
	// reproducible.
	if req.BaseRef == "" {
		req.BaseRef = "HEAD"
	}
	canonicalOverrides, marshalErr := typed.Overrides.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape; a 500 keeps an invariant break from
		// being reported as the author's fault.
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}

	task, err := api.queries.CreateTask(r.Context(), sqlc.CreateTaskParams{
		TenantID:     principal.TenantID,
		UserID:       principal.UserID,
		ProjectID:    req.ProjectID,
		PipelinePack: req.PipelinePack,
		Title:        req.Title,
		Description:  req.Description,
		Overrides:    canonicalOverrides,
		BaseRef:      req.BaseRef,
	})
	if err != nil {
		logUnexpected(api.log, err, "CreateTask")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	// Initialize the evidence manifest for the task. Best-effort: a failure
	// here is logged but does not fail the task creation — the runner's
	// recordInitialEvidence is a backstop, and the read handlers return a
	// clear "not initialized" 404 rather than crashing.
	if api.mfst != nil {
		if initErr := api.mfst.Init(r.Context(), principal.TenantID, principal.UserID, task.ID); initErr != nil {
			api.log.Warn("init manifest at task creation", "task", task.ID, "error", initErr)
		}
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
