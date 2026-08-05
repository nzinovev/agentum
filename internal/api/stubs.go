package api

import "net/http"

// Stubs for every /api/v1/* endpoint declared in docs/api.md but not yet
// implemented. Each returns 501 {code: not_implemented, message: ...}. The
// implementation lands with its epic (see docs/api.md "Status"). Declaring the
// routes now gives Epic 7 (UI) a stable surface to code against and makes the
// contract testable: a documented route never 404s.

// Epic tags name where each stub's implementation lands. Centralized as
// constants so a renumber stays a one-line edit.
const (
	epicMemory       = "Epic 1"
	epicGateActions  = "Epic 2"
	epicCatalogReads = "Epic 5.1"
)

// --- Stage invocations (read-only) ---
//
// GET /tasks/{id}/invocations and GET /tasks/{id}/invocations/{iid} are
// implemented in invocations.go (the per-attempt read surface, including the
// cycle column that distinguishes retries from resumes).

// --- Gate actions (§3.2 stop conditions → continue semantics) ---
//
// continue / advance / cancel / approve are implemented in lifecycle.go (the
// runner's flow controls). The remaining gate actions land with Epic 2.

// POST /api/v1/tasks/{id}/invocations/{iid}/edit
// Edit-and-approve: the human edits the artifact directly; the edit IS the
// approval.
func (api *API) handleInvocationEdit(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicGateActions, "POST /tasks/{id}/invocations/{iid}/edit")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/ask-to-edit
// Scoped agent-mediated edit; re-stops for review (recursive stop point).
func (api *API) handleInvocationAskToEdit(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicGateActions, "POST /tasks/{id}/invocations/{iid}/ask-to-edit")
}

// POST /api/v1/tasks/{id}/invocations/{iid}/add-context
// Additive guidance; the agent resumes (does not regenerate).
func (api *API) handleInvocationAddContext(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicGateActions, "POST /tasks/{id}/invocations/{iid}/add-context")
}

// --- Artifacts ---
//
// GET / PUT /tasks/{id}/invocations/{iid}/artifacts/{name} are implemented in
// artifact_edit.go (the human-edit path over the immutable revisions store).

// GET /api/v1/projects/{id}/memory?keyword=...
func (api *API) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicMemory, "GET /projects/{id}/memory")
}

// --- Packs ---

// GET /api/v1/packs
func (api *API) handleListPacks(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicCatalogReads, "GET /packs")
}

// GET /api/v1/packs/{name}
func (api *API) handleGetPack(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, epicCatalogReads, "GET /packs/{name}")
}
