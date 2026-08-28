package manifest

import (
	"encoding/json"
	"testing"

	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/taskinput"
)

func TestDiffManifests_IdenticalEmpty(t *testing.T) {
	t.Parallel()
	diff := DiffManifests(Body{Schema: "1"}, Body{Schema: "1"})
	if !diff.Empty() {
		t.Errorf("diff of empty bodies not empty: %+v", diff)
	}
}

func TestDiffManifests_InputRevision(t *testing.T) {
	t.Parallel()
	left := Body{Input: &InputEvidence{TaskID: "T1", Revision: "v1"}}
	right := Body{Input: &InputEvidence{TaskID: "T1", Revision: "v2"}}
	diff := DiffManifests(left, right)
	if diff.Input == nil {
		t.Fatal("Input diff missing for revision change")
	}
	if diff.Input.Reason != "input-revision" {
		t.Errorf("Reason = %q", diff.Input.Reason)
	}
}

// TestDiffManifests_InputRevisionCanonicalAcrossFormatting is the ADR 0004 D9
// acceptance: two runs whose request bodies differ only in key order and
// whitespace produce the same canonical revision, so the input diff axis
// reports NO delta — the fix the axis has always claimed to mean. A different
// description still reports input-revision.
func TestDiffManifests_InputRevisionCanonicalAcrossFormatting(t *testing.T) {
	t.Parallel()
	inputEvidenceFor := func(description, overridesJSON string) *InputEvidence {
		overrides, parseErr := taskinput.ParseOverrides([]byte(overridesJSON))
		if parseErr != nil {
			t.Fatalf("parse %q: %v", overridesJSON, parseErr)
		}
		request := taskinput.Request{
			Title:       "Baseline run for pack stack-neutrality validation",
			Description: description,
			Overrides:   overrides,
		}
		return &InputEvidence{
			TaskID: "T1", Title: request.Title,
			Description: request.Description,
			Revision:    request.Revision(),
			PipelineRef: "backend-development@0.1.0",
		}
	}
	compact := Body{Input: inputEvidenceFor("Log /healthz at Debug.", `{"checks":{"required":["verify"],"optional":[]}}`)}
	reformatted := Body{Input: inputEvidenceFor("Log /healthz at Debug.", `{
		"checks": { "optional": [], "required": ["verify"] }
	}`)}
	if delta := DiffManifests(compact, reformatted).Input; delta != nil {
		t.Errorf("same request, different formatting: unexpected input delta %+v", delta)
	}

	changed := Body{Input: inputEvidenceFor("Log /healthz AND /readyz at Debug.", `{"checks":{"required":["verify"],"optional":[]}}`)}
	if delta := DiffManifests(compact, changed).Input; delta == nil || delta.Reason != "input-revision" {
		t.Errorf("changed description: want input-revision delta, got %+v", delta)
	}
}

func TestDiffManifests_PackVersionChange(t *testing.T) {
	t.Parallel()
	left := Body{Pack: &PackEvidence{Name: "p", Version: "1.0.0", ContentHash: "h1"}}
	right := Body{Pack: &PackEvidence{Name: "p", Version: "1.1.0", ContentHash: "h2"}}
	diff := DiffManifests(left, right)
	if diff.Pack == nil {
		t.Fatal("Pack diff missing for version change")
	}
}

func TestDiffManifests_PackSameHashNoDiff(t *testing.T) {
	t.Parallel()
	left := Body{Pack: &PackEvidence{Name: "p", Version: "1.0.0", ContentHash: "same"}}
	right := Body{Pack: &PackEvidence{Name: "p", Version: "1.0.0", ContentHash: "same"}}
	if !DiffManifests(left, right).Empty() {
		t.Error("identical packs produced a diff")
	}
}

func TestDiffManifests_PromptMissingOnOneSide(t *testing.T) {
	t.Parallel()
	left := Body{Prompts: []PromptRevision{{StageID: "spec", Hash: "h1"}}}
	right := Body{}
	diff := DiffManifests(left, right)
	if diff.Prompts == nil {
		t.Fatal("expected Prompts delta for missing-on-right")
	}
}

func TestDiffManifests_PromptHashDiffers(t *testing.T) {
	t.Parallel()
	left := Body{Prompts: []PromptRevision{{StageID: "spec", Hash: "h1"}}}
	right := Body{Prompts: []PromptRevision{{StageID: "spec", Hash: "h2"}}}
	diff := DiffManifests(left, right)
	if diff.Prompts == nil {
		t.Fatal("expected Prompts delta for hash change")
	}
	if diff.Prompts.Reason != "prompt-hash" {
		t.Errorf("Reason = %q", diff.Prompts.Reason)
	}
}

// diffBodyOfOneAttempt builds a body carrying one invocation record, the
// minimal shape the per-attempt axes read.
func diffBodyOfOneAttempt(record InvocationEvidence) Body {
	return Body{Schema: schemaVersion, Invocations: []InvocationEvidence{record}}
}

func TestDiffManifests_ModelTierChange(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-1", "spec", 0))
	tierChanged := testInvocation("inv-2", "spec", 0)
	tierChanged.Model.Tier = "fast"
	right := diffBodyOfOneAttempt(tierChanged)
	delta := DiffManifests(left, right).Model
	if delta == nil {
		t.Fatal("expected Model delta for tier change")
	}
	if delta.Reason != "model-tier" {
		t.Errorf("Reason = %q, want model-tier", delta.Reason)
	}
}

func TestDiffManifests_ModelIdChange(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-1", "spec", 0))
	modelChanged := testInvocation("inv-2", "spec", 0)
	modelChanged.Model.Options.Model = "other/model"
	right := diffBodyOfOneAttempt(modelChanged)
	delta := DiffManifests(left, right).Model
	if delta == nil {
		t.Fatal("expected Model delta for id change")
	}
	if delta.Reason != "model-id" {
		t.Errorf("Reason = %q, want model-id", delta.Reason)
	}
}

func TestDiffManifests_CapabilitySetChange(t *testing.T) {
	t.Parallel()
	left := Body{Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}}}
	right := Body{Capabilities: &CapabilityProfile{Declared: []string{"fs.read", "fs.write"}}}
	if DiffManifests(left, right).Capabilities == nil {
		t.Error("expected Capabilities delta for set change")
	}
}

func TestDiffManifests_CapabilityOrderIndependent(t *testing.T) {
	t.Parallel()
	left := Body{Capabilities: &CapabilityProfile{Declared: []string{"a", "b"}}}
	right := Body{Capabilities: &CapabilityProfile{Declared: []string{"b", "a"}}}
	if DiffManifests(left, right).Capabilities != nil {
		t.Error("expected no delta for reordered same set")
	}
}

func TestDiffManifests_MemorySliceChange(t *testing.T) {
	t.Parallel()
	left := Body{Memory: &MemorySlice{Scope: "project", Hashes: []string{"m1", "m2"}, Entries: 2}}
	right := Body{Memory: &MemorySlice{Scope: "project", Hashes: []string{"m1"}, Entries: 1}}
	if DiffManifests(left, right).Memory == nil {
		t.Error("expected Memory delta for slice change")
	}
}

func TestDiffManifests_InputArtifactsChange(t *testing.T) {
	t.Parallel()
	left := Body{Artifacts: &ArtifactEvidence{
		Inputs: []ArtifactRef{{Name: "spec.md", ContentHash: "h1"}},
	}}
	right := Body{Artifacts: &ArtifactEvidence{
		Inputs: []ArtifactRef{{Name: "spec.md", ContentHash: "h2"}},
	}}
	if DiffManifests(left, right).InputArtifacts == nil {
		t.Error("expected InputArtifacts delta for hash change")
	}
}

func TestDiffManifests_InputArtifactsOutputIgnored(t *testing.T) {
	t.Parallel()
	// Outputs are results, not inputs; the diff ignores them.
	left := Body{Artifacts: &ArtifactEvidence{
		Outputs: []ArtifactRef{{Name: "impl.md", ContentHash: "h1"}},
	}}
	right := Body{Artifacts: &ArtifactEvidence{
		Outputs: []ArtifactRef{{Name: "impl.md", ContentHash: "h2"}},
	}}
	if DiffManifests(left, right).InputArtifacts != nil {
		t.Error("expected no InputArtifacts delta for output-only change")
	}
}

func TestDiffManifests_GitBaseChange(t *testing.T) {
	t.Parallel()
	left := Body{Git: &GitEvidence{BaseCommit: "b1"}}
	right := Body{Git: &GitEvidence{BaseCommit: "b2"}}
	if DiffManifests(left, right).GitBase == nil {
		t.Error("expected GitBase delta for base_commit change")
	}
}

func TestDiffManifests_ExecutionCoordinateChange(t *testing.T) {
	t.Parallel()
	left := Body{ExecutionCoordinate: &ExecutionCoordinate{DeliveryStep: "step-1"}}
	right := Body{ExecutionCoordinate: &ExecutionCoordinate{DeliveryStep: "step-2"}}
	if DiffManifests(left, right).ExecutionCoordinate == nil {
		t.Error("expected ExecutionCoordinate delta")
	}
}

func TestDiffManifests_ExecutionCoordinateSingleUnitVsSingleUnit(t *testing.T) {
	t.Parallel()
	// Both empty (single-unit): no delta — absence is not a difference.
	if DiffManifests(Body{}, Body{}).ExecutionCoordinate != nil {
		t.Error("expected no ExecutionCoordinate delta for two single-unit runs")
	}
}

func TestDiffManifests_AdapterVersionChange(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-1", "spec", 0))
	versionChanged := testInvocation("inv-2", "spec", 0)
	versionChanged.Adapter.AdapterVersion = "2.0.0"
	right := diffBodyOfOneAttempt(versionChanged)
	delta := DiffManifests(left, right).Adapter
	if delta == nil {
		t.Fatal("expected Adapter delta for adapter version change")
	}
	if delta.Reason != "adapter-version" {
		t.Errorf("Reason = %q, want adapter-version", delta.Reason)
	}
}

func TestDiffManifests_ChecksVersionChange(t *testing.T) {
	t.Parallel()
	left := Body{Checks: &CheckEvidence{SetVersion: "v1"}}
	right := Body{Checks: &CheckEvidence{SetVersion: "v2"}}
	if DiffManifests(left, right).Checks == nil {
		t.Error("expected Checks delta for set version change")
	}
}

func TestDiffManifests_ProjectBaseChange(t *testing.T) {
	t.Parallel()
	left := Body{Project: &ProjectEvidence{ProjectID: "P1", BaseCommit: "b1"}}
	right := Body{Project: &ProjectEvidence{ProjectID: "P1", BaseCommit: "b2"}}
	if DiffManifests(left, right).Project == nil {
		t.Error("expected Project delta for base_commit change")
	}
}

func TestDiffManifests_OneSideMissingSection(t *testing.T) {
	t.Parallel()
	left := Body{Adapter: &AdapterEvidence{Name: "opencode", Version: "v1"}}
	right := Body{}
	if DiffManifests(left, right).Adapter == nil {
		t.Error("expected Adapter delta for missing-on-right")
	}
}

// TestDiffManifests_IdenticalRunsEmpty (ADR 0005 D10): two runs with the same
// attempt set and the same per-attempt inputs diff to empty.
func TestDiffManifests_IdenticalRunsEmpty(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-left", "spec", 0))
	left.Invocations = append(left.Invocations, testInvocation("inv-left-2", "review", 0))
	right := diffBodyOfOneAttempt(testInvocation("inv-right", "spec", 0))
	right.Invocations = append(right.Invocations, testInvocation("inv-right-2", "review", 0))
	if diff := DiffManifests(left, right); !diff.Empty() {
		t.Errorf("identical runs must diff empty, got %+v", diff)
	}
}

// TestDiffManifests_RuntimeVersionOnly (ADR 0005 D10): same tier and model,
// different runtime build - adapter-runtime-version and NOTHING else. This is
// the axis that answers "identical configuration, different result".
func TestDiffManifests_RuntimeVersionOnly(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-left", "spec", 0))
	swapped := testInvocation("inv-right", "spec", 0)
	swapped.Adapter.RuntimeVersion = "1.19.0"
	swapped.Prompt.RenderedHash = "rendered-inv-right" // distinct, must not matter
	right := diffBodyOfOneAttempt(swapped)
	diff := DiffManifests(left, right)
	if diff.Adapter == nil || diff.Adapter.Reason != "adapter-runtime-version" {
		t.Fatalf("Adapter delta = %+v, want adapter-runtime-version", diff.Adapter)
	}
	if diff.Prompts != nil || diff.Model != nil || diff.Capabilities != nil {
		t.Errorf("runtime-version-only change must light no other axis: %+v", diff)
	}
}

// TestDiffManifests_FixCycleSecondAttemptDifferentModel (ADR 0005 D10): both
// runs share (review, 0) and (review, 1); the second attempt ran on a
// different model on the right - the shared key reports model-id, not
// model-set, because the value difference is the more specific answer and
// shared keys are compared first.
func TestDiffManifests_FixCycleSecondAttemptDifferentModel(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-left", "review", 0))
	left.Invocations = append(left.Invocations, testInvocation("inv-left-2", "review", 1))

	right := diffBodyOfOneAttempt(testInvocation("inv-right", "review", 0))
	secondAttempt := testInvocation("inv-right-2", "review", 1)
	secondAttempt.Model.Options.Model = "other/model"
	right.Invocations = append(right.Invocations, secondAttempt)

	delta := DiffManifests(left, right).Model
	if delta == nil {
		t.Fatal("expected Model delta for a differing (review, 1) model")
	}
	if delta.Reason != "model-id" {
		t.Errorf("Reason = %q, want model-id (shared-key value difference, not model-set)", delta.Reason)
	}
}

// TestDiffManifests_ExtraFixCycleReportsModelSet (ADR 0005 D10): a run with an
// extra attempt where the other has none reports the set difference.
func TestDiffManifests_ExtraFixCycleReportsModelSet(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-left", "review", 0))
	left.Invocations = append(left.Invocations, testInvocation("inv-left-2", "review", 1))
	right := diffBodyOfOneAttempt(testInvocation("inv-right", "review", 0))

	diff := DiffManifests(left, right)
	if diff.Model == nil || diff.Model.Reason != "model-set" {
		t.Fatalf("Model delta = %+v, want model-set", diff.Model)
	}
	if diff.Prompts == nil || diff.Prompts.Reason != "prompt-set" {
		t.Fatalf("Prompts delta = %+v, want prompt-set", diff.Prompts)
	}
}

// TestDiffManifests_V1AgainstV2OfSameRunIsEmpty (ADR 0005 D9/D10): a schema-1
// body and a schema-2 body describing the same run diff to empty - the
// accessor synthesizes equivalent records, one code path for both versions.
func TestDiffManifests_V1AgainstV2OfSameRunIsEmpty(t *testing.T) {
	t.Parallel()
	v1 := Body{
		Schema:  schemaVersionV1,
		Prompts: []PromptRevision{{StageID: "spec", Hash: "hash-spec"}},
		Model: &ModelEvidence{
			PerStage: []StageModel{{Stage: "spec", Tier: "strong", Model: "stub/model"}},
		},
		Capabilities: &CapabilityProfile{
			Declared:  []string{"fs.read"},
			Effective: []StageCapabilityProfile{{Stage: "spec", Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)}},
		},
		Adapter: &AdapterEvidence{Name: "stub", Version: "1.0.0", DeclaredCapabilities: []string{"fs.read"}},
	}
	v2 := Body{
		Schema: schemaVersion,
		Invocations: []InvocationEvidence{{
			Stage:        "spec",
			Adapter:      InvocationAdapter{ID: "stub", AdapterVersion: "1.0.0"},
			Model:        models.Selection{Tier: "strong", Provider: "stub", Options: models.Options{Model: "stub/model"}},
			Prompt:       InvocationPrompt{StagePromptHash: "hash-spec"},
			Capabilities: InvocationCaps{Role: "implementer", Profile: json.RawMessage(`{"grants":[]}`)},
		}},
		Capabilities: &CapabilityProfile{Declared: []string{"fs.read"}},
		Adapter:      &AdapterEvidence{ID: "stub", AdapterVersion: "1.0.0", DeclaredCapabilities: []string{"fs.read"}},
	}
	if diff := DiffManifests(v1, v2); !diff.Empty() {
		t.Errorf("a v1 manifest against a v2 manifest of the same run must diff empty, got %+v", diff)
	}
}

// TestDiffManifests_RenderedHashIsNotADiffAxis (ADR 0005 D8): the rendered
// prompt hash differs on every run by construction (task id, absolute paths),
// so it must never light any axis.
func TestDiffManifests_RenderedHashIsNotADiffAxis(t *testing.T) {
	t.Parallel()
	left := diffBodyOfOneAttempt(testInvocation("inv-left", "spec", 0))
	renderedOnly := testInvocation("inv-right", "spec", 0)
	renderedOnly.Prompt.RenderedHash = "a-totally-different-render"
	right := diffBodyOfOneAttempt(renderedOnly)
	if diff := DiffManifests(left, right); !diff.Empty() {
		t.Errorf("a rendered-hash-only difference must light no axis, got %+v", diff)
	}
}

// TestDiffManifests_ResumeDoesNotCollapseAnAttempt (ADR 0005 D10): a resume
// inherits the resumed attempt's cycle (ADR 0001 D4) but gets its own
// stage_invocations row, so one run can hold two records sharing (stage,
// cycle). Keying the index on that pair alone dropped the earlier record and
// made every per-attempt axis blind to it at once. The ordinal — derived from
// Sequence at index time — is what keeps both comparable.
func TestDiffManifests_ResumeDoesNotCollapseAnAttempt(t *testing.T) {
	t.Parallel()
	leftFirst := testInvocation("inv-left-1", "plan", 0)
	leftFirst.Sequence = 1
	leftResume := testInvocation("inv-left-2", "plan", 0)
	leftResume.Sequence = 2
	left := Body{Schema: schemaVersion, Invocations: []InvocationEvidence{leftFirst, leftResume}}

	// The right run differs ONLY in its first attempt; its resume matches.
	rightFirst := testInvocation("inv-right-1", "plan", 0)
	rightFirst.Sequence = 1
	rightFirst.Prompt.StagePromptHash = "hash-plan-edited"
	rightFirst.Model.Options.Model = "other/model"
	rightResume := testInvocation("inv-right-2", "plan", 0)
	rightResume.Sequence = 2
	right := Body{Schema: schemaVersion, Invocations: []InvocationEvidence{rightFirst, rightResume}}

	if indexed := indexInvocations(left.Invocations); len(indexed) != 2 {
		t.Fatalf("indexInvocations kept %d of 2 records; the resumed attempt was collapsed", len(indexed))
	}
	diff := DiffManifests(left, right)
	if diff.Prompts == nil || diff.Prompts.Reason != "prompt-hash" {
		t.Errorf("Prompts delta = %+v, want prompt-hash from the first attempt", diff.Prompts)
	}
	if diff.Model == nil || diff.Model.Reason != "model-id" {
		t.Errorf("Model delta = %+v, want model-id from the first attempt", diff.Model)
	}
}

// TestDiffManifests_ResumeIsAnAttemptInTheSetDiff (ADR 0005 D10): a run whose
// stage was resumed carries one more attempt at the same coordinate than a run
// that was not, and the set difference must say so rather than reporting the
// two runs as equal.
func TestDiffManifests_ResumeIsAnAttemptInTheSetDiff(t *testing.T) {
	t.Parallel()
	first := testInvocation("inv-left-1", "plan", 0)
	first.Sequence = 1
	resume := testInvocation("inv-left-2", "plan", 0)
	resume.Sequence = 2
	withResume := Body{Schema: schemaVersion, Invocations: []InvocationEvidence{first, resume}}
	withoutResume := diffBodyOfOneAttempt(testInvocation("inv-right-1", "plan", 0))

	diff := DiffManifests(withResume, withoutResume)
	if diff.Prompts == nil || diff.Prompts.Reason != "prompt-set" {
		t.Errorf("Prompts delta = %+v, want prompt-set", diff.Prompts)
	}
	if diff.Model == nil || diff.Model.Reason != "model-set" {
		t.Errorf("Model delta = %+v, want model-set", diff.Model)
	}
}
