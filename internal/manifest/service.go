// Package manifest owns the evidence manifest for a task run. The manifest is
// the immutable record of what went into a single run: the input task and its
// revision, the project and base commit, the pack and its resolved version +
// hash, the prompt revisions the adapter saw, the adapter + its declared
// capabilities, the model + tier, the effective capability profile, the memory
// slice pulled, the input / output artifact revisions, the check set version +
// results, the human gate decisions, and the git lineage (branch, checkpoints,
// base / result commit).
//
// Lifecycle (see internal/store/migrations/0006_manifests.sql):
//
//   - Init creates one manifest row per task at task creation.
//   - AddEvidence merges keys into the body. Append-only by convention; the Go
//     service constructs patches that extend arrays rather than replace them.
//   - Seal sets sealed_at + seal_reason. After sealing, AddEvidence is a typed
//     error; corrections go through Correct, which adds a linked row in
//     task_manifest_corrections.
//
// Subsystems not yet wired (project memory, capability enforcement, project
// checks) are recorded as explicit `missing` entries — the manifest never
// hides an absent input, it surfaces it.
package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// SealReason is the lifecycle reason a manifest was sealed. Stored on the row
// as seal_reason.
type SealReason string

const (
	// SealCompleted is the normal terminal path: task reached `done`.
	SealCompleted SealReason = "completed"
	// SealInterrupted is for an unclean exit: process crash, reconciler moved
	// the task to paused_user_stop with reason `interrupted`. The manifest is
	// sealed anyway — the evidence gathered so far is the durable record.
	SealInterrupted SealReason = "interrupted"
	// SealCancelled is the terminal abort path (POST /tasks/{id}/cancel).
	SealCancelled SealReason = "cancelled"
	// SealFailed is the failure path (runner.failTask).
	SealFailed SealReason = "failed"
)

// Service is the manifest lifecycle owner. It composes the sqlc.Queries index
// with the Go-side body shape (the JSONB body is opaque to SQL; the service
// marshals and merges).
type Service struct {
	db      *sql.DB
	queries *sqlc.Queries
	log     *slog.Logger
}

// Deps bundles Service construction.
type Deps struct {
	DB      *sql.DB
	Queries *sqlc.Queries
	Log     *slog.Logger
}

// New returns a Service.
func New(deps Deps) *Service {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: deps.DB, queries: deps.Queries, log: log}
}

// ErrSealed is returned by AddEvidence when the manifest is already sealed.
// Callers must use Correct to amend a sealed manifest.
var ErrSealed = errors.New("manifest: sealed; use Correct to amend")

// ErrNoManifest is returned when a task has no manifest row yet (Init was not
// called or failed).
var ErrNoManifest = errors.New("manifest: no manifest for task")

// Init creates the per-task manifest row. Idempotent: a second call for the
// same task is a no-op (ON CONFLICT DO NOTHING). The initial body carries the
// schema version and an empty evidence map; sections fill in via AddEvidence.
func (service *Service) Init(ctx context.Context, tenantID, userID, taskID string) error {
	empty := newEmptyBody()
	encoded, err := encodeBody(empty)
	if err != nil {
		return fmt.Errorf("manifest: encode initial body: %w", err)
	}
	_, err = service.queries.InitManifest(ctx, sqlc.InitManifestParams{
		TenantID: tenantID, UserID: userID, TaskID: taskID, Body: encoded,
	})
	if err != nil {
		return fmt.Errorf("manifest: init: %w", err)
	}
	return nil
}

// RecordGap records that an evidence write the orchestrator attempted failed,
// so the fact is carried on the sealed manifest rather than swallowed. It is
// itself best-effort: a failure here is returned to the caller, which logs and
// moves on rather than recursing — the gap is recorded when it can be, and the
// absence of a gap row does not imply the evidence succeeded. Sealed manifests
// refuse the gap (it would mutate an immutable body); the caller drops it.
func (service *Service) RecordGap(ctx context.Context, tenantID, taskID string, gap EvidenceGap) error {
	patch := Body{EvidenceGaps: []EvidenceGap{gap}}
	if err := service.AddEvidence(ctx, tenantID, taskID, patch); err != nil {
		if errors.Is(err, ErrSealed) {
			return nil
		}
		return err
	}
	return nil
}

// AddEvidence merges a patch into the manifest body. Append-only: arrays in the
// patch are appended to existing arrays; scalars are set once and refused on a
// second set (the caller constructs patches that respect this). Refuses to
// write when the manifest is sealed — returns ErrSealed.
//
// The merge base is the body read under the row lock inside the write
// transaction, so two AddEvidence calls racing cannot lose each other's
// contribution: the second writer blocks on the lock, then re-reads the body
// the first writer committed and merges onto that. The SQL itself is a full
// body replacement (not a JSONB merge) — the deep merge happens once, here.
//
// The patch is the body delta as a Body; fields the caller leaves zero stay
// unchanged. Fields the caller sets are merged in. The merge is conservative:
// an unknown section in the patch is an error.
func (service *Service) AddEvidence(
	ctx context.Context,
	tenantID, taskID string,
	patch Body,
) error {
	tx, txErr := service.db.BeginTx(ctx, nil)
	if txErr != nil {
		return fmt.Errorf("manifest: begin tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := service.queries.WithTx(tx)
	if err := service.AddEvidenceTx(ctx, qtx, tenantID, taskID, patch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: commit evidence: %w", err)
	}
	return nil
}

// AddEvidenceTx merges a patch into the manifest body using the caller's
// transaction, so evidence can be committed atomically with the state change it
// describes (a human-gate decision lands in the same tx as the FSM transition
// that advances the task past the gate). AddEvidence is this with a transaction
// of its own.
//
// The caller's transaction must have begun before this is called; AddEvidenceTx
// neither commits nor rolls it back. The merge base is the body read under the
// row lock via GetManifestForUpdate on the same transaction, so concurrent
// AddEvidenceTx / AddEvidence calls serialize correctly.
func (service *Service) AddEvidenceTx(
	ctx context.Context,
	qtx *sqlc.Queries,
	tenantID, taskID string,
	patch Body,
) error {
	// Lock the row and read the body under the lock in the same transaction that
	// writes. This is the whole fix: the merge base is now the body as it is at
	// write time, not a pre-transaction snapshot a concurrent writer can have
	// changed between read and write.
	locked, err := qtx.GetManifestForUpdate(ctx, sqlc.GetManifestForUpdateParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoManifest
		}
		return fmt.Errorf("manifest: lock: %w", err)
	}
	if locked.SealedAt.Valid {
		return ErrSealed
	}
	encoded, encErr := mergeIntoLocked(locked.Body, patch)
	if encErr != nil {
		return fmt.Errorf("manifest: merge evidence: %w", encErr)
	}
	if _, err := qtx.AddManifestEvidence(ctx, sqlc.AddManifestEvidenceParams{
		ID: locked.ID, TenantID: tenantID, Body: encoded,
	}); err != nil {
		// :one with a full-replacement UPDATE returns no rows only when the
		// sealed_at IS NULL guard rejects it — i.e. a concurrent Seal committed
		// between our locking read and our write. That is a sealed manifest,
		// not a generic store failure.
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSealed
		}
		return fmt.Errorf("manifest: add evidence: %w", err)
	}
	return nil
}

// mergeIntoLocked decodes the locked manifest body, merges the patch into it,
// and returns the canonical encoded result. Extracted from AddEvidenceTx so the
// "decode → merge → encode" step is unit-testable without a database: a body
// that fails to decode is an error rather than a silently-empty merge base,
// because merging onto an empty body would clobber the existing evidence with a
// patch-shaped subset of it.
func mergeIntoLocked(locked []byte, patch Body) ([]byte, error) {
	existing, err := decodeBody(locked)
	if err != nil {
		return nil, fmt.Errorf("decode locked body: %w", err)
	}
	merged := mergeBodies(existing, patch)
	return encodeBody(merged)
}

// Seal finalizes the manifest. Sets sealed_at + seal_reason + sealed_by, and
// rewrites the body so the seal carries the derived `missing` list and an
// `evidence_complete` flag computed from the body as it actually is at seal
// time — not the list asserted once at init, which could grow stale (and did:
// `capabilities` was listed missing on bodies that carried a populated
// capabilities section). After sealing the body is immutable; corrections land
// via Correct. Idempotent: a second Seal is a no-op.
func (service *Service) Seal(
	ctx context.Context,
	tenantID, userID, taskID string,
	reason SealReason,
) error {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("manifest: begin seal tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := service.queries.WithTx(tx)

	locked, err := qtx.GetManifestForUpdate(ctx, sqlc.GetManifestForUpdateParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoManifest
		}
		return fmt.Errorf("manifest: load for seal: %w", err)
	}
	if locked.SealedAt.Valid {
		return nil // already sealed — idempotent
	}

	// Derive the missing list and the completeness flag from the body under the
	// seal lock, then write them as part of the same seal transaction. The
	// completeness flag is false when any section is degraded (an evidence gap
	// was recorded) OR when a section the run should have produced is absent.
	body, err := decodeBody(locked.Body)
	if err != nil {
		return fmt.Errorf("manifest: decode body for seal: %w", err)
	}
	body.Missing = body.MissingSections()
	complete := body.IsEvidenceComplete()
	body.EvidenceComplete = &complete
	sealedBody, err := encodeBody(body)
	if err != nil {
		return fmt.Errorf("manifest: encode sealed body: %w", err)
	}
	if _, err := qtx.AddManifestEvidence(ctx, sqlc.AddManifestEvidenceParams{
		ID: locked.ID, TenantID: tenantID, Body: sealedBody,
	}); err != nil {
		return fmt.Errorf("manifest: write derived missing at seal: %w", err)
	}

	if _, err := qtx.SealManifest(ctx, sqlc.SealManifestParams{
		ID: locked.ID, TenantID: tenantID,
		SealedBy:   nullString(userID),
		SealReason: nullString(string(reason)),
	}); err != nil {
		return fmt.Errorf("manifest: seal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: commit seal: %w", err)
	}
	return nil
}

// Correct adds a post-seal amendment as a linked correction row. The sealed
// body is never edited; each correction's body is a full snapshot that already
// includes every earlier correction. Correction N's body is correction N-1's
// body with patch N merged in (falling back to the sealed body for N=1), so the
// corrections form a chain and the newest one is the authoritative state by
// construction.
//
// The chain invariant is enforced by reading the latest correction and the
// parent manifest under the manifest's row lock inside one transaction: two
// concurrent corrections serialize on the parent row, and the second one reads
// the first one's snapshot as its base rather than the stale sealed body. There
// is no correction row to lock for the first correction, which is exactly why
// the parent manifest row is the lock target.
func (service *Service) Correct(
	ctx context.Context,
	tenantID, userID, taskID string,
	reason string,
	correction Body,
) error {
	tx, txErr := service.db.BeginTx(ctx, nil)
	if txErr != nil {
		return fmt.Errorf("manifest: begin correction tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := service.queries.WithTx(tx)
	// Lock the parent manifest. This is what serializes concurrent corrections
	// (and corrections against a late AddEvidence): every correction takes this
	// lock, so only one runs at a time per task.
	locked, err := qtx.GetManifestForUpdate(ctx, sqlc.GetManifestForUpdateParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoManifest
		}
		return fmt.Errorf("manifest: load for correction: %w", err)
	}
	if !locked.SealedAt.Valid {
		return errors.New("manifest: cannot correct an unsealed manifest; use AddEvidence")
	}
	latest, err := qtx.LatestManifestCorrection(ctx, sqlc.LatestManifestCorrectionParams{
		ManifestID: locked.ID, TenantID: tenantID,
	})
	var latestBody *Body
	if err == nil {
		decoded, decodeErr := decodeBody(latest.Body)
		if decodeErr != nil {
			return fmt.Errorf("manifest: decode latest correction: %w", decodeErr)
		}
		latestBody = &decoded
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("manifest: load latest correction: %w", err)
	}

	sealed, err := decodeBody(locked.Body)
	if err != nil {
		return fmt.Errorf("manifest: decode sealed body: %w", err)
	}
	base := correctionBase(sealed, latestBody)
	merged := mergeBodies(base, correction)
	encoded, encodeErr := encodeBody(merged)
	if encodeErr != nil {
		return fmt.Errorf("manifest: encode corrected body: %w", encodeErr)
	}
	if _, err := qtx.CreateManifestCorrection(ctx, sqlc.CreateManifestCorrectionParams{
		TenantID: tenantID, UserID: userID, ManifestID: locked.ID,
		Body: encoded, Reason: reason,
	}); err != nil {
		return fmt.Errorf("manifest: insert correction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: commit correction: %w", err)
	}
	return nil
}

// correctionBase selects the body a new correction should merge onto. The
// invariant is that correction N chains onto correction N-1, so when a latest
// correction exists it is the base; otherwise the sealed body is the base for
// the first correction. Extracted as a pure helper so the chaining rule is
// unit-testable without a database.
func correctionBase(sealed Body, latest *Body) Body {
	if latest != nil {
		return *latest
	}
	return sealed
}

// Get returns the manifest body, the seal metadata, and any corrections. The
// corrections list is empty when no corrections exist. The body the caller
// gets back is the most recent state — the sealed body with the latest
// correction applied (if any). The raw sealed body and the per-correction
// deltas are available via GetRaw for callers that need the audit trail.
func (service *Service) Get(
	ctx context.Context,
	tenantID, taskID string,
) (Body, SealInfo, []Correction, error) {
	row, err := service.queries.GetManifest(ctx, sqlc.GetManifestParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Body{}, SealInfo{}, nil, ErrNoManifest
		}
		return Body{}, SealInfo{}, nil, fmt.Errorf("manifest: get: %w", err)
	}
	body, err := decodeBody(row.Body)
	if err != nil {
		return Body{}, SealInfo{}, nil, fmt.Errorf("manifest: decode body: %w", err)
	}
	rows, err := service.queries.ListManifestCorrections(ctx, sqlc.ListManifestCorrectionsParams{
		ManifestID: row.ID, TenantID: tenantID,
	})
	if err != nil {
		return Body{}, SealInfo{}, nil, fmt.Errorf("manifest: list corrections: %w", err)
	}
	corrections := make([]Correction, 0, len(rows))
	for _, correctionRow := range rows {
		correctionBody, decodeErr := decodeBody(correctionRow.Body)
		if decodeErr != nil {
			return Body{}, SealInfo{}, nil, fmt.Errorf("manifest: decode correction body: %w", decodeErr)
		}
		corrections = append(corrections, Correction{
			ID: correctionRow.ID, Reason: correctionRow.Reason,
			Body: correctionBody, CreatedAt: correctionRow.CreatedAt,
		})
	}
	// The latest correction is the authoritative body. This is sound only
	// because Correct chains each correction onto the prior one, so the newest
	// snapshot already contains every earlier correction's changes; taking the
	// last body once, after the loop, reflects the full correction chain.
	if last := len(corrections); last > 0 {
		body = corrections[last-1].Body
	}
	sealInfo := SealInfo{
		Sealed:    row.SealedAt.Valid,
		Reason:    nullStringOr(row.SealReason),
		SealedBy:  nullStringOr(row.SealedBy),
		SealedAt:  row.SealedAt,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	return body, sealInfo, corrections, nil
}

// SealInfo is the seal metadata for a manifest.
type SealInfo struct {
	Sealed    bool
	Reason    string
	SealedBy  string
	SealedAt  sql.NullTime
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Correction is one post-seal amendment. Body is the corrected full snapshot.
type Correction struct {
	ID        string
	Reason    string
	Body      Body
	CreatedAt time.Time
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullStringOr(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}
