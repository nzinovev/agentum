//go:build integration

// The integration test in this file drives a real opencode subprocess and is
// excluded from CI (which has no opencode binary or credentials). Run locally:
//
//	go test -tags integration ./internal/agent/ -run TestOpencodeLive -v
//
// It proves the F.2 reference adapter end-to-end: invoke → stream → session-id
// capture → result.json read+parse.
package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpencodeLive_InvokeStreamResult(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}

	workdir := t.TempDir()
	stage := "spec"
	artifactDir := filepath.Join(workdir, ".agentum", "wt", ".ag-artifacts", stage)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}

	prompt := "You are the spec stage. In one sentence, describe what you would do, then finish."
	routing := "## Stage\nspec\n\n" + ResultContractPreamble(artifactDir)

	a := NewOpencodeAdapter("opencode")

	// First invocation: fresh. Capture the session id for the resume step.
	first := runOnce(t, a, Invocation{
		Workdir:      workdir,
		ArtifactDir:  artifactDir,
		Prompt:       prompt,
		RoutingBlock: routing,
	})
	if first.SessionID == "" {
		t.Fatal("first run: SessionID not captured from the stream")
	}
	if !strings.HasPrefix(first.SessionID, "ses_") {
		t.Errorf("first run: SessionID = %q, want ses_ prefix", first.SessionID)
	}
	if first.Status == "" {
		t.Error("first run: result.json status empty — file not parsed")
	}
	t.Logf("first run ok: session=%s status=%q tokens=%d cost=%.4f",
		first.SessionID, first.Status, first.Telemetry.Tokens.Total, first.Telemetry.Cost)

	// Second invocation: resume the same session. Proves --session passthrough
	// and non-destructive resume — the agent keeps its reasoning thread.
	second := runOnce(t, a, Invocation{
		Workdir:       workdir,
		ArtifactDir:   artifactDir,
		Prompt:        "Now reply with the single word: done",
		RoutingBlock:  routing,
		ResumeSession: first.SessionID,
	})
	if second.SessionID != first.SessionID {
		t.Errorf("resume: session id changed: first=%s second=%s", first.SessionID, second.SessionID)
	}
	if second.Status == "" {
		t.Error("resume: result.json status empty — file not parsed")
	}
	t.Logf("resume ok: session=%s status=%q tokens=%d cost=%.4f",
		second.SessionID, second.Status, second.Telemetry.Tokens.Total, second.Telemetry.Cost)
}

// runOnce invokes the adapter and returns the terminal Result, failing the test
// on any stream error.
func runOnce(t *testing.T, a *OpencodeAdapter, inv Invocation) *Result {
	t.Helper()
	ch, err := a.Invoke(context.Background(), inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var (
		gotResult  *Result
		gotError   error
		streamText strings.Builder
	)
	for ev := range ch {
		switch ev.Kind {
		case EventStream:
			streamText.WriteString(ev.Chunk)
		case EventResult:
			gotResult = ev.Result
		case EventError:
			gotError = ev.Err
		}
	}
	if gotError != nil {
		t.Fatalf("run errored: %v (stream so far: %q)", gotError, streamText.String())
	}
	if gotResult == nil {
		t.Fatal("no EventResult on the stream")
	}
	return gotResult
}
