package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nzinovev/agentum/internal/publish"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

type planEditBudget struct {
	Used int `json:"used"`
	Max  int `json:"max"`
}

type runDirtyEntry struct {
	Status string `json:"status"`
	Path   string `json:"path"`
}

type runRestoreTarget struct {
	Label  string `json:"label"`
	Commit string `json:"commit"`
}

type runWorktreeView struct {
	State         string            `json:"state"`
	Path          string            `json:"path,omitempty"`
	Head          string            `json:"head,omitempty"`
	Dirty         bool              `json:"dirty"`
	DirtyEntries  []runDirtyEntry   `json:"dirty_entries"`
	RestoreTarget *runRestoreTarget `json:"restore_target,omitempty"`
	LastError     *runResourceError `json:"last_error,omitempty"`
}

type runResourceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// populateRunReadState adds the live local resources and the pinned pack's
// request-changes budget to the run read. Destructive decisions use the live
// worktree HEAD, so a stored result commit cannot stand in for this read.
func (api *API) populateRunReadState(ctx context.Context, run sqlc.Run, response *runResponse) error {
	if run.StopReason == "base_not_on_target" || run.StopReason == "base_target_unverifiable" {
		remote := api.publication.remote
		if remote == "" {
			remote = "origin"
		}
		branch := api.publication.baseBranch
		if branch == "" {
			branch, _ = publish.BaseBranchFromRef(run.BaseRef, remote)
		}
		if branch != "" {
			response.PublicationTargetRef = "refs/remotes/" + remote + "/" + branch
		}
	}
	response.Worktree = &runWorktreeView{State: "not_created", DirtyEntries: []runDirtyEntry{}}
	response.BranchState = "not_created"
	if !run.BaseCommit.Valid || run.BaseCommit.String == "" {
		return nil
	}
	project, err := api.queries.GetProject(ctx, sqlc.GetProjectParams{ID: run.ProjectID, TenantID: run.TenantID})
	if err != nil {
		return fmt.Errorf("read project for local resources: %w", err)
	}
	checkoutPath := run.CheckoutPath
	if checkoutPath == "" {
		checkoutPath = project.RepoPath
	}
	wtPath := worktree.PathFor(checkoutPath, run.ID)
	response.Worktree.Path = wtPath
	response.Worktree.State = "removed"
	response.BranchState = "removed"
	if _, statErr := os.Stat(checkoutPath); statErr != nil {
		return nil
	}
	budget, budgetErr := api.resolveRunPack(ctx, run)
	if budgetErr == nil && budget != nil {
		used, countErr := api.queries.CountJobsOfKindForRun(ctx, sqlc.CountJobsOfKindForRunParams{
			RunID: run.ID, TenantID: run.TenantID, Kind: jobKindAskToEdit,
		})
		if countErr != nil {
			return fmt.Errorf("count plan edits: %w", countErr)
		}
		response.PlanEdits = &planEditBudget{Used: int(used), Max: budget.Budgets.AskToEdit}
	}
	manager := worktree.New()
	if _, pathErr := os.Stat(wtPath); pathErr == nil {
		response.Worktree.State = "present"
		if !worktree.DirPresent(wtPath) {
			response.Worktree.LastError = &runResourceError{Code: "worktree_unreadable", Message: "worktree link is missing; HEAD and uncommitted changes cannot be read"}
		} else if head, headErr := manager.HeadCommit(ctx, wtPath); headErr != nil {
			response.Worktree.LastError = &runResourceError{Code: "worktree_unreadable", Message: headErr.Error()}
		} else {
			response.Worktree.Head = head
			changes, changesErr := manager.WorktreeChanges(ctx, wtPath)
			if changesErr != nil {
				response.Worktree.LastError = &runResourceError{Code: "worktree_unreadable", Message: changesErr.Error()}
			} else {
				for _, change := range changes {
					response.Worktree.DirtyEntries = append(response.Worktree.DirtyEntries, runDirtyEntry{Status: change.Code, Path: change.Path})
				}
				response.Worktree.Dirty = len(changes) > 0
			}
		}
	}
	if tip, tipErr := manager.ResolveRef(ctx, checkoutPath, "refs/heads/"+response.Branch); tipErr == nil {
		response.BranchState = "present"
		response.BranchTip = tip
	}
	resourceJobs := []struct {
		kind      string
		state     *string
		lastError **runResourceError
	}{
		{kind: "discard_worktree", state: &response.Worktree.State, lastError: &response.Worktree.LastError},
		{kind: "cleanup", state: &response.BranchState, lastError: &response.BranchLastError},
	}
	resourceJobSeen := false
	for _, resource := range resourceJobs {
		job, jobErr := api.queries.LatestResourceJobForRun(ctx, sqlc.LatestResourceJobForRunParams{
			RunID: run.ID, TenantID: run.TenantID, UserID: run.UserID, Kind: resource.kind,
		})
		if errors.Is(jobErr, sql.ErrNoRows) {
			continue
		}
		if jobErr != nil {
			return fmt.Errorf("read %s status: %w", resource.kind, jobErr)
		}
		resourceJobSeen = true
		if job.Status == "pending" || job.Status == "running" {
			*resource.lastError = nil
			if *resource.state == "present" {
				*resource.state = "removing"
			}
		}
		if job.Status == "failed" && job.LastError.Valid {
			*resource.lastError = &runResourceError{Code: "job_failed", Message: job.LastError.String}
		}
	}
	if !resourceJobSeen && response.Worktree.State == "removed" && response.BranchState == "removed" {
		response.Worktree.State = "not_created"
		response.BranchState = "not_created"
	}
	checkpoint, checkpointErr := api.queries.LatestCheckpointForRun(ctx, sqlc.LatestCheckpointForRunParams{
		RunID: run.ID, TenantID: run.TenantID,
	})
	if checkpointErr == nil {
		response.Worktree.RestoreTarget = &runRestoreTarget{Label: checkpoint.Label, Commit: checkpoint.CommitSha}
	} else if errors.Is(checkpointErr, sql.ErrNoRows) {
		response.Worktree.RestoreTarget = &runRestoreTarget{Label: "base", Commit: run.BaseCommit.String}
	} else {
		return fmt.Errorf("read restore target: %w", checkpointErr)
	}
	return nil
}
