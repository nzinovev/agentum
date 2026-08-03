package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// SQLStore is the production Store: a sqlc.Queries-backed revision index
// composed with a content-addressed ObjectStore. The blob write happens first
// (idempotent on hash) so the index never references a hash the FS does not
// have. The read of the prior current revision, the demotion of that exact
// revision, and the insert of the new one all happen inside one transaction, so
// the (task, name) chain cannot fork under concurrent writers.
type SQLStore struct {
	db      *sql.DB
	queries *sqlc.Queries
	blobs   ObjectStore
	scanner Scanner
	log     *slog.Logger
}

// SQLStoreDeps bundles SQLStore construction.
type SQLStoreDeps struct {
	DB      *sql.DB
	Queries *sqlc.Queries
	Blobs   ObjectStore
	// Scanner gates what enters the store. nil → the default scanner under
	// ScanPolicy (or PolicyRedact when that is empty too). Use NoScan to
	// disable scanning entirely.
	Scanner Scanner
	// ScanPolicy configures the default scanner. Ignored when Scanner is set.
	ScanPolicy ScanPolicy
	Log        *slog.Logger
}

// NewSQLStore builds a SQLStore. Log defaults to slog.Default().
func NewSQLStore(deps SQLStoreDeps) *SQLStore {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	scanner := deps.Scanner
	if scanner == nil {
		scanner = NewDefaultScanner(deps.ScanPolicy)
	}
	return &SQLStore{
		db:      deps.DB,
		queries: deps.Queries,
		blobs:   deps.Blobs,
		scanner: scanner,
		log:     log,
	}
}

// Put implements Store. It scans (name and content), hashes, writes the blob,
// and commits the revision row in one transaction. If the bytes hash-equal the
// current revision, Put returns the existing row without writing a new one — an
// edit that produces identical content is a no-op.
//
// Returns ErrSecretDetected when the scanner refuses the write, and
// ErrRevisionConflict when the chain moved between this call's read and its
// write (or when PutParams.ExpectedCurrentRevision no longer holds).
func (sqlStore *SQLStore) Put(ctx context.Context, params PutParams) (Revision, error) {
	if params.Name == "" {
		return Revision{}, errors.New("artifacts: Put requires a name")
	}
	if params.Actor == "" {
		params.Actor = ActorSystem
	}

	bytesToStore, contentHash, err := sqlStore.scanAndHash(params)
	if err != nil {
		return Revision{}, err
	}

	// Write the blob before the transaction. It is content-addressed and
	// idempotent, so an aborted transaction leaves at most an unreferenced blob
	// — cheap and harmless, where the reverse order would let the index
	// reference bytes that are not on disk yet.
	if err := sqlStore.blobs.Put(contentHash, bytesToStore); err != nil {
		return Revision{}, fmt.Errorf("artifacts: write blob: %w", err)
	}

	return sqlStore.commitRevision(ctx, params, contentHash, bytesToStore)
}

// scanAndHash returns the bytes to store and their sha256 hash. The hash is
// taken over the post-scan bytes so the index and the FS blob always agree.
// A credential-shaped name is refused before the content is even read.
func (sqlStore *SQLStore) scanAndHash(params PutParams) ([]byte, string, error) {
	if sqlStore.scanner == nil {
		return params.Bytes, Hash(params.Bytes), nil
	}
	if err := sqlStore.scanner.ScanName(params.Name); err != nil {
		return nil, "", err
	}
	result, err := sqlStore.scanner.Scan(params.Name, params.Kind, params.Bytes)
	if err != nil {
		return nil, "", err
	}
	if !result.Clean() {
		// An artifact that was altered — or that carries a credential we could
		// not alter, because it is binary — is something an operator has to be
		// able to see after the fact.
		sqlStore.log.Warn("artifact scan findings",
			"task", params.TaskID, "name", params.Name,
			"findings", strings.Join(result.Findings, ","), "rewritten", result.Rewritten)
	}
	return result.Bytes, Hash(result.Bytes), nil
}

// revisionPlan is what the in-transaction read decided: whether the write is
// still needed at all, and what it chains onto.
type revisionPlan struct {
	// noop is set when the current revision already carries these bytes.
	noop bool
	// current is the revision to demote and chain from. Zero when the artifact
	// has no current revision yet (a create).
	current sqlc.ArtifactRevision
	// hasCurrent distinguishes "no current revision" from a zero row.
	hasCurrent bool
}

// action returns the action_type this plan implies.
func (plan revisionPlan) action() Action {
	if plan.hasCurrent {
		return ActionEdit
	}
	return ActionCreate
}

// prev returns the prev_revision_id this plan chains to.
func (plan revisionPlan) prev() sql.NullString {
	if plan.hasCurrent {
		return sql.NullString{String: plan.current.ID, Valid: true}
	}
	return sql.NullString{}
}

// planRevision decides what to do with a prior current revision. Split out from
// the transaction so the precedence between the three outcomes — precondition
// violated, content unchanged, chain onto the prior revision — is unit-testable
// without a database.
//
// The precondition is checked before the idempotency shortcut on purpose: a
// caller that pinned a revision which is no longer current has lost the race
// regardless of what the bytes happen to say, and reporting success would tell
// them their edit landed on the revision they were looking at.
func planRevision(prior sqlc.ArtifactRevision, hasPrior bool, expected, contentHash string) (revisionPlan, error) {
	if expected != "" {
		switch {
		case !hasPrior:
			return revisionPlan{}, fmt.Errorf("%w: expected current revision %s but the artifact has none", ErrRevisionConflict, expected)
		case prior.ID != expected:
			return revisionPlan{}, fmt.Errorf("%w: expected current revision %s but it is %s", ErrRevisionConflict, expected, prior.ID)
		}
	}
	if hasPrior && prior.ContentHash == contentHash {
		return revisionPlan{noop: true, current: prior, hasCurrent: true}, nil
	}
	return revisionPlan{current: prior, hasCurrent: hasPrior}, nil
}

// commitRevision runs the whole read-decide-write inside one transaction. The
// prior current revision is read under a row lock, so a concurrent Put for the
// same (task, name) blocks until this one commits and then sees the new
// current — rather than both reading the same prior revision and chaining two
// siblings off it.
func (sqlStore *SQLStore) commitRevision(
	ctx context.Context,
	params PutParams,
	contentHash string,
	bytesToStore []byte,
) (Revision, error) {
	tx, txErr := sqlStore.db.BeginTx(ctx, nil)
	if txErr != nil {
		return Revision{}, fmt.Errorf("artifacts: begin tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }()

	qtx := sqlStore.queries.WithTx(tx)
	prior, hasPrior, priorErr := lockCurrent(ctx, qtx, params)
	if priorErr != nil {
		return Revision{}, priorErr
	}
	plan, planErr := planRevision(prior, hasPrior, params.ExpectedCurrentRevision, contentHash)
	if planErr != nil {
		return Revision{}, planErr
	}
	if plan.noop {
		// Identical content under the same (task, name): return the existing
		// revision rather than growing the chain with a revision that changed
		// nothing. The transaction rolls back; it only ever read.
		return fromRow(plan.current), nil
	}

	if plan.hasCurrent {
		affected, demoteErr := qtx.DemoteArtifactRevisionIfCurrent(ctx, sqlc.DemoteArtifactRevisionIfCurrentParams{
			TaskID: params.TaskID, Name: params.Name, ID: plan.current.ID,
		})
		if demoteErr != nil {
			return Revision{}, fmt.Errorf("artifacts: demote prior current: %w", demoteErr)
		}
		if affected == 0 {
			// The row we locked is no longer current. Under the row lock this
			// should be unreachable; it is checked anyway because the
			// alternative is silently inserting a second current revision and
			// leaving the unique index to reject it with a driver-level error.
			return Revision{}, fmt.Errorf("%w: revision %s was superseded during the write", ErrRevisionConflict, plan.current.ID)
		}
	}

	row, insertErr := qtx.CreateArtifactRevision(ctx, sqlc.CreateArtifactRevisionParams{
		TenantID:           params.TenantID,
		UserID:             params.UserID,
		TaskID:             params.TaskID,
		Name:               params.Name,
		Kind:               params.Kind,
		ContentHash:        contentHash,
		ContentSize:        int64(len(bytesToStore)),
		ActionType:         string(plan.action()),
		PrevRevisionID:     plan.prev(),
		SourceInvocationID: toNullString(params.Source),
		DeliveryStep:       toNullString(params.Coordinate.DeliveryStep),
		ExecutionUnit:      toNullString(params.Coordinate.ExecutionUnit),
		Phase:              toNullString(params.Coordinate.Phase),
		Actor:              string(params.Actor),
	})
	if insertErr != nil {
		// Two first-creates racing have no row to lock, so the partial unique
		// index is what serializes them. The loser sees a constraint violation,
		// which is a conflict for the caller, not an internal error.
		if isCurrentRevisionConflict(insertErr) {
			return Revision{}, fmt.Errorf("%w: a concurrent write created the current revision of %q", ErrRevisionConflict, params.Name)
		}
		return Revision{}, fmt.Errorf("artifacts: insert revision: %w", insertErr)
	}

	if err := tx.Commit(); err != nil {
		return Revision{}, fmt.Errorf("artifacts: commit revision tx: %w", err)
	}
	return fromRow(row), nil
}

// lockCurrent reads the current revision of (task, name) under a row lock.
// Reports (row, true) when one exists and (zero, false) when none does; a real
// store error is propagated rather than being flattened into "no current",
// because chaining a revision as a create when the lookup merely failed would
// silently fork the chain.
func lockCurrent(ctx context.Context, qtx *sqlc.Queries, params PutParams) (sqlc.ArtifactRevision, bool, error) {
	prior, err := qtx.LockCurrentArtifactRevisionForName(ctx, sqlc.LockCurrentArtifactRevisionForNameParams{
		TaskID: params.TaskID, TenantID: params.TenantID, Name: params.Name,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.ArtifactRevision{}, false, nil
		}
		return sqlc.ArtifactRevision{}, false, fmt.Errorf("artifacts: lock prior current: %w", err)
	}
	return prior, true, nil
}

// currentRevisionIndex is the partial unique index that enforces one current
// revision per (task_id, name). Named here so the driver-agnostic conflict
// check has something to match on.
const currentRevisionIndex = "idx_artifact_rev_current"

// isCurrentRevisionConflict reports whether err is the unique-violation raised
// by the one-current-per-name index. Matched on the index name rather than on a
// driver error type: the store is wired through database/sql and both lib/pq
// and pgx stdlib are in play, so the message is the one thing they agree on.
func isCurrentRevisionConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), currentRevisionIndex)
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
