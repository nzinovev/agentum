package runner

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type shortPlanAdapter struct {
	stubExecution
	profiles []caps.Profile
	resumes  []string
}

func (adapter *shortPlanAdapter) Supported() []caps.Category {
	return []caps.Category{caps.CatFsRead, caps.CatFsWrite, caps.CatGitRead, caps.CatGitWrite, caps.CatExecBash, caps.CatArtifactWrite}
}

func (adapter *shortPlanAdapter) Invoke(_ context.Context, invocation agent.Invocation) (<-chan agent.Event, error) {
	adapter.profiles = append(adapter.profiles, invocation.Profile)
	adapter.resumes = append(adapter.resumes, invocation.ResumeSession)
	if len(adapter.profiles) == 1 {
		if err := os.WriteFile(filepath.Join(invocation.ArtifactDir, "plan.md"), []byte("short plan"), 0o644); err != nil {
			return nil, err
		}
	}
	stream := make(chan agent.Event, 1)
	stream <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "short-plan-session", ResultJSON: agent.ResultJSON{SchemaVersion: "1", Status: agent.StatusComplete}}}
	close(stream)
	return stream, nil
}

func TestWithinStageApprovalResumesImplementWithSourceWrite(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatal(err)
	}
	runPack := &pack.Pack{
		API: pack.APIVersion, Pack: pack.Meta{Name: "short", Version: "0.1.0"},
		Capabilities: []string{"fs.read", "fs.write", "git.read", "git.write", "exec.bash"},
		Tiers:        pack.Tiers{Default: "fast"}, Entry: "implement",
		Approvals: []pack.Approval{{Name: "short_plan", Stage: "implement", Artifact: "plan.md", Unlocks: "source_write", WithinStage: true}},
		Stages: map[string]pack.Stage{
			"implement": {Gate: pack.GateHumanApproval, Role: "implementer", Prompt: "implement.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
		PromptText: map[string]string{"implement": "write a short plan, then edit after approval"},
	}
	record := sqlc.Run{ID: "T-short", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "short", RouteSource: sql.NullString{String: "request", Valid: true}}
	store := newFakeStore(record, sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"})
	adapter := &shortPlanAdapter{}
	artifactStore := newRecordingStore()
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Artifacts: artifactStore})
	if err := runner.HandleRun(t.Context(), job("run", record.ID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	if store.taskState() != "paused_gate" || len(adapter.profiles) != 1 || adapter.profiles[0].Has(caps.CatFsWrite) || adapter.profiles[0].Has(caps.CatExecBash) {
		t.Fatalf("short plan ran with wrong state/profile: state=%s profiles=%+v", store.taskState(), adapter.profiles)
	}
	store.approvals = map[string]sqlc.RunApproval{
		approvalKey(record.ID, "short_plan"): {RunID: record.ID, TenantID: "tn", Name: "short_plan", Decision: "approved"},
	}
	if err := runner.HandleAdvance(t.Context(), job("advance", record.ID, "tn", "us")); err != nil {
		t.Fatal(err)
	}
	if len(adapter.profiles) != 2 || !adapter.profiles[1].Has(caps.CatFsWrite) || adapter.resumes[1] != "short-plan-session" {
		t.Errorf("edits did not resume the same stage with approval: profiles=%+v resumes=%v", adapter.profiles, adapter.resumes)
	}
	if store.taskState() != "awaiting_final_review" {
		t.Errorf("state=%s, want final review", store.taskState())
	}
	if len(store.invocations) != 2 || store.invocations[0].Stage != "implement" || store.invocations[1].Stage != "implement" {
		t.Errorf("invocations=%+v, want two implement attempts", store.invocations)
	}
	planCaptures := 0
	for _, name := range artifactStore.names() {
		if name == "implement/plan.md" {
			planCaptures++
		}
	}
	if planCaptures != 1 {
		t.Errorf("plan captured %d times, want one approved revision", planCaptures)
	}
}
