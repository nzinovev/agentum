package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// fakeRevisionIndex is a hand-rolled revisionIndex fake for the Syncer. It
// returns programmed rows for the two methods the Syncer calls.
type fakeRevisionIndex struct {
	mu         sync.Mutex
	revisions  map[string]sqlc.ArtifactRevision // by id
	byTaskName map[string]sqlc.ArtifactRevision // by taskID|name (current)
}

func newFakeRevisionIndex() *fakeRevisionIndex {
	return &fakeRevisionIndex{
		revisions:  make(map[string]sqlc.ArtifactRevision),
		byTaskName: make(map[string]sqlc.ArtifactRevision),
	}
}

func keyForCurrent(taskID, name string) string { return taskID + "|" + name }

// seed inserts a revision that CurrentArtifactRevisionForName and
// GetArtifactRevision will return.
func (fake *fakeRevisionIndex) seed(revision sqlc.ArtifactRevision) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.revisions[revision.ID] = revision
	fake.byTaskName[keyForCurrent(revision.TaskID, revision.Name)] = revision
}

func (fake *fakeRevisionIndex) GetArtifactRevision(_ context.Context, arg sqlc.GetArtifactRevisionParams) (sqlc.ArtifactRevision, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	row, ok := fake.revisions[arg.ID]
	if !ok {
		return sqlc.ArtifactRevision{}, sql.ErrNoRows
	}
	return row, nil
}

func (fake *fakeRevisionIndex) CurrentArtifactRevisionForName(_ context.Context, arg sqlc.CurrentArtifactRevisionForNameParams) (sqlc.ArtifactRevision, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	row, ok := fake.byTaskName[keyForCurrent(arg.TaskID, arg.Name)]
	if !ok {
		return sqlc.ArtifactRevision{}, sql.ErrNoRows
	}
	return row, nil
}

func TestSyncer_SyncWritesFile(t *testing.T) {
	t.Parallel()
	blobRoot := t.TempDir()
	blobs := NewBlobStore(blobRoot)
	payload := []byte("synced-by-syncer")
	hash := Hash(payload)
	if err := blobs.Put(hash, payload); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	queries := newFakeRevisionIndex()
	revisionID := "rev-1"
	taskID := "T1"
	queries.seed(sqlc.ArtifactRevision{
		ID: revisionID, TenantID: "tn", TaskID: taskID, Name: "specs/auth.md",
		Kind: "spec", ContentHash: hash, IsCurrent: true,
	})

	syncer := newSyncerForTest(queries, blobs)

	root := t.TempDir()
	target := filepath.Join(root, "specs/auth.md")
	results, err := syncer.Sync(context.Background(), "tn", taskID, root, []SyncTarget{
		{Path: target, Name: "specs/auth.md"},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Skipped {
		t.Errorf("result skipped unexpectedly")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	if string(got) != "synced-by-syncer" {
		t.Errorf("synced bytes = %q", got)
	}
}

func TestSyncer_SyncSkipsMissingRevision(t *testing.T) {
	t.Parallel()
	blobs := NewBlobStore(t.TempDir())
	queries := newFakeRevisionIndex()
	syncer := newSyncerForTest(queries, blobs)

	root := t.TempDir()
	target := filepath.Join(root, "specs/missing.md")
	results, err := syncer.Sync(context.Background(), "tn", "T1", root, []SyncTarget{
		{Path: target, Name: "specs/missing.md"},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !results[0].Skipped {
		t.Errorf("expected skipped=true for missing revision")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("expected no file written for skipped revision")
	}
}

func TestSyncer_RejectsEscapePath(t *testing.T) {
	t.Parallel()
	blobs := NewBlobStore(t.TempDir())
	hash := Hash([]byte("escape-attempt"))
	_ = blobs.Put(hash, []byte("escape-attempt"))
	queries := newFakeRevisionIndex()
	queries.seed(sqlc.ArtifactRevision{
		ID: "rev-1", TenantID: "tn", TaskID: "T1",
		Name: "specs/auth.md", ContentHash: hash, IsCurrent: true,
	})
	syncer := newSyncerForTest(queries, blobs)

	root := t.TempDir()
	escapePath := filepath.Join(root, "..", "..", "escape.md")
	if _, err := syncer.Sync(context.Background(), "tn", "T1", root, []SyncTarget{
		{Path: escapePath, Name: "specs/auth.md"},
	}); err == nil {
		t.Errorf("expected escape-path error")
	}
}

func TestSyncer_PinnedRevisionID(t *testing.T) {
	t.Parallel()
	blobs := NewBlobStore(t.TempDir())
	hash := Hash([]byte("pinned-revision"))
	_ = blobs.Put(hash, []byte("pinned-revision"))
	queries := newFakeRevisionIndex()
	queries.seed(sqlc.ArtifactRevision{
		ID: "rev-pinned", TenantID: "tn", TaskID: "T1",
		Name: "specs/auth.md", ContentHash: hash, IsCurrent: false,
	})
	syncer := newSyncerForTest(queries, blobs)

	root := t.TempDir()
	target := filepath.Join(root, "specs/auth.md")
	results, err := syncer.Sync(context.Background(), "tn", "T1", root, []SyncTarget{
		{Path: target, Name: "specs/auth.md", RevisionID: "rev-pinned"},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if results[0].RevisionID != "rev-pinned" {
		t.Errorf("RevisionID = %q", results[0].RevisionID)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "pinned-revision" {
		t.Errorf("synced bytes = %q", got)
	}
}

func TestCoordinate_Empty(t *testing.T) {
	t.Parallel()
	empty := Coordinate{}
	withStep := Coordinate{DeliveryStep: "s"}
	withPhase := Coordinate{Phase: "p"}
	if !empty.Empty() {
		t.Error("zero Coordinate not Empty")
	}
	if withStep.Empty() {
		t.Error("Coordinate with DeliveryStep not Empty")
	}
	if withPhase.Empty() {
		t.Error("Coordinate with Phase not Empty")
	}
}
