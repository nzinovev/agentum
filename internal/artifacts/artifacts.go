// Package artifacts owns the immutable, content-addressed store for stage and
// edit output. It exists independently of any worktree: the worktree is a
// disposable scratch space, while a revision row + its FS blob are the durable
// record of what a given invocation or edit produced.
//
// Design (see docs/execution.md "Evidence manifest and artifact revisions"):
//   - Bytes live on the local FS in a canonical, worktree-independent location
//     (configured ArtifactRoot, default <repo>/.agentum/artifacts). The path is
//     derived from the sha256 of the bytes, so identical content is stored once.
//   - A revision row (artifact_revisions) is the durable index. Each edit
//     creates a new revision that chains back to the prior one via
//     prev_revision_id. Revisions are never modified in place; the
//     "is_current" flag is the single mutable bit and only one revision per
//     (task, name) is current at a time.
//   - The optional execution coordinate (delivery_step / execution_unit /
//     phase) rides along as provenance. It is inert unless Epic 8 fills it;
//     single-unit runs leave it NULL and behavior is unchanged.
//
// Secrets / credentials: the secret redactor strips obvious high-entropy
// tokens from text-kind artifacts before they are written. It is a best-effort
// guard, not a security boundary — operators remain responsible for what their
// agents emit. The redactor never modifies the bytes the agent wrote; it only
// gates what enters the durable store.
package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Action is the kind of change a revision records. Stored on the row as
// action_type.
type Action string

const (
	// ActionCreate is the first revision of a (task, name).
	ActionCreate Action = "create"
	// ActionEdit is a subsequent revision that chains to a prior one.
	ActionEdit Action = "edit"
)

// Actor is who produced the revision. Stored on the row as actor.
type Actor string

const (
	// ActorHuman is a user-initiated edit via the API.
	ActorHuman Actor = "human"
	// ActorAgent is the coding agent producing stage output.
	ActorAgent Actor = "agent"
	// ActorSystem is an orchestrator-internal change (e.g. a fix-loop).
	ActorSystem Actor = "system"
)

// Revision is the Go-shaped view of an artifact_revisions row.
type Revision struct {
	ID          string
	TenantID    string
	UserID      string
	TaskID      string
	Name        string
	Kind        string
	ContentHash string
	ContentSize int64
	ActionType  Action
	Prev        string // previous revision id; empty for ActionCreate
	Source      string // source invocation id; empty for human edits
	// ExecutionCoordinate is the optional (delivery_step, execution_unit,
	// phase) triple. Empty for single-unit runs.
	ExecutionCoordinate Coordinate
	Actor               Actor
	CreatedAt           time.Time
	IsCurrent           bool
}

// Coordinate is the optional execution coordinate a revision may carry. It
// records where in a multi-step delivery this revision was produced. All three
// fields are empty for a single-unit run; their absence is the signal that the
// coordinate does not apply.
type Coordinate struct {
	DeliveryStep  string
	ExecutionUnit string
	Phase         string
}

// Empty reports whether the coordinate is unset. Single-unit runs leave it
// empty; the runner does not fill it and the manifest treats it as inert.
func (coordinate Coordinate) Empty() bool {
	return coordinate.DeliveryStep == "" && coordinate.ExecutionUnit == "" && coordinate.Phase == ""
}

// PutParams is the input to Store.Put. Bytes are the body the caller wants
// stored; the store hashes them, writes the blob if absent, and inserts a new
// revision row chained to the prior current revision of (task, name).
type PutParams struct {
	TenantID string
	UserID   string
	TaskID   string
	Name     string
	Kind     string
	Bytes    []byte
	Source   string // invocation id; empty for human edits
	Actor    Actor
	Coordinate
}

// Store is the durable artifact revisions store. Implementations keep the FS
// blob and the Postgres index in step: a successful Put writes the blob (idempotent
// on hash) and commits the revision row in one transaction.
type Store interface {
	// Put writes the bytes as a new immutable revision and returns the row.
	// The prior current revision of (task, name) is demoted; the new revision
	// is the single current one. If the bytes hash equals the current
	// revision's hash, Put is a no-op and returns the existing row (an edit
	// that produces identical content does not create a redundant revision).
	Put(ctx context.Context, params PutParams) (Revision, error)

	// Get returns the revision row (no bytes).
	Get(ctx context.Context, tenantID, revisionID string) (Revision, error)

	// GetBytes returns the blob bytes for a revision.
	GetBytes(ctx context.Context, tenantID, revisionID string) ([]byte, error)

	// Reader returns a streaming reader over the blob bytes. Callers that
	// only need to copy the bytes to an io.Writer should prefer CopyTo.
	Reader(ctx context.Context, tenantID, revisionID string) (io.ReadCloser, error)

	// CopyTo writes the blob bytes for revisionID to writer. Returns the byte
	// count. The HTTP GET handler uses this to stream artifact contents without
	// buffering them in memory.
	CopyTo(ctx context.Context, tenantID, revisionID string, writer io.Writer) (int64, error)

	// Current returns the current revision for (task, name). Returns
	// ErrNoCurrentRevision when the task has no revision of that name yet.
	Current(ctx context.Context, tenantID, taskID, name string) (Revision, error)

	// ListForTask returns every revision of every name in the task, ordered by
	// name then newest-first. Includes superseded, non-current revisions.
	ListForTask(ctx context.Context, tenantID, taskID string) ([]Revision, error)

	// ListCurrent returns only the current revisions of every name in the
	// task — the snapshot a resume / comparison reads.
	ListCurrent(ctx context.Context, tenantID, taskID string) ([]Revision, error)

	// ListForInvocation returns the revisions produced by one invocation.
	ListForInvocation(ctx context.Context, tenantID, invocationID string) ([]Revision, error)
}

// ErrNoCurrentRevision is returned by Store.Current when the task has no
// revision of the given name yet (e.g. a fresh task before any stage runs).
var ErrNoCurrentRevision = errors.New("artifacts: no current revision for name")

// errStore is a typed wrapper for store errors that callers may branch on.
type errStore struct{ cause error }

func (storeErr *errStore) Error() string { return storeErr.cause.Error() }
func (storeErr *errStore) Unwrap() error { return storeErr.cause }

// Wrap adapts a sqlc store error to the artifacts layer. sql.ErrNoRows becomes
// ErrNoCurrentRevision (for the Current path) or a wrapped error otherwise.
func wrapNoRows(err error, target error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return target
	}
	return err
}

// fromRow lifts a sqlc row to the package-level Revision shape. Centralized so
// the nullable → empty-string mapping has one home.
func fromRow(row sqlc.ArtifactRevision) Revision {
	revision := Revision{
		ID:          row.ID,
		TenantID:    row.TenantID,
		UserID:      row.UserID,
		TaskID:      row.TaskID,
		Name:        row.Name,
		Kind:        row.Kind,
		ContentHash: row.ContentHash,
		ContentSize: row.ContentSize,
		ActionType:  Action(row.ActionType),
		Actor:       Actor(row.Actor),
		CreatedAt:   row.CreatedAt,
		IsCurrent:   row.IsCurrent,
	}
	if row.PrevRevisionID.Valid {
		revision.Prev = row.PrevRevisionID.String
	}
	if row.SourceInvocationID.Valid {
		revision.Source = row.SourceInvocationID.String
	}
	revision.ExecutionCoordinate = Coordinate{
		DeliveryStep:  nullStringOr(row.DeliveryStep),
		ExecutionUnit: nullStringOr(row.ExecutionUnit),
		Phase:         nullStringOr(row.Phase),
	}
	return revision
}

func nullStringOr(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

// Redactor strips secret-shaped substrings from text-kind artifact bytes
// before they are stored. The default implementation (NewDefaultRedactor)
// matches common token patterns (Bearer headers, long hex / base64 runs,
// AKIA-style AWS keys). It never modifies the bytes the agent wrote; it only
// gates what enters the durable store. A nil Redactor skips redaction.
type Redactor interface {
	// Redact returns the bytes to store and a non-nil error if redaction is
	// mandatory and the input cannot be cleaned (the default impl never errors).
	Redact(name, kind string, bytes []byte) ([]byte, error)
}

// NoRedaction is a Redactor that does nothing. Used in tests where the
// redactor would obscure intentional content.
type NoRedaction struct{}

// Redact implements Redactor.
func (NoRedaction) Redact(string, string, []byte) ([]byte, error) { return nil, nil }
