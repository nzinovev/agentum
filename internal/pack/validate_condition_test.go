package pack

import (
	"strings"
	"testing"
)

// TestValidate_LoopFixture loads the committed branching fixture (spec → review
// ⇄ fix → done) and expects it to validate. This is the happy path for D6
// rules 1–4: conditional verdict edges cover the enum, the cycle review→fix→
// review contains a fixer-role stage, and budgets.fix_cycles >= 1.
func TestValidate_LoopFixture(t *testing.T) {
	t.Parallel()
	loaded, err := Load("testdata/loop")
	if err != nil {
		t.Fatalf("Load testdata/loop: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate loop fixture: %v", err)
	}
	fixerStages := loaded.FixerStages()
	if len(fixerStages) != 1 || fixerStages[0] != "fix" {
		t.Errorf("FixerStages = %v, want [fix]", fixerStages)
	}
}

// branchingManifest is a known-good conditional manifest reused across negative
// cases; each case mutates one field to force exactly one validation problem.
// It mirrors validManifest's discipline (load_test.go) but adds the verdict
// branch and the fixer-bounded cycle.
const branchingManifest = `api: agentum/v1
pack:
  name: probe
  version: 1.0.0
  persona: engineering
memory:
  reads: [project]
  writes: true
capabilities: [fs.read]
budgets:
  fix_cycles: 2
  ask_to_edit: 1
tiers:
  default: fast
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: review
  review:
    gate: auto
    prompt: prompts/review.md
    transitions:
      - to: fix
        condition: verdict == "changes_requested"
      - to: done
        condition: verdict == "approved"
  fix:
    gate: auto_if_clean
    role: fixer
    prompt: prompts/fix.md
    transitions:
      - to: review
  done: {}
`

func branchingPrompts() map[string]string {
	return map[string]string{
		"prompts/spec.md":   "spec body",
		"prompts/review.md": "review body",
		"prompts/fix.md":    "fix body",
	}
}

func TestValidate_BranchingHappyPath(t *testing.T) {
	t.Parallel()
	loaded, err := Load(writePack(t, branchingManifest, branchingPrompts()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate branching manifest: %v", err)
	}
}

func TestValidate_ConditionErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(manifest string) string
		wantError string
	}{
		{
			name: "unknown condition",
			mutate: func(manifest string) string {
				return strings.Replace(manifest, `verdict == "changes_requested"`, `mood == "happy"`, 1)
			},
			wantError: "must start with one of",
		},
		{
			name: "unknown verdict literal",
			mutate: func(manifest string) string {
				return strings.Replace(manifest, `verdict == "changes_requested"`, `verdict == "maybe"`, 1)
			},
			wantError: "not in the closed set",
		},
		{
			name: "non-total stage (one verdict literal, no fallback)",
			mutate: func(manifest string) string {
				// Drop the approved edge so review has only changes_requested
				// and no fallback — verdict approved is uncovered.
				return strings.Replace(manifest, `      - to: done
        condition: verdict == "approved"
`, "", 1)
			},
			wantError: "do not cover verdict/status exhaustively",
		},
		{
			name: "fallback before conditional edge",
			mutate: func(manifest string) string {
				// Replace review's transitions: unconditional first, then a
				// conditional edge — the fallback shadows the conditional.
				return strings.Replace(manifest, `    transitions:
      - to: fix
        condition: verdict == "changes_requested"
      - to: done
        condition: verdict == "approved"
`, `    transitions:
      - to: done
      - to: fix
        condition: verdict == "changes_requested"
`, 1)
			},
			wantError: "must be last",
		},
		{
			name: "cycle fix_cycles zero",
			mutate: func(manifest string) string {
				return strings.Replace(manifest, "fix_cycles: 2", "fix_cycles: 0", 1)
			},
			wantError: "requires budgets.fix_cycles >= 1",
		},
		{
			name: "cycle with no fixer-role stage",
			mutate: func(manifest string) string {
				// Make the fix stage an analyst instead of a fixer; the cycle
				// review→fix→review still exists but no node is budget-boundable.
				return strings.Replace(manifest, "    role: fixer\n    prompt: prompts/fix.md", "    role: analyst\n    prompt: prompts/fix.md", 1)
			},
			wantError: "no fixer-role stage",
		},
		{
			name: "transition to undefined stage",
			mutate: func(manifest string) string {
				return strings.Replace(manifest, "      - to: review\n", "      - to: nonexistent\n", 1)
			},
			wantError: "is not a defined stage",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			manifest := testCase.mutate(branchingManifest)
			loaded, err := Load(writePack(t, manifest, branchingPrompts()))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = loaded.Validate()
			if err == nil {
				t.Fatalf("Validate: want error containing %q, got nil", testCase.wantError)
			}
			if !strings.Contains(err.Error(), testCase.wantError) {
				t.Errorf("Validate: error = %q, want substring %q", err.Error(), testCase.wantError)
			}
		})
	}
}

// TestValidate_BudgetWithoutCycleStaysValid confirms the one-way implication
// of D6 rule 4: a pack may declare budgets.fix_cycles without declaring a
// cycle. packs/minimal and testdata/minimal both do this.
func TestValidate_BudgetWithoutCycleStaysValid(t *testing.T) {
	t.Parallel()
	loaded, err := Load("testdata/minimal")
	if err != nil {
		t.Fatalf("Load testdata/minimal: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Errorf("Validate testdata/minimal (budget, no cycle): %v", err)
	}
}

// TestValidate_PerComponentCycleCheck confirms rule 4 is per-SCC, not
// whole-graph: a pack with one bounded loop and one unbounded loop must be
// rejected, even though a fixer stage exists somewhere in the graph.
func TestValidate_PerComponentCycleCheck(t *testing.T) {
	t.Parallel()
	// Two cycles reachable from entry: review→fix→review (fixer-bounded) and
	// qa→qa2→qa (no fixer). The whole graph has a fixer, but the qa loop's
	// component does not, so the pack must be rejected. spec dispatches to
	// review via a verdict edge and to qa unconditionally (last = fallback).
	manifest := `api: agentum/v1
pack:
  name: twoloops
  version: 1.0.0
  persona: engineering
memory:
  reads: [project]
  writes: true
capabilities: [fs.read]
budgets:
  fix_cycles: 2
  ask_to_edit: 1
tiers:
  default: fast
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: review
        condition: status == "complete"
      - to: qa
  review:
    gate: auto
    prompt: prompts/review.md
    transitions:
      - to: fix
        condition: verdict == "changes_requested"
      - to: done
        condition: verdict == "approved"
  fix:
    gate: auto_if_clean
    role: fixer
    prompt: prompts/fix.md
    transitions:
      - to: review
  qa:
    gate: auto
    prompt: prompts/qa.md
    transitions:
      - to: qa2
  qa2:
    gate: auto
    prompt: prompts/qa2.md
    transitions:
      - to: qa
  done: {}
`
	prompts := map[string]string{
		"prompts/spec.md":   "spec",
		"prompts/review.md": "review",
		"prompts/fix.md":    "fix",
		"prompts/qa.md":     "qa",
		"prompts/qa2.md":    "qa2",
	}
	loaded, err := Load(writePack(t, manifest, prompts))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = loaded.Validate()
	if err == nil {
		t.Fatal("Validate: want error naming the unbounded qa loop, got nil")
	}
	if !strings.Contains(err.Error(), "no fixer-role stage") {
		t.Errorf("Validate: error = %q, want substring %q", err.Error(), "no fixer-role stage")
	}
}
