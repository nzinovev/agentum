package manifest

import (
	"testing"

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

func TestDiffManifests_ModelTierChange(t *testing.T) {
	t.Parallel()
	left := Body{Model: &ModelEvidence{Tier: "fast", Model: "m1"}}
	right := Body{Model: &ModelEvidence{Tier: "strong", Model: "m1"}}
	if DiffManifests(left, right).Model == nil {
		t.Error("expected Model delta for tier change")
	}
}

func TestDiffManifests_ModelIdChange(t *testing.T) {
	t.Parallel()
	left := Body{Model: &ModelEvidence{Tier: "fast", Model: "m1"}}
	right := Body{Model: &ModelEvidence{Tier: "fast", Model: "m2"}}
	if DiffManifests(left, right).Model == nil {
		t.Error("expected Model delta for id change")
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
	left := Body{Adapter: &AdapterEvidence{Name: "opencode", Version: "v1"}}
	right := Body{Adapter: &AdapterEvidence{Name: "opencode", Version: "v2"}}
	if DiffManifests(left, right).Adapter == nil {
		t.Error("expected Adapter delta for version change")
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
