package routing

import (
	"strings"
	"testing"
)

func TestRender_RequiredFields(t *testing.T) {
	t.Parallel()
	got := Render(Block{TaskID: "T1", ProjectName: "My App", Stage: "spec", Gate: "human_approval", ArtifactDir: "/wt/.agentum/T1/.ag-artifacts/spec"})

	checks := map[string]bool{
		"stage id present":         strings.Contains(got, "stage **spec**"),
		"gate present":             strings.Contains(got, "gate: human_approval"),
		"task id present":          strings.Contains(got, "task T1"),
		"project name present":     strings.Contains(got, "project My App"),
		"artifact dir in contract": strings.Contains(got, "/wt/.agentum/T1/.ag-artifacts/spec/result.json"),
		"result.json contract":     strings.Contains(got, "`schema_version`: \"1\""),
		"status enum documented":   strings.Contains(got, "\"complete\" | \"partial\" | \"blocked\""),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("Render missing expected content: %s", name)
		}
	}
}

func TestRender_MemoryStub_WhenEmpty(t *testing.T) {
	t.Parallel()
	got := Render(Block{Stage: "spec", Gate: "auto", ArtifactDir: "/x"})
	if !strings.Contains(got, "No project decisions injected yet") {
		t.Error("empty Memory must render the inert stub so the section is always present")
	}
}

func TestRender_MemoryInjected_WhenProvided(t *testing.T) {
	t.Parallel()
	got := Render(Block{Stage: "spec", Gate: "auto", ArtifactDir: "/x", Memory: "- [auth] Use OAuth2 (task 3)"})
	if !strings.Contains(got, "Use OAuth2 (task 3)") {
		t.Error("provided Memory block must appear verbatim in the output")
	}
	if strings.Contains(got, "No project decisions injected yet") {
		t.Error("stub must not appear when Memory is provided")
	}
}

func TestRender_Capabilities(t *testing.T) {
	t.Parallel()
	t.Run("declared", func(t *testing.T) {
		t.Parallel()
		got := Render(Block{Stage: "impl", Gate: "auto", ArtifactDir: "/x", Capabilities: []string{"fs.read", "git"}})
		if !strings.Contains(got, "Granted: fs.read, git") {
			t.Errorf("capabilities not rendered; got:\n%s", got)
		}
	})
	t.Run("none declared renders stub", func(t *testing.T) {
		t.Parallel()
		got := Render(Block{Stage: "impl", Gate: "auto", ArtifactDir: "/x"})
		if !strings.Contains(got, "No capabilities declared") {
			t.Error("absent capabilities must render the inert stub")
		}
	})
}

func TestRender_PriorStages(t *testing.T) {
	t.Parallel()
	// First stage has no prior stages — the section is omitted entirely.
	first := Render(Block{Stage: "spec", Gate: "auto", ArtifactDir: "/x"})
	if strings.Contains(first, "Prior stage artifacts") {
		t.Error("first stage must not render a Prior stage artifacts section")
	}

	// A later stage references its predecessors via filesystem-as-bus.
	later := Render(Block{
		Stage:       "implement",
		Gate:        "auto_on_approval",
		ArtifactDir: "/wt/.agentum/T1/.ag-artifacts/implement",
		PriorStages: []PriorStage{
			{Stage: "spec", Path: "/wt/.agentum/T1/.ag-artifacts/spec/result.json"},
		},
	})
	if !strings.Contains(later, "**spec**: /wt/.agentum/T1/.ag-artifacts/spec/result.json") {
		t.Errorf("prior stage reference not rendered; got:\n%s", later)
	}
}

func TestRender_Deterministic(t *testing.T) {
	t.Parallel()
	// Same input → identical output. The runner caches/prompts rely on this.
	block := Block{Stage: "s", Gate: "auto", ArtifactDir: "/a", Capabilities: []string{"x"}}
	first := Render(block)
	second := Render(block)
	if first != second {
		t.Error("Render must be deterministic for identical input")
	}
}
