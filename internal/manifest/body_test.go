package manifest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/models"
)

func TestEncodeDecode_Roundtrip(t *testing.T) {
	t.Parallel()
	body := Body{
		Schema: schemaVersion,
		Input: &InputEvidence{
			TaskID: "T1", Title: "title", Revision: "abc",
			Description: "the requested behaviour",
			Overrides:   []byte(`{"checks":{"required":["verify"],"optional":[]}}`),
		},
		Invocations: []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
		Missing:     []string{"memory"},
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
	// ADR 0004 D9: the typed request round-trips — a reviewer reads the
	// description the run existed to satisfy straight off the manifest.
	if decoded.Input.Description != "the requested behaviour" {
		t.Errorf("Input.Description not preserved: %+v", decoded.Input)
	}
	if string(decoded.Input.Overrides) != `{"checks":{"required":["verify"],"optional":[]}}` {
		t.Errorf("Input.Overrides not preserved: %s", decoded.Input.Overrides)
	}
	if len(decoded.Invocations) != 1 || decoded.Invocations[0].Stage != "spec" {
		t.Errorf("Invocations not preserved: %+v", decoded.Invocations)
	}
	if decoded.Invocations[0].Prompt.RenderedHash == "" {
		t.Errorf("RenderedHash not preserved: %+v", decoded.Invocations[0].Prompt)
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

// TestMergeBodies_TwoAttemptsAtOneStageSurviveAsTwoRecords is the headline
// regression test of ADR 0005 D6: a fix cycle re-running `review` must leave
// TWO evidence records - keying by stage (the schema-1 merge rule) replaced
// the first attempt's model, tier and capability snapshot with the second's.
// The merge key is the invocation id and nothing else.
func TestMergeBodies_TwoAttemptsAtOneStageSurviveAsTwoRecords(t *testing.T) {
	t.Parallel()
	base := Body{Schema: schemaVersion, Invocations: []InvocationEvidence{
		testInvocation("inv-first", "review", 0),
	}}
	patch := Body{Invocations: []InvocationEvidence{
		testInvocation("inv-second", "review", 1),
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Invocations) != 2 {
		t.Fatalf("merged invocations = %d, want 2 (same stage, different ids): %+v",
			len(merged.Invocations), merged.Invocations)
	}
	if merged.Invocations[0].InvocationID != "inv-first" || merged.Invocations[1].InvocationID != "inv-second" {
		t.Errorf("records reordered or replaced: %+v", merged.Invocations)
	}
	if merged.Invocations[0].Prompt.RenderedHash == merged.Invocations[1].Prompt.RenderedHash {
		t.Errorf("two attempts must carry distinct rendered hashes: %+v", merged.Invocations)
	}
}

// TestMergeBodies_SameIDPatchedTwiceMergesIntoOne is the two-pass write of
// ADR 0005 D7: the open record (identity, model, prompts, profile) is written
// before Invoke; the close record (telemetry, stop reason) fills the same
// record afterwards without duplicating it or erasing the open fields.
func TestMergeBodies_SameIDPatchedTwiceMergesIntoOne(t *testing.T) {
	t.Parallel()
	open := testInvocation("inv-1", "spec", 0)
	base := Body{Schema: schemaVersion, Invocations: []InvocationEvidence{open}}

	closed := InvocationEvidence{
		InvocationID: "inv-1",
		Telemetry:    &InvocationTelemetry{Tokens: TokenUsage{Total: 42}, Cost: 0.5},
		StopReason:   "adapter_error",
	}
	merged := mergeBodies(base, Body{Invocations: []InvocationEvidence{closed}})

	if len(merged.Invocations) != 1 {
		t.Fatalf("merged invocations = %d, want 1: %+v", len(merged.Invocations), merged.Invocations)
	}
	record := merged.Invocations[0]
	if record.Model.Tier != open.Model.Tier || record.Prompt.StagePromptHash == "" || record.Capabilities.Role == "" {
		t.Errorf("close patch erased open fields: %+v", record)
	}
	if record.Telemetry == nil || record.Telemetry.Tokens.Total != 42 || record.StopReason != "adapter_error" {
		t.Errorf("close fields not filled: %+v", record)
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
	merged := mergeBodies(base, Body{Invocations: []InvocationEvidence{testInvocation("inv-9", "s", 0)}})
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
		Invocations:  []InvocationEvidence{testInvocation("inv-spec", "spec", 0)},
		HumanGates:   []HumanDecision{{Stage: "spec", Gate: "final", Decision: "approved", Actor: "alice", Timestamp: parseTestTime(t, "2026-01-01T00:00:00Z")}},
		Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "spec.md", RevisionID: "rev-1", ContentHash: "h1", Stage: "spec"}}},
		Checks:       &CheckEvidence{SetVersion: "v1", Results: []CheckResult{{Name: "build", Status: "pass"}}},
		Missing:      []string{"capabilities"},
		Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
	}
	patchB := Body{
		Invocations: []InvocationEvidence{testInvocation("inv-impl", "impl", 0)},
		HumanGates:  []HumanDecision{{Stage: "impl", Gate: "final", Decision: "approved", Actor: "bob", Timestamp: parseTestTime(t, "2026-01-02T00:00:00Z")}},
		Artifacts:   &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "impl.md", RevisionID: "rev-2", ContentHash: "h2", Stage: "impl"}}},
		Checks:      &CheckEvidence{SetVersion: "v2", Results: []CheckResult{{Name: "test", Status: "pass"}}},
		Missing:     []string{"human_gates"},
	}

	afterA := mergeBodies(base, patchA)
	afterB := mergeBodies(afterA, patchB)

	if len(afterB.Invocations) != 2 {
		t.Errorf("Invocations: A's spec record lost; got %+v", afterB.Invocations)
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
	if afterB.Capabilities == nil || len(afterB.Capabilities.Declared) == 0 {
		t.Errorf("Capabilities.Declared: A's declared ceiling lost; got %+v", afterB.Capabilities)
	}
}

// testInvocation builds one schema-2 invocation record with every section
// populated, so completeness/merge tests share one shape.
func testInvocation(invocationID, stage string, cycle int32) InvocationEvidence {
	return InvocationEvidence{
		InvocationID: invocationID, Stage: stage, Sequence: cycle + 1, Cycle: cycle,
		Adapter:      InvocationAdapter{ID: "stub", AdapterVersion: "1.0.0", RuntimeVersion: "1.18.11"},
		Model:        models.Selection{Tier: "strong", Provider: "stub", Options: models.Options{Model: "stub/model"}},
		Prompt:       InvocationPrompt{StagePromptHash: "hash-" + stage, RenderedHash: "rendered-" + invocationID},
		Capabilities: InvocationCaps{Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)},
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
			body: Body{Schema: schemaVersion},
			want: []string{"memory", "context", "human_gates", "artifacts", "checks", "capabilities", "invocations"},
		},
		{
			name: "fully populated body reports nothing missing",
			body: Body{
				Memory:       &MemorySlice{Entries: 1},
				HumanGates:   []HumanDecision{{Stage: "s", Gate: "final", Decision: "approved"}},
				Artifacts:    &ArtifactEvidence{Outputs: []ArtifactRef{{Name: "x"}}},
				Checks:       &CheckEvidence{SetVersion: "v1", Ran: true},
				Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
				Invocations:  []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
				Context:      &ContextEvidence{SkillsProbe: "ok"},
			},
			want: nil,
		},
		{
			name: "populated capabilities is not reported missing (stale-list regression)",
			body: Body{
				Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
				Invocations:  []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
			},
			want: []string{"memory", "context", "human_gates", "artifacts", "checks"},
		},
		{
			name: "a record without a prompt hash leaves invocations missing",
			body: Body{
				Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
				Invocations: []InvocationEvidence{{
					InvocationID: "inv-1", Stage: "spec",
					Capabilities: InvocationCaps{Role: "implementer", Profile: json.RawMessage(`{}`)},
				}},
			},
			want: []string{"memory", "context", "human_gates", "artifacts", "checks", "invocations"},
		},
		{
			name: "a record without a profile leaves capabilities missing",
			body: Body{
				Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
				Invocations: []InvocationEvidence{{
					InvocationID: "inv-1", Stage: "spec",
					Prompt: InvocationPrompt{StagePromptHash: "h"},
				}},
			},
			want: []string{"memory", "context", "human_gates", "artifacts", "checks", "capabilities"},
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
		Invocations:  []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
		Context:      &ContextEvidence{SkillsProbe: "ok"},
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
		Invocations:  []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
		Context:      &ContextEvidence{SkillsProbe: "ok"},
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

// TestDecodeV1Body_RetainsLegacySectionsVerbatim (ADR 0005 D9): a schema-1
// body decodes with its prompts / model / effective sections intact and its
// schema still reading "1" - nothing is dropped on read, so GET on an old
// sealed manifest returns exactly what was sealed.
func TestDecodeV1Body_RetainsLegacySectionsVerbatim(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"schema_version": "1",
		"prompts": [{"stage_id": "spec", "hash": "spec-hash"}],
		"model": {"tier": "strong", "model": "stub/model", "agent_name": "stub",
			"per_stage": [{"stage": "spec", "tier": "strong", "model": "stub/model", "agent_name": "stub"}]},
		"capabilities": {"declared": ["fs.read"], "effective": [{"stage": "spec", "role": "implementer", "profile": {"grants":[]}}]},
		"adapter": {"name": "stub", "version": "1.0.0", "declared_capabilities": ["fs.read"]}
	}`)
	body, err := decodeBody(raw)
	if err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if body.Schema != schemaVersionV1 {
		t.Errorf("Schema = %q, want 1 preserved on read", body.Schema)
	}
	if len(body.Prompts) != 1 || body.Prompts[0].Hash != "spec-hash" {
		t.Errorf("legacy Prompts not retained: %+v", body.Prompts)
	}
	if body.Model == nil || body.Model.Tier != "strong" || len(body.Model.PerStage) != 1 {
		t.Errorf("legacy Model not retained: %+v", body.Model)
	}
	if body.Capabilities == nil || len(body.Capabilities.Effective) != 1 {
		t.Errorf("legacy Effective not retained: %+v", body.Capabilities)
	}
	if body.Adapter == nil || body.Adapter.Name != "stub" || body.Adapter.Version != "1.0.0" {
		t.Errorf("legacy adapter fields not retained: %+v", body.Adapter)
	}
}

// TestDecodeUnknownSchemaIsATypedError (ADR 0005 D9): a body whose
// schema_version is neither 1 nor 2 is refused, not silently mis-decoded.
func TestDecodeUnknownSchemaIsATypedError(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"schema_version": "9", "input": {"task_id": "T1"}}`)
	_, err := decodeBody(raw)
	if err == nil {
		t.Fatal("unknown schema version must be an error")
	}
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Errorf("error must wrap ErrUnsupportedSchema: %v", err)
	}
	var schemaError *UnsupportedSchemaError
	if !errors.As(err, &schemaError) || schemaError.Version != "9" {
		t.Errorf("error must name the unsupported version: %v", err)
	}
}

// TestMergeBodies_V1ExistingUpgradesToV2 (ADR 0005 D9): merging a v2 patch
// onto a v1 existing body converts the legacy sections into invocation
// records, clears them, and yields exactly one schema-2 body - losing
// nothing. The only body this happens to is an unsealed one from a run in
// flight across the upgrade.
func TestMergeBodies_V1ExistingUpgradesToV2(t *testing.T) {
	t.Parallel()
	existing := Body{
		Schema: schemaVersionV1,
		Prompts: []PromptRevision{
			{StageID: "spec", Hash: "spec-hash"},
			{StageID: "impl", Hash: "impl-hash"},
		},
		Model: &ModelEvidence{
			Tier: "strong", Model: "stub/model", AgentName: "stub",
			PerStage: []StageModel{
				{Stage: "spec", Tier: "strong", Model: "stub/model", AgentName: "stub"},
			},
		},
		Capabilities: &CapabilityProfile{
			Declared:  []string{"fs.read"},
			Effective: []StageCapabilityProfile{{Stage: "spec", Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)}},
		},
		Adapter: &AdapterEvidence{Name: "stub", Version: "1.0.0"},
	}
	patch := Body{Invocations: []InvocationEvidence{testInvocation("inv-run-2", "review", 0)}}

	merged := mergeBodies(existing, patch)

	if merged.Schema != schemaVersion {
		t.Fatalf("Schema = %q, want upgraded to 2", merged.Schema)
	}
	if len(merged.Prompts) != 0 || merged.Model != nil || len(merged.Capabilities.Effective) != 0 {
		t.Errorf("legacy sections must be cleared after the upgrade: %+v", merged)
	}
	if merged.Adapter != nil && (merged.Adapter.Name != "" || merged.Adapter.Version != "") {
		t.Errorf("legacy adapter fields must be cleared: %+v", merged.Adapter)
	}
	if len(merged.Invocations) != 3 {
		t.Fatalf("invocations = %d, want 3 (spec + impl synthesized, review real): %+v",
			len(merged.Invocations), merged.Invocations)
	}
	// Nothing was lost: the synthesized records carry the legacy facts.
	byStage := make(map[string]InvocationEvidence, len(merged.Invocations))
	for _, record := range merged.Invocations {
		byStage[record.Stage] = record
	}
	spec := byStage["spec"]
	if spec.Prompt.StagePromptHash != "spec-hash" || spec.Model.Options.Model != "stub/model" ||
		spec.Capabilities.Role != "implementer" || spec.Adapter.ID != "stub" {
		t.Errorf("spec synthesis lost legacy facts: %+v", spec)
	}
	if byStage["impl"].Prompt.StagePromptHash != "impl-hash" {
		t.Errorf("impl synthesis lost its prompt hash: %+v", byStage["impl"])
	}
	if byStage["review"].InvocationID != "inv-run-2" {
		t.Errorf("real v2 record must survive the upgrade: %+v", byStage["review"])
	}
}

// TestEncodeBody_NeverWritesLegacyFields (ADR 0005 D9): the write path speaks
// one schema - even a decoded v1 body re-encodes as schema 2 with its legacy
// sections converted, never alongside invocations.
func TestEncodeBody_NeverWritesLegacyFields(t *testing.T) {
	t.Parallel()
	v1 := Body{
		Schema:  schemaVersionV1,
		Prompts: []PromptRevision{{StageID: "spec", Hash: "spec-hash"}},
	}
	encoded, err := encodeBody(v1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded) == "" {
		t.Fatal("empty encode")
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if generic["schema_version"] != schemaVersion {
		t.Errorf("schema_version = %v, want 2", generic["schema_version"])
	}
	if _, present := generic["prompts"]; present {
		t.Error("encoded body still carries the legacy prompts section")
	}
	if _, present := generic["model"]; present {
		t.Error("encoded body still carries the legacy model section")
	}
	if _, present := generic["invocations"]; !present {
		t.Error("upgraded body must carry the invocations section")
	}
}

// TestInvocationRecords_V1AndV2Equivalent (ADR 0005 D9): the accessor
// synthesizes records for a v1 body that are equivalent to a v2 body
// describing the same run - one code path for every consumer.
func TestInvocationRecords_V1AndV2Equivalent(t *testing.T) {
	t.Parallel()
	v1 := Body{
		Schema:  schemaVersionV1,
		Prompts: []PromptRevision{{StageID: "spec", Hash: "spec-hash"}},
		Model: &ModelEvidence{
			PerStage: []StageModel{{Stage: "spec", Tier: "strong", Model: "stub/model"}},
		},
		Capabilities: &CapabilityProfile{
			Effective: []StageCapabilityProfile{{Stage: "spec", Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)}},
		},
		Adapter: &AdapterEvidence{Name: "stub", Version: "1.0.0"},
	}
	v2 := Body{
		Schema: schemaVersion,
		Invocations: []InvocationEvidence{{
			Stage:        "spec",
			Adapter:      InvocationAdapter{ID: "stub", AdapterVersion: "1.0.0"},
			Model:        models.Selection{Tier: "strong", Provider: "stub", Options: models.Options{Model: "stub/model"}},
			Prompt:       InvocationPrompt{StagePromptHash: "spec-hash"},
			Capabilities: InvocationCaps{Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)},
		}},
	}
	left := v1.InvocationRecords()
	right := v2.InvocationRecords()
	if len(left) != 1 || len(right) != 1 {
		t.Fatalf("records = %d / %d, want 1 / 1", len(left), len(right))
	}
	if left[0].Stage != right[0].Stage ||
		left[0].Model.Options.Model != right[0].Model.Options.Model ||
		left[0].Prompt.StagePromptHash != right[0].Prompt.StagePromptHash ||
		left[0].Capabilities.Role != right[0].Capabilities.Role ||
		left[0].Adapter.ID != right[0].Adapter.ID {
		t.Errorf("synthesized v1 record is not equivalent to the v2 record:\n v1: %+v\n v2: %+v", left[0], right[0])
	}
}

// TestCarriesLegacySections backs the corrections-endpoint refusal: each
// schema-1-only shape is detected, a clean v2 patch is not.
func TestCarriesLegacySections(t *testing.T) {
	t.Parallel()
	if (Body{}).CarriesLegacySections() {
		t.Error("empty body must not carry legacy sections")
	}
	if !(Body{Prompts: []PromptRevision{{StageID: "s"}}}).CarriesLegacySections() {
		t.Error("prompts must be detected")
	}
	if !(Body{Model: &ModelEvidence{Tier: "t"}}).CarriesLegacySections() {
		t.Error("model must be detected")
	}
	if !(Body{Capabilities: &CapabilityProfile{Effective: []StageCapabilityProfile{{Stage: "s"}}}}).CarriesLegacySections() {
		t.Error("capabilities.effective must be detected")
	}
	if !(Body{Adapter: &AdapterEvidence{Name: "stub"}}).CarriesLegacySections() {
		t.Error("adapter.name must be detected")
	}
	if (Body{Invocations: []InvocationEvidence{testInvocation("inv-1", "s", 0)}}).CarriesLegacySections() {
		t.Error("a v2 patch carrying only invocations must pass")
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
		Invocations:  []InvocationEvidence{testInvocation("inv-1", "spec", 0)},
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

// TestMergeBodies_TransitionsAppendUnique confirms the D7 transitions section
// appends by (From, To, Condition, Cycle) so a re-resolution of the same edge
// under a retry collapses to one record.
func TestMergeBodies_TransitionsAppendUnique(t *testing.T) {
	t.Parallel()
	base := Body{Transitions: []TransitionRecord{
		{From: "review", To: "fix", Condition: `verdict == "changes_requested"`, Cycle: 0},
	}}
	patch := Body{Transitions: []TransitionRecord{
		{From: "review", To: "fix", Condition: `verdict == "changes_requested"`, Cycle: 1},
		{From: "review", To: "done", Condition: `verdict == "approved"`, Cycle: 1},
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Transitions) != 3 {
		t.Fatalf("merged transitions len = %d, want 3 (distinct edges), got %+v", len(merged.Transitions), merged.Transitions)
	}
}

// TestMergeBodies_StopsAppendUnique confirms the stops section appends by
// (Stage, Reason, Cycle) so a repeat pause collapses.
func TestMergeBodies_StopsAppendUnique(t *testing.T) {
	t.Parallel()
	base := Body{Stops: []StopRecord{
		{Stage: "fix", Reason: "fix_budget_exhausted", Cycle: 1},
	}}
	patch := Body{Stops: []StopRecord{
		{Stage: "fix", Reason: "fix_budget_exhausted", Cycle: 1}, // duplicate -> collapsed
		{Stage: "review", Reason: "verdict_unreadable", Cycle: 0},
	}}
	merged := mergeBodies(base, patch)
	if len(merged.Stops) != 2 {
		t.Fatalf("merged stops len = %d, want 2 (duplicate collapsed), got %+v", len(merged.Stops), merged.Stops)
	}
}

// TestNewSections_DoNotAffectCompleteness is the D7 invariant: the transitions
// and stops sections must NOT drive evidence_complete, so a pack with no branch
// does not read as degraded. A body with the branching sections populated but
// none of the completeness-driving sections present must still report
// incomplete (and list the same missing sections as before).
func TestNewSections_DoNotAffectCompleteness(t *testing.T) {
	t.Parallel()
	body := Body{
		Schema:      "1",
		Transitions: []TransitionRecord{{From: "review", To: "fix", Cycle: 0}},
		Stops:       []StopRecord{{Stage: "fix", Reason: "fix_budget_exhausted"}},
	}
	if body.IsEvidenceComplete() {
		t.Error("a body with only transitions/stops must not be evidence-complete")
	}
	missing := body.MissingSections()
	for _, section := range missing {
		if section == "transitions" || section == "stops" {
			t.Errorf("MissingSections reports %q — the new sections must not be in expectedSections", section)
		}
	}
	// The expected sections list now includes context (ADR 0002).
	wantMissing := []string{"memory", "context", "human_gates", "artifacts", "checks", "capabilities", "invocations"}
	if len(missing) != len(wantMissing) {
		t.Fatalf("MissingSections = %v, want %v", missing, wantMissing)
	}
}
