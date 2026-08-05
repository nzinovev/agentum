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
	t.Run("none granted renders deny-by-default notice", func(t *testing.T) {
		t.Parallel()
		got := Render(Block{Stage: "impl", Gate: "auto", ArtifactDir: "/x"})
		// An empty capability set is a deny-by-default profile, not "native
		// defaults" — the rendered text must say the agent may only write its
		// structured result, so a reader does not assume the runtime left tools
		// unrestricted.
		if !strings.Contains(got, "No capabilities granted") {
			t.Error("absent capabilities must render the deny-by-default notice")
		}
		if strings.Contains(got, "native defaults") {
			t.Error("the old 'native defaults' wording must not appear — profiles are code-enforced")
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

func TestRender_VerdictPath_WhenSet(t *testing.T) {
	t.Parallel()
	got := Render(Block{
		Stage: "review", Gate: "auto", ArtifactDir: "/wt/.agentum/T1/.ag-artifacts/review",
		VerdictPath: "/wt/.agentum/T1/.ag-artifacts/review/verdict.json",
	})
	if !strings.Contains(got, "/wt/.agentum/T1/.ag-artifacts/review/verdict.json") {
		t.Errorf("verdict path not rendered; got:\n%s", got)
	}
	if !strings.Contains(got, "verdict") || !strings.Contains(got, "approved | changes_requested") {
		t.Errorf("verdict contract schema not rendered; got:\n%s", got)
	}
	if !strings.Contains(got, "changes_requested requires at least one finding") {
		t.Errorf("changes_requested-requires-findings rule not rendered; got:\n%s", got)
	}
}

func TestRender_VerdictPath_OmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	got := Render(Block{Stage: "spec", Gate: "auto", ArtifactDir: "/x"})
	if strings.Contains(got, "Reviewer verdict") {
		t.Errorf("verdict section must not render when VerdictPath is empty; got:\n%s", got)
	}
}

func TestRender_ReviewFindings_WhenSet(t *testing.T) {
	t.Parallel()
	got := Render(Block{
		Stage: "fix", Gate: "auto_if_clean", ArtifactDir: "/wt/.agentum/T1/.ag-artifacts/fix",
		ReviewFindings: &ReviewRef{
			Stage: "review", Path: "/wt/.agentum/T1/.ag-artifacts/review/verdict.json", Count: 3,
		},
	})
	if !strings.Contains(got, "review") {
		t.Errorf("reviewer stage not named; got:\n%s", got)
	}
	if !strings.Contains(got, "/wt/.agentum/T1/.ag-artifacts/review/verdict.json") {
		t.Errorf("findings path not rendered; got:\n%s", got)
	}
	if !strings.Contains(got, "3 finding(s)") {
		t.Errorf("finding count not rendered; got:\n%s", got)
	}
}

func TestRender_ReviewFindings_OmittedWhenNil(t *testing.T) {
	t.Parallel()
	got := Render(Block{Stage: "fix", Gate: "auto", ArtifactDir: "/x"})
	if strings.Contains(got, "Reviewer findings to address") {
		t.Errorf("findings section must not render when ReviewFindings is nil; got:\n%s", got)
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
