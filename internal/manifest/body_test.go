package manifest

import (
	"testing"
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
