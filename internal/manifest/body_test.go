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

// TestMergeBodies_SequentialPatchesPreserveEarlierContribution pins the
// append-only merge semantics the transactional write path relies on: two
// patches that each add to an append-only section, merged one after the other
// onto the same base, must both survive in every section that appends.
//
// Scope note (known coverage gap): this exercises the *merge functions*, which
// were never the D1 defect — the merge was always correct given the right base.
// D1 was a lost update caused by reading the merge base outside the transaction
// and a shallow SQL `||`. That defect is not unit-testable without a database
// harness (which this repo deliberately does not have): the failure requires two
// concurrent transactions racing on the same row. The fix is verified by reading
// the transactional structure of AddEvidenceTx (merge base = locked body, SQL =
// full replacement) and is review-only, not test-covered. This test stays
// because it pins the merge contract the write path depends on; it is not a D1
// regression test and was mislabeled as one in the original plan.
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
				Checks:       &CheckEvidence{SetVersion: "v1", Ran: true},
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

// TestEvidenceComplete_MemoryDoesNotBlockCompleteness is the D5 flag fix: a run
// that produced every section the system can write is complete, even though
// `memory` is absent — the memory subsystem is not wired, and its absence is a
// permanent build-level gap, not a degradation of this run's evidence. Counting
// it would make evidence_complete permanently false and conflate "subsystem not
// built" with "evidence degraded," which is the confusion the flag exists to
// dispel. A reviewer reads `missing` (memory included, honestly) for the gap
// list and `evidence_complete` for whether this run's evidence degraded.
func TestEvidenceComplete_MemoryDoesNotBlockCompleteness(t *testing.T) {
	t.Parallel()
	body := Body{
		// Memory intentionally nil — the subsystem is not wired.
		HumanGates:   []HumanDecision{{Stage: "s", Gate: "final", Decision: "approved"}},
		Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "x"}}},
		Checks:       &CheckEvidence{SetVersion: "v1", Ran: true},
		Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
		Prompts:      []PromptRevision{{StageID: "spec", Hash: "h"}},
	}
	if !body.IsEvidenceComplete() {
		t.Error("a run with every wired section present should be complete despite memory being absent")
	}
	// And `missing` still honestly reports memory.
	if missing := body.MissingSections(); len(missing) != 1 || missing[0] != "memory" {
		t.Errorf("MissingSections = %v, want [memory] — the gap list must still report it", missing)
	}
}

// TestEvidenceComplete_GapOrAbsentSectionBlocks guards the flag's other axis:
// an evidence gap (degraded write) or an absent wired section makes the run
// incomplete, which is the whole point of distinguishing it from `missing`.
func TestEvidenceComplete_GapOrAbsentSectionBlocks(t *testing.T) {
	t.Parallel()
	complete := Body{
		HumanGates:   []HumanDecision{{Stage: "s", Gate: "final", Decision: "approved"}},
		Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "x"}}},
		Checks:       &CheckEvidence{SetVersion: "v1", Ran: true},
		Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
		Prompts:      []PromptRevision{{StageID: "spec", Hash: "h"}},
	}
	if !complete.IsEvidenceComplete() {
		t.Fatal("baseline body should be complete")
	}
	// An evidence gap blocks.
	withGap := complete
	withGap.EvidenceGaps = []EvidenceGap{{Section: "prompts", Reason: "db down"}}
	if withGap.IsEvidenceComplete() {
		t.Error("an evidence gap did not block completeness")
	}
	// An absent wired section blocks.
	noChecks := complete
	noChecks.Checks = nil
	if noChecks.IsEvidenceComplete() {
		t.Error("an absent checks section did not block completeness")
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

// TestMergeBodies_PerStageModelAccumulates is D8: each stage's model must
// accumulate into Model.PerStage, not overwrite the run-level Model pointer. A
// pipeline that runs different tiers per stage (cheap for analysis, expensive
// for implementation) must record which model served each stage; the scalar
// alone recorded only whichever stage ran last.
func TestMergeBodies_PerStageModelAccumulates(t *testing.T) {
	t.Parallel()
	base := Body{Model: &ModelEvidence{
		Tier: "fast", Model: "model-fast", AgentName: "opencode",
		PerStage: []StageModel{{Stage: "spec", Tier: "fast", Model: "model-fast", AgentName: "opencode"}},
	}}
	patch := Body{Model: &ModelEvidence{
		Tier: "strong", Model: "model-strong", AgentName: "opencode",
		PerStage: []StageModel{{Stage: "impl", Tier: "strong", Model: "model-strong", AgentName: "opencode"}},
	}}
	merged := mergeBodies(base, patch)

	// Scalars take the patch's value (the run-level summary is the last stage).
	if merged.Model.Tier != "strong" || merged.Model.Model != "model-strong" {
		t.Errorf("scalars = %s/%s, want strong/model-strong", merged.Model.Tier, merged.Model.Model)
	}
	// PerStage accumulates both stages.
	if len(merged.Model.PerStage) != 2 {
		t.Fatalf("PerStage = %d entries, want 2: %+v", len(merged.Model.PerStage), merged.Model.PerStage)
	}
	stages := make(map[string]StageModel, len(merged.Model.PerStage))
	for _, entry := range merged.Model.PerStage {
		stages[entry.Stage] = entry
	}
	if stages["spec"].Model != "model-fast" {
		t.Errorf("spec stage model lost: %+v", stages["spec"])
	}
	if stages["impl"].Model != "model-strong" {
		t.Errorf("impl stage model lost: %+v", stages["impl"])
	}
}

// TestMergeBodies_PerStageModelReRunReplaces covers the resume case: a stage
// re-run after resume supersedes its prior entry rather than appending a
// duplicate, matching how the capability section handles a re-run.
func TestMergeBodies_PerStageModelReRunReplaces(t *testing.T) {
	t.Parallel()
	base := Body{Model: &ModelEvidence{
		PerStage: []StageModel{{Stage: "spec", Tier: "fast", Model: "model-fast-v1"}},
	}}
	patch := Body{Model: &ModelEvidence{
		PerStage: []StageModel{{Stage: "spec", Tier: "strong", Model: "model-strong-v2"}},
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Model.PerStage) != 1 {
		t.Fatalf("PerStage = %d entries, want 1 (replaced, not duplicated): %+v", len(merged.Model.PerStage), merged.Model.PerStage)
	}
	if merged.Model.PerStage[0].Model != "model-strong-v2" {
		t.Errorf("spec stage not superseded: %+v", merged.Model.PerStage[0])
	}
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

// TestMergeBodies_CheckEvidenceRanIsMonotonic pins the Ran OR-merge: once any
// delivery-boundary run executed checks (Ran=true), a later partial patch must
// not flip it back to false. Ran distinguishes "the gate ran" from "no checks
// defined"; a re-run after resume that records an empty set must not erase the
// fact that checks ran earlier. This mirrors MandatoryPassed's monotonicity.
func TestMergeBodies_CheckEvidenceRanIsMonotonic(t *testing.T) {
	t.Parallel()
	base := Body{Checks: &CheckEvidence{Commit: "c1", Ran: true, MandatoryPassed: true}}
	// Patch with Ran=false (e.g. a re-run resolved an empty set) must not clear it.
	patch := Body{Checks: &CheckEvidence{Commit: "c2", Ran: false}}
	merged := mergeBodies(base, patch)
	if !merged.Checks.Ran {
		t.Error("Ran must be monotonic (OR): once true, a later patch must not flip it back to false")
	}
	if merged.Checks.Commit != "c2" {
		t.Errorf("Commit scalar should take patch value, got %q", merged.Checks.Commit)
	}
}

// TestEvidenceComplete_ChecksWithoutRanBlocks is the E4 completeness fix: a
// checks section that recorded no run (Ran=false — the project defines no
// checks) must not satisfy evidence_complete. The flag reads as "the delivery
// gate ran," and an empty set is not that. MissingSections still reports checks
// honestly so a reviewer sees the gap either way.
func TestEvidenceComplete_ChecksWithoutRanBlocks(t *testing.T) {
	t.Parallel()
	body := Body{
		HumanGates:   []HumanDecision{{Stage: "s", Gate: "final", Decision: "approved"}},
		Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "x"}}},
		Checks:       &CheckEvidence{SetVersion: "v1", Ran: false, MandatoryPassed: true},
		Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
		Prompts:      []PromptRevision{{StageID: "spec", Hash: "h"}},
	}
	if body.IsEvidenceComplete() {
		t.Error("a checks section with Ran=false must not satisfy completeness even if MandatoryPassed=true")
	}
	// And MissingSections reports the gap by the same predicate, so the two
	// cannot drift (the drift hazard the expectedSections list collapses).
	missing := body.MissingSections()
	found := false
	for _, section := range missing {
		if section == "checks" {
			found = true
		}
	}
	if !found {
		t.Errorf("MissingSections = %v; a Ran=false checks section must be reported missing", missing)
	}
}
