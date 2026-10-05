package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/nzinovev/agentum/internal/pack"
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

type runRouteGraph struct {
	Description string            `json:"description"`
	Version     string            `json:"version"`
	Entry       string            `json:"entry"`
	FixCycles   int               `json:"fix_cycles"`
	AskToEdit   int               `json:"ask_to_edit"`
	Approvals   []packApprovalRef `json:"approvals"`
	Nodes       []packStageView   `json:"nodes"`
	Progress    *runRouteProgress `json:"progress,omitempty"`
}

// populateRunReadState adds the live local resources and the pinned pack's
// request-changes budget to the run read. Destructive decisions use the live
// worktree HEAD, so a stored result commit cannot stand in for this read.
func (api *API) populateRunReadState(ctx context.Context, run sqlc.Run, response *runResponse) error {
	if err := api.populateRunRoute(ctx, run, response); err != nil {
		return err
	}
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
	if response.RouteGraph != nil {
		used, countErr := api.queries.CountJobsOfKindForRun(ctx, sqlc.CountJobsOfKindForRunParams{
			RunID: run.ID, TenantID: run.TenantID, Kind: jobKindAskToEdit,
		})
		if countErr != nil {
			return fmt.Errorf("count plan edits: %w", countErr)
		}
		response.PlanEdits = &planEditBudget{Used: int(used), Max: response.RouteGraph.AskToEdit}
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

func (api *API) populateRunRoute(ctx context.Context, run sqlc.Run, response *runResponse) error {
	if !run.RouteSource.Valid || run.RouteSource.String == "" {
		return nil
	}
	resolvedPack, resolveErr := api.resolveRunPack(ctx, run)
	if resolveErr != nil {
		response.RouteResolveError = safeRouteResolveError(resolveErr)
		return nil
	}
	if resolvedPack != nil {
		response.RouteGraph = routeGraphOf(resolvedPack)
		if api.queries != nil {
			progress, err := api.routeProgress(ctx, run, response.RouteGraph)
			if err != nil {
				return fmt.Errorf("read route progress: %w", err)
			}
			response.RouteGraph.Progress = progress
		}
	}
	return nil
}

// safeRouteResolveError keeps read failures useful without exposing checkout
// paths or raw git diagnostics through a polled run response.
func safeRouteResolveError(err error) *runResourceError {
	switch {
	case errors.Is(err, pack.ErrPackNotFound):
		return &runResourceError{Code: "pack_not_found", Message: "The selected pack is absent at this run's base commit."}
	case errors.Is(err, pack.ErrPackReadFailed):
		return &runResourceError{Code: "pack_read_failed", Message: "The selected pack could not be read from this run's base commit."}
	case errors.Is(err, pack.ErrBothPackFiles):
		return &runResourceError{Code: "pack_ambiguous", Message: "The selected project pack has both manifest.yaml and overrides.yaml at this run's base commit."}
	case errors.Is(err, pack.ErrNoPackFile):
		return &runResourceError{Code: "pack_manifest_missing", Message: "The selected project pack has no manifest.yaml or overrides.yaml at this run's base commit."}
	default:
		return &runResourceError{Code: "pack_resolve_failed", Message: "The selected pack could not be resolved from this run's base commit. Check its manifest at that commit."}
	}
}

func routeGraphOf(runPack *pack.Pack) *runRouteGraph {
	detail := packDetailOf(&pack.Resolved{Pack: runPack})
	for index := range detail.Stages {
		if detail.Stages[index].Terminal {
			detail.Stages[index].FinalReviewGate = true
		}
	}
	return &runRouteGraph{
		Description: detail.Description, Version: detail.Version, Entry: detail.Entry,
		FixCycles: detail.Budgets.FixCycles, Approvals: detail.Approvals, Nodes: detail.Stages,
		AskToEdit: detail.Budgets.AskToEdit,
	}
}
