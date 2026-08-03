package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
	blobs ObjectStore
}

// NewSyncer returns a Syncer backed by the given SQLStore. The Syncer reaches
// into the store's queries + blobs; this is the production wiring.
func NewSyncer(store *SQLStore) *Syncer {
	return &Syncer{index: store.queries, blobs: store.blobs}
}

// newSyncerForTest is the test-only constructor that wires an arbitrary
// revisionIndex + ObjectStore. Lives in the production file (not _test.go)
// because the test that needs it lives in the same package; the unexported
// name keeps it out of the public API.
func newSyncerForTest(index revisionIndex, blobs ObjectStore) *Syncer {
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
//
// Every write goes through a Container rooted at rootDir. The write side needs
// the same containment as the read side: a revision name is durable state that
// an earlier stage influenced, and the worktree it is written back into is a
// tree the agent had write access to — so an unchecked write could follow a
// planted link out of the worktree and overwrite a host file.
func (syncer *Syncer) Sync(
	ctx context.Context,
	tenantID, taskID string,
	rootDir string,
	targets []SyncTarget,
) ([]SyncResult, error) {
	results := make([]SyncResult, 0, len(targets))
	container, err := OpenContainer(rootDir)
	if err != nil {
		return results, err
	}
	defer func() { _ = container.Close() }()

	for _, target := range targets {
		result := SyncResult{Target: target}
		revision, found, resolveErr := syncer.resolveRevision(ctx, tenantID, taskID, target)
		if resolveErr != nil {
			return results, resolveErr
		}
		if !found {
			result.Skipped = true
			results = append(results, result)
			continue
		}
		// Resolve the destination before reading the blob: a target that fails
		// containment should cost nothing.
		destination, pathErr := container.Resolve(target.Path)
		if pathErr != nil {
			return results, fmt.Errorf("artifacts: sync %q: %w", target.Name, pathErr)
		}
		bytes, blobErr := syncer.blobs.Get(revision.ContentHash)
		if blobErr != nil {
			return results, fmt.Errorf("artifacts: sync %q: read blob: %w", target.Name, blobErr)
		}
		if writeErr := container.WriteFile(destination.Name, bytes, 0o644); writeErr != nil {
			return results, fmt.Errorf("artifacts: sync %q: %w", target.Name, writeErr)
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
