package routing

import (
	"strings"
	"testing"
)

// TestRender_ContinuationInsideTaskSection: the user's continuation text lands
// inside the Task section, after the original request, between explicit
// markers — the delivery contract the continue endpoint's text relies on.
func TestRender_ContinuationInsideTaskSection(t *testing.T) {
	t.Parallel()
	got := Render(Block{
		RunID: "T1", ProjectName: "P", Stage: "spec", Gate: "auto", ArtifactDir: "/x",
		Title: "Lower the log level", Description: "Log /healthz at Debug.",
		Continuation: "Use PostgreSQL 17. No new table.",
	})
	if !strings.Contains(got, "--- BEGIN USER CONTINUATION ---") || !strings.Contains(got, "--- END USER CONTINUATION ---") {
		t.Errorf("continuation markers missing; got:\n%s", got)
	}
	if !strings.Contains(got, "Use PostgreSQL 17. No new table.") {
		t.Errorf("continuation text missing; got:\n%s", got)
	}
	// The text must follow the request it continues, inside the same Task
	// section, not after the output contract.
	taskSectionEnd := strings.Index(got, "## Your output contract")
	continuationAt := strings.Index(got, "--- BEGIN USER CONTINUATION ---")
	if taskSectionEnd < 0 || continuationAt < 0 || continuationAt > taskSectionEnd {
		t.Errorf("continuation must render inside the Task section, before the output contract; got:\n%s", got)
	}
	// The original request stays: the continuation adds to the task, it does
	// not replace it.
	if !strings.Contains(got, "Lower the log level") || !strings.Contains(got, "Log /healthz at Debug.") {
		t.Errorf("the original request must survive beside the continuation; got:\n%s", got)
	}
}

// TestRender_NoContinuationSectionWhenEmpty: an empty continuation renders
// nothing — every non-continue invocation, and a continue without text, sees
// the same block as before the field existed.
func TestRender_NoContinuationSectionWhenEmpty(t *testing.T) {
	t.Parallel()
	got := Render(Block{
		RunID: "T1", ProjectName: "P", Stage: "spec", Gate: "auto", ArtifactDir: "/x",
		Title: "Lower the log level", Description: "Log /healthz at Debug.",
	})
	if strings.Contains(got, "USER CONTINUATION") {
		t.Errorf("empty continuation must render no continuation section; got:\n%s", got)
	}
}
