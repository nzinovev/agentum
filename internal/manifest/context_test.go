package manifest

import (
	"reflect"
	"testing"
	"time"
)

// TestMergeContext_InstructionsAppendUniqueByPathAndHash: instructions merge
// append-unique by (path, delivered_hash); a re-pin that delivered the same
// bytes collapses, a re-pin that truncated differently adds a new entry.
func TestMergeContext_InstructionsAppendUniqueByPathAndHash(t *testing.T) {
	t.Parallel()
	existing := &ContextEvidence{Instructions: []InstructionRef{
		{Path: "AGENTS.md", SourceHash: "h1", DeliveredHash: "h1", DeliveredBytes: 10},
	}}
	patch := &ContextEvidence{Instructions: []InstructionRef{
		{Path: "AGENTS.md", SourceHash: "h1", DeliveredHash: "h1", DeliveredBytes: 10},                 // dup → collapse
		{Path: "AGENTS.md", SourceHash: "h1", DeliveredHash: "h2", DeliveredBytes: 6, Truncated: true}, // new hash → add
		{Path: "docs/x.md", SourceHash: "h3", DeliveredHash: "h3", DeliveredBytes: 5},                  // new path → add
	}}
	merged := mergeContextEvidence(existing, patch)
	if len(merged.Instructions) != 3 {
		t.Fatalf("got %d instructions, want 3: %+v", len(merged.Instructions), merged.Instructions)
	}
}

// TestMergeContext_SkillsAppendUniqueByNameAndHash: a skill whose body changed
// adds a new entry rather than replacing the old — the change is the evidence.
func TestMergeContext_SkillsAppendUniqueByNameAndHash(t *testing.T) {
	t.Parallel()
	existing := &ContextEvidence{Skills: []SkillRef{
		{Name: "user-skill", Hash: "old", Bytes: 10},
	}}
	patch := &ContextEvidence{Skills: []SkillRef{
		{Name: "user-skill", Hash: "old", Bytes: 10}, // dup → collapse
		{Name: "user-skill", Hash: "new", Bytes: 12}, // changed body → add
	}}
	merged := mergeContextEvidence(existing, patch)
	if len(merged.Skills) != 2 {
		t.Fatalf("got %d skills, want 2 (both versions kept): %+v", len(merged.Skills), merged.Skills)
	}
}

// TestMergeContext_RestorationsAppendByStagePathAt: restorations de-duplicate
// by (stage, path, at) so a retry that re-ran the same stage at the same time
// does not duplicate.
func TestMergeContext_RestorationsAppendByStagePathAt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	existing := &ContextEvidence{Restorations: []InstructionRestoration{
		{Stage: "implement", Path: "AGENTS.md", Action: "restored", FoundHash: "tampered", At: at},
	}}
	patch := &ContextEvidence{Restorations: []InstructionRestoration{
		{Stage: "implement", Path: "AGENTS.md", Action: "restored", FoundHash: "tampered", At: at}, // dup
		{Stage: "review", Path: "AGENTS.md", Action: "restored", FoundHash: "tampered2", At: at},   // different stage
	}}
	merged := mergeContextEvidence(existing, patch)
	if len(merged.Restorations) != 2 {
		t.Fatalf("got %d restorations, want 2: %+v", len(merged.Restorations), merged.Restorations)
	}
}

// TestMergeContext_WorstSkillsProbeWins: a failed probe on either side stays
// failed — a retry that succeeded does not erase the fact that an earlier probe
// failed (the run passed through a window where the skill set was unknown).
func TestMergeContext_WorstSkillsProbeWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		left, right string
		want        string
	}{
		{"both ok", "ok", "ok", "ok"},
		{"left failed", "failed: timeout", "ok", "failed: timeout"},
		{"right failed", "ok", "failed: json", "failed: json"},
		{"both failed", "failed: timeout", "failed: json", "failed: timeout"},
		{"unsupported is not failure", "unsupported", "ok", "unsupported"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := worstSkillsProbe(testCase.left, testCase.right)
			if got != testCase.want {
				t.Errorf("worstSkillsProbe(%q, %q) = %q, want %q", testCase.left, testCase.right, got, testCase.want)
			}
		})
	}
}

// TestEvidenceComplete_EmptyContextSectionStillSeals: a context section that is
// present but empty (a project with no AGENTS.md and no skills) still satisfies
// evidence completeness — the section is written on every run so absence is
// explicit, not a gap.
func TestEvidenceComplete_EmptyContextSectionStillSeals(t *testing.T) {
	t.Parallel()
	body := Body{
		Context: &ContextEvidence{SkillsProbe: "ok"},
		// Fill the other required sections so completeness is gated only by
		// context.
		GateDecisions: []GateDecision{{Stage: "review", Decision: "approved", Timestamp: time.Now()}},
		Artifacts:     &ArtifactEvidence{},
		Capabilities:  &CapabilityProfile{Declared: []string{"fs.read"}},
		Invocations:   []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
	}
	// Checks must report Ran:true to count.
	body.Checks = &CheckEvidence{Ran: true}
	if !body.IsEvidenceComplete() {
		t.Errorf("empty-but-present context should seal evidence_complete=true; missing=%v", body.MissingSections())
	}
}

// TestEvidenceComplete_FailedContextProbeSealsFalse: a failed skill probe does
// not itself set evidence_complete false (the section is present); the runner
// records an EvidenceGap for context.skills, and THAT is what makes it false.
// This test pins that the mechanic still works when context is present.
func TestEvidenceComplete_FailedContextProbeSealsFalse(t *testing.T) {
	t.Parallel()
	body := Body{
		Context:       &ContextEvidence{SkillsProbe: "failed: timeout"},
		GateDecisions: []GateDecision{{Stage: "review", Decision: "approved", Timestamp: time.Now()}},
		Artifacts:     &ArtifactEvidence{},
		Checks:        &CheckEvidence{Ran: true},
		Capabilities:  &CapabilityProfile{Declared: []string{"fs.read"}},
		Invocations:   []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
		EvidenceGaps:  []EvidenceGap{{Section: "context.skills", Reason: "probe failed", At: time.Now()}},
	}
	if body.IsEvidenceComplete() {
		t.Error("a body with a context.skills EvidenceGap must not be complete")
	}
}

// TestDiffManifests_ContextSkillChange: two runs differing only in one skill
// hash surface a context-skills delta, even with identical task/commit/config.
func TestDiffManifests_ContextSkillChange(t *testing.T) {
	t.Parallel()
	left := Body{Context: &ContextEvidence{Skills: []SkillRef{{Name: "user-skill", Hash: "old"}}}}
	right := Body{Context: &ContextEvidence{Skills: []SkillRef{{Name: "user-skill", Hash: "new"}}}}
	diff := DiffManifests(left, right)
	if diff.Context == nil {
		t.Fatal("expected Context delta for changed skill hash, got nil")
	}
	if diff.Context.Reason != "context-skills" {
		t.Errorf("Context.Reason = %q, want context-skills", diff.Context.Reason)
	}
}

// TestDiffManifests_ContextInstructionChange: two runs differing in an
// instruction delivered hash surface a context-instructions delta.
func TestDiffManifests_ContextInstructionChange(t *testing.T) {
	t.Parallel()
	left := Body{Context: &ContextEvidence{Instructions: []InstructionRef{
		{Path: "AGENTS.md", DeliveredHash: "h1"},
	}}}
	right := Body{Context: &ContextEvidence{Instructions: []InstructionRef{
		{Path: "AGENTS.md", DeliveredHash: "h2"},
	}}}
	diff := DiffManifests(left, right)
	if diff.Context == nil || diff.Context.Reason != "context-instructions" {
		t.Fatalf("Context delta = %+v, want reason context-instructions", diff.Context)
	}
}

// TestDiffManifests_ContextSameIsSilent: two runs with identical instruction
// and skill sets produce no context delta.
func TestDiffManifests_ContextSameIsSilent(t *testing.T) {
	t.Parallel()
	left := Body{Context: &ContextEvidence{
		Instructions: []InstructionRef{{Path: "AGENTS.md", DeliveredHash: "h1"}},
		Skills:       []SkillRef{{Name: "user-skill", Hash: "h"}},
	}}
	right := Body{Context: &ContextEvidence{
		Instructions: []InstructionRef{{Path: "AGENTS.md", DeliveredHash: "h1"}},
		Skills:       []SkillRef{{Name: "user-skill", Hash: "h"}},
	}}
	if diff := DiffManifests(left, right); diff.Context != nil {
		t.Errorf("expected no Context delta for identical sets, got %+v", diff.Context)
	}
}

// TestIndexHelpers round-trips the index functions used by diffContext so a
// future shape change to InstructionRef/SkillRef does not silently break the
// diff axis.
func TestIndexHelpers(t *testing.T) {
	t.Parallel()
	instructions := indexInstructions([]InstructionRef{
		{Path: "AGENTS.md", DeliveredHash: "h"},
	})
	if !reflect.DeepEqual(instructions, map[string]string{"AGENTS.md": "h"}) {
		t.Errorf("indexInstructions = %v", instructions)
	}
	skills := indexSkills([]SkillRef{{Name: "s", Hash: "h"}})
	if !reflect.DeepEqual(skills, map[string]string{"s": "h"}) {
		t.Errorf("indexSkills = %v", skills)
	}
}
