package api

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Pack catalog endpoints. GET /packs lists builtin packs plus, for a project,
// the project's packs at an explicit ref; GET /packs/{name} returns one pack
// with its field origins. Both carry every pack's origin — builtin, project
// (a replacement or new-name manifest), or project+builtin (inheritance) — so
// a caller composing a run always knows which layer a name resolves to.

// msgPackRefRequired mirrors run creation's base_ref contract: no silent
// HEAD. A project carries no default ref, and the listing must show packs at
// the ref the caller will pass as base_ref.
const msgPackRefRequired = "ref is required with project_id: name the target branch (e.g. refs/remotes/origin/main) or a commit — the same ref the run will build on"

const msgProjectNotFound = "project not found"

// packListEntry is one pack in the listing.
type packListEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Persona     string `json:"persona"`
	Description string `json:"description,omitempty"`
	Origin      string `json:"origin"`
}

// packListResponse answers GET /packs. Commit is set only for a project-scoped
// listing: the commit the project's packs were read at, so the caller can see
// exactly what the listing describes.
type packListResponse struct {
	Packs  []packListEntry `json:"packs"`
	Commit string          `json:"commit,omitempty"`
}

// packDetailResponse answers GET /packs/{name}: the assembled pack with the
// origin of its bytes and, for inherited packs, which fields the project
// layer set.
type packDetailResponse struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Persona     string `json:"persona"`
	Description string `json:"description,omitempty"`
	Origin      string `json:"origin"`
	// Commit is set only for a project-scoped read: the commit ref resolved
	// to, so the answer names exactly what it describes (as the listing does).
	Commit string `json:"commit,omitempty"`
	Entry  string `json:"entry"`
	Memory struct {
		Reads  []string `json:"reads"`
		Writes bool     `json:"writes"`
	} `json:"memory"`
	Capabilities []string          `json:"capabilities"`
	Budgets      packBudgetsView   `json:"budgets"`
	Checks       packChecksView    `json:"checks"`
	Approvals    []packApprovalRef `json:"approvals"`
	Stages       []packStageView   `json:"stages"`
	FieldOrigins map[string]string `json:"field_origins,omitempty"`
}

type packBudgetsView struct {
	FixCycles int `json:"fix_cycles"`
	AskToEdit int `json:"ask_to_edit"`
}

type packChecksView struct {
	Required []string `json:"required,omitempty"`
	Optional []string `json:"optional,omitempty"`
}

type packApprovalRef struct {
	Name     string `json:"name"`
	Stage    string `json:"stage"`
	Artifact string `json:"artifact"`
	Unlocks  string `json:"unlocks"`
}

type packStageView struct {
	ID           string               `json:"id"`
	Gate         string               `json:"gate"`
	Tier         string               `json:"tier,omitempty"`
	Role         string               `json:"role,omitempty"`
	Prompt       string               `json:"prompt,omitempty"`
	Terminal     bool                 `json:"terminal"`
	Capabilities []string             `json:"capabilities,omitempty"`
	Transitions  []packTransitionView `json:"transitions,omitempty"`
}

type packTransitionView struct {
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

// handleListPacks GET /api/v1/packs
func (api *API) handleListPacks(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionPackList, "")
	if !ok {
		return
	}
	if api.packs == nil {
		writeError(w, http.StatusServiceUnavailable, codeInternal, "pack catalog not wired")
		return
	}
	if projectID := r.URL.Query().Get("project_id"); projectID != "" {
		api.handleListProjectPacks(w, r, principal, projectID)
		return
	}
	builtin, err := api.packs.ListBuiltin(r.Context())
	if err != nil {
		logUnexpected(api.log, err, "ListBuiltin(packs)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	entries := make([]packListEntry, 0, len(builtin))
	for _, meta := range builtin {
		entries = append(entries, packListEntry{
			Name: meta.Name, Version: meta.Version, Persona: meta.Persona,
			Description: meta.Description, Origin: string(pack.OriginBuiltin),
		})
	}
	writeJSON(w, http.StatusOK, packListResponse{Packs: entries})
}

// handleListProjectPacks serves the ?project_id= listing: builtin packs plus
// the project's packs at the caller's explicit ref, project packs shadowing
// same-named builtins. A project pack that fails to resolve keeps its entry
// with origin and empty metadata — one broken pack must not hide the catalog,
// and the detail endpoint names the configuration error.
func (api *API) handleListProjectPacks(w http.ResponseWriter, r *http.Request, principal authz.Principal, projectID string) {
	// The ref contract is checked before the project lookup: a missing ref is
	// the caller's own input and reveals nothing, and the 400 names the same
	// expectation run creation's base_ref error does.
	if strings.TrimSpace(r.URL.Query().Get("ref")) == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, msgPackRefRequired)
		return
	}
	project, err := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: projectID, TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgProjectNotFound)
			return
		}
		logUnexpected(api.log, err, "GetProject(packs)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	if !authorize(w, r, principal, authz.ActionProjectRead, project.ID) {
		return
	}
	if api.gitRefs == nil {
		writeError(w, http.StatusServiceUnavailable, codeInternal, "ref resolver not wired")
		return
	}
	commit, err := api.gitRefs.ResolveRef(r.Context(), project.RepoPath, strings.TrimSpace(r.URL.Query().Get("ref")))
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput,
			"ref does not resolve in the project's repository: "+err.Error())
		return
	}

	builtin, err := api.packs.ListBuiltin(r.Context())
	if err != nil {
		logUnexpected(api.log, err, "ListBuiltin(packs)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	projectPacks, err := api.packs.ListProjectPacks(r.Context(), project.RepoPath, commit)
	if err != nil {
		logUnexpected(api.log, err, "ListProjectPacks(packs)")
		writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
		return
	}
	// Fill the identity block when a project pack resolves; a configuration
	// error leaves it empty rather than failing the whole listing.
	projectIdentity := func(name string) (pack.Meta, bool) {
		resolved, resolveErr := api.packs.ResolveForCommit(r.Context(), name, project.RepoPath, commit)
		if resolveErr != nil {
			return pack.Meta{}, false
		}
		return resolved.Pack.Pack, true
	}
	writeJSON(w, http.StatusOK, packListResponse{
		Packs:  mergePackListings(builtin, projectPacks, projectIdentity),
		Commit: commit,
	})
}

// mergePackListings unions the builtin catalog with the project's packs at a
// commit: a project pack replaces the builtin of the same name in this
// project (the builtin's entry must not survive alongside it — replacement,
// not a second row), an overrides directory lists as project+builtin, and the
// result is sorted by name. projectIdentity fills a project pack's identity
// block when it resolves; a broken pack keeps its entry with the origin and
// empty metadata, so one configuration error cannot hide the catalog.
func mergePackListings(builtin []pack.Meta, project []pack.ProjectPackEntry, projectIdentity func(name string) (pack.Meta, bool)) []packListEntry {
	shadowed := map[string]bool{}
	for _, entry := range project {
		shadowed[entry.Name] = true
	}
	entries := make([]packListEntry, 0, len(builtin)+len(project))
	for _, meta := range builtin {
		// A project pack replaces the builtin of the same name in this
		// project; the builtin's entry must not survive alongside it.
		if shadowed[meta.Name] {
			continue
		}
		entries = append(entries, packListEntry{
			Name: meta.Name, Version: meta.Version, Persona: meta.Persona,
			Description: meta.Description, Origin: string(pack.OriginBuiltin),
		})
	}
	for _, entry := range project {
		origin := string(pack.OriginProject)
		if entry.Kind == "overrides" {
			origin = string(pack.OriginProjectOverBuiltin)
		}
		listed := packListEntry{Name: entry.Name, Origin: origin}
		if meta, resolves := projectIdentity(entry.Name); resolves {
			listed.Version = meta.Version
			listed.Persona = meta.Persona
			listed.Description = meta.Description
		}
		entries = append(entries, listed)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	return entries
}

// handleGetPack GET /api/v1/packs/{name}
func (api *API) handleGetPack(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionPackRead, "")
	if !ok {
		return
	}
	if api.packs == nil {
		writeError(w, http.StatusServiceUnavailable, codeInternal, "pack catalog not wired")
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, codeBadInput, "pack name is required")
		return
	}

	var resolved *pack.Resolved
	resolvedCommit := ""
	if projectID := r.URL.Query().Get("project_id"); projectID != "" {
		if strings.TrimSpace(r.URL.Query().Get("ref")) == "" {
			writeError(w, http.StatusBadRequest, codeBadInput, msgPackRefRequired)
			return
		}
		project, err := api.queries.GetProject(r.Context(), sqlc.GetProjectParams{ID: projectID, TenantID: principal.TenantID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, codeNotFound, msgProjectNotFound)
				return
			}
			logUnexpected(api.log, err, "GetProject(pack)")
			writeError(w, http.StatusInternalServerError, codeInternal, err.Error())
			return
		}
		if !authorize(w, r, principal, authz.ActionProjectRead, project.ID) {
			return
		}
		if api.gitRefs == nil {
			writeError(w, http.StatusServiceUnavailable, codeInternal, "ref resolver not wired")
			return
		}
		commit, err := api.gitRefs.ResolveRef(r.Context(), project.RepoPath, strings.TrimSpace(r.URL.Query().Get("ref")))
		if err != nil {
			writeError(w, http.StatusBadRequest, codeBadInput,
				"ref does not resolve in the project's repository: "+err.Error())
			return
		}
		resolved, err = api.packs.ResolveForCommit(r.Context(), name, project.RepoPath, commit)
		if err != nil {
			api.writePackResolutionError(w, err)
			return
		}
		resolvedCommit = commit
	} else {
		var err error
		resolved, err = api.packs.ResolveBuiltin(r.Context(), name)
		if err != nil {
			api.writePackResolutionError(w, err)
			return
		}
	}
	detail := packDetailOf(resolved)
	detail.Commit = resolvedCommit
	writeJSON(w, http.StatusOK, detail)
}

// writePackResolutionError maps a pack resolution failure. An unknown name is
// a 404. A failure to read the pack's bytes (the commit tree, the scratch
// directory) is the server's own and a logged 500 — it says nothing about the
// pack, and its text (git's stderr) is not the caller's to act on. Everything
// else describes the pack or the ref the caller named — both documents
// present, a symlinked entry, a base naming a project pack, a constraint the
// pack's version does not satisfy, a manifest that does not validate — and is
// a 400 carrying that description.
func (api *API) writePackResolutionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pack.ErrPackNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "pack not found")
	case errors.Is(err, pack.ErrPackReadFailed):
		logUnexpected(api.log, err, "resolve pack")
		writeError(w, http.StatusInternalServerError, codeInternal, "the pack could not be read")
	default:
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
	}
}

// packDetailOf renders a resolved pack with its field origins.
func packDetailOf(resolved *pack.Resolved) packDetailResponse {
	runPack := resolved.Pack
	detail := packDetailResponse{
		Name:         runPack.Pack.Name,
		Version:      runPack.Pack.Version,
		Persona:      runPack.Pack.Persona,
		Description:  runPack.Pack.Description,
		Origin:       string(resolved.Origin),
		Entry:        runPack.Entry,
		Capabilities: append([]string(nil), runPack.Capabilities...),
		Budgets:      packBudgetsView{FixCycles: runPack.Budgets.FixCycles, AskToEdit: runPack.Budgets.AskToEdit},
		Checks:       packChecksView{Required: runPack.Checks.Required, Optional: runPack.Checks.Optional},
		FieldOrigins: resolved.FieldOrigins,
	}
	detail.Memory.Reads = make([]string, 0, len(runPack.Memory.Reads))
	for _, scope := range runPack.Memory.Reads {
		detail.Memory.Reads = append(detail.Memory.Reads, string(scope))
	}
	detail.Memory.Writes = runPack.Memory.Writes
	for _, approval := range runPack.Approvals {
		detail.Approvals = append(detail.Approvals, packApprovalRef{
			Name: approval.Name, Stage: approval.Stage, Artifact: approval.Artifact, Unlocks: approval.Unlocks,
		})
	}
	stageIDs := make([]string, 0, len(runPack.Stages))
	for stageID := range runPack.Stages {
		stageIDs = append(stageIDs, stageID)
	}
	sort.Strings(stageIDs)
	for _, stageID := range stageIDs {
		stage := runPack.Stages[stageID]
		view := packStageView{
			ID: stageID, Gate: string(stage.Gate), Tier: stage.Tier, Role: stage.Role,
			Prompt: stage.Prompt, Terminal: stage.Terminal(),
			Capabilities: append([]string(nil), stage.Capabilities...),
		}
		for _, transition := range stage.Transitions {
			view.Transitions = append(view.Transitions, packTransitionView{To: transition.To, Condition: transition.Condition})
		}
		detail.Stages = append(detail.Stages, view)
	}
	return detail
}
