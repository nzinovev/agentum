package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// revisionIndex is the minimal subset of *sqlc.Queries the Syncer needs.
// Extracted as an interface so tests can substitute a fake; the production
// SQLStore satisfies it via its embedded *sqlc.Queries.
type revisionIndex interface {
	GetArtifactRevision(ctx context.Context, arg sqlc.GetArtifactRevisionParams) (sqlc.ArtifactRevision, error)
	CurrentArtifactRevisionForName(ctx context.Context, arg sqlc.CurrentArtifactRevisionForNameParams) (sqlc.ArtifactRevision, error)
}

// Syncer materializes current revisions back into a working directory. It is
// used on resume / advance: when an invocation starts, the runner asks the
// Syncer to drop the current revisions of the artifacts a stage consumes into
// the worktree at their expected paths. This is what "the chosen revision
// syncs into the agent's working environment" means in practice.
//
// The Syncer is read-only against the revisions index and write-only against
// the worktree FS — it never touches the index, never creates revisions.
type Syncer struct {
	index revisionIndex
	blobs *BlobStore
}

// NewSyncer returns a Syncer backed by the given SQLStore. The Syncer reaches
// into the store's queries + blobs; this is the production wiring.
func NewSyncer(store *SQLStore) *Syncer {
	return &Syncer{index: store.queries, blobs: store.blobs}
}

// newSyncerForTest is the test-only constructor that wires an arbitrary
// revisionIndex + BlobStore. Lives in the production file (not _test.go)
// because the test that needs it lives in the same package; the unexported
// name keeps it out of the public API.
func newSyncerForTest(index revisionIndex, blobs *BlobStore) *Syncer {
	return &Syncer{index: index, blobs: blobs}
}

// SyncTarget is one artifact to materialize. Path is absolute; RevisionID
// pins the specific revision (used when a later stage wants a non-current
// revision, e.g. the original spec even after the spec was edited).
type SyncTarget struct {
	// Path is the absolute path inside the worktree where the bytes go.
	Path string
	// Name is the revisions-index name (the (task, name) key). Used when
	// RevisionID is empty — Sync then reads the current revision for that name.
	Name string
	// RevisionID optionally pins a specific revision. Empty → current.
	RevisionID string
}

// SyncResult records one materialization. Empty RevisionID + Hash when the
// target name had no revision yet (the file is left untouched in that case).
type SyncResult struct {
	Target     SyncTarget
	RevisionID string
	Hash       string
	Skipped    bool // true when no revision was found for the name
}

// Sync writes the bytes of each target's resolved revision into the worktree.
// It overwrites the file at Path. Skips (with Skipped=true) when the name has
// no current revision and RevisionID is empty. Returns one SyncResult per
// target, in order.
func (syncer *Syncer) Sync(
	ctx context.Context,
	tenantID, taskID string,
	rootDir string,
	targets []SyncTarget,
) ([]SyncResult, error) {
	results := make([]SyncResult, 0, len(targets))
	for _, target := range targets {
		result := SyncResult{Target: target}
		revision, found, err := syncer.resolveRevision(ctx, tenantID, taskID, target)
		if err != nil {
			return results, err
		}
		if !found {
			result.Skipped = true
			results = append(results, result)
			continue
		}
		bytes, err := syncer.blobs.Get(revision.ContentHash)
		if err != nil {
			return results, fmt.Errorf("artifacts: sync %q: read blob: %w", target.Name, err)
		}
		absPath := target.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(rootDir, absPath)
		}
		// Sanitize: the resolved path must stay under rootDir. An absolute
		// target.Path outside rootDir is rejected; a relative path with ../
		// escapes is rejected after Join.
		if err := ensureInside(rootDir, absPath); err != nil {
			return results, fmt.Errorf("artifacts: sync %q: %w", target.Name, err)
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return results, fmt.Errorf("artifacts: sync %q: mkdir: %w", target.Name, err)
		}
		if err := os.WriteFile(absPath, bytes, 0o644); err != nil {
			return results, fmt.Errorf("artifacts: sync %q: write: %w", target.Name, err)
		}
		result.RevisionID = revision.ID
		result.Hash = revision.ContentHash
		results = append(results, result)
	}
	return results, nil
}

func (syncer *Syncer) resolveRevision(
	ctx context.Context,
	tenantID, taskID string,
	target SyncTarget,
) (sqlc.ArtifactRevision, bool, error) {
	if target.RevisionID != "" {
		row, err := syncer.index.GetArtifactRevision(ctx, sqlc.GetArtifactRevisionParams{
			ID: target.RevisionID, TenantID: tenantID,
		})
		if err != nil {
			return sqlc.ArtifactRevision{}, false, err
		}
		return row, true, nil
	}
	row, err := syncer.index.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: taskID, TenantID: tenantID, Name: target.Name,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ArtifactRevision{}, false, nil
		}
		return sqlc.ArtifactRevision{}, false, err
	}
	return row, true, nil
}

// ensureInside verifies absPath is contained by rootDir. Both must be absolute.
// Returns an error when the path escapes via ../ or by being absolute outside
// the root.
func ensureInside(rootDir, absPath string) error {
	rel, err := filepath.Rel(rootDir, absPath)
	if err != nil {
		return fmt.Errorf("resolve %q under %q: %w", absPath, rootDir, err)
	}
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path %q escapes root %q", absPath, rootDir)
	}
	return nil
}
