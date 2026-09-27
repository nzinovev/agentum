package runner

import (
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/worktree"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationDescriptionSyncStaysOutsideCheckpoint(test *testing.T) {
	database := dbtest.Store(test)
	fixture := newCaptureFixture(test)
	const tenant = "00000000-0000-0000-0000-000000000001"
	var runID string
	err := database.Store.DB.QueryRowContext(test.Context(), `WITH project AS (
 INSERT INTO projects (tenant_id,user_id,repo_identity,repo_root_commits,repo_path,name,related_projects)
 VALUES ($1,$1,'sync-fixture','{}','/tmp/sync-fixture','fixture','{}') RETURNING id)
 INSERT INTO runs (tenant_id,user_id,project_id,pipeline_pack,title,description,overrides,base_ref,state)
 SELECT $1,$1,project.id,'backend-development','fixture','fixture','{}','main','awaiting_final_review' FROM project RETURNING id`, tenant).Scan(&runID)
	if err != nil {
		test.Fatal(err)
	}
	store := artifacts.NewSQLStore(artifacts.SQLStoreDeps{DB: database.Store.DB, Queries: database.Queries, Blobs: artifacts.NewBlobStore(test.TempDir())})
	_, err = store.Put(test.Context(), artifacts.PutParams{TenantID: tenant, UserID: tenant, RunID: runID, Name: "publication/pr-description.md", Kind: "pr_description", Bytes: []byte("Stored PR text"), Actor: artifacts.ActorSystem})
	if err != nil {
		test.Fatal(err)
	}
	fixture.runner.art, fixture.runner.syncer = store, artifacts.NewSyncer(store)
	fixture.run.record.ID, fixture.run.record.TenantID, fixture.run.record.UserID = runID, tenant, tenant
	runGitInTest(test, fixture.worktreeDir, "init")
	if err := os.WriteFile(filepath.Join(fixture.worktreeDir, ".git", "info", "exclude"), []byte(".agentum/\n"), 0600); err != nil {
		test.Fatal(err)
	}
	fixture.runner.syncRevisionsIntoWorktree(test.Context(), fixture.run, "review")
	stored, err := os.ReadFile(filepath.Join(worktree.ArtifactDir(fixture.worktreeDir, runID, "publication"), "pr-description.md"))
	if err != nil || string(stored) != "Stored PR text" {
		test.Fatalf("redirected text=%q err=%v", stored, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.worktreeDir, "publication", "pr-description.md")); !os.IsNotExist(err) {
		test.Fatalf("description entered source tree: %v", err)
	}
	runGitInTest(test, fixture.worktreeDir, "add", "-A")
	if staged := mustGitRaw(test, fixture.worktreeDir, "diff", "--cached", "--name-only"); staged != "" {
		test.Fatalf("description staged for checkpoint: %s", staged)
	}
}
