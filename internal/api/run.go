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
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
	"github.com/nzinovev/agentum/internal/worktree"
)

// runResponse is the public run shape. tenant_id and user_id are
// intentionally absent: identity is implicit in the Principal, not echoed
// back. description is the request; overrides is how this run differs from the
// project defaults — orchestrator-facing, echoed for the author but never
// delivered to the agent. base_commit / result_commit / branch expose the git
// egress surface (F.6.1 AC #7): base_ref is the user-supplied input,
// base_commit the once-resolved immutable lineage anchor, result_commit the
// recorded tip at terminal teardown, and branch the resolvable delivery ref
// that survives worktree teardown.
type runResponse struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	PipelinePack string `json:"pipeline_pack"`
	// PipelinePackOrigin says where the executed pack's bytes came from —
	// builtin, project, or project+builtin — pinned with the base_commit the
	// pack was read from. Empty until the run starts and resolves its pack.
	PipelinePackOrigin string          `json:"pipeline_pack_origin"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Overrides          json.RawMessage `json:"overrides"`
	State              string          `json:"state"`
	CurrentStage       string          `json:"current_stage"`
	StopReason         string          `json:"stop_reason"`
	Error              string          `json:"error"`
	CancelReason       string          `json:"cancel_reason"`
	OpenQuestions      *[]string       `json:"open_questions,omitempty"`
	BaseRef            string          `json:"base_ref"`
	BaseCommit         string          `json:"base_commit"`
	ResultCommit       string          `json:"result_commit"`
	Branch             string          `json:"branch"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

func toRunResponse(run sqlc.Run) runResponse {
	overrides := run.Overrides
	if len(overrides) == 0 {
		overrides = json.RawMessage("{}")
	}
	return runResponse{
		ID:                 run.ID,
		ProjectID:          run.ProjectID,
		PipelinePack:       run.PipelinePack,
		PipelinePackOrigin: nullStringOr(run.PipelinePackOrigin),
		Title:              run.Title,
		Description:        run.Description,
		Overrides:          overrides,
		State:              run.State,
		CurrentStage:       nullStringOr(run.CurrentStage),
		StopReason:         run.StopReason,
		Error:              run.Error,
		CancelReason:       run.CancelReason,
		BaseRef:            run.BaseRef,
		BaseCommit:         nullStringOr(run.BaseCommit),
		ResultCommit:       nullStringOr(run.ResultCommit),
		Branch:             worktree.BranchFor(run.ID),
		CreatedAt:          run.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:          run.UpdatedAt.UTC().Format(time.RFC3339Nano),
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

// runCreateRequest is the POST /runs body. The request half — title +
// description — reaches the model; the overrides half configures the run and
// is orchestrator-only. Decoded with DisallowUnknownFields: a typo'd or legacy
// `input` blob is a 400 that names the field, not an ignored key that weakens
// the run.
type runCreateRequest struct {
	ProjectID    string          `json:"project_id"`
	PipelinePack string          `json:"pipeline_pack"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Overrides    json.RawMessage `json:"overrides"`
	BaseRef      string          `json:"base_ref"`
}

// maxRunCreateBytes caps the create body on the transport. It must be sized
// for the ENCODED body while the field budgets are measured on the DECODED
// string, and those differ by more than a rounding error: a client that
// ASCII-escapes non-ASCII (the default for Python's json.dumps and Jackson)
// sends a Cyrillic description at ~2.5x its UTF-8 size, and a per-character
// worst case (an escaped control char, 1 byte -> 6) is 6x. Sizing the cap at
// the budget plus a few KiB would reject an in-budget Russian description as
// malformed JSON — and the repo's own example run is Russian.
//
// The cap is memory hygiene, not the contract: taskinput.Validate owns the
// real limit, on the decoded string, where the author can be told which field
// is too long.
const maxRunCreateBytes = 6*taskinput.MaxDescriptionBytes + (16 << 10)

// readRequestBody reads the whole request body under cap. MaxBytesReader, not
// io.LimitReader: LimitReader reports EOF at the cap with no error, so an
// oversized body arrives truncated and fails as "invalid JSON" — a message
// that sends the author looking for a syntax error that is not there.
// MaxBytesReader returns a real error instead.
//
// The wrapper is deliberately not closed: its Close forwards to r.Body.Close
// and nothing else, and net/http closes the request body itself once the
// handler returns. It holds no descriptor or buffer of its own, so the leak
// inspection is suppressed rather than answered with a Close that would imply
// an ownership this handler does not have.
// noinspection GoResourceLeak
func readRequestBody(w http.ResponseWriter, r *http.Request, limitBytes int64) ([]byte, bool) {
	bodyBytes, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, limitBytes))
	if readErr != nil {
		var toolarge *http.MaxBytesError
		if errors.As(readErr, &toolarge) {
			writeError(w, http.StatusBadRequest, codeBadInput,
				fmt.Sprintf("request body exceeds %d bytes", toolarge.Limit))
			return nil, false
		}
		writeError(w, http.StatusBadRequest, codeBadInput, "could not read request body: "+readErr.Error())
		return nil, false
	}
	return bodyBytes, true
}

// parseRunCreate turns the raw body into the typed, validated request. Pure
// (no DB, no HTTP): every validation rule of the boundary is exercisable
// without a database. The secret scan runs here so a credential-shaped
// description is refused before any row exists; ErrSecretDetected flows out
// for the handler to map.
func parseRunCreate(body []byte) (runCreateRequest, taskinput.Request, error) {
	var req runCreateRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return runCreateRequest{}, taskinput.Request{}, err
	}
	if req.PipelinePack == "" {
		req.PipelinePack = "backend-development"
	}
	overrides, err := taskinput.ParseOverrides(req.Overrides)
	if err != nil {
		return runCreateRequest{}, taskinput.Request{}, err
	}
	// base_ref is required and explicit. The old silent default to HEAD made
	// every run start from whatever the operator's checkout happened to be
	// on — a developer branch's unpushed commits rode into the run's lineage
	// and later into its pull request. "HEAD" itself stays a legal EXPLICIT
	// choice (run from the current local branch); only the silent one is
	// gone. The column default remains as a backstop for non-API writers.
	if strings.TrimSpace(req.BaseRef) == "" {
		return runCreateRequest{}, taskinput.Request{}, errors.New(
			"base_ref is required: name the target branch the run builds on (e.g. refs/remotes/origin/main) or the commit it starts from; HEAD only as an explicit choice to run from the current local branch")
	}
	typed := taskinput.Request{
		Title:       req.Title,
		Description: req.Description,
		Overrides:   overrides,
	}
	if err := typed.Validate(); err != nil {
		return runCreateRequest{}, taskinput.Request{}, err
	}
	if scanErr := scanRequestForCredentials(typed); scanErr != nil {
		return runCreateRequest{}, taskinput.Request{}, scanErr
	}
	return req, typed, nil
}

// scanRequestForCredentials is the containment guard for the request fields.
// BOTH title and description are scanned: each is delivered verbatim to a
// model through the routing block's requested-work section and recorded
// verbatim in the evidence manifest, which is the whole justification for
// scanning either. Scanning only the description would leave the same leak
// one field to the left.
//
// NewProseScanner, not NewDefaultScanner: these are sentences a human wrote,
// so only the credential-shape rules apply. The label-context rules reject
// ordinary run descriptions ("Add Bearer authentication to /settings"), and
// an author who cannot create the run has no way to override the refusal.
//
// PolicyReject, not redact: a redacted run request is a corrupted run
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
		if _, scanErr := scanner.Scan(field.name, "run_request", []byte(field.text)); scanErr != nil {
			return fmt.Errorf("%s: %w", field.name, scanErr)
		}
	}
	return nil
}

// writeRequestBodyError maps a request-body parse or scan failure onto the
// boundary's HTTP contract: everything malformed or over-budget is a 400; a
// detected credential is a 422 bad_input, the same mapping artifact_edit.go
// uses for ErrSecretDetected (do not invent a new code). Shared by the
// create-run and continue handlers, whose bodies face the same failure set.
func writeRequestBodyError(w http.ResponseWriter, err error) {
	field := requestErrorField(err)
	if errors.Is(err, artifacts.ErrSecretDetected) {
		writeFieldError(w, http.StatusUnprocessableEntity, codeBadInput, err.Error(), field)
		return
	}
	writeFieldError(w, http.StatusBadRequest, codeBadInput, err.Error(), field)
}

func requestErrorField(err error) string {
	message := err.Error()
	for _, field := range []string{"project_id", "pipeline_pack", "title", "description", "overrides", "base_ref"} {
		if strings.HasPrefix(message, field+" ") || strings.HasPrefix(message, field+":") || strings.HasPrefix(message, field+".") || strings.Contains(message, `unknown field "`+field+`"`) {
			return field
		}
	}
	return ""
}

// handleCreateRun POST /api/v1/runs
// Body: {project_id, pipeline_pack, title, description, overrides?, base_ref?}.
// tenant/user come from the Principal, never the body. The stored overrides
// are the canonical serialization, so two identically-valued requests produce
// identical rows and identical revisions.
func (api *API) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionRunCreate, "")
	if !ok {
		return
	}

	bodyBytes, read := readRequestBody(w, r, maxRunCreateBytes)
	if !read {
		return
	}
	req, typed, parseErr := parseRunCreate(bodyBytes)
	if parseErr != nil {
		writeRequestBodyError(w, parseErr)
		return
	}
	// title is deliberately absent here: parseRunCreate already rejected a
	// blank one through taskinput.Validate, with a message naming the field.
	// base_ref is likewise already required by parseRunCreate — the rule
	// lives with the other pure body rules, testable without a database.
	if req.ProjectID == "" {
		writeFieldError(w, http.StatusBadRequest, codeBadInput, "project_id is required", "project_id")
		return
	}
	canonicalOverrides, marshalErr := typed.Overrides.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape; a 500 keeps an invariant break from
		// being reported as the author's fault.
		writeError(w, http.StatusInternalServerError, codeInternal, marshalErr.Error())
		return
	}

	run, err := api.queries.CreateRun(r.Context(), sqlc.CreateRunParams{
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
		logUnexpected(api.log, err, "CreateRun")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	// Initialize the evidence manifest for the run. Best-effort: a failure
	// here is logged but does not fail the run creation — the runner's
	// recordInitialEvidence is a backstop, and the read handlers return a
	// clear "not initialized" 404 rather than crashing.
	if api.mfst != nil {
		if initErr := api.mfst.Init(r.Context(), principal.TenantID, principal.UserID, run.ID); initErr != nil {
			api.log.Warn("init manifest at run creation", "run", run.ID, "error", initErr)
		}
	}
	writeJSON(w, http.StatusCreated, toRunResponse(run))
}

// handleGetRun GET /api/v1/runs/{id}
func (api *API) handleGetRun(w http.ResponseWriter, r *http.Request) {
	_, run, ok := api.requireRunForAction(w, r, authz.ActionRunRead, "GetRun")
	if !ok {
		return
	}
	response := toRunResponse(run)
	if run.State == string(engine.StatePausedOpenQuestions) {
		latest, latestErr := api.queries.LatestStageForRun(r.Context(), sqlc.LatestStageForRunParams{RunID: run.ID, TenantID: run.TenantID})
		if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
			logUnexpected(api.log, latestErr, "LatestStageForRun(get run)")
			writeError(w, http.StatusInternalServerError, codeInternal, latestErr.Error())
			return
		}
		questions := []string{}
		if latestErr == nil && latest.Result.Valid && len(latest.Result.RawMessage) > 0 {
			var result agent.ResultJSON
			if err := json.Unmarshal(latest.Result.RawMessage, &result); err != nil {
				logUnexpected(api.log, err, "decode latest invocation result")
				writeError(w, http.StatusInternalServerError, codeInternal, "could not read open questions")
				return
			}
			if result.OpenQuestions != nil {
				questions = result.OpenQuestions
			}
		}
		response.OpenQuestions = &questions
	}
	writeJSON(w, http.StatusOK, response)
}

// handleListRuns GET /api/v1/runs?project_id=...&limit=...&offset=...
func (api *API) handleListRuns(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionRunList, "")
	if !ok {
		return
	}

	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "project_id query parameter is required")
		return
	}
	limit := clampInt(queryInt(r, "limit", 50), 1, 200)
	offset := clampInt(queryInt(r, "offset", 0), 0, 10000)

	runs, err := api.queries.ListRunsByProject(r.Context(), sqlc.ListRunsByProjectParams{
		TenantID:  principal.TenantID,
		ProjectID: projectID,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		logUnexpected(api.log, err, "ListRunsByProject")
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	resp := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		resp = append(resp, toRunResponse(run))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStartRun POST /api/v1/runs/{id}/start
// Transitions created -> running through engine.Next and enqueues a run job.
// The worker (not this request) drives the stages; the handler returns as soon
// as the job is queued. An illegal transition is a 409; the write does not
// happen.
func (api *API) handleStartRun(w http.ResponseWriter, r *http.Request) {
	principal, run, ok := api.requireRunForAction(w, r, authz.ActionRunStart, "GetRun")
	if !ok {
		return
	}

	next, err := engine.Next(engine.RunState(run.State), engine.EventStart)
	if err != nil {
		writeError(w, http.StatusConflict, codeIllegalTransition, err.Error())
		return
	}

	// Transactional outbox (F.6.1 AC #6): the FSM transition and the run-job
	// enqueue commit in one tx. A failed enqueue rolls back the transition, so
	// the run can never be left running with no driver intent.
	var updated sqlc.Run
	if err := api.runInTx(r.Context(), func(qtx *sqlc.Queries) error {
		transitioned, transitionErr := qtx.UpdateRunState(r.Context(), sqlc.UpdateRunStateParams{
			ID:       run.ID,
			TenantID: principal.TenantID,
			State:    string(next),
		})
		if transitionErr != nil {
			return transitionErr
		}
		if _, enqueueErr := qtx.EnqueueJob(r.Context(), sqlc.EnqueueJobParams{
			TenantID: principal.TenantID, UserID: principal.UserID, RunID: run.ID, Kind: "run",
			Payload: []byte("{}"),
		}); enqueueErr != nil {
			return enqueueErr
		}
		updated = transitioned
		return nil
	}); err != nil {
		logUnexpected(api.log, err, "StartRun tx")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toRunResponse(updated))
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
