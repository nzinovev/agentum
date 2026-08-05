package runner

import (
	"testing"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
)

// TestEffectiveRole_DriftWithCaps asserts the pack-side role mirror agrees
// with caps.DeriveRole on the fallback path (no explicit role). This is the
// only package that imports both, so it is the only place the drift can be
// caught. The mirror exists because internal/pack must not import caps; the two
// derivations MUST stay in sync or a stage's capability profile will be
// computed against the wrong template.
//
// Drift is a fallback-only assertion: when a stage declares an explicit role,
// EffectiveRole returns that role by design (it does NOT agree with
// DeriveRole). The precedence case is covered separately below.
func TestEffectiveRole_DriftWithCaps(t *testing.T) {
	t.Parallel()
	stageIDs := []string{
		"", "spec", "analyze", "design", "plan", "research",
		"review", "pre_review", "review-v2", "code_review",
		"implement", "build", "implement_v2",
		"fix", "quick-fix", "patch", "apply_patch",
		"unknown-stage", "step3", "do_thing",
	}
	for _, stageID := range stageIDs {
		stageID := stageID
		t.Run(stageID, func(t *testing.T) {
			t.Parallel()
			got := pack.EffectiveRole(stageID, pack.Stage{})
			want := string(caps.DeriveRole(stageID))
			if got != want {
				t.Errorf("EffectiveRole(%q, Stage{}) = %q, want %q (caps.DeriveRole)", stageID, got, want)
			}
		})
	}
}

// TestEffectiveRole_ExplicitRoleWins asserts the precedence rule: an explicit
// role on the stage overrides the convention. This is a precedence test, not a
// drift test — by design the explicit role does not agree with DeriveRole.
func TestEffectiveRole_ExplicitRoleWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		stageID string
		role    string
	}{
		{"spec", "reviewer"},
		{"review", "implementer"},
		{"fix", "analyst"},
		{"", "fixer"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.stageID+"/"+testCase.role, func(t *testing.T) {
			t.Parallel()
			stage := pack.Stage{Role: testCase.role}
			if got := pack.EffectiveRole(testCase.stageID, stage); got != testCase.role {
				t.Errorf("EffectiveRole(%q, Role=%q) = %q, want %q (explicit role wins)",
					testCase.stageID, testCase.role, got, testCase.role)
			}
		})
	}
}
