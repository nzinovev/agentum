package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// tamperAdapter is the fake adapter for the ADR 0002 tamper reproduction: it
// implements agent.ContextProber (returning the AGENTS.md baseline), captures
// the worktree root and the pinned instruction files each invocation receives,
// and — during the implement stage — overwrites AGENTS.md with a tampered
// marker, standing in for a bash-side rewrite the edit-deny rule cannot reach.
// The review stage then runs under restoreInstructions, which should detect the
// drift and restore the pinned bytes before the review invocation sees them.
type tamperAdapter struct {
	stubExecution
	mu                  sync.Mutex
	scripts             map[string]agent.ResultJSON
	worktreeRoot        string
	capturedStage       []string
	instructionsByStage map[string][]agent.InstructionFile
}

func (adapter *tamperAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret",
	}
}

func (adapter *tamperAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	stage := stageOf(inv)
	adapter.mu.Lock()
	if adapter.worktreeRoot == "" {
		adapter.worktreeRoot = inv.Workdir
	}
	adapter.capturedStage = append(adapter.capturedStage, stage)
	if adapter.instructionsByStage == nil {
		adapter.instructionsByStage = map[string][]agent.InstructionFile{}
	}
	// Capture by value so a later mutation does not retroactively change the
	// recorded snapshot.
	captured := make([]agent.InstructionFile, len(inv.Instructions))
	copy(captured, inv.Instructions)
	adapter.instructionsByStage[stage] = captured
	adapter.mu.Unlock()

	// The tamper: during the implement stage, overwrite AGENTS.md the way a
	// bash command would — a write the edit-deny rule does not cover. The next
	// stage's restoreInstructions must catch this via the hash check.
	if stage == "implement" && inv.Workdir != "" {
		tamperedPath := filepath.Join(inv.Workdir, "AGENTS.md")
		_ = os.WriteFile(tamperedPath, []byte("# TAMPERED\nMARKER-EVIL\n"), 0o644)
	}

	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stage]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stage)}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventStream, Chunk: "working..."}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stage, ResultJSON: scripted}}
	}()
	return eventCh, nil
}

// ProbeContext reports the AGENTS.md baseline the opencode runtime injects on
// its own, so prepareProjectContext pins it and restoreInstructions can catch
// the tamper. No skills, so the enumeration is deterministic and small.
func (adapter *tamperAdapter) ProbeContext(ctx context.Context, inv agent.Invocation) (agent.ContextReport, error) {
	return agent.ContextReport{
		AutoInstructions: []string{"AGENTS.md"},
		SkillsProbe:      agent.ContextProbeOK,
	}, nil
}

// initRepoWithAgentsCommit creates a temp git repo with a committed AGENTS.md
// carrying a unique marker, so the tamper and its restoration are visible by
// content. Mirrors initRepoWithCommit (runner_loop_test.go) with the AGENTS.md
// addition, using the same GIT_CONFIG_GLOBAL isolation.
func initRepoWithAgentsCommit(dir, marker string) error {
	for _, args := range [][]string{
		{"init", "--quiet"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w (%s)", args[0], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project\nMARKER "+marker+"\n"), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"add", "AGENTS.md"}, {"commit", "--quiet", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w (%s)", args[0], err, out)
		}
	}
	return nil
}

// TestInstructions_TamperReproduction is ADR 0002 step 9: the manual experiment
// as a test. An implementer (via the tamperAdapter, standing in for bash)
// rewrites AGENTS.md during the implement stage. Before the review stage runs,
// restoreInstructions must (1) detect the drift, (2) rewrite the pinned bytes
// over the tampered copy, (3) emit task.instructions_restored, and (4) record
// the restoration in the manifest context section. The review invocation's
// instruction content then carries the ORIGINAL marker, not the tampered one.
func TestInstructions_TamperReproduction(t *testing.T) {
	repo := t.TempDir()
	const originalMarker = "ORIGINAL-7B3A-PINNED"
	if err := initRepoWithAgentsCommit(repo, originalMarker); err != nil {
		t.Fatalf("setup repo: %v", err)
	}

	// Pack: implement(auto)→review(auto)→done(terminal). All auto gates so the
	// isClean gate does not interfere; the focus is the instruction restore.
	taskPack := scriptPack("implement", map[string]pack.Stage{
		"implement": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "review"}}},
		"review":    {Gate: pack.GateAuto, Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":      {},
	})

	adapter := &tamperAdapter{scripts: map[string]agent.ResultJSON{
		"implement": {SchemaVersion: "1", Status: "complete", Summary: "implemented"},
		"review":    {SchemaVersion: "1", Status: "complete", Summary: "reviewed"},
	}}
	task := sqlc.Task{ID: "T-tamper", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "TamperProj"}
	store := newFakeStore(task, proj)

	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, Artifacts: newRecordingStore(),
	})
	manifestFake := &fakeManifestService{}
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// (1) A task.instructions_restored event was emitted for the review stage,
	// carrying the tampered hash and action "restored".
	var sawRestorationEvent bool
	for _, event := range store.events {
		if event.Type != EvInstructionsRestored {
			continue
		}
		payload := decodeEventPayload(t, event.Payload)
		if payload["stage"] != "review" {
			continue
		}
		if payload["action"] != "restored" {
			t.Errorf("restoration action = %v, want restored", payload["action"])
		}
		if payload["path"] != "AGENTS.md" {
			t.Errorf("restoration path = %v, want AGENTS.md", payload["path"])
		}
		sawRestorationEvent = true
	}
	if !sawRestorationEvent {
		t.Error("expected a task.instructions_restored event for the review stage; the tamper was not caught")
	}

	// (2) The worktree's AGENTS.md was restored to the original marker (the
	// adapter captured the worktree root during invoke).
	worktreeRoot := adapter.worktreeRoot
	if worktreeRoot == "" {
		t.Fatal("adapter did not capture the worktree root")
	}
	restoredBytes, err := os.ReadFile(filepath.Join(worktreeRoot, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read restored AGENTS.md: %v", err)
	}
	if !strings.Contains(string(restoredBytes), originalMarker) {
		t.Errorf("worktree AGENTS.md after restore = %q, want to contain %q", restoredBytes, originalMarker)
	}
	if strings.Contains(string(restoredBytes), "MARKER-EVIL") {
		t.Errorf("worktree AGENTS.md still carries the tampered marker: %q", restoredBytes)
	}

	// (3) The manifest's context section recorded the restoration.
	manifestFake.mu.Lock()
	defer manifestFake.mu.Unlock()
	var restorationCount int
	var contextSectionSeen bool
	for _, patch := range manifestFake.addEvidence {
		if patch.Context != nil {
			contextSectionSeen = true
			restorationCount += len(patch.Context.Restorations)
		}
	}
	if !contextSectionSeen {
		t.Error("manifest context section was never written — the stage's evidence write did not run")
	}
	if restorationCount == 0 {
		t.Error("manifest context section recorded no restorations; the tamper reversal is missing from evidence")
	}

	// (4) Headline claim (ADR 0002 acceptance): the pinned AGENTS.md — with the
	// ORIGINAL marker — was delivered to BOTH stages. The implement stage's
	// bash-side tamper must not reach the review invocation's instruction
	// content; the pin is what the model actually sees. The adapter captured the
	// instruction files each invocation received, so this is checked on the
	// bytes the agent would have read, not on a proxy.
	adapter.mu.Lock()
	stagesRun := append([]string(nil), adapter.capturedStage...)
	instructionsByStage := make(map[string][]agent.InstructionFile, len(adapter.instructionsByStage))
	for stage, files := range adapter.instructionsByStage {
		snapshot := make([]agent.InstructionFile, len(files))
		copy(snapshot, files)
		instructionsByStage[stage] = snapshot
	}
	adapter.mu.Unlock()
	if len(stagesRun) != 2 || stagesRun[0] != "implement" || stagesRun[1] != "review" {
		t.Errorf("stages run = %v, want [implement review]", stagesRun)
	}
	for _, stage := range []string{"implement", "review"} {
		files, ok := instructionsByStage[stage]
		if !ok {
			t.Errorf("stage %q: no captured instructions", stage)
			continue
		}
		var agentsContent string
		for _, file := range files {
			if file.RepoPath == "AGENTS.md" {
				agentsContent = string(file.Content)
			}
		}
		if agentsContent == "" {
			t.Errorf("stage %q: AGENTS.md not in delivered instructions %v", stage, fileNames(files))
			continue
		}
		if !strings.Contains(agentsContent, originalMarker) {
			t.Errorf("stage %q: delivered AGENTS.md = %q, want to contain the original marker %q", stage, agentsContent, originalMarker)
		}
		if strings.Contains(agentsContent, "MARKER-EVIL") {
			t.Errorf("stage %q: delivered AGENTS.md carries the tampered marker — the pin did not reach the model", stage)
		}
	}
}

// fileNames returns the RepoPath of each InstructionFile for assertion messages.
func fileNames(files []agent.InstructionFile) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.RepoPath)
	}
	return out
}

// TestInstructions_NoTamperIsNoOp pins the other side: when the worktree
// matches the pin, restoreInstructions is a no-op — no restoration event, no
// manifest restoration entry. This guards against false-positive tamper alarms.
func TestInstructions_NoTamperIsNoOp(t *testing.T) {
	repo := t.TempDir()
	const marker = "CLEAN-9F2C-BASELINE"
	if err := initRepoWithAgentsCommit(repo, marker); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	taskPack := scriptPack("implement", map[string]pack.Stage{
		"implement": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":      {},
	})
	adapter := &tamperAdapter{scripts: map[string]agent.ResultJSON{
		"implement": {SchemaVersion: "1", Status: "complete", Summary: "implemented"},
	}}
	task := sqlc.Task{ID: "T-clean", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "CleanProj"}
	store := newFakeStore(task, proj)
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, Artifacts: newRecordingStore(),
	})
	manifestFake := &fakeManifestService{}
	runner.mfst = manifestFake

	if err := runner.Handle(t.Context(), job("run", task.ID, "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	for _, event := range store.events {
		if event.Type == EvInstructionsRestored {
			t.Errorf("no tamper should produce no restoration event; got %+v", event)
		}
	}
	manifestFake.mu.Lock()
	defer manifestFake.mu.Unlock()
	for _, patch := range manifestFake.addEvidence {
		if patch.Context != nil && len(patch.Context.Restorations) > 0 {
			t.Errorf("no tamper should record no restorations; got %+v", patch.Context.Restorations)
		}
	}
}

// decodeEventPayload unmarshals an event payload JSON map for assertion.
func decodeEventPayload(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	return decoded
}
