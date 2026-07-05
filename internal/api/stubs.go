package api

import "net/http"

// Stubs for every /api/v1/* endpoint declared in docs/api.md but not yet
// implemented. Each returns 501 {code: not_implemented, message: ...}. The
// implementation lands with its epic (see docs/api.md "Status"). Declaring the
// routes now gives Epic 7 (UI) a stable surface to code against and makes the
// contract testable: a documented route never 404s.

// POST /api/v1/tasks/{id}/cancel — cancel any non-terminal task. Lifecycle
// sibling of /start; trivial but kept as a stub for F.3 scope.
func (a *API) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic: foundation", "POST /tasks/{id}/cancel")
}

// --- Stage invocations (read-only) ---

// GET /api/v1/tasks/{id}/invocations
func (a *API) handleListInvocations(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 5.1", "GET /tasks/{id}/invocations")
}

// GET /api/v1/tasks/{id}/invocations/{iid}
func (a *API) handleGetInvocation(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 5.1", "GET /tasks/{id}/invocations/{iid}")
}

// --- Gate actions (§3.2 stop conditions → continue semantics) ---

// POST /api/v1/tasks/{id}/invocations/{iid}/continue
// Resume after open_questions / user_stop (session-id resume).
func (a *API) handleInvocationContinue(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/continue")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/advance
// Pass a gate → the next stage runs (a fresh invocation).
func (a *API) handleInvocationAdvance(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/advance")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/approve
// Final approval → task done + memory commits.
func (a *API) handleInvocationApprove(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/approve")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/edit
// Edit-and-approve: the human edits the artifact directly; the edit IS the
// approval.
func (a *API) handleInvocationEdit(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/edit")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/ask-to-edit
// Scoped agent-mediated edit; re-stops for review (recursive stop point).
func (a *API) handleInvocationAskToEdit(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/ask-to-edit")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/add-context
// Additive guidance; the agent resumes (does not regenerate).
func (a *API) handleInvocationAddContext(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "POST /tasks/{id}/invocations/{iid}/add-context")
}

// --- Artifacts ---

// GET /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}
func (a *API) handleArtifactGet(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "GET /tasks/{id}/invocations/{iid}/artifacts/{name}")
}

// PUT /api/v1/tasks/{id}/invocations/{iid}/artifacts/{name}
func (a *API) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 2", "PUT /tasks/{id}/invocations/{iid}/artifacts/{name}")
}

// --- Memory (keyword-pull handle) ---

// GET /api/v1/projects/{id}/memory?keyword=...
func (a *API) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 1", "GET /projects/{id}/memory")
}

// --- Packs ---

// GET /api/v1/packs
func (a *API) handleListPacks(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 5.1", "GET /packs")
}

// GET /api/v1/packs/{name}
func (a *API) handleGetPack(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "Epic 5.1", "GET /packs/{name}")
}
