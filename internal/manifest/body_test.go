package manifest

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEncodeDecode_Roundtrip(t *testing.T) {
	t.Parallel()
	body := Body{
		Schema: "1",
		Input: &InputEvidence{
			TaskID: "T1", Title: "title", Revision: "abc",
			Input: []byte(`{"foo":"bar"}`),
		},
		Prompts: []PromptRevision{{StageID: "spec", Hash: "h1"}},
		Missing: []string{"memory"},
	}
	encoded, err := encodeBody(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeBody(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Schema != body.Schema {
		t.Errorf("Schema = %q", decoded.Schema)
	}
	if decoded.Input == nil || decoded.Input.Revision != "abc" {
		t.Errorf("Input not preserved: %+v", decoded.Input)
	}
	if len(decoded.Prompts) != 1 || decoded.Prompts[0].StageID != "spec" {
		t.Errorf("Prompts not preserved: %+v", decoded.Prompts)
	}
	if len(decoded.Missing) != 1 || decoded.Missing[0] != "memory" {
		t.Errorf("Missing not preserved: %+v", decoded.Missing)
	}
}

func TestDecodeEmptyBodyReturnsSchema(t *testing.T) {
	t.Parallel()
	body, err := decodeBody(nil)
	if err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if body.Schema != schemaVersion {
		t.Errorf("Schema = %q, want %q", body.Schema, schemaVersion)
	}
}

func TestMergeBodies_ScalarOverwrites(t *testing.T) {
	t.Parallel()
	base := Body{Input: &InputEvidence{TaskID: "T1", Revision: "v1"}}
	patch := Body{Input: &InputEvidence{TaskID: "T1", Revision: "v2"}}
	merged := mergeBodies(base, patch)
	if merged.Input.Revision != "v2" {
		t.Errorf("scalar not overwritten: %+v", merged.Input)
	}
}

func TestMergeBodies_PromptsAppendUnique(t *testing.T) {
	t.Parallel()
	base := Body{Prompts: []PromptRevision{{StageID: "spec", Hash: "h1"}}}
	patch := Body{Prompts: []PromptRevision{
		{StageID: "spec", Hash: "h1"}, // dup → ignored
		{StageID: "impl", Hash: "h2"}, // new
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Prompts) != 2 {
		t.Fatalf("merged prompts = %d, want 2: %+v", len(merged.Prompts), merged.Prompts)
	}
}

func TestMergeBodies_MissingUnioned(t *testing.T) {
	t.Parallel()
	base := Body{Missing: []string{"memory", "checks"}}
	patch := Body{Missing: []string{"checks", "capabilities"}}
	merged := mergeBodies(base, patch)
	if len(merged.Missing) != 3 {
		t.Errorf("Missing union wrong: %+v", merged.Missing)
	}
}

func TestMergeBodies_ArtifactEvidenceAppends(t *testing.T) {
	t.Parallel()
	base := Body{Artifacts: &ArtifactEvidence{
		Inputs: []ArtifactRef{{Name: "spec.md", ContentHash: "h1"}},
	}}
	patch := Body{Artifacts: &ArtifactEvidence{
		Inputs:  []ArtifactRef{{Name: "spec.md", ContentHash: "h1"}}, // dup
		Outputs: []ArtifactRef{{Name: "impl.md", ContentHash: "h2"}}, // new
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Artifacts.Inputs) != 1 {
		t.Errorf("Inputs not deduped: %+v", merged.Artifacts.Inputs)
	}
	if len(merged.Artifacts.Outputs) != 1 {
		t.Errorf("Outputs not appended: %+v", merged.Artifacts.Outputs)
	}
}

func TestMergeBodies_GitEvidenceOverwritesScalarAppendsCheckpoint(t *testing.T) {
	t.Parallel()
	base := Body{Git: &GitEvidence{
		Branch: "agentum/T1", BaseCommit: "b1",
		Checkpoints: []CheckpointRef{{Label: "base", Commit: "b1"}},
	}}
	patch := Body{Git: &GitEvidence{
		BaseCommit:   "b1",
		ResultCommit: "r1",
		Checkpoints: []CheckpointRef{
			{Label: "base", Commit: "b1"},      // dup
			{Label: "post-spec", Commit: "c1"}, // new
		},
	}}
	merged := mergeBodies(base, patch)
	if merged.Git.BaseCommit != "b1" {
		t.Errorf("BaseCommit not preserved")
	}
	if merged.Git.ResultCommit != "r1" {
		t.Errorf("ResultCommit not set: %q", merged.Git.ResultCommit)
	}
	if len(merged.Git.Checkpoints) != 2 {
		t.Errorf("Checkpoints not appended+deduped: %+v", merged.Git.Checkpoints)
	}
}

func TestMergeBodies_NilPatchArtifactsPreservesBase(t *testing.T) {
	t.Parallel()
	base := Body{Artifacts: &ArtifactEvidence{Inputs: []ArtifactRef{{Name: "x", ContentHash: "h1"}}}}
	merged := mergeBodies(base, Body{Prompts: []PromptRevision{{StageID: "s", Hash: "h"}}})
	if merged.Artifacts == nil || len(merged.Artifacts.Inputs) != 1 {
		t.Errorf("Artifacts not preserved: %+v", merged.Artifacts)
	}
}

// TestMergeBodies_SequentialPatchesPreserveEarlierContribution is the regression
// for the lost update at the merge layer (D1). Two patches that each add to an
// append-only section, merged one after the other onto the same base, must both
// survive. The pre-fix AddEvidence read its merge base outside the transaction,
// so the second writer's body replaced the first's at the top level; this test
// pins the merge semantics the transactional write path now relies on by
// exercising every append-only section.
func TestMergeBodies_SequentialPatchesPreserveEarlierContribution(t *testing.T) {
	t.Parallel()
	base := Body{Schema: "1", Missing: []string{"memory"}}
	patchA := Body{
		Prompts:    []PromptRevision{{StageID: "spec", Hash: "spec-hash"}},
		HumanGates: []HumanDecision{{Stage: "spec", Gate: "final", Decision: "approved", Actor: "alice", Timestamp: parseTestTime(t, "2026-01-01T00:00:00Z")}},
		Artifacts:  &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "spec.md", RevisionID: "rev-1", ContentHash: "h1", Stage: "spec"}}},
		Checks:     &CheckEvidence{SetVersion: "v1", Results: []CheckResult{{Name: "build", Status: "pass"}}},
		Missing:    []string{"capabilities"},
		Capabilities: &CapabilityProfile{Effective: []StageCapabilityProfile{{
			Stage: "spec", Role: "implementer", Profile: json.RawMessage("{}"),
		}}},
	}
	patchB := Body{
		Prompts:    []PromptRevision{{StageID: "impl", Hash: "impl-hash"}},
		HumanGates: []HumanDecision{{Stage: "impl", Gate: "final", Decision: "approved", Actor: "bob", Timestamp: parseTestTime(t, "2026-01-02T00:00:00Z")}},
		Artifacts:  &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "impl.md", RevisionID: "rev-2", ContentHash: "h2", Stage: "impl"}}},
		Checks:     &CheckEvidence{SetVersion: "v2", Results: []CheckResult{{Name: "test", Status: "pass"}}},
		Missing:    []string{"human_gates"},
		Capabilities: &CapabilityProfile{Effective: []StageCapabilityProfile{{
			Stage: "impl", Role: "implementer", Profile: json.RawMessage("{}"),
		}}},
	}

	afterA := mergeBodies(base, patchA)
	afterB := mergeBodies(afterA, patchB)

	if len(afterB.Prompts) != 2 {
		t.Errorf("Prompts: A's spec prompt lost; got %+v", afterB.Prompts)
	}
	if len(afterB.HumanGates) != 2 {
		t.Errorf("HumanGates: A's decision lost; got %+v", afterB.HumanGates)
	}
	if afterB.Artifacts == nil || len(afterB.Artifacts.Outputs) != 2 {
		t.Errorf("Artifacts.Outputs: A's revision lost; got %+v", afterB.Artifacts)
	}
	if afterB.Checks == nil || len(afterB.Checks.Results) != 2 {
		t.Errorf("Checks.Results: A's build result lost; got %+v", afterB.Checks)
	}
	if len(afterB.Missing) != 3 {
		t.Errorf("Missing: union wrong; got %+v", afterB.Missing)
	}
	if afterB.Capabilities == nil || len(afterB.Capabilities.Effective) != 2 {
		t.Errorf("Capabilities.Effective: A's spec profile lost; got %+v", afterB.Capabilities)
	}
}

func parseTestTime(t *testing.T, value string) (ts time.Time) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return ts
}

// TestMissingSections_DerivesFromBody is the D6 fix: `missing` must describe
// the body as it actually is, not a list asserted once at init. The stale case
// — a body with a populated capabilities section that the old hardcoded list
// still called "missing" — is the specific regression this pins.
func TestMissingSections_DerivesFromBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body Body
		want []string
	}{
		{
			name: "empty body reports every expected section missing",
			body: Body{Schema: "1"},
			want: []string{"memory", "human_gates", "artifacts", "checks", "capabilities", "prompts"},
		},
		{
			name: "fully populated body reports nothing missing",
			body: Body{
				Memory:       &MemorySlice{Entries: 1},
				HumanGates:   []HumanDecision{{Stage: "s", Gate: "final", Decision: "approved"}},
				Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "x"}}},
				Checks:       &CheckEvidence{SetVersion: "v1"},
				Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
				Prompts:      []PromptRevision{{StageID: "spec", Hash: "h"}},
			},
			want: nil,
		},
		{
			name: "populated capabilities is not reported missing (stale-list regression)",
			body: Body{
				Capabilities: &CapabilityProfile{Effective: []StageCapabilityProfile{{Stage: "spec"}}},
				Prompts:      []PromptRevision{{StageID: "spec", Hash: "h"}},
			},
			want: []string{"memory", "human_gates", "artifacts", "checks"},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := testCase.body.MissingSections()
			if !equalStringSet(got, testCase.want) {
				t.Errorf("MissingSections = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestMergeBodies_EvidenceGapsAppendDedup is the D5 merge: a failed evidence
// write appends a gap, and the same failure recorded twice (a retry that
// failed identically) de-duplicates rather than growing the list.
func TestMergeBodies_EvidenceGapsAppendDedup(t *testing.T) {
	t.Parallel()
	at1 := parseTestTime(t, "2026-01-01T00:00:00Z")
	at2 := parseTestTime(t, "2026-01-02T00:00:00Z")
	base := Body{EvidenceGaps: []EvidenceGap{
		{Section: "prompts_model_capabilities", Stage: "spec", Reason: "db down", At: at1},
	}}
	// Same (Section, Stage, Reason) → dedup, keeping the later At.
	patch := Body{EvidenceGaps: []EvidenceGap{
		{Section: "prompts_model_capabilities", Stage: "spec", Reason: "db down", At: at2},
		{Section: "git", Stage: "", Reason: "checkpoint list failed", At: at2},
	}}
	merged := mergeBodies(base, patch)
	if len(merged.EvidenceGaps) != 2 {
		t.Fatalf("EvidenceGaps = %d, want 2 (deduped): %+v", len(merged.EvidenceGaps), merged.EvidenceGaps)
	}
	for _, gap := range merged.EvidenceGaps {
		if gap.Section == "prompts_model_capabilities" && !gap.At.Equal(at2) {
			t.Errorf("dedup did not keep the later At: %+v", gap)
		}
	}
}

// TestMergeBodies_DoesNotCarryEvidenceCompleteFromPatch guards the seal-time
// invariant: EvidenceComplete is set by the seal transaction, never by a patch.
// A patch carrying it would let a caller assert completeness out of band; the
// merge must ignore it so "not yet sealed" stays distinguishable from "sealed
// and incomplete".
func TestMergeBodies_DoesNotCarryEvidenceCompleteFromPatch(t *testing.T) {
	t.Parallel()
	base := Body{Schema: "1"}
	complete := true
	patch := Body{EvidenceComplete: &complete}
	merged := mergeBodies(base, patch)
	if merged.EvidenceComplete != nil {
		t.Errorf("merge accepted EvidenceComplete from a patch: %+v", merged.EvidenceComplete)
	}
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, value := range a {
		seen[value] = true
	}
	for _, value := range b {
		if !seen[value] {
			return false
		}
	}
	return true
}

func TestMergeBodies_CheckEvidenceAppendsDedupAndMonotonicPass(t *testing.T) {
	t.Parallel()
	base := Body{Checks: &CheckEvidence{
		SetVersion: "v1", Commit: "c1", MandatoryPassed: true,
		Results: []CheckResult{{Name: "build", Status: "pass", DefinitionRevision: "dr1"}},
	}}
	patch := Body{Checks: &CheckEvidence{
		SetVersion: "v2", Commit: "c2",
		Results: []CheckResult{
			{Name: "build", Status: "fail", DefinitionRevision: "dr1"}, // same name → replace
			{Name: "test", Status: "pass"},                             // new
		},
	}}
	merged := mergeBodies(base, patch)
	if merged.Checks.SetVersion != "v2" {
		t.Errorf("SetVersion should take patch value, got %q", merged.Checks.SetVersion)
	}
	if merged.Checks.Commit != "c2" {
		t.Errorf("Commit should take patch value, got %q", merged.Checks.Commit)
	}
	if !merged.Checks.MandatoryPassed {
		t.Error("MandatoryPassed must be monotonic (OR): once true, stays true")
	}
	if len(merged.Checks.Results) != 2 {
		t.Fatalf("expected 2 deduped results, got %d: %+v", len(merged.Checks.Results), merged.Checks.Results)
	}
	for _, result := range merged.Checks.Results {
		if result.Name == "build" && result.Status != "fail" {
			t.Errorf("build result should be replaced with fail, got %q", result.Status)
		}
	}
}
