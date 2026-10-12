package runner

import (
	"context"
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
	"github.com/nzinovev/agentum/internal/worktree"
)

// referenceAdapter is the fake adapter for the reference-file channel. On each
// invocation it records the routing block and the files it finds in the
// worktree's reference copy. During the implement stage it edits its copy,
// adds a file to it, and edits the source directory in the checkout, standing
// in for an agent and an operator who both change the files mid-run.
type referenceAdapter struct {
	stubExecution
	mu            sync.Mutex
	scripts       map[string]agent.ResultJSON
	checkout      string
	runID         string
	worktreeRoot  string
	routingBlocks map[string]string
	copyByStage   map[string]map[string]string
}

func (adapter *referenceAdapter) Supported() []caps.Category {
	return []caps.Category{
		caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
		caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret",
	}
}

func (adapter *referenceAdapter) Invoke(_ context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	stage := stageOf(inv)
	copyRoot := worktree.ReferenceDir(inv.Workdir, adapter.runID)

	adapter.mu.Lock()
	adapter.worktreeRoot = inv.Workdir
	if adapter.routingBlocks == nil {
		adapter.routingBlocks = map[string]string{}
		adapter.copyByStage = map[string]map[string]string{}
	}
	adapter.routingBlocks[stage] = inv.RoutingBlock
	adapter.copyByStage[stage] = readTree(copyRoot)
	adapter.mu.Unlock()

	if stage == "implement" {
		_ = os.WriteFile(filepath.Join(copyRoot, "design", "README.md"), []byte("EDITED-BY-IMPLEMENTER"), 0o644)
		_ = os.WriteFile(filepath.Join(copyRoot, "design", "added.md"), []byte("added by implementer"), 0o644)
		_ = os.WriteFile(filepath.Join(adapter.checkout, "design", "README.md"), []byte("EDITED-IN-CHECKOUT"), 0o644)
	}

	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stage]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: fmt.Errorf("no script for stage %q", stage)}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{SessionID: "sess-" + stage, ResultJSON: scripted}}
	}()
	return eventCh, nil
}

// readTree returns every regular file under root as forward-slash relative
// path → content. An absent root yields an empty map.
func readTree(root string) map[string]string {
	files := map[string]string{}
	_ = filepath.WalkDir(root, func(walked string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		content, readErr := os.ReadFile(walked)
		if readErr != nil {
			return nil
		}
		relativePath, _ := filepath.Rel(root, walked)
		files[filepath.ToSlash(relativePath)] = string(content)
		return nil
	})
	return files
}

// initRepoWithReferences creates a repository whose only commit holds an
// .agentum.yaml declaring `design` as a reference path and one tracked file
// under it. It then adds the files the project keeps out of git, and an
// uncommitted edit of the tracked file.
func initRepoWithReferences(t *testing.T, dir string) {
	t.Helper()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v (%s)", args[0], err, out)
		}
	}
	write := func(relativePath, content string) {
		t.Helper()
		absolutePath := filepath.Join(dir, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", relativePath, err)
		}
		if err := os.WriteFile(absolutePath, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", relativePath, err)
		}
	}
	runGit("init", "--quiet")
	runGit("config", "user.email", "t@t")
	runGit("config", "user.name", "t")
	write(".agentum.yaml", "api: agentum/v1\nreferences:\n  - design\n  - notes\n")
	write(".gitignore", "design/README.md\ndesign/system/\ndesign/.env\n")
	write("design/tracked.md", "committed version")
	runGit("add", ".agentum.yaml", ".gitignore", "design/tracked.md")
	runGit("commit", "--quiet", "-m", "init")

	write("design/README.md", "ORIGINAL-README")
	write("design/system/tokens.css", "ORIGINAL-TOKENS")
	write("design/.env", "API_KEY=not-for-the-agent")
	write("design/tracked.md", "uncommitted edit")
}

// TestReferences_SnapshotReachesEveryStage: the files under a declared
// reference path that are not in the repository are present in the worktree's
// reference copy when each stage is invoked, with the bytes the checkout held
// when the run started. An edit of the copy by one stage and an edit of the
// source directory during the run do not reach the next stage. Tracked files
// and credential-named files are left out, the copy stays out of
// `git status`, and the routing block and the manifest both describe it.
func TestReferences_SnapshotReachesEveryStage(t *testing.T) {
	repo := t.TempDir()
	initRepoWithReferences(t, repo)

	runPack := scriptPack("implement", map[string]pack.Stage{
		"implement": {Gate: pack.GateAuto, Prompt: "impl.md", Transitions: []pack.Transition{{To: "review"}}},
		"review":    {Gate: pack.GateAuto, Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
		"done":      {},
	})
	runPack.Capabilities = []string{"fs.read", "fs.write", "artifact.write", "exec.bash", "git.read", "git.write"}

	record := sqlc.Run{ID: "T-refs", TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "RefProj"}
	store := newFakeStore(record, proj)
	adapter := &referenceAdapter{
		checkout: repo, runID: record.ID,
		scripts: map[string]agent.ResultJSON{
			"implement": {SchemaVersion: "1", Status: "complete", Summary: "implemented"},
			"review":    {SchemaVersion: "1", Status: "complete", Summary: "reviewed"},
		},
	}
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Artifacts: newRecordingStore(),
	})
	manifestFake := &fakeManifestService{}
	runner.mfst = manifestFake

	if err := runner.HandleRun(t.Context(), job("run", record.ID, "tn", "us")); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	adapter.mu.Lock()
	defer adapter.mu.Unlock()

	wantCopy := map[string]string{
		"design/README.md":         "ORIGINAL-README",
		"design/system/tokens.css": "ORIGINAL-TOKENS",
	}
	for _, stage := range []string{"implement", "review"} {
		got, invoked := adapter.copyByStage[stage]
		if !invoked {
			t.Fatalf("stage %q was not invoked", stage)
		}
		if len(got) != len(wantCopy) {
			t.Errorf("stage %q: reference copy holds %v, want exactly %v", stage, got, wantCopy)
		}
		for copiedPath, wantContent := range wantCopy {
			if got[copiedPath] != wantContent {
				t.Errorf("stage %q: %s = %q, want %q", stage, copiedPath, got[copiedPath], wantContent)
			}
		}
	}

	copyLocation := filepath.Join(worktree.ReferenceDir(adapter.worktreeRoot, record.ID), "design")
	block := adapter.routingBlocks["review"]
	for _, fragment := range []string{
		"## Reference files (not in the repository)",
		"- **design**: " + copyLocation + " (2 file(s), 30 bytes); 2 path(s) were not copied",
		"  - README.md",
		"  - system/tokens.css",
		"- **notes**: declared by the project but absent from the local checkout",
	} {
		if !strings.Contains(block, fragment) {
			t.Errorf("review routing block missing %q", fragment)
		}
	}
	if strings.Contains(block, ".env") || strings.Contains(block, "tracked.md") {
		t.Error("routing block lists a file the snapshot left out")
	}

	statusCmd := exec.Command("git", "-C", adapter.worktreeRoot, "status", "--porcelain")
	statusCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if status, statusErr := statusCmd.CombinedOutput(); statusErr != nil || strings.Contains(string(status), "refs") {
		t.Errorf("reference copy shows in git status: %q, %v", status, statusErr)
	}

	var pinnedEvents, restoredEvents int
	for _, event := range store.events {
		switch event.Type {
		case EvReferencesPinned:
			pinnedEvents++
		case EvReferencesRestored:
			restoredEvents++
			payload := decodeEventPayload(t, event.Payload)
			if payload["stage"] != "review" || payload["changes_total"] != float64(2) {
				t.Errorf("restoration event = %v, want stage review with 2 changes", payload)
			}
		}
	}
	if pinnedEvents != 1 || restoredEvents != 1 {
		t.Errorf("events: %d pinned and %d restored, want 1 and 1", pinnedEvents, restoredEvents)
	}

	manifestFake.mu.Lock()
	defer manifestFake.mu.Unlock()
	var recorded bool
	for _, patch := range manifestFake.addEvidence {
		if patch.Context == nil || len(patch.Context.References) == 0 {
			continue
		}
		recorded = true
		design, notes := patch.Context.References[0], patch.Context.References[1]
		if design.Path != "design" || design.Files != 2 || design.TreeHash == "" || design.SkippedTotal != 2 {
			t.Errorf("manifest design ref = %+v", design)
		}
		reasons := map[string]string{}
		for _, skipped := range design.Skipped {
			reasons[skipped.Path] = skipped.Reason
		}
		if reasons["design/tracked.md"] != "tracked" || reasons["design/.env"] != "secret_name" {
			t.Errorf("manifest skip reasons = %v, want tracked.md tracked and .env secret_name", reasons)
		}
		if notes.Path != "notes" || !notes.Missing {
			t.Errorf("manifest notes ref = %+v, want missing", notes)
		}
	}
	if !recorded {
		t.Error("manifest context section recorded no references")
	}
}

// TestReferences_StoreRemovedWithTheWorktree: discarding a run's worktree also
// removes the run's pristine reference snapshot from the checkout.
func TestReferences_StoreRemovedWithTheWorktree(t *testing.T) {
	repo := t.TempDir()
	initRepoWithReferences(t, repo)
	manager := worktree.New()
	const runID = "T-refs-discard"

	runWorktree, err := manager.Create(context.Background(), repo, runID, "")
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	head, err := manager.HeadCommit(context.Background(), runWorktree.Root)
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	record := sqlc.Run{ID: runID, TenantID: "tn", UserID: "us", ProjectID: "P1",
		State: "cancelled", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "RefProj"}
	store := newFakeStore(record, proj)
	runner := New(Deps{Store: store, Packs: &staticSource{pk: scriptPack("implement", map[string]pack.Stage{"implement": {}})}, Adapter: &referenceAdapter{}})

	run := stageRun{record: record, project: proj, worktree: runWorktree}
	runner.pinReferences(context.Background(), &run, head, []string{"design"})
	storeDir := worktree.ReferenceStoreDir(repo, runID)
	if _, statErr := os.Stat(filepath.Join(storeDir, "index.json")); statErr != nil {
		t.Fatalf("pristine store was not created: %v", statErr)
	}

	discardJob := sqlc.Job{ID: 7, TenantID: "tn", UserID: "us", RunID: runID, Kind: "discard_worktree",
		Payload: []byte(`{"expected_head":"` + head + `","discard_uncommitted":true}`)}
	if err := runner.HandleDiscardWorktree(context.Background(), discardJob); err != nil {
		t.Fatalf("discard worktree: %v", err)
	}
	if _, statErr := os.Stat(storeDir); !os.IsNotExist(statErr) {
		t.Errorf("pristine store survived the worktree removal: %v", statErr)
	}
}
