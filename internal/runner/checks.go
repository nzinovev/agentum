package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// runDeliveryChecks enforces the orchestrator-owned project checks at the final
// delivery boundary (a terminal stage reached, or a complete outcome that would
// fire the final gate). It runs the resolved set against the worktree HEAD —
// the post-stage checkpoint commit, which is also the commit that will become
// result_commit — records the outcome as manifest evidence, and blocks delivery
// when a mandatory check fails by failing the task. A nil executor (unit tests)
// is a no-op. A resolution error (e.g. a pack referencing an unknown check name)
// or a registry load error also fails the task: the pack is misconfigured for
// this project.
func (runner *Runner) runDeliveryChecks(ctx context.Context, run stageRun) error {
	if runner.checkExec == nil {
		return nil
	}
	report, mandatoryPassed, err := runner.enforceProjectChecks(ctx, run)
	if err != nil {
		return runner.failTask(ctx, run.task, fmt.Errorf("project checks: %w", err))
	}
	if !mandatoryPassed {
		failed := report.FailedMandatory()
		return runner.failTask(ctx, run.task,
			fmt.Errorf("mandatory project check failed: %s", strings.Join(failed, ", ")))
	}
	return nil
}

// enforceProjectChecks loads the project registry, resolves the effective set
// (project baseline ∪ pack ∪ task), runs it, records the evidence, and returns
// the report plus whether mandatory checks passed. A nil registry (project
// defines no checks) is a valid empty run that passes.
//
// The registry is read from the task's base_commit (the lineage anchor, captured
// before the worktree is created), NOT from the agent-mutable worktree. This is
// the agent-immutability seam: an implementer with fs.write to its worktree
// cannot weaken the checks that gate its own delivery by editing .agentum.yaml,
// because the definitions come from a commit it cannot reach. The checks still
// EXECUTE in the worktree, against the post-stage checkpoint HEAD.
func (runner *Runner) enforceProjectChecks(ctx context.Context, run stageRun) (checks.Report, bool, error) {
	registry, err := runner.loadRegistryAtBaseCommit(ctx, run)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("load registry: %w", err)
	}
	packRequests := packCheckRequests(run.taskPack)
	taskRequests := taskCheckRequests(run.task.Input)
	set, err := checks.Resolve(registry, packRequests, taskRequests)
	if err != nil {
		return checks.Report{}, false, err
	}

	if set.Empty() {
		// The project defines no checks (or none are referenced). Record that
		// the executor ran and nothing blocked delivery, so the manifest never
		// hides the absence — it surfaces it explicitly.
		empty := checks.Report{Set: set, Profile: checks.ProfileLabel}
		runner.recordCheckEvidence(ctx, run.task, empty, "")
		return empty, true, nil
	}

	commit, err := runner.wt.HeadCommit(ctx, run.worktree.Root)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("read head for project checks: %w", err)
	}
	report, err := runner.checkExec.Run(ctx, set, run.worktree.Root)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("run checks: %w", err)
	}
	report.Commit = commit
	runner.recordCheckEvidence(ctx, run.task, report, commit)
	runner.emit(ctx, run.task, EvProjectChecksRun, map[string]any{
		"commit":            commit,
		"set_version":       set.SetVersion,
		"registry_revision": set.RegistryRevision,
		"mandatory_passed":  report.MandatoryPassed(),
		"failed":            report.FailedMandatory(),
	})
	return report, report.MandatoryPassed(), nil
}

// loadRegistryAtBaseCommit reads .agentum.yaml from the project repo at the
// task's lineage anchor. A missing file (os.ErrNotExist) is a nil registry —
// the project defines no checks. Any other read/parse error fails the run.
func (runner *Runner) loadRegistryAtBaseCommit(ctx context.Context, run stageRun) (*checks.Registry, error) {
	baseCommit := run.task.BaseCommit.String
	if !run.task.BaseCommit.Valid || baseCommit == "" {
		// No anchor yet: nothing to gate against. Treat as no registry rather
		// than failing — checks are a delivery boundary, and a task without a
		// pinned base has not reached a deliverable state.
		return nil, nil
	}
	raw, err := runner.wt.FileAtCommit(ctx, run.project.RepoPath, baseCommit, checks.ConfigFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return checks.Parse(raw)
}

// recordCheckEvidence writes the project-check outcome into the manifest. The
// full per-check results (status, exit code, duration, capped output, reason,
// definition revision, source) become the evidence a final review reconstructs.
// No-op when the manifest service is nil (unit tests).
func (runner *Runner) recordCheckEvidence(ctx context.Context, task sqlc.Task, report checks.Report, commit string) {
	if runner.mfst == nil {
		return
	}
	results := make([]manifest.CheckResult, 0, len(report.Outcomes))
	for _, outcome := range report.Outcomes {
		results = append(results, manifest.CheckResult{
			Name:               outcome.Item.Definition.Name,
			Required:           outcome.Item.Required,
			Status:             string(outcome.Status),
			ExitCode:           outcome.ExitCode,
			DurationMs:         outcome.Duration.Milliseconds(),
			Stdout:             outcome.Stdout,
			Stderr:             outcome.Stderr,
			Reason:             outcome.Reason,
			DefinitionRevision: outcome.DefinitionRevision,
			Source:             strings.Join(outcome.Item.Sources, ","),
		})
	}
	patch := manifest.Body{
		Checks: &manifest.CheckEvidence{
			SetVersion:       setVersionOf(report.Set),
			RegistryRevision: registryRevisionOf(report.Set),
			Commit:           commit,
			Profile:          report.Profile,
			MandatoryPassed:  report.MandatoryPassed(),
			Results:          results,
		},
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record check evidence", "task", task.ID, "error", err)
	}
}

// setVersionOf / registryRevisionOf safely dereference a possibly-nil Set so the
// evidence helpers read cleanly. Kept as one-liners so the intent (nil-safe) is
// explicit at every call site.
func setVersionOf(set *checks.Set) string {
	if set == nil {
		return ""
	}
	return set.SetVersion
}

func registryRevisionOf(set *checks.Set) string {
	if set == nil {
		return ""
	}
	return set.RegistryRevision
}

// packCheckRequests turns the resolved pack's CheckPolicy into the checks
// Request list. Required names are mandatory; optional names run but do not
// block. Names are validated against the project registry at resolve time.
func packCheckRequests(taskPack *pack.Pack) []checks.Request {
	if taskPack == nil {
		return nil
	}
	requests := make([]checks.Request, 0, len(taskPack.Checks.Required)+len(taskPack.Checks.Optional))
	for _, name := range taskPack.Checks.Required {
		requests = append(requests, checks.Request{Name: name, Required: true})
	}
	for _, name := range taskPack.Checks.Optional {
		requests = append(requests, checks.Request{Name: name, Required: false})
	}
	return requests
}

// taskCheckRequests reads optional per-task check requests from the task input
// JSON. A task may carry `{"checks": {"required": [...], "optional": [...]}}` to
// add checks for this run; the names must exist in the project registry (the
// resolve step rejects unknown names). Absent or malformed input yields no
// requests — task input is agent-shaped, so a missing field is the common case.
func taskCheckRequests(input json.RawMessage) []checks.Request {
	if len(input) == 0 {
		return nil
	}
	var decoded struct {
		Checks struct {
			Required []string `json:"required"`
			Optional []string `json:"optional"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(input, &decoded); err != nil {
		return nil
	}
	requests := make([]checks.Request, 0, len(decoded.Checks.Required)+len(decoded.Checks.Optional))
	for _, name := range decoded.Checks.Required {
		requests = append(requests, checks.Request{Name: name, Required: true})
	}
	for _, name := range decoded.Checks.Optional {
		requests = append(requests, checks.Request{Name: name, Required: false})
	}
	return requests
}
