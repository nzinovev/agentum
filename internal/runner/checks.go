package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
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

// ErrDirtyTreeAtDeliveryBoundary is returned by enforceProjectChecks when the
// worktree is not clean at the delivery boundary. A dirty tree means something
// wrote after the checkpoint commit; running the checks against it would test
// content that exists in no commit while the manifest asserts a specific SHA was
// verified. The checks must not run and claim a commit they are not testing.
var ErrDirtyTreeAtDeliveryBoundary = errors.New("runner: worktree dirty at delivery boundary; cannot bind checks to a commit")

// enforceProjectChecks loads the project registry, resolves the effective set
// (project baseline ∪ pack ∪ task), runs it, records the evidence, and returns
// the report plus whether mandatory checks passed. A nil registry (project
// defines no checks) is a valid empty run that passes — recorded as Ran:false so
// the manifest never presents an absent gate as a cleared one.
//
// The registry is read from the task's base_commit (the lineage anchor, captured
// before the worktree is created), NOT from the agent-mutable worktree. This is
// the agent-immutability seam: an implementer with fs.write to its worktree
// cannot weaken the checks that gate its own delivery by editing .agentum.yaml,
// because the definitions come from a commit it cannot reach.
//
// Commit binding (E2): the verified commit is established BEFORE the checks run
// and cannot drift under them. After E1 the post-stage checkpoint is a real
// commit the orchestrator authored, so the worktree HEAD at this point is that
// commit. The tree is asserted clean first — a dirty tree means something wrote
// after the checkpoint, and the checks must not claim to have verified a commit
// whose tree they did not test. Only then does the executor run, and the
// recorded checks.commit is the checkpoint SHA, not a pre-run HEAD read that
// could be stale by the time the checks finish.
func (runner *Runner) enforceProjectChecks(ctx context.Context, run stageRun) (checks.Report, bool, error) {
	registry, err := runner.loadRegistryAtBaseCommit(ctx, run)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("load registry: %w", err)
	}
	// Independent strict parse, for the same reason this function keeps its own
	// registry load (PR #23): the value cached for rendering never reaches the
	// delivery gate. After the API boundary guarantees well-formed overrides, a
	// decode failure here is an invariant break and must be loud.
	taskOverrides, overridesErr := taskinput.ParseOverrides(run.task.Overrides)
	if overridesErr != nil {
		return checks.Report{}, false, fmt.Errorf("parse task overrides: %w", overridesErr)
	}
	packRequests := packCheckRequests(run.taskPack)
	taskRequests := taskCheckRequests(taskOverrides)
	set, err := checks.Resolve(registry, packRequests, taskRequests)
	if err != nil {
		return checks.Report{}, false, err
	}

	// Establish the verification commit before anything runs. The worktree HEAD
	// is the post-stage checkpoint commit (E1 created it); resolving it now and
	// asserting the tree is clean binds the checks to exactly that commit's tree.
	commit, err := runner.wt.HeadCommit(ctx, run.worktree.Root)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("read checkpoint commit for project checks: %w", err)
	}
	clean, err := runner.wt.IsClean(ctx, run.worktree.Root)
	if err != nil {
		return checks.Report{}, false, fmt.Errorf("check worktree clean for project checks: %w", err)
	}
	if !clean {
		// A dirty tree at the delivery boundary is a broken invariant, not a
		// recoverable state: the checkpoint commit exists but the working tree
		// has drifted off it, so checks against the tree would not be checks
		// against the commit. Fail the task rather than claim a verification we
		// cannot stand behind.
		return checks.Report{}, false, fmt.Errorf(
			"%w: worktree has uncommitted changes after checkpoint %s", ErrDirtyTreeAtDeliveryBoundary, commit,
		)
	}

	if set.Empty() {
		// The project defines no checks (or none are referenced). Record that
		// the executor reached the boundary and nothing blocked delivery, with
		// Ran:false so the manifest distinguishes "no checks defined" from "the
		// gate ran and cleared it." The commit is still recorded: the clean-tree
		// precondition held, so the boundary commit is the honest anchor even
		// when no check verified it.
		empty := checks.Report{Set: set, Commit: commit, Profile: checks.ProfileLabel}
		runner.recordCheckEvidence(ctx, run.task, empty, commit)
		return empty, true, nil
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
// the project defines no checks, which is a real configuration. An absent or
// empty base_commit is an error, not an early exit: this method runs only at the
// delivery boundary, so by construction the task has reached exactly the state
// whose entire purpose is to be anchored. A missing anchor there is a broken
// invariant, and fail-closed at this boundary is non-negotiable — returning a
// nil registry would route to an empty set and MandatoryPassed()=true vacuously,
// which is the mirror image of the fail-open defects PR C and PR D fixed.
func (runner *Runner) loadRegistryAtBaseCommit(ctx context.Context, run stageRun) (*checks.Registry, error) {
	baseCommit := run.task.BaseCommit.String
	if !run.task.BaseCommit.Valid || baseCommit == "" {
		return nil, errors.New("delivery boundary reached without a resolved base_commit; lineage anchor is required to gate delivery")
	}
	raw, err := runner.wt.FileAtCommit(ctx, checkoutPathOf(run.task, run.project), baseCommit, checks.ConfigFile)
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
// Ran reflects whether any check actually executed (!set.Empty()), so an empty
// set is recorded honestly rather than as a cleared gate. No-op when the
// manifest service is nil (unit tests).
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
	ran := report.Set != nil && !report.Set.Empty()
	patch := manifest.Body{
		Checks: &manifest.CheckEvidence{
			SetVersion:       setVersionOf(report.Set),
			RegistryRevision: registryRevisionOf(report.Set),
			Commit:           commit,
			Profile:          report.Profile,
			Ran:              ran,
			MandatoryPassed:  report.MandatoryPassed(),
			Results:          results,
		},
	}
	if err := runner.mfst.AddEvidence(ctx, task.TenantID, task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			return
		}
		runner.log.Warn("record check evidence", "task", task.ID, "error", err)
		runner.recordEvidenceGap(ctx, task, "checks", "", err)
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

// taskCheckRequests maps the typed task overrides onto the checks Request
// list. The names must exist in the project registry — the resolve step
// rejects unknown ones. The caller owns the strict decode: this function never
// sees raw bytes, so the old lenient `return nil` on a malformed column cannot
// come back through it.
func taskCheckRequests(overrides taskinput.Overrides) []checks.Request {
	requests := make([]checks.Request, 0, len(overrides.Checks.Required)+len(overrides.Checks.Optional))
	for _, name := range overrides.Checks.Required {
		requests = append(requests, checks.Request{Name: name, Required: true})
	}
	for _, name := range overrides.Checks.Optional {
		requests = append(requests, checks.Request{Name: name, Required: false})
	}
	if len(requests) == 0 {
		return nil
	}
	return requests
}
