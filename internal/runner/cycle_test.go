package runner

import (
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestNextCycleForStage covers D4's cycle-assignment table: fresh entry -> 0,
// repeat entry -> 1, resume inherits, and a third stage's cycles are
// independent of the first stage's. The fake store seeds invocations directly
// and the runner derives the cycle off them, mirroring how a worker restart
// recomputes the same answer from committed rows.
func TestNextCycleForStage(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{ID: "T-cycle", TenantID: "tn", UserID: "us", State: "running"}
	store := newFakeStore(task, sqlc.Project{})
	runner := New(Deps{Store: store})
	ctx := t.Context()

	// Fresh entry into "fix": no invocations yet -> cycle 0.
	cycle, err := runner.nextCycleForStage(ctx, task, "fix", nil)
	if err != nil {
		t.Fatalf("fresh entry: %v", err)
	}
	if cycle != 0 {
		t.Errorf("fresh entry cycle = %d, want 0", cycle)
	}

	// Seed a fix invocation at cycle 0 (as the runner would after the first run).
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		ID: "inv-1", TaskID: "T-cycle", TenantID: "tn", Stage: "fix", Sequence: 1, Cycle: 0,
	})

	// Repeat fresh entry into "fix" -> cycle 1.
	cycle, err = runner.nextCycleForStage(ctx, task, "fix", nil)
	if err != nil {
		t.Fatalf("repeat entry: %v", err)
	}
	if cycle != 1 {
		t.Errorf("repeat entry cycle = %d, want 1", cycle)
	}

	// Resume inherits the resumed invocation's cycle (0), so a continue does
	// not consume budget.
	resumed := store.invocations[0]
	cycle, err = runner.nextCycleForStage(ctx, task, "fix", &resumed)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if cycle != 0 {
		t.Errorf("resume cycle = %d, want 0 (inherited)", cycle)
	}

	// A different stage's cycles are independent: "review" has never run -> 0,
	// even though "fix" is at cycle 0/1.
	cycle, err = runner.nextCycleForStage(ctx, task, "review", nil)
	if err != nil {
		t.Fatalf("independent stage: %v", err)
	}
	if cycle != 0 {
		t.Errorf("review cycle = %d, want 0 (independent)", cycle)
	}

	// Seed review at cycle 0; another review entry -> 1, while fix is unaffected.
	store.invocations = append(store.invocations, sqlc.StageInvocation{
		ID: "inv-2", TaskID: "T-cycle", TenantID: "tn", Stage: "review", Sequence: 2, Cycle: 0,
	})
	cycle, err = runner.nextCycleForStage(ctx, task, "review", nil)
	if err != nil {
		t.Fatalf("review repeat: %v", err)
	}
	if cycle != 1 {
		t.Errorf("review repeat cycle = %d, want 1", cycle)
	}
	cycle, err = runner.nextCycleForStage(ctx, task, "fix", nil)
	if err != nil {
		t.Fatalf("fix after review: %v", err)
	}
	if cycle != 1 {
		t.Errorf("fix cycle after review activity = %d, want 1 (independent)", cycle)
	}
}

// TestMaxCycleForStagesFake_Sentinel confirms the fake returns -1 (the COALESCE
// sentinel) when no invocation matches, so nextCycleForStage maps it to 0
// entries — the same shape the real query produces.
func TestMaxCycleForStagesFake_Sentinel(t *testing.T) {
	t.Parallel()
	task := sqlc.Task{ID: "T-sentinel", TenantID: "tn", UserID: "us", State: "running"}
	store := newFakeStore(task, sqlc.Project{})
	max, err := store.MaxCycleForStages(t.Context(), sqlc.MaxCycleForStagesParams{
		TaskID: "T-sentinel", TenantID: "tn", Column3: []string{"fix"},
	})
	if err != nil {
		t.Fatalf("MaxCycleForStages: %v", err)
	}
	if max != -1 {
		t.Errorf("empty MaxCycleForStages = %d, want -1 sentinel", max)
	}
}
