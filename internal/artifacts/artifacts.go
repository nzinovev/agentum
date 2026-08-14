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
// Secrets / credentials: the Scanner inspects both the artifact's name and its
// bytes before they are written. Text content is redacted (or the write is
// rejected, per ScanPolicy); binary content is reported but never rewritten,
// since rewriting it would corrupt the blob; a credential-shaped name is always
// refused, because a path has nothing to redact. It is a best-effort guard, not
// a security boundary — operators remain responsible for what their agents
// emit. The scanner never modifies the bytes on disk in the worktree; it only
// gates what enters the durable store.
//
// Containment: agent-declared artifact paths are untrusted input that the
// orchestrator reads with its own privileges. ResolveDeclared is the single
// gate — see path.go for the three escapes it closes.
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

// DiffTruncationMarker is the byte sequence the runner appends to a diff.patch
// revision capped at the size cap, so a reader can tell the patch was cut.
// Defined here (not in the runner) because two packages need it — the runner
// produces the marker and the final-review payload detects it by scanning the
// stored bytes; content_size cannot distinguish a capped patch from a
// cap-sized one. Changing this string is a wire-format break that breaks
// truncation detection; keep it stable.
const DiffTruncationMarker = "\n--- diff truncated by Agentum at the size cap; see diff.stat and read named files directly ---\n"

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

	// ExpectedCurrentRevision is an optimistic-concurrency precondition. When
	// set, Put commits only if that revision is still the current one for
	// (task, name) at the moment of the write; otherwise it fails with
	// ErrRevisionConflict and writes nothing.
	//
	// Empty means "no precondition": the write chains onto whatever is current,
	// which is what a stage capture wants (the runner is the only writer for
	// the duration of an invocation). A human edit that was composed against a
	// revision the user has seen should set it, so two editors racing on the
	// same artifact produce a conflict instead of a silent lost update.
	ExpectedCurrentRevision string
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

// ErrRevisionConflict is returned by Store.Put when the revision chain moved
// under the caller: either PutParams.ExpectedCurrentRevision no longer names
// the current revision, or a concurrent writer committed between this call's
// read and its write. The write is rolled back in full — no blob is orphaned in
// the index and no revision row is created. HTTP callers map it to 409.
var ErrRevisionConflict = errors.New("artifacts: revision conflict")

// ObjectStore is the content-addressed byte store behind the revisions index.
// A revision row references a blob by hash; the blob itself carries no
// metadata, so the two layers compose without either knowing the other's
// schema.
//
// The interface exists so the durable layer is not welded to the local
// filesystem: an S3/GCS-backed implementation is a drop-in for a deployment
// where the orchestrator is not the only reader of the blobs. *BlobStore is the
// only implementation today.
type ObjectStore interface {
	// Put writes the bytes under the hash-derived address if absent.
	// Idempotent on the hash: writing the same content twice is one write.
	Put(contentHash string, bytes []byte) error
	// Get returns the bytes for a content hash.
	Get(contentHash string) ([]byte, error)
	// CopyTo streams the bytes for a content hash to writer, returning the
	// count. Preferred over Get for large objects.
	CopyTo(contentHash string, writer io.Writer) (int64, error)
	// Reader returns a streaming reader over the bytes. The caller owns Close.
	Reader(contentHash string) (io.ReadCloser, error)
}

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

// ErrSecretDetected is returned by Store.Put when the scanner found
// credential-shaped content and the configured policy is PolicyReject. Nothing
// is written: no blob, no revision row.
var ErrSecretDetected = errors.New("artifacts: credential-shaped content rejected")

// ScanPolicy decides what happens when the scanner finds something.
type ScanPolicy string

const (
	// PolicyRedact substitutes [REDACTED] for each match in text content and
	// stores the result. Binary content cannot be rewritten without corrupting
	// it, so a binary finding is reported and the bytes are stored as-is —
	// PolicyReject is the only policy that actually stops a binary leak.
	PolicyRedact ScanPolicy = "redact"
	// PolicyReject refuses the write outright with ErrSecretDetected. The
	// fail-closed choice for deployments where an artifact carrying a
	// credential is an incident rather than a nuisance.
	PolicyReject ScanPolicy = "reject"
)

// ScanResult is what a Scanner reports about one artifact.
type ScanResult struct {
	// Bytes are the bytes to store. Equal to the input when nothing matched, or
	// when the content is binary (which is never rewritten).
	Bytes []byte
	// Findings names the rules that matched, in rule order, deduplicated. Empty
	// when the content is clean. Callers log it — an operator needs to know an
	// artifact was altered, and which rule did it.
	Findings []string
	// Rewritten reports whether Bytes differ from the input.
	Rewritten bool
}

// Clean reports whether the scan found nothing.
func (result ScanResult) Clean() bool { return len(result.Findings) == 0 }

// Scanner inspects artifact bytes and names before they enter the durable
// store. It is a containment guard, not a security boundary: it catches the
// credential shapes that show up in real configs and logs, and operators remain
// responsible for what their agents emit.
//
// Two things are scanned, and they fail differently:
//
//	content   secret-shaped bytes    → redacted or rejected, per ScanPolicy
//	name      credential-shaped path → always rejected; a path cannot be redacted
//
// A nil Scanner on SQLStoreDeps means "use the default"; use NoScan to disable
// scanning in tests where it would obscure intentional content.
type Scanner interface {
	// ScanName reports whether the artifact name itself is credential-shaped
	// (".ssh/id_rsa", ".aws/credentials", ".env"). Returns an
	// ErrSecretDetected-wrapping error when it is.
	ScanName(name string) error
	// Scan inspects the bytes and returns what to store. Returns an
	// ErrSecretDetected-wrapping error when the policy is PolicyReject and
	// something matched.
	Scan(name, kind string, bytes []byte) (ScanResult, error)
}

// NoScan is a Scanner that inspects nothing. Used in tests where scanning would
// obscure intentional content.
type NoScan struct{}

// ScanName implements Scanner.
func (NoScan) ScanName(string) error { return nil }

// Scan implements Scanner.
func (NoScan) Scan(_ string, _ string, bytes []byte) (ScanResult, error) {
	return ScanResult{Bytes: bytes}, nil
}
