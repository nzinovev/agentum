package pack

import (
	"path/filepath"
	"testing"
)

// TestShipped_BackendDevelopmentPack loads and validates the committed
// packs/backend-development pack the way DirSource.Resolve does, so a malformed
// shipped pack fails CI rather than a run. The pack is task 3's deliverable
// (ADR 0003 D1) and its prompts reference routing sections and contract fields
// that the validator cannot check — this test is the structural floor.
func TestShipped_BackendDevelopmentPack(t *testing.T) {
	t.Parallel()
	packDir := filepath.Join("..", "..", "packs", "backend-development")
	loaded, err := Load(packDir)
	if err != nil {
		t.Fatalf("Load shipped backend-development pack: %v", err)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("Validate shipped backend-development pack: %v", err)
	}
	if loaded.Pack.Name != "backend-development" {
		t.Errorf("pack.name = %q, want backend-development (must equal dir name)", loaded.Pack.Name)
	}
	if loaded.Entry != "plan" {
		t.Errorf("entry = %q, want plan", loaded.Entry)
	}
	// The source_write approval is the load-bearing declaration of the whole
	// pipeline (ADR 0003 D3).
	approval, ok := loaded.SourceWriteApproval()
	if !ok {
		t.Fatal("backend-development pack must declare a source_write approval")
	}
	if approval.Name != "plan" || approval.Stage != "plan" || approval.Artifact != "plan.md" {
		t.Errorf("approval = %+v, want {plan plan plan.md source_write}", approval)
	}
	// implement and fix are the source-writing stages the approval protects.
	sources := loaded.SourceWritingStages()
	if !contains(sources, "implement") || !contains(sources, "fix") {
		t.Errorf("SourceWritingStages = %v, want implement and fix", sources)
	}
	// Every non-terminal stage must have loaded prompt text.
	for stageID, stage := range loaded.Stages {
		if stage.Terminal() {
			continue
		}
		if stage.PromptText() == "" {
			t.Errorf("stage %q has empty prompt text", stageID)
		}
	}
}
