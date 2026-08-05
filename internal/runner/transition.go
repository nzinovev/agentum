package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TransitionContext is the durable-state bundle ResolveTransition reads. The
// runner fills it BEFORE calling Evaluate, so the gate logic and the branch
// logic remain one pure function (D3). Every field is value-derived from
// committed state (the verdict artifact, the result, the cycle counter, the
// budget) so two workers computing it for the same task agree.
type TransitionContext struct {
	// Verdict is the parsed verdict.json verdict of the transitioning stage.
	// Empty when no verdict artifact exists or it failed to parse — the
	// Unreadable flag distinguishes the two.
	Verdict string

	// Status is the transitioning stage's result.json status.
	Status string

	// FixCyclesUsed counts fixer-role entries already made in this run
	// (MAX(cycle) + 1 over the fixer set, 0 when none has run). The budget
	// guard compares it against Budget.
	FixCyclesUsed int

	// Budget is budgets.fix_cycles. With FixCyclesUsed it bounds fixer entries:
	// the next fixer entry is refused when FixCyclesUsed >= Budget.
	Budget int

	// FixerStages is the pack's fixer-role stage ids. Carried for evidence and
	// so ResolveTransition can test whether the resolved target is a fixer.
	FixerStages []string

	// Unreadable is true when the stage sources a verdict condition but no
	// parseable verdict.json could be read. The runner surfaces it as the
	// verdict_unreadable retryable stop-point. Distinguishing absent (no
	// artifact) from unparseable (a real parse error) is the caller's job;
	// both set this flag, but VerdictReason carries the parse-error text when
	// applicable so the stop record names the cause.
	Unreadable bool

	// VerdictReason carries the verdict-read error text when Unreadable is true
	// due to a parse failure (not an absent artifact). Empty for the absent
	// case and for a clean read.
	VerdictReason string

	// FindingsCount is the number of findings in the parsed verdict, carried so
	// the routing block can point the fixer at the findings without re-reading
	// and re-parsing the artifact to fill one integer (D7 evidence + step 7
	// hand-off).
	FindingsCount int
}

// Resolution is ResolveTransition's verdict: where the pipeline goes next, and
// whether it should halt instead. State is represented ONCE — StopReason is
// empty when the pipeline proceeds, otherwise it names the halt cause
// (fix_budget_exhausted / verdict_unreadable). There are no parallel booleans
// to drift out of sync.
type Resolution struct {
	// To is the resolved target stage id. Empty when StopReason is set.
	To string

	// Condition is the matched condition's raw text (the auditable form for the
	// transition record and the stage.transition event).
	Condition string

	// Verdict echoes the verdict that matched, when the matched edge was a
	// verdict condition; empty otherwise.
	Verdict string

	// Cycle is the prospective cycle the next invocation of To will receive
	// (MaxCycleForStages([To]) + 1, or 0 when To has never run). It populates
	// stage.transition's cycle and TransitionRecord.Cycle (commit 8) without a
	// second computation.
	Cycle int

	// StopReason is empty when the pipeline proceeds to To. When non-empty it
	// names the halt cause and To is empty:
	//   - fix_budget_exhausted: the resolved target is a fixer stage and
	//     FixCyclesUsed >= Budget (the N+1-th fixer entry is refused).
	//   - verdict_unreadable: the stage sources a verdict condition but no
	//     parseable verdict.json exists; VerdictReason carries the detail.
	StopReason string
}

// stageTransition is the entry descriptor threaded through runLoop →
// processStage → invokeStage. It carries the transition that brought the run
// to the current stage, so processStage can emit the stage.transition event
// and (in commit 7) hand the fixer the findings, and commit 8 can record the
// TransitionRecord — without any of them re-deriving the facts.
type stageTransition struct {
	// from is the stage that transitioned to the current one. Empty for the
	// entry stage (no predecessor).
	from string

	// condition is the matched condition's raw text. Empty for an unconditional
	// edge or the entry stage.
	condition string

	// verdict is the matched verdict, when the edge was verdict-conditioned.
	verdict string

	// verdictPath is the orchestrator-constructed path to the predecessor's
	// verdict.json artifact, for the fixer hand-off (commit 7).
	verdictPath string

	// cycle is the prospective cycle the current stage's invocation will
	// receive (carried from the Resolution so commit 8's TransitionRecord has
	// a source).
	cycle int

	// findingsCount is the number of findings in the predecessor's verdict,
	// carried so commit 7's ReviewFindings.Count does not re-parse the artifact.
	findingsCount int
}

// buildTransitionContext assembles the durable-state bundle for stageID from
// committed rows and the artifact store. Signature takes (task, taskPack,
// stageID, result) rather than stageRun because stageRun is built after
// entryPoint runs (the advance path resolves a transition before the loop
// starts).
//
// A nil artifact store (unit tests) yields an empty verdict — which is exactly
// the verdict_unreadable path, so nothing fails silently. A real artifact that
// fails to parse is logged and the error text threaded into VerdictReason so
// the stop record names the cause; the absent case (no current revision) sets
// Unreadable with an empty reason.
func (runner *Runner) buildTransitionContext(
	ctx context.Context,
	task sqlc.Task,
	taskPack *pack.Pack,
	stageID string,
	result *agent.ResultJSON,
) (TransitionContext, error) {
	stage, ok := taskPack.Stages[stageID]
	if !ok {
		return TransitionContext{}, fmt.Errorf("build transition context: stage %q not in pack", stageID)
	}

	transitionContext := TransitionContext{
		Budget:      taskPack.Budgets.FixCycles,
		FixerStages: taskPack.FixerStages(),
	}

	if result != nil {
		transitionContext.Status = string(result.Status)
	}

	fixerCycles, err := runner.store.MaxCycleForStages(ctx, sqlc.MaxCycleForStagesParams{
		TaskID: task.ID, TenantID: task.TenantID, Column3: transitionContext.FixerStages,
	})
	if err != nil {
		return TransitionContext{}, fmt.Errorf("load fixer cycles: %w", err)
	}
	if fixerCycles < 0 {
		// -1 sentinel: no fixer stage has run yet -> 0 used.
		transitionContext.FixCyclesUsed = 0
	} else {
		transitionContext.FixCyclesUsed = int(fixerCycles) + 1
	}

	// Only read the verdict artifact when the stage actually sources a verdict
	// condition. A stage that does not branch on verdict has no verdict.json to
	// read; treating its absence as unreadable would pause every non-review
	// stage.
	if stage.SourcesVerdict() {
		verdict, unreadable, reason := runner.readVerdict(ctx, task, stageID)
		transitionContext.Verdict = string(verdict.Verdict)
		transitionContext.Unreadable = unreadable
		transitionContext.VerdictReason = reason
		transitionContext.FindingsCount = len(verdict.Findings)
	}

	return transitionContext, nil
}

// readVerdict reads and parses the stage's verdict.json from the artifact
// store. Returns (verdict, unreadable, reason):
//   - absent (no current revision, or nil store): empty verdict, unreadable=true,
//     empty reason — the retryable shape, no log spam;
//   - unparseable: empty verdict, unreadable=true, reason=the parse error text
//     — logged so the cause is visible, and threaded into the stop record.
func (runner *Runner) readVerdict(ctx context.Context, task sqlc.Task, stageID string) (agent.VerdictJSON, bool, string) {
	if runner.art == nil {
		return agent.VerdictJSON{}, true, ""
	}
	revision, err := runner.art.Current(ctx, task.TenantID, task.ID, stageID+"/"+agent.VerdictFileName)
	if err != nil {
		if errors.Is(err, artifacts.ErrNoCurrentRevision) {
			return agent.VerdictJSON{}, true, ""
		}
		runner.log.Warn("read verdict artifact: current", "task", task.ID, "stage", stageID, "error", err)
		return agent.VerdictJSON{}, true, ""
	}
	bytes, err := runner.art.GetBytes(ctx, task.TenantID, revision.ID)
	if err != nil {
		runner.log.Warn("read verdict artifact: bytes", "task", task.ID, "stage", stageID, "error", err)
		return agent.VerdictJSON{}, true, ""
	}
	verdict, parseErr := agent.ParseVerdictJSON(bytes)
	if parseErr != nil {
		runner.log.Warn("parse verdict artifact", "task", task.ID, "stage", stageID, "error", parseErr)
		return agent.VerdictJSON{}, true, parseErr.Error()
	}
	return verdict, false, ""
}

// ResolveTransition is the pure resolver called from both the loop path
// (inside Evaluate) and the advance job path (inside entryPoint). It matches
// the stage's first applicable transition, then applies the D4 budget guard
// when the target is a fixer stage. One resolver, not two implementations.
//
// The guard is `FixCyclesUsed >= Budget`, not `>`: FixCyclesUsed counts entries
// already made, so at N entries the budget is spent and the next entry is the
// N+1-th. Under `>` a budget of 2 would permit three fixer runs. The `>=` form
// is also what makes fix_cycles: 0 mean "no fixer entry at all".
func ResolveTransition(stage pack.Stage, stageID string, transitionContext TransitionContext) (Resolution, error) {
	// A verdict-sourcing stage that could not read its verdict halts before
	// matching: routing on an unknown verdict would be a guess. This is checked
	// here (not in buildTransitionContext) so the rule lives with the resolver.
	if transitionContext.Unreadable {
		reason := "verdict_unreadable"
		if transitionContext.VerdictReason != "" {
			reason = "verdict_unreadable: " + transitionContext.VerdictReason
		}
		return Resolution{StopReason: reason}, nil
	}

	matched, found, err := stage.NextTransition(pack.ConditionInput{
		Verdict:   transitionContext.Verdict,
		Status:    transitionContext.Status,
		FixCycles: transitionContext.FixCyclesUsed,
		Budget:    transitionContext.Budget,
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve transition on stage %q: %w", stageID, err)
	}
	if !found {
		// D6 rule 3 makes this unreachable for a valid pack: a branching stage
		// is total. Reaching it means the validator was bypassed — fail with a
		// precise error, the same choice checks.go makes for a dirty tree.
		return Resolution{}, fmt.Errorf("resolve transition on stage %q: no transition matched (validator bypassed)", stageID)
	}

	// D4 budget guard: refuse the next fixer entry when the budget is spent.
	if isFixerStage(matched.To, transitionContext.FixerStages) && transitionContext.FixCyclesUsed >= transitionContext.Budget {
		return Resolution{
			Condition: matched.Condition, Verdict: transitionContext.Verdict,
			StopReason: "fix_budget_exhausted",
		}, nil
	}

	cycle := resolveProspectiveCycle(matched.To, transitionContext)
	return Resolution{
		To: matched.To, Condition: matched.Condition,
		Verdict: transitionContext.Verdict, Cycle: cycle,
	}, nil
}

// resolveProspectiveCycle returns the cycle the next invocation of target will
// receive. For a fixer target it is exactly FixCyclesUsed (the count of fixer
// entries already made): cycle 0 for the first entry, 1 for the second, etc.
// For a non-fixer target the precise per-stage cycle is computed at invoke
// time (nextCycleForStage); the budget-relevant value carried here is what the
// transition record and the stage.transition event need for the loop case, and
// 0 is the honest "first entry" for a non-fixing target.
func resolveProspectiveCycle(target string, transitionContext TransitionContext) int {
	if isFixerStage(target, transitionContext.FixerStages) {
		return transitionContext.FixCyclesUsed
	}
	return 0
}

// isFixerStage reports whether target is one of the pack's fixer-role stages.
func isFixerStage(target string, fixerStages []string) bool {
	for _, fixer := range fixerStages {
		if fixer == target {
			return true
		}
	}
	return false
}

// verdictPayload is the stage.transition event body (D7). Emitted at the
// resolution point so the branch is auditable even when the next stage never
// starts. Exhaustion needs no new event type: task.state_changed already
// carries stop_reason.
type verdictPayload struct {
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Condition string `json:"condition,omitempty"`
	Cycle     int    `json:"cycle,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
}

func (runner *Runner) emitStageTransition(ctx context.Context, task sqlc.Task, payload verdictPayload) {
	runner.emit(ctx, task, EvStageTransition, payload)
}
