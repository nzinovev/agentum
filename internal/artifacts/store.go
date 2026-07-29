package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// SQLStore is the production Store: a sqlc.Queries-backed revision index
// composed with a content-addressed BlobStore. The blob write happens first
// (idempotent on hash) so the index never references a hash the FS does not
// have. The revision insert + prior-current demotion happen in one tx so the
// partial unique index idx_artifact_rev_current never sees two currents at
// once.
type SQLStore struct {
	db       *sql.DB
	queries  *sqlc.Queries
	blobs    *BlobStore
	redactor Redactor
	log      *slog.Logger
}

// SQLStoreDeps bundles SQLStore construction.
type SQLStoreDeps struct {
	DB       *sql.DB
	Queries  *sqlc.Queries
	Blobs    *BlobStore
	Redactor Redactor // nil → no redaction (testing only)
	Log      *slog.Logger
}

// NewSQLStore builds a SQLStore. Log defaults to slog.Default().
func NewSQLStore(deps SQLStoreDeps) *SQLStore {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	redactor := deps.Redactor
	if redactor == nil {
		redactor = NewDefaultRedactor()
	}
	return &SQLStore{
		db:       deps.DB,
		queries:  deps.Queries,
		blobs:    deps.Blobs,
		redactor: redactor,
		log:      log,
	}
}

// Put implements Store. It redacts (text content only), hashes, writes the
// blob, and inserts the revision row in one tx. If the bytes hash-equal the
// prior current revision, Put returns the existing row without writing a new
// one — an edit that produces identical content is a no-op.
func (sqlStore *SQLStore) Put(ctx context.Context, params PutParams) (Revision, error) {
	if params.Name == "" {
		return Revision{}, errors.New("artifacts: Put requires a name")
	}
	if params.Actor == "" {
		params.Actor = ActorSystem
	}

	bytesToStore, contentHash, err := sqlStore.redactAndHash(params)
	if err != nil {
		return Revision{}, err
	}

	// Idempotent edit: identical content under the same (task, name) is a
	// no-op. Checked before the transaction so the no-op path does not open a
	// tx for nothing.
	if prior, hit, priorErr := sqlStore.currentIfUnchanged(ctx, params, contentHash); priorErr != nil {
		return Revision{}, priorErr
	} else if hit {
		return fromRow(prior), nil
	}

	// Write the blob first. Idempotent on hash: a concurrent Put for the same
	// bytes is a single write.
	if err := sqlStore.blobs.Put(contentHash, bytesToStore); err != nil {
		return Revision{}, fmt.Errorf("artifacts: write blob: %w", err)
	}

	actionType, prevID := sqlStore.editBase(ctx, params)
	return sqlStore.commitRevision(ctx, params, contentHash, bytesToStore, actionType, prevID)
}

// redactAndHash returns the bytes to store (redacted when a redactor is
// configured and the kind is text) and their sha256 hash. The hash is taken
// over the redacted bytes so the index and the FS blob always agree.
func (sqlStore *SQLStore) redactAndHash(params PutParams) ([]byte, string, error) {
	bytesToStore := params.Bytes
	if sqlStore.redactor != nil {
		redacted, err := sqlStore.redactor.Redact(params.Name, params.Kind, params.Bytes)
		if err != nil {
			return nil, "", fmt.Errorf("artifacts: redact %q: %w", params.Name, err)
		}
		if redacted != nil {
			bytesToStore = redacted
		}
	}
	return bytesToStore, Hash(bytesToStore), nil
}

// currentIfUnchanged loads the current revision of (task, name) and reports
// whether it already carries contentHash (an idempotent hit). Returns the row,
// whether it hit, and any real store error — sql.ErrNoRows is not an error
// here, it just means "no prior revision".
func (sqlStore *SQLStore) currentIfUnchanged(ctx context.Context, params PutParams, contentHash string) (sqlc.ArtifactRevision, bool, error) {
	prior, err := sqlStore.queries.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: params.TaskID, TenantID: params.TenantID, Name: params.Name,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ArtifactRevision{}, false, nil
		}
		return sqlc.ArtifactRevision{}, false, fmt.Errorf("artifacts: load prior current: %w", err)
	}
	return prior, prior.ContentHash == contentHash, nil
}

// editBase decides whether this revision is a create or an edit, returning the
// action and the prev_revision_id to chain to. An absent prior current ⇒ create.
func (sqlStore *SQLStore) editBase(ctx context.Context, params PutParams) (Action, sql.NullString) {
	if _, prior := sqlStore.lookupCurrent(ctx, params.TenantID, params.TaskID, params.Name); prior != nil {
		return ActionEdit, sql.NullString{String: prior.ID, Valid: true}
	}
	return ActionCreate, sql.NullString{}
}

// commitRevision runs the demote-then-insert in one transaction so the partial
// unique index on (task_id, name) WHERE is_current never sees two currents at
// once. The prior current is demoted before the insert for that reason.
func (sqlStore *SQLStore) commitRevision(ctx context.Context, params PutParams, contentHash string, bytesToStore []byte, actionType Action, prevID sql.NullString) (Revision, error) {
	tx, txErr := sqlStore.db.BeginTx(ctx, nil)
	if txErr != nil {
		return Revision{}, fmt.Errorf("artifacts: begin tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }()

	qtx := sqlStore.queries.WithTx(tx)
	if err := qtx.DemoteArtifactRevision(ctx, sqlc.DemoteArtifactRevisionParams{
		TaskID: params.TaskID, Name: params.Name,
	}); err != nil {
		return Revision{}, fmt.Errorf("artifacts: demote prior current: %w", err)
	}

	row, insertErr := qtx.CreateArtifactRevision(ctx, sqlc.CreateArtifactRevisionParams{
		TenantID:           params.TenantID,
		UserID:             params.UserID,
		TaskID:             params.TaskID,
		Name:               params.Name,
		Kind:               params.Kind,
		ContentHash:        contentHash,
		ContentSize:        int64(len(bytesToStore)),
		ActionType:         string(actionType),
		PrevRevisionID:     prevID,
		SourceInvocationID: toNullString(params.Source),
		DeliveryStep:       toNullString(params.Coordinate.DeliveryStep),
		ExecutionUnit:      toNullString(params.Coordinate.ExecutionUnit),
		Phase:              toNullString(params.Coordinate.Phase),
		Actor:              string(params.Actor),
	})
	if insertErr != nil {
		return Revision{}, fmt.Errorf("artifacts: insert revision: %w", insertErr)
	}

	if err := tx.Commit(); err != nil {
		return Revision{}, fmt.Errorf("artifacts: commit revision tx: %w", err)
	}
	return fromRow(row), nil
}

// lookupCurrent is the internal "find prior current revision" helper. Returns
// (row, nil) when found, (zero, nil) when none exists, and (zero, err) on a
// real store error.
func (sqlStore *SQLStore) lookupCurrent(ctx context.Context, tenantID, taskID, name string) (sqlc.ArtifactRevision, *sqlc.ArtifactRevision) {
	prior, err := sqlStore.queries.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: taskID, TenantID: tenantID, Name: name,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ArtifactRevision{}, nil
		}
		return sqlc.ArtifactRevision{}, nil
	}
	return prior, &prior
}

// Get implements Store.
func (sqlStore *SQLStore) Get(ctx context.Context, tenantID, revisionID string) (Revision, error) {
	row, err := sqlStore.queries.GetArtifactRevision(ctx, sqlc.GetArtifactRevisionParams{
		ID: revisionID, TenantID: tenantID,
	})
	if err != nil {
		return Revision{}, wrapNoRows(err, ErrNoCurrentRevision)
	}
	return fromRow(row), nil
}

// GetBytes implements Store.
func (sqlStore *SQLStore) GetBytes(ctx context.Context, tenantID, revisionID string) ([]byte, error) {
	revision, err := sqlStore.Get(ctx, tenantID, revisionID)
	if err != nil {
		return nil, err
	}
	return sqlStore.blobs.Get(revision.ContentHash)
}

// CopyTo implements Store.
func (sqlStore *SQLStore) CopyTo(ctx context.Context, tenantID, revisionID string, writer io.Writer) (int64, error) {
	revision, err := sqlStore.Get(ctx, tenantID, revisionID)
	if err != nil {
		return 0, err
	}
	return sqlStore.blobs.CopyTo(revision.ContentHash, writer)
}

// Reader implements Store.
func (sqlStore *SQLStore) Reader(ctx context.Context, tenantID, revisionID string) (io.ReadCloser, error) {
	revision, err := sqlStore.Get(ctx, tenantID, revisionID)
	if err != nil {
		return nil, err
	}
	return sqlStore.blobs.Reader(revision.ContentHash)
}

// Current implements Store.
func (sqlStore *SQLStore) Current(ctx context.Context, tenantID, taskID, name string) (Revision, error) {
	row, err := sqlStore.queries.CurrentArtifactRevisionForName(ctx, sqlc.CurrentArtifactRevisionForNameParams{
		TaskID: taskID, TenantID: tenantID, Name: name,
	})
	if err != nil {
		return Revision{}, wrapNoRows(err, ErrNoCurrentRevision)
	}
	return fromRow(row), nil
}

// ListForTask implements Store.
func (sqlStore *SQLStore) ListForTask(ctx context.Context, tenantID, taskID string) ([]Revision, error) {
	rows, err := sqlStore.queries.ListArtifactRevisionsForTask(ctx, sqlc.ListArtifactRevisionsForTaskParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out, nil
}

// ListCurrent implements Store.
func (sqlStore *SQLStore) ListCurrent(ctx context.Context, tenantID, taskID string) ([]Revision, error) {
	rows, err := sqlStore.queries.ListCurrentArtifactRevisionsForTask(ctx, sqlc.ListCurrentArtifactRevisionsForTaskParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out, nil
}

// ListForInvocation implements Store.
func (sqlStore *SQLStore) ListForInvocation(ctx context.Context, tenantID, invocationID string) ([]Revision, error) {
	rows, err := sqlStore.queries.ListArtifactRevisionsForInvocation(ctx, sqlc.ListArtifactRevisionsForInvocationParams{
		SourceInvocationID: sql.NullString{String: invocationID, Valid: invocationID != ""},
		TenantID:           tenantID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Revision, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out, nil
}

// toNullString adapts a Go string to sql.NullString. Empty → NULL.
func toNullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}
