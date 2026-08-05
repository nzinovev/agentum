package runner

import (
	"testing"

	"github.com/nzinovev/agentum/internal/pack"
)

// reviewBranchStage is the canonical branching stage: verdict-conditioned edges
// to a fixer and to done, no fallback (the two verdicts cover the enum).
func reviewBranchStage() pack.Stage {
	return pack.Stage{Transitions: []pack.Transition{
		{To: "fix", Condition: `verdict == "changes_requested"`},
		{To: "done", Condition: `verdict == "approved"`},
	}}
}

func TestResolveTransition_VerdictMatches(t *testing.T) {
	t.Parallel()
	stage := reviewBranchStage()
	fixerStages := []string{"fix"}

	// changes_requested -> fix, within budget.
	transitionContext := TransitionContext{
		Verdict: "changes_requested", FixCyclesUsed: 0, Budget: 2, FixerStages: fixerStages,
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("changes_requested: %v", err)
	}
	if resolution.StopReason != "" {
		t.Errorf("changes_requested: StopReason = %q, want empty", resolution.StopReason)
	}
	if resolution.To != "fix" {
		t.Errorf("changes_requested: To = %q, want fix", resolution.To)
	}
	if resolution.Cycle != 0 {
		t.Errorf("changes_requested: Cycle = %d, want 0 (first fixer entry)", resolution.Cycle)
	}

	// approved -> done.
	transitionContext.Verdict = "approved"
	resolution, err = ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("approved: %v", err)
	}
	if resolution.To != "done" {
		t.Errorf("approved: To = %q, want done", resolution.To)
	}
	if resolution.StopReason != "" {
		t.Errorf("approved: StopReason = %q, want empty", resolution.StopReason)
	}
}

func TestResolveTransition_BudgetGuard(t *testing.T) {
	t.Parallel()
	stage := reviewBranchStage()
	fixerStages := []string{"fix"}

	// fix_cycles: 1 allows exactly one fixer entry. At 0 used, the first entry
	// proceeds (0 < 1).
	transitionContext := TransitionContext{
		Verdict: "changes_requested", FixCyclesUsed: 0, Budget: 1, FixerStages: fixerStages,
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("first fixer entry: %v", err)
	}
	if resolution.To != "fix" {
		t.Errorf("first fixer entry: To = %q, want fix", resolution.To)
	}

	// At 1 used, the budget is spent: FixCyclesUsed >= Budget refuses the entry.
	transitionContext.FixCyclesUsed = 1
	resolution, err = ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("second fixer entry: %v", err)
	}
	if resolution.StopReason != "fix_budget_exhausted" {
		t.Errorf("second fixer entry: StopReason = %q, want fix_budget_exhausted", resolution.StopReason)
	}
	if resolution.To != "" {
		t.Errorf("second fixer entry: To = %q, want empty (halted)", resolution.To)
	}
}

func TestResolveTransition_BudgetZeroAllowsNoFixerEntry(t *testing.T) {
	t.Parallel()
	stage := reviewBranchStage()
	fixerStages := []string{"fix"}
	// fix_cycles: 0 means "no fixer entry at all": even the first entry (0
	// used) is refused because 0 >= 0.
	transitionContext := TransitionContext{
		Verdict: "changes_requested", FixCyclesUsed: 0, Budget: 0, FixerStages: fixerStages,
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("budget zero: %v", err)
	}
	if resolution.StopReason != "fix_budget_exhausted" {
		t.Errorf("budget zero: StopReason = %q, want fix_budget_exhausted", resolution.StopReason)
	}
}

func TestResolveTransition_VerdictUnreadableHalts(t *testing.T) {
	t.Parallel()
	stage := reviewBranchStage()
	transitionContext := TransitionContext{
		Unreadable:    true,
		VerdictReason: "verdict.json: invalid JSON: unexpected token",
		FixerStages:   []string{"fix"}, Budget: 2,
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("unreadable: %v", err)
	}
	if resolution.StopReason == "" {
		t.Fatal("unreadable: StopReason empty, want verdict_unreadable")
	}
	if resolution.To != "" {
		t.Errorf("unreadable: To = %q, want empty (halted)", resolution.To)
	}
	// The parse-error reason is threaded into the stop reason.
	if resolution.StopReason != "verdict_unreadable: verdict.json: invalid JSON: unexpected token" {
		t.Errorf("unreadable: StopReason = %q, want the reason threaded", resolution.StopReason)
	}
}

func TestResolveTransition_VerdictUnreadableAbsentNoReason(t *testing.T) {
	t.Parallel()
	stage := reviewBranchStage()
	// Absent verdict (no artifact): Unreadable true, empty reason.
	transitionContext := TransitionContext{
		Unreadable: true, FixerStages: []string{"fix"}, Budget: 2,
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("absent: %v", err)
	}
	if resolution.StopReason != "verdict_unreadable" {
		t.Errorf("absent: StopReason = %q, want verdict_unreadable", resolution.StopReason)
	}
}

func TestResolveTransition_FirstMatchWins(t *testing.T) {
	t.Parallel()
	// A stage with a fix_cycles edge before a verdict edge: the first matching
	// edge wins, so a low fix_cycles routes to fix even when the verdict would
	// say approved.
	stage := pack.Stage{Transitions: []pack.Transition{
		{To: "fix", Condition: `fix_cycles < 2`},
		{To: "done", Condition: `verdict == "approved"`},
	}}
	transitionContext := TransitionContext{
		Verdict: "approved", FixCyclesUsed: 0, Budget: 5, FixerStages: []string{"fix"},
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("first-match: %v", err)
	}
	if resolution.To != "fix" {
		t.Errorf("first-match: To = %q, want fix (fix_cycles < 2 matched first)", resolution.To)
	}
}

func TestResolveTransition_NonFixerTargetBypassesBudget(t *testing.T) {
	t.Parallel()
	// A transition to a non-fixer stage is never budget-guarded, even when the
	// fixer budget is exhausted.
	stage := pack.Stage{Transitions: []pack.Transition{
		{To: "done", Condition: `verdict == "approved"`},
	}}
	transitionContext := TransitionContext{
		Verdict: "approved", FixCyclesUsed: 5, Budget: 1, FixerStages: []string{"fix"},
	}
	resolution, err := ResolveTransition(stage, "review", transitionContext)
	if err != nil {
		t.Fatalf("non-fixer: %v", err)
	}
	if resolution.StopReason != "" {
		t.Errorf("non-fixer: StopReason = %q, want empty (budget does not bind)", resolution.StopReason)
	}
	if resolution.To != "done" {
		t.Errorf("non-fixer: To = %q, want done", resolution.To)
	}
}
