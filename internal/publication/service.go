// Package publication is the publication coordinator: the job handler that
// turns a finished run into a delivered branch and a draft pull request. It
// assembles the delivery input from durable rows, enforces the delivery gate,
// leases the publication row, calls the provider through the contract in
// internal/publish, and records the outcome — the row, the events, and the
// manifest evidence.
//
// The coordinator knows the provider only as a registry id. The provider
// implementation is reached exclusively through publish.Publisher, and the
// packages are mutually blind: the runner and this package never import each
// other — the queue's kind table is the whole connection between them.
//
// A publication failure is a recorded outcome, not a job failure: the queue's
// poison bound exists for processing errors, and an unreachable provider is an
// expected external state with its own behaviour. The handler returns an error
// only when it cannot write the publication row itself.
package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/publish"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Event types the publication coordinator emits. The durable log frames
// whatever string lands here; the actor column is always system — a
// publication is the orchestrator delivering, never a person acting.
const (
	EvPublicationStarted = "run.publication_started"
	EvPublished          = "run.published"
	EvPublicationFailed  = "run.publication_failed"
)

// DefaultLeaseTTL bounds how long one publication attempt may hold the lease
// before the recovery probe considers its worker dead.
const DefaultLeaseTTL = 5 * time.Minute

// correctionReasonPublication is the manifest-correction reason a publication
// recorded after the seal carries. The sealed body stays the snapshot of seal
// time; the correction chain's newest link is the current state.
const correctionReasonPublication = "publication_updated"

// Store is the sqlc surface the coordinator needs, in plain Go types. The
// nullable columns are adapted where used; the interface exists so unit tests
// substitute a fake without a database.
type Store interface {
	GetRun(ctx context.Context, arg sqlc.GetRunParams) (sqlc.Run, error)
	GetProject(ctx context.Context, arg sqlc.GetProjectParams) (sqlc.Project, error)
	GetPublicationForRun(ctx context.Context, arg sqlc.GetPublicationForRunParams) (sqlc.RunPublication, error)
	EnsurePublication(ctx context.Context, arg sqlc.EnsurePublicationParams) (sqlc.RunPublication, error)
	ClaimPublication(ctx context.Context, arg sqlc.ClaimPublicationParams) (sqlc.RunPublication, error)
	RecordPublicationSuccess(ctx context.Context, arg sqlc.RecordPublicationSuccessParams) (sqlc.RunPublication, error)
	RecordPublicationFailure(ctx context.Context, arg sqlc.RecordPublicationFailureParams) (sqlc.RunPublication, error)
	AppendEvent(ctx context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error)
}

// ManifestEvidence is the manifest surface the coordinator needs: reading the
// body for the gate, writing the publication section, and amending a sealed
// manifest through the correction chain. *manifest.Service satisfies it.
type ManifestEvidence interface {
	Get(ctx context.Context, tenantID, runID string) (manifest.Body, manifest.SealInfo, []manifest.Correction, error)
	AddEvidence(ctx context.Context, tenantID, runID string, patch manifest.Body) error
	Correct(ctx context.Context, tenantID, userID, runID string, reason string, correction manifest.Body) error
}

// providerRegistry is the registry surface the coordinator needs: resolve a
// provider id. *publish.Registry satisfies it; the seam exists so a test can
// serve a scripted publisher without a second registry implementation in
// production code.
type providerRegistry interface {
	Resolve(id publish.ProviderID) (publish.Publisher, error)
}

// Service is the publication coordinator. It serves the "publish" job kind.
type Service struct {
	store    Store
	mfst     ManifestEvidence
	registry providerRegistry
	leaseTTL time.Duration
	log      *slog.Logger
}

// Deps bundles Service construction. Manifest may be nil (unit tests);
// evidence writes become no-ops then. Registry nil means the real registry
// with its default table. LeaseTTL zero means DefaultLeaseTTL.
type Deps struct {
	Store    Store
	Manifest ManifestEvidence
	Registry providerRegistry
	LeaseTTL time.Duration
	Log      *slog.Logger
}

// New builds a Service.
func New(deps Deps) *Service {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	leaseTTL := deps.LeaseTTL
	if leaseTTL == 0 {
		leaseTTL = DefaultLeaseTTL
	}
	registry := deps.Registry
	if registry == nil {
		registry = publish.NewRegistry(publish.RegistryOptions{})
	}
	return &Service{
		store: deps.Store, mfst: deps.Manifest, registry: registry,
		leaseTTL: leaseTTL, log: log,
	}
}

// Handle is the publish job: lease, gate, deliver, record. Every publication
// outcome — refused, blocked, delivered — is written to the publication row
// and the event log, and the job itself succeeds: a recorded refusal is the
// expected handling of an external state, not a processing failure. The
// returned error is reserved for failures to write the row, where the queue's
// retry and poison bound apply.
func (service *Service) Handle(ctx context.Context, job sqlc.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := service.store.GetRun(ctx, sqlc.GetRunParams{ID: job.RunID, TenantID: job.TenantID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The run is gone; the cascade took the publication row with it.
			service.log.Warn("publication: run missing", "run", job.RunID)
			return nil
		}
		return fmt.Errorf("publication: load run: %w", err)
	}
	row, err := service.loadOrCreateRow(ctx, record)
	if err != nil {
		return err
	}

	// Mutual exclusion by lease: the network call below must not run inside a
	// transaction, so a racing worker resolves by this conditional UPDATE and
	// the loser reads no row.
	claimed, err := service.store.ClaimPublication(ctx, sqlc.ClaimPublicationParams{
		TenantID:       record.TenantID,
		RunID:          record.ID,
		LeaseOwner:     sql.NullString{String: fmt.Sprintf("job-%d", job.ID), Valid: true},
		LeaseExpiresAt: sql.NullTime{Time: time.Now().Add(service.leaseTTL), Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			service.log.Info("publication: lease held elsewhere", "run", record.ID)
			return nil
		}
		return fmt.Errorf("publication: claim: %w", err)
	}
	service.emit(ctx, record, EvPublicationStarted, map[string]any{
		"attempts": claimed.Attempts, "state": claimed.State,
	})

	// The delivery gate reads the manifest the run itself recorded: the
	// checks section's verdict and its bound commit. The checks are never
	// re-run here — the gate's input is the durable record, not a fresh
	// execution whose outcome could differ from what was reviewed.
	body, sealInfo := service.readManifest(ctx, record)
	if refusalCode, refusalMessage, refused := gateRefusal(body, nullStringOr(record.ResultCommit)); refused {
		return service.recordRefusal(ctx, record, refusalCode, refusalMessage, publish.Result{})
	}

	provider, resolveErr := service.registry.Resolve(publish.ProviderID(row.Provider))
	if resolveErr != nil {
		return service.recordRefusal(ctx, record, publish.ReasonProviderError, resolveErr.Error(), publish.Result{})
	}

	delivery := service.assembleDelivery(ctx, record, row, body, sealInfo)
	result, publishErr := provider.Publish(ctx, delivery)
	if publishErr != nil {
		code, message := publish.Classify(publishErr)
		return service.recordRefusal(ctx, record, code, message, result)
	}

	updated, err := service.store.RecordPublicationSuccess(ctx, sqlc.RecordPublicationSuccessParams{
		TenantID: record.TenantID, RunID: record.ID,
		TargetHost:       delivery.Target.Host,
		TargetOwner:      delivery.Target.Owner,
		TargetRepository: delivery.Target.Repository,
		BaseBranch:       delivery.Target.BaseBranch,
		PrNumber:         sql.NullInt32{Int32: int32(result.PullRequest), Valid: result.PullRequest > 0},
		PrUrl:            sql.NullString{String: result.PullRequestURL, Valid: result.PullRequestURL != ""},
		PrState:          sql.NullString{String: result.PullRequestState, Valid: result.PullRequestState != ""},
	})
	if err != nil {
		return fmt.Errorf("publication: record success: %w", err)
	}
	service.emit(ctx, record, EvPublished, map[string]any{
		"pull_request": updated.PrNumber.Int32,
		"url":          updated.PrUrl.String,
		"branch":       updated.RemoteBranch,
	})
	service.recordEvidence(ctx, record, updated, provider)
	return nil
}

// loadOrCreateRow reads the publication row, creating it when a publish job
// arrives for a run whose final gate ran while publication was disabled — the
// explicit retry is then the row's creator. The created row names the
// registry's default provider, the same id an enabled gate records.
func (service *Service) loadOrCreateRow(ctx context.Context, record sqlc.Run) (sqlc.RunPublication, error) {
	row, err := service.store.GetPublicationForRun(ctx, sqlc.GetPublicationForRunParams{
		TenantID: record.TenantID, RunID: record.ID,
	})
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return sqlc.RunPublication{}, fmt.Errorf("publication: load row: %w", err)
	}
	defaultProvider, resolveErr := service.registry.Resolve("")
	if resolveErr != nil {
		return sqlc.RunPublication{}, fmt.Errorf("publication: resolve default provider: %w", resolveErr)
	}
	ensureParams := sqlc.EnsurePublicationParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Provider:        string(defaultProvider.ID()),
		RemoteBranch:    "agentum/" + record.ID,
		PublishedCommit: record.ResultCommit.String,
	}
	if _, ensureErr := service.store.EnsurePublication(ctx, ensureParams); ensureErr != nil && !errors.Is(ensureErr, sql.ErrNoRows) {
		return sqlc.RunPublication{}, fmt.Errorf("publication: create row: %w", ensureErr)
	}
	created, readErr := service.store.GetPublicationForRun(ctx, sqlc.GetPublicationForRunParams{
		TenantID: record.TenantID, RunID: record.ID,
	})
	if readErr != nil {
		return sqlc.RunPublication{}, fmt.Errorf("publication: reload row: %w", readErr)
	}
	return created, nil
}

// readManifest returns the run's manifest body and seal metadata, or zero
// values when no manifest exists or it cannot be read. A zero body fails the
// delivery gate with checks_not_passed: no recorded evidence says the checks
// ran, and the gate's input is the record, never a guess.
func (service *Service) readManifest(ctx context.Context, record sqlc.Run) (manifest.Body, manifest.SealInfo) {
	if service.mfst == nil {
		return manifest.Body{}, manifest.SealInfo{}
	}
	body, sealInfo, _, err := service.mfst.Get(ctx, record.TenantID, record.ID)
	if err != nil {
		return manifest.Body{}, manifest.SealInfo{}
	}
	return body, sealInfo
}

// gateRefusal enforces the delivery gate: a publication leaves the host only
// for a commit the mandatory checks verified. Three facts must hold — the
// manifest carries a checks section, its mandatory set passed, and the commit
// it verified is the run's pinned result commit. A project that declares no
// checks passes (an empty mandatory set does not fail); the pull request body
// carries a line saying so, so the reader does not mistake silence for a
// cleared gate.
func gateRefusal(body manifest.Body, resultCommit string) (publish.ReasonCode, string, bool) {
	if body.Checks == nil {
		return publish.ReasonChecksNotPassed, "the manifest carries no checks section", true
	}
	if !body.Checks.MandatoryPassed {
		return publish.ReasonChecksNotPassed, "the mandatory checks did not pass", true
	}
	if resultCommit == "" {
		return publish.ReasonCommitMismatch, "no result commit is pinned at the gate", true
	}
	if body.Checks.Commit != resultCommit {
		return publish.ReasonCommitMismatch,
			fmt.Sprintf("checks verified commit %s, result commit is %s", body.Checks.Commit, resultCommit), true
	}
	return "", "", false
}

// assembleDelivery builds the provider's input from the durable rows: the
// run, its project, the frozen publication target, and the manifest. Every
// value is a scalar or a hash the provider re-reads from nothing. The plan
// and verdict references fill in when the pull request body is rendered; no
// provider reads them before that.
func (service *Service) assembleDelivery(ctx context.Context, record sqlc.Run, row sqlc.RunPublication, body manifest.Body, sealInfo manifest.SealInfo) publish.Delivery {
	delivery := publish.Delivery{
		Run: publish.RunRef{
			ID: record.ID, TenantID: record.TenantID, CreatorUserID: record.UserID,
			InputRevision: inputRevisionOf(body),
		},
		Target: publish.Target{
			Provider:     publish.ProviderID(row.Provider),
			Host:         row.TargetHost,
			Owner:        row.TargetOwner,
			Repository:   row.TargetRepository,
			BaseBranch:   row.BaseBranch,
			RemoteBranch: row.RemoteBranch,
		},
		Branch:       row.RemoteBranch,
		BaseCommit:   record.BaseCommit.String,
		ResultCommit: record.ResultCommit.String,
		Request: publish.RequestRef{
			Title: record.Title, Description: record.Description,
			Revision: inputRevisionOf(body),
		},
		Evidence: publish.EvidenceRef{
			Sealed:   sealInfo.SealedAt.Valid,
			Complete: body.IsEvidenceComplete(),
			Missing:  body.MissingSections(),
		},
	}
	if project, err := service.store.GetProject(ctx, sqlc.GetProjectParams{
		ID: record.ProjectID, TenantID: record.TenantID,
	}); err == nil {
		delivery.Project = publish.ProjectRef{
			ID: project.ID, RepoIdentity: project.RepoIdentity, CheckoutPath: record.CheckoutPath,
		}
	}
	if body.Checks != nil {
		seal := publish.ChecksSeal{
			Ran:              body.Checks.Ran,
			MandatoryPassed:  body.Checks.MandatoryPassed,
			Commit:           body.Checks.Commit,
			SetVersion:       body.Checks.SetVersion,
			RegistryRevision: body.Checks.RegistryRevision,
		}
		for _, result := range body.Checks.Results {
			seal.Checks = append(seal.Checks, publish.CheckSummary{
				Name: result.Name, Required: result.Required,
				Status: result.Status, DurationMs: result.DurationMs,
			})
		}
		delivery.Checks = seal
	}
	delivery.Review = publish.ReviewRef{FixCycles: maxInvocationCycle(body)}
	return delivery
}

// recordRefusal writes a refused attempt: the row gets the reason code, the
// provider's message, and the state — failed when a retry can clear the
// reason, blocked when it cannot. The event and the manifest evidence say
// the same thing. A pushed branch is kept: an attempt can push and still fail
// the pull request.
func (service *Service) recordRefusal(ctx context.Context, record sqlc.Run, code publish.ReasonCode, message string, result publish.Result) error {
	state := "blocked"
	if code.Retryable() {
		state = "failed"
	}
	updated, err := service.store.RecordPublicationFailure(ctx, sqlc.RecordPublicationFailureParams{
		TenantID: record.TenantID, RunID: record.ID,
		State:            state,
		LastErrorCode:    sql.NullString{String: string(code), Valid: true},
		LastErrorMessage: sql.NullString{String: message, Valid: message != ""},
		BranchPushedAt:   sql.NullTime{Time: time.Now().UTC(), Valid: result.BranchPushed},
	})
	if err != nil {
		return fmt.Errorf("publication: record failure: %w", err)
	}
	service.emit(ctx, record, EvPublicationFailed, map[string]any{
		"code": string(code), "state": state, "message": message,
	})
	service.recordEvidence(ctx, record, updated, nil)
	return nil
}

// recordEvidence writes the publication section into the manifest. On a
// sealed manifest the write becomes a correction — the sealed body is the
// snapshot of seal time, and the correction chain's newest link carries the
// current state. Best-effort: a write failure is logged, and the publication
// row remains the primary record either way.
func (service *Service) recordEvidence(ctx context.Context, record sqlc.Run, row sqlc.RunPublication, provider publish.Publisher) {
	if service.mfst == nil {
		return
	}
	patch := manifest.Body{Publication: &manifest.PublicationEvidence{
		Provider:        row.Provider,
		Host:            row.TargetHost,
		Owner:           row.TargetOwner,
		Repository:      row.TargetRepository,
		BaseBranch:      row.BaseBranch,
		RemoteBranch:    row.RemoteBranch,
		PublishedCommit: row.PublishedCommit,
		State:           row.State,
		PullRequest:     int(row.PrNumber.Int32),
		PullRequestURL:  row.PrUrl.String,
		Attempts:        int(row.Attempts),
		LastErrorCode:   row.LastErrorCode.String,
		At:              time.Now().UTC(),
	}}
	if provider != nil {
		descriptor := provider.Describe()
		patch.Publication.Profile = fmt.Sprintf("publisher-%s-%s", descriptor.ID, descriptor.ProviderVersion)
	}
	if err := service.mfst.AddEvidence(ctx, record.TenantID, record.ID, patch); err != nil {
		if !errors.Is(err, manifest.ErrSealed) {
			service.log.Warn("publication: record evidence", "run", record.ID, "error", err)
			return
		}
		if correctErr := service.mfst.Correct(ctx, record.TenantID, record.UserID, record.ID,
			correctionReasonPublication, patch); correctErr != nil {
			service.log.Warn("publication: correct evidence", "run", record.ID, "error", correctErr)
		}
	}
}

// emit appends a publication event. Every publication event carries the
// system actor: the orchestrator delivered, and the run author's name must
// not read as the hand that pushed.
func (service *Service) emit(ctx context.Context, record sqlc.Run, eventType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	if _, err := service.store.AppendEvent(ctx, sqlc.AppendEventParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: nullStringEvent(record.ID),
		Type: eventType, Payload: raw, Actor: string(authz.ActorSystem),
	}); err != nil {
		service.log.Warn("publication: emit event", "type", eventType, "run", record.ID, "error", err)
	}
}

// inputRevisionOf reads the request revision the manifest's input section
// recorded, empty when the section is absent.
func inputRevisionOf(body manifest.Body) string {
	if body.Input == nil {
		return ""
	}
	return body.Input.Revision
}

// maxInvocationCycle returns the highest fix-cycle any stage invocation
// reached — the number of review ⇄ fix cycles the run completed.
func maxInvocationCycle(body manifest.Body) int {
	highest := 0
	for _, invocation := range body.InvocationRecords() {
		if int(invocation.Cycle) > highest {
			highest = int(invocation.Cycle)
		}
	}
	return highest
}

// nullStringOr returns the inner string when Valid, else "".
func nullStringOr(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

// nullStringEvent adapts a run id to the nullable uuid shape AppendEvent
// expects. Present → the run id.
func nullStringEvent(runID string) sql.NullString {
	return sql.NullString{String: runID, Valid: runID != ""}
}
