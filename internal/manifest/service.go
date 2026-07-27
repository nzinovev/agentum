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

// AddEvidence merges a patch into the manifest body. Append-only: arrays in the
// patch are appended to existing arrays; scalars are set once and refused on a
// second set (the caller constructs patches that respect this). Refuses to
// write when the manifest is sealed — returns ErrSealed.
//
// The patch is the body delta as a Body; fields the caller leaves zero stay
// unchanged. Fields the caller sets are merged in. The merge is conservative:
// an unknown section in the patch is an error.
func (service *Service) AddEvidence(
	ctx context.Context,
	tenantID, taskID string,
	patch Body,
) error {
	current, err := service.queries.GetManifest(ctx, sqlc.GetManifestParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoManifest
		}
		return fmt.Errorf("manifest: load: %w", err)
	}
	if current.SealedAt.Valid {
		return ErrSealed
	}

	existing, err := decodeBody(current.Body)
	if err != nil {
		return fmt.Errorf("manifest: decode body: %w", err)
	}
	merged := mergeBodies(existing, patch)
	encoded, err := encodeBody(merged)
	if err != nil {
		return fmt.Errorf("manifest: encode merged body: %w", err)
	}

	tx, txErr := service.db.BeginTx(ctx, nil)
	if txErr != nil {
		return fmt.Errorf("manifest: begin tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := service.queries.WithTx(tx)
	// Lock the row and re-check the seal under the lock so a concurrent Seal
	// cannot race this AddEvidence.
	locked, err := qtx.GetManifestForUpdate(ctx, sqlc.GetManifestForUpdateParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		return fmt.Errorf("manifest: lock: %w", err)
	}
	if locked.SealedAt.Valid {
		return ErrSealed
	}
	if _, err := qtx.AddManifestEvidence(ctx, sqlc.AddManifestEvidenceParams{
		ID: locked.ID, TenantID: tenantID, Body: encoded,
	}); err != nil {
		return fmt.Errorf("manifest: add evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: commit evidence: %w", err)
	}
	return nil
}

// Seal finalizes the manifest. Sets sealed_at + seal_reason + sealed_by. After
// sealing the body is immutable; corrections land via Correct. Idempotent: a
// second Seal is a no-op.
func (service *Service) Seal(
	ctx context.Context,
	tenantID, userID, taskID string,
	reason SealReason,
) error {
	// Pin the git lineage (base_commit, branch, checkpoints, result_commit) at
	// seal time — these may have been recorded late by the runner. The merge is
	// a no-op when nothing new is provided; the seal itself is the
	// authoritative freeze.
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
// body is never edited; the correction's body is a full snapshot of the
// corrected manifest. Consumers read the sealed row joined with its
// corrections ordered by created_at.
func (service *Service) Correct(
	ctx context.Context,
	tenantID, userID, taskID string,
	reason string,
	correction Body,
) error {
	current, err := service.queries.GetManifest(ctx, sqlc.GetManifestParams{
		TaskID: taskID, TenantID: tenantID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoManifest
		}
		return fmt.Errorf("manifest: load for correction: %w", err)
	}
	if !current.SealedAt.Valid {
		return errors.New("manifest: cannot correct an unsealed manifest; use AddEvidence")
	}
	sealed, err := decodeBody(current.Body)
	if err != nil {
		return fmt.Errorf("manifest: decode sealed body: %w", err)
	}
	merged := mergeBodies(sealed, correction)
	encoded, encodeErr := encodeBody(merged)
	if encodeErr != nil {
		return fmt.Errorf("manifest: encode corrected body: %w", encodeErr)
	}
	if _, err := service.queries.CreateManifestCorrection(ctx, sqlc.CreateManifestCorrectionParams{
		TenantID: tenantID, UserID: userID, ManifestID: current.ID,
		Body: encoded, Reason: reason,
	}); err != nil {
		return fmt.Errorf("manifest: insert correction: %w", err)
	}
	return nil
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
		// The latest correction is the authoritative body. Earlier corrections
		// are part of the audit trail; the last one wins for the "current
		// state" view Get returns.
		if len(corrections) > 0 {
			body = correctionBody
		}
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
