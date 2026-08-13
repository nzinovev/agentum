package api

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/runner"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// finalReviewResponse is the payload assembled for the final-review screen (ADR
// 0003 D8). Every field is read from Postgres or the revisions store; the
// handler executes no git. Content is fetched through the existing
// revision-content endpoints — the payload carries ids, not blobs, so a large
// diff does not have to be inlined.
type finalReviewResponse struct {
	Task      taskResponse          `json:"task"`
	Plan      *finalReviewPlan      `json:"plan,omitempty"`
	Git       *finalReviewGit       `json:"git,omitempty"`
	Diff      *finalReviewDiff      `json:"diff,omitempty"`
	Stages    []finalReviewStage    `json:"stages,omitempty"`
	Review    *finalReviewVerdict   `json:"review,omitempty"`
	Checks    *finalReviewChecks    `json:"checks,omitempty"`
	Manifest  *finalReviewManifest  `json:"manifest,omitempty"`
	Decisions []finalReviewDecision `json:"decisions,omitempty"`
}

type finalReviewPlan struct {
	RevisionID  string `json:"revision_id"`
	Name        string `json:"name"`
	ApprovedBy  string `json:"approved_by,omitempty"`
	ApprovedAt  string `json:"approved_at,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

type finalReviewGit struct {
	Branch       string `json:"branch"`
	BaseCommit   string `json:"base_commit"`
	ResultCommit string `json:"result_commit,omitempty"`
}

type finalReviewDiff struct {
	PatchRevisionID string `json:"patch_revision_id,omitempty"`
	StatRevisionID  string `json:"stat_revision_id,omitempty"`
	Truncated       bool   `json:"truncated,omitempty"`
}

type finalReviewStage struct {
	Stage     string                `json:"stage"`
	Cycle     int32                 `json:"cycle"`
	Status    string                `json:"status"`
	Artifacts []finalReviewArtifact `json:"artifacts,omitempty"`
}

type finalReviewArtifact struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	RevisionID string `json:"revision_id"`
}

type finalReviewVerdict struct {
	Verdict  string          `json:"verdict,omitempty"`
	Findings []agent.Finding `json:"findings,omitempty"`
}

type finalReviewChecks struct {
	Commit          string `json:"commit,omitempty"`
	MandatoryPassed bool   `json:"mandatory_passed"`
}

type finalReviewManifest struct {
	Sealed           bool     `json:"sealed"`
	EvidenceComplete bool     `json:"evidence_complete"`
	Missing          []string `json:"missing,omitempty"`
}

type finalReviewDecision struct {
	Gate     string `json:"gate"`
	Decision string `json:"decision"`
	Actor    string `json:"actor"`
	At       string `json:"at"`
}

// handleFinalReview GET /api/v1/tasks/{id}/final-review
// Returns 200 in awaiting_final_review AND in done/cancelled/failed — "the
// result stays reviewable after the worktree is removed" is a requirement, not
// a nicety (ADR 0003 D8). 409 for a task that has not reached the gate.
// Assembled from durable rows + revisions only; the handler runs no git.
func (api *API) handleFinalReview(w http.ResponseWriter, r *http.Request) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if decision := authz.Can(r.Context(), principal, authz.ActionTaskRead, r.PathValue("id")); !decision.Allowed {
		writeError(w, http.StatusForbidden, codeForbidden, decision.Reason)
		return
	}
	task, err := api.queries.GetTask(r.Context(), sqlc.GetTaskParams{ID: r.PathValue("id"), TenantID: principal.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, codeNotFound, msgTaskNotFound)
			return
		}
		writeError(w, http.StatusBadRequest, codeBadInput, err.Error())
		return
	}
	state := engine.TaskState(task.State)
	if state != engine.StateAwaitingFinalReview && !engine.IsTerminal(state) {
		writeError(w, http.StatusConflict, codeIllegalTransition,
			"final-review requires awaiting_final_review or a terminal state; task is "+task.State)
		return
	}
	response := finalReviewResponse{Task: toTaskResponse(task)}
	response.Git = &finalReviewGit{
		Branch:       branchForTask(task.ID),
		BaseCommit:   nullStringOr(task.BaseCommit),
		ResultCommit: nullStringOr(task.ResultCommit),
	}
	// Plan: the pack-declared approval artifact's current revision, plus the
	// approval row that bound the human decision to it.
	if api.packs != nil {
		if taskPack, pErr := api.packs.Resolve(r.Context(), task.PipelinePack); pErr == nil {
			if approval, hasApproval := taskPack.SourceWriteApproval(); hasApproval {
				response.Plan = api.finalReviewPlan(r.Context(), task, approval)
			}
		}
	}
	// Decisions: the approval rows, oldest first.
	response.Decisions = api.finalReviewDecisions(r.Context(), task)
	// Stages + artifacts: current revisions grouped by stage.
	response.Stages = api.finalReviewStages(r.Context(), task)
	// Diff + review verdict: scanned from the current revisions.
	response.Diff, response.Review = api.finalReviewDiffAndVerdict(r.Context(), task, response.Stages)
	// Manifest summary (when wired).
	response.Manifest = api.finalReviewManifest(r.Context(), task)
	writeJSON(w, http.StatusOK, response)
}

// finalReviewPlan reads the approval artifact's current revision and the
// matching approval row. Returns nil when there is no current revision.
func (api *API) finalReviewPlan(ctx context.Context, task sqlc.Task, approval pack.Approval) *finalReviewPlan {
	revisionName := approval.Stage + "/" + approval.Artifact
	rev, err := api.queries.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: task.ID, TenantID: task.TenantID, Name: revisionName,
	})
	if err != nil {
		return nil
	}
	out := &finalReviewPlan{
		RevisionID:  rev.ID,
		Name:        revisionName,
		ContentHash: rev.ContentHash,
	}
	if row, err := api.queries.GetApproval(ctx, sqlc.GetApprovalParams{
		TenantID: task.TenantID, TaskID: task.ID, Name: approval.Name,
	}); err == nil {
		out.ApprovedBy = row.Actor
		out.ApprovedAt = row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	return out
}

// finalReviewDecisions reads every approval row for the task as the audit
// narrative of human decisions at both gates.
func (api *API) finalReviewDecisions(ctx context.Context, task sqlc.Task) []finalReviewDecision {
	rows, err := api.queries.ListApprovalsForTask(ctx, sqlc.ListApprovalsForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	})
	if err != nil {
		return nil
	}
	out := make([]finalReviewDecision, 0, len(rows))
	for _, row := range rows {
		gate := row.Name
		if gate == "final_review" {
			gate = "final_review"
		}
		out = append(out, finalReviewDecision{
			Gate: gate, Decision: row.Decision, Actor: row.Actor,
			At: row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		})
	}
	return out
}

// finalReviewStages groups the current artifact revisions by stage for the
// stages section of the payload. Only artifact-dir-resident kinds (result_json,
// verdict_json, plan_md, diff, diff_stat) are stage-scoped — their revision
// names are "<stage>/<file>". Agent-declared source files (kind file/code/test,
// names like "internal/api/foo.go") are worktree paths, not stage artifacts, and
// must not be split into a fake stage named "internal".
func (api *API) finalReviewStages(ctx context.Context, task sqlc.Task) []finalReviewStage {
	revs, err := api.queries.ListCurrentArtifactRevisionsForTask(ctx, sqlc.ListCurrentArtifactRevisionsForTaskParams{
		TaskID: task.ID, TenantID: task.TenantID,
	})
	if err != nil || len(revs) == 0 {
		return nil
	}
	byStage := map[string]*finalReviewStage{}
	order := []string{}
	for _, rev := range revs {
		if !isStageScopedKind(rev.Kind) {
			continue
		}
		stage, file, ok := splitStageFile(rev.Name)
		if !ok {
			continue
		}
		entry, exists := byStage[stage]
		if !exists {
			entry = &finalReviewStage{Stage: stage}
			byStage[stage] = entry
			order = append(order, stage)
		}
		entry.Artifacts = append(entry.Artifacts, finalReviewArtifact{
			Name: file, Kind: rev.Kind, RevisionID: rev.ID,
		})
	}
	out := make([]finalReviewStage, 0, len(order))
	for _, stage := range order {
		out = append(out, *byStage[stage])
	}
	return out
}

// isStageScopedKind reports whether a revision kind lives in a per-stage artifact
// dir (name "<stage>/<file>") rather than the worktree proper. Mirrors the
// runner's isArtifactDirKind so the payload and the sync redirect agree on which
// revisions are stage-scoped. Agent-declared source files (kind "file"/"code")
// are NOT stage-scoped and are excluded from the stages section.
func isStageScopedKind(kind string) bool {
	switch kind {
	case "result_json", "verdict_json", "plan_md", "diff", "diff_stat":
		return true
	}
	return false
}

// finalReviewDiffAndVerdict scans the current revisions for the latest diff and
// verdict artifacts. The diff's Truncated flag is read from the patch bytes
// (the orchestrator appends an explicit marker when it caps the patch — a size
// comparison would not distinguish a cap-sized patch from a truncated one). The
// review verdict is parsed from the latest reviewer's verdict.json so the
// payload carries the actual verdict + findings, not an empty struct. Both are
// best-effort: a missing store, a missing revision, or an unparseable verdict
// yield nil rather than failing the whole payload.
func (api *API) finalReviewDiffAndVerdict(ctx context.Context, task sqlc.Task, stages []finalReviewStage) (*finalReviewDiff, *finalReviewVerdict) {
	var diff *finalReviewDiff
	// Collect the diff revisions across reviewer stages (each reviewer stage
	// produced its own diff against base_commit; the payload surfaces all of
	// them by revision id, and flags truncation if ANY patch was capped).
	for _, stage := range stages {
		for _, artifact := range stage.Artifacts {
			if artifact.Kind == "diff" && diff == nil {
				diff = &finalReviewDiff{PatchRevisionID: artifact.RevisionID}
			}
			if artifact.Kind == "diff_stat" && diff != nil {
				diff.StatRevisionID = artifact.RevisionID
			}
		}
	}
	if diff != nil && diff.PatchRevisionID != "" && api.art != nil {
		if patchBytes, err := api.art.GetBytes(ctx, task.TenantID, diff.PatchRevisionID); err == nil {
			diff.Truncated = bytes.Contains(patchBytes, []byte(runner.DiffTruncationMarker))
		}
	}
	// Review verdict: read the latest reviewer's verdict.json content and parse
	// it. Iterate newest-stage-first so the most recent review wins.
	var review *finalReviewVerdict
	for index := len(stages) - 1; index >= 0 && review == nil; index-- {
		stage := stages[index]
		for _, artifact := range stage.Artifacts {
			if artifact.Kind != "verdict_json" || api.art == nil {
				continue
			}
			verdictBytes, err := api.art.GetBytes(ctx, task.TenantID, artifact.RevisionID)
			if err != nil || len(verdictBytes) == 0 {
				continue
			}
			parsed, parseErr := agent.ParseVerdictJSON(verdictBytes)
			if parseErr != nil {
				continue // unparseable verdict — surface nothing rather than a guess
			}
			review = &finalReviewVerdict{
				Verdict:  string(parsed.Verdict),
				Findings: parsed.Findings,
			}
		}
	}
	return diff, review
}

// finalReviewManifest summarizes the manifest seal/evidence state when the
// manifest service is wired. Returns nil otherwise.
func (api *API) finalReviewManifest(ctx context.Context, task sqlc.Task) *finalReviewManifest {
	if api.mfst == nil {
		return nil
	}
	body, sealInfo, _, err := api.mfst.Get(ctx, task.TenantID, task.ID)
	if err != nil {
		return nil
	}
	missing := body.MissingSections()
	return &finalReviewManifest{
		Sealed:           sealInfo.SealedAt.Valid,
		EvidenceComplete: body.IsEvidenceComplete(),
		Missing:          missing,
	}
}

// splitStageFile splits "<stage>/<file>" for the stages grouping. Returns
// ok=false when the name has no slash or an empty half.
func splitStageFile(name string) (stage, file string, ok bool) {
	for index := 0; index < len(name); index++ {
		if name[index] == '/' {
			if index == 0 || index == len(name)-1 {
				return "", "", false
			}
			return name[:index], name[index+1:], true
		}
	}
	return "", "", false
}

// branchForTask mirrors worktree.BranchFor without importing the runner. The
// branch name is a pure function of the task id, so the API can render it.
func branchForTask(taskID string) string { return "agentum/" + taskID }
