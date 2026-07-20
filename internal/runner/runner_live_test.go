//go:build integration

// The integration test in this file drives a real opencode subprocess through
// the full runner loop (real adapter + real worktree + real result.json parse +
// real evaluator + real FSM). It is excluded from CI (no opencode binary or
// credentials). Run locally:
//
//	go test -tags integration ./internal/runner/ -run TestRunnerLive -v -timeout 5m
//
// It proves the F.6 end-to-end path: a run drives the minimal pack via the
// opencode adapter to a stop point, then advance moves toward done.
package runner

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestRunnerLive_EndToEnd runs a real opencode invocation through the runner
// using the real minimal pack: the spec stage (human_approval) is invoked by the
// real adapter, which writes a real result.json; the evaluator maps it to a pause
// at paused_gate. This is the F.6 proof that the loop works with a live agent,
// not just fakes.
func TestRunnerLive_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}

	// Load the real minimal pack (faithful to "runs the minimal pack"). Resolved
	// relative to the package dir; skipped if not found (e.g. run elsewhere).
	taskPack, err := pack.Load("../../packs/minimal")
	if err != nil {
		t.Skipf("minimal pack not loadable from %s: %v", "../../packs/minimal", err)
	}

	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}

	task := sqlc.Task{ID: "LIVE1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "minimal@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "LiveProj"}
	store := newFakeStore(task, proj)

	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack},
		Adapter: agent.NewOpencodeAdapter("opencode"), AgentName: "opencode",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// run: spec stage invoked by the real adapter → result.json → human_approval
	// gate pauses the task.
	if err := runner.Handle(ctx, job("run", "LIVE1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if got := store.taskState(); got != "paused_gate" {
		t.Fatalf("after live run, state = %q, want paused_gate", got)
	}
	store.mu.Lock()
	nInv := len(store.invocations)
	sess := ""
	if nInv > 0 {
		sess = store.invocations[0].SessionID.String
	}
	store.mu.Unlock()
	if nInv != 1 {
		t.Fatalf("expected 1 invocation after run, got %d", nInv)
	}
	if sess == "" {
		t.Fatal("no session_id captured — the real adapter did not produce one")
	}
	t.Logf("live spec OK: session=%s, paused at gate", sess)
}
