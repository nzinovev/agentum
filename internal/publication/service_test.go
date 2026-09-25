package publication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"log/slog"

	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/publish"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The coordinator's contract: the delivery gate refuses an unverifiable
// commit before anything leaves the host, a provider refusal lands on the
// row as a recorded outcome while the job itself succeeds, and evidence
// written after the seal rides the correction chain instead of mutating the
// sealed body.

const (
	coordinatorTestTenant = "5cf4d0a7-3b6c-4e2a-9f4d-8b9c0d1e2f3a"
	coordinatorTestRun    = "6da5e1b8-4c7d-4f3b-8a5e-9c0d1e2f3a4b"
	coordinatorTestCommit = "1111111111111111111111111111111111111111"
)

// fakeStore records the coordinator's writes. claimLost scripts the lease
// race; failureWrite scripts the one error the job is allowed to fail on.
type fakeStore struct {
	mu sync.Mutex

	run    sqlc.Run
	row    sqlc.RunPublication
	hasRow bool

	events []string

	claimLost     bool
	failureWrite  error
	successWrites int
}

func (store *fakeStore) GetRun(context.Context, sqlc.GetRunParams) (sqlc.Run, error) {
	return store.run, nil
}

func (store *fakeStore) GetProject(context.Context, sqlc.GetProjectParams) (sqlc.Project, error) {
	return sqlc.Project{ID: "project-1"}, nil
}

func (store *fakeStore) GetPublicationForRun(context.Context, sqlc.GetPublicationForRunParams) (sqlc.RunPublication, error) {
	if !store.hasRow {
		return sqlc.RunPublication{}, sql.ErrNoRows
	}
	return store.row, nil
}

func (store *fakeStore) EnsurePublication(_ context.Context, arg sqlc.EnsurePublicationParams) (sqlc.RunPublication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.hasRow {
		return sqlc.RunPublication{}, sql.ErrNoRows
	}
	store.hasRow = true
	store.row = sqlc.RunPublication{
		TenantID: arg.TenantID, UserID: arg.UserID, RunID: arg.RunID,
		Provider: arg.Provider, RemoteBranch: arg.RemoteBranch,
		PublishedCommit: arg.PublishedCommit, State: "pending",
	}
	return store.row, nil
}

func (store *fakeStore) ClaimPublication(_ context.Context, _ sqlc.ClaimPublicationParams) (sqlc.RunPublication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimLost {
		return sqlc.RunPublication{}, sql.ErrNoRows
	}
	store.row.State = "publishing"
	store.row.Attempts++
	return store.row, nil
}

func (store *fakeStore) RecordPublicationSuccess(_ context.Context, arg sqlc.RecordPublicationSuccessParams) (sqlc.RunPublication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.successWrites++
	store.row.State = "published"
	store.row.PrNumber = arg.PrNumber
	store.row.PrUrl = arg.PrUrl
	return store.row, nil
}

func (store *fakeStore) RecordPublicationFailure(_ context.Context, arg sqlc.RecordPublicationFailureParams) (sqlc.RunPublication, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failureWrite != nil {
		return sqlc.RunPublication{}, store.failureWrite
	}
	store.row.State = arg.State
	store.row.LastErrorCode = arg.LastErrorCode
	store.row.LastErrorMessage = arg.LastErrorMessage
	return store.row, nil
}

func (store *fakeStore) AppendEvent(_ context.Context, arg sqlc.AppendEventParams) (sqlc.Event, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events = append(store.events, arg.Type)
	return sqlc.Event{}, nil
}

// eventCount counts the emitted events of one type.
func (store *fakeStore) eventCount(eventType string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for _, emitted := range store.events {
		if emitted == eventType {
			count++
		}
	}
	return count
}

// fakeManifest scripts the manifest reads and records the evidence writes.
type fakeManifest struct {
	body     manifest.Body
	sealed   bool
	added    []manifest.Body
	corrects []manifest.Body
	addErr   error
	getErr   error // injected: a read that did not complete
}

func (manifestFake *fakeManifest) Get(context.Context, string, string) (manifest.Body, manifest.SealInfo, []manifest.Correction, error) {
	if manifestFake.getErr != nil {
		return manifest.Body{}, manifest.SealInfo{}, nil, manifestFake.getErr
	}
	sealInfo := manifest.SealInfo{}
	if manifestFake.sealed {
		sealInfo.SealedAt = sql.NullTime{Valid: true}
	}
	return manifestFake.body, sealInfo, nil, nil
}

func (manifestFake *fakeManifest) AddEvidence(_ context.Context, _, _ string, patch manifest.Body) error {
	if manifestFake.addErr != nil {
		return manifestFake.addErr
	}
	manifestFake.added = append(manifestFake.added, patch)
	return nil
}

func (manifestFake *fakeManifest) Correct(_ context.Context, _, _, _, _ string, correction manifest.Body) error {
	manifestFake.corrects = append(manifestFake.corrects, correction)
	return nil
}

// scriptedPublisher serves a fixed outcome per provider id.
type scriptedPublisher struct {
	id      publish.ProviderID
	result  publish.Result
	refusal *publish.Refusal
}

func (publisher scriptedPublisher) ID() publish.ProviderID { return publisher.id }

func (publisher scriptedPublisher) Describe() publish.Descriptor {
	return publish.Descriptor{ID: publisher.id, ProviderVersion: "9.9.9"}
}

func (scriptedPublisher) Probe(context.Context) (publish.ProbeResult, error) {
	return publish.ProbeResult{Ready: true}, nil
}

func (publisher scriptedPublisher) Publish(context.Context, publish.Delivery) (publish.Result, error) {
	if publisher.refusal != nil {
		return publisher.result, publisher.refusal
	}
	return publisher.result, nil
}

// scriptedRegistry resolves every id to one scripted publisher, or fails
// with the scripted error.
type scriptedRegistry struct {
	publisher  publish.Publisher
	resolveErr error
}

func (registry scriptedRegistry) Resolve(publish.ProviderID) (publish.Publisher, error) {
	if registry.resolveErr != nil {
		return nil, registry.resolveErr
	}
	return registry.publisher, nil
}

// coordinatorHarness assembles a Service over the fakes with the run and row
// a delivered run carries: a pinned result commit and a pending publication.
type coordinatorHarness struct {
	service *Service
	store   *fakeStore
	mfst    *fakeManifest
}

func newCoordinatorHarness(t *testing.T, checks *manifest.CheckEvidence, provider publish.Publisher) *coordinatorHarness {
	t.Helper()
	body := manifest.Body{}
	if checks != nil {
		body.Checks = checks
	}
	store := &fakeStore{
		run: sqlc.Run{
			ID: coordinatorTestRun, TenantID: coordinatorTestTenant, UserID: "user-1",
			State: "awaiting_final_review", Title: "title", Description: "description",
			ResultCommit: sql.NullString{String: coordinatorTestCommit, Valid: true},
		},
		row: sqlc.RunPublication{
			TenantID: coordinatorTestTenant, RunID: coordinatorTestRun,
			Provider: "noop", RemoteBranch: "agentum/" + coordinatorTestRun,
			PublishedCommit: coordinatorTestCommit, State: "pending",
		},
		hasRow: true,
	}
	manifestFake := &fakeManifest{body: body}
	service := New(Deps{
		Store:    store,
		Manifest: manifestFake,
		Registry: scriptedRegistry{publisher: provider},
		Log:      slog.New(slog.DiscardHandler),
	})
	return &coordinatorHarness{service: service, store: store, mfst: manifestFake}
}

func (harness *coordinatorHarness) handle(t *testing.T) error {
	t.Helper()
	return harness.service.Handle(t.Context(), sqlc.Job{
		ID: 7, TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Kind: "publish",
	})
}

func passingChecks() *manifest.CheckEvidence {
	return &manifest.CheckEvidence{
		Ran: true, MandatoryPassed: true, Commit: coordinatorTestCommit, SetVersion: "v1",
	}
}

// TestDeliveryGateRefusesUnverifiedCommit: publication is refused while the
// checks verdict or its commit binding is missing — the checks table covers
// a failed mandatory set, an absent checks section, a mismatched verified
// commit, and an unpinned result commit. Each lands as blocked with its own
// code, and no provider call happens (the scripted publisher would fail the
// test on being reached... it cannot refuse a gate refusal, so the assertion
// is the outcome, not the call).
func TestDeliveryGateRefusesUnverifiedCommit(t *testing.T) {
	t.Parallel()
	otherCommit := "2222222222222222222222222222222222222222"
	tests := []struct {
		name        string
		checks      *manifest.CheckEvidence
		resultEmpty bool
		wantCode    string
	}{
		{
			name:     "no checks section",
			checks:   nil,
			wantCode: "checks_not_passed",
		},
		{
			name:     "mandatory set failed",
			checks:   &manifest.CheckEvidence{Ran: true, MandatoryPassed: false, Commit: coordinatorTestCommit},
			wantCode: "checks_not_passed",
		},
		{
			name:     "verified commit differs from the result commit",
			checks:   &manifest.CheckEvidence{Ran: true, MandatoryPassed: true, Commit: otherCommit},
			wantCode: "commit_mismatch",
		},
		{
			name:        "no result commit pinned",
			checks:      passingChecks(),
			resultEmpty: true,
			wantCode:    "commit_mismatch",
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			publisher := scriptedPublisher{id: "gate", result: publish.Result{
				BranchPushed: true, PullRequest: 9, PullRequestURL: "https://example.com/9",
			}}
			harness := newCoordinatorHarness(t, testCase.checks, publisher)
			if testCase.resultEmpty {
				harness.store.run.ResultCommit = sql.NullString{}
			}

			if err := harness.handle(t); err != nil {
				t.Fatalf("Handle returned %v; a gate refusal is an outcome, not a job failure", err)
			}
			if harness.store.row.State != "blocked" {
				t.Errorf("state = %q, want blocked — a gate refusal is not cleared by retrying", harness.store.row.State)
			}
			if harness.store.row.LastErrorCode.String != testCase.wantCode {
				t.Errorf("last_error_code = %q, want %q", harness.store.row.LastErrorCode.String, testCase.wantCode)
			}
			if harness.store.successWrites != 0 {
				t.Error("a gate refusal recorded a success")
			}
			if count := harness.store.eventCount(EvPublicationFailed); count != 1 {
				t.Errorf("publication_failed events = %d, want 1", count)
			}
			if count := harness.store.eventCount(EvPublicationStarted); count != 1 {
				t.Errorf("publication_started events = %d, want 1", count)
			}
		})
	}
}

// TestRefusedProviderOutcomeIsRecordedNotFailed: a provider refusal (here the
// placeholder's credentials_missing) lands on the row with its code and the
// failed state — retryable — while the job returns nil. The events and the
// manifest section say the same thing the row does.
func TestRefusedProviderOutcomeIsRecordedNotFailed(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{
		id:      "refusing",
		refusal: &publish.Refusal{Code: publish.ReasonCredentialsMissing, Message: "no token"},
	}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v; a provider refusal is a recorded outcome, not a job failure", err)
	}
	if harness.store.row.State != "failed" {
		t.Errorf("state = %q, want failed — credentials_missing is cleared by a fixed configuration", harness.store.row.State)
	}
	if harness.store.row.LastErrorCode.String != "credentials_missing" {
		t.Errorf("last_error_code = %q, want credentials_missing", harness.store.row.LastErrorCode.String)
	}
	if harness.store.row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", harness.store.row.Attempts)
	}
	if count := harness.store.eventCount(EvPublicationFailed); count != 1 {
		t.Errorf("publication_failed events = %d, want 1", count)
	}
	if len(harness.mfst.added) != 1 || harness.mfst.added[0].Publication == nil {
		t.Fatalf("evidence writes = %d, want one publication section", len(harness.mfst.added))
	}
	if evidence := harness.mfst.added[0].Publication; evidence.State != "failed" || evidence.LastErrorCode != "credentials_missing" {
		t.Errorf("evidence = %+v; want failed/credentials_missing", evidence)
	}
}

// TestSuccessfulPublicationRecordsRowEventAndEvidence: a completed
// publication writes the published row, the run.published event with the
// pull request identity, and the manifest section carrying the provider
// profile label.
func TestSuccessfulPublicationRecordsRowEventAndEvidence(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{id: "github", result: publish.Result{
		BranchPushed: true, PullRequest: 42,
		PullRequestURL: "https://github.com/example/repo/pull/42", PullRequestState: "open",
	}}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if harness.store.row.State != "published" {
		t.Errorf("state = %q, want published", harness.store.row.State)
	}
	if harness.store.row.PrNumber.Int32 != 42 {
		t.Errorf("pr_number = %d, want 42", harness.store.row.PrNumber.Int32)
	}
	if count := harness.store.eventCount(EvPublished); count != 1 {
		t.Errorf("published events = %d, want 1", count)
	}
	if count := harness.store.eventCount(EvPublicationFailed); count != 0 {
		t.Errorf("publication_failed events = %d, want 0", count)
	}
	if len(harness.mfst.added) != 1 {
		t.Fatalf("evidence writes = %d, want 1", len(harness.mfst.added))
	}
	evidence := harness.mfst.added[0].Publication
	if evidence == nil || evidence.State != "published" || evidence.PullRequest != 42 {
		t.Fatalf("evidence = %+v; want published with pull request 42", evidence)
	}
	if evidence.Profile != "publisher-github-9.9.9" {
		t.Errorf("profile = %q, want publisher-github-9.9.9", evidence.Profile)
	}
}

// TestChecksNotDeclaredStillPublishes: a project with no declared checks
// passes the gate — the pull request body carries the fact in a line of its
// own, and the delivery itself is not blocked on a registry the project
// never wrote.
func TestChecksNotDeclaredStillPublishes(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{id: "github", result: publish.Result{BranchPushed: true, PullRequest: 3}}
	// Ran=false, MandatoryPassed=true: the empty mandatory set does not fail.
	harness := newCoordinatorHarness(t, &manifest.CheckEvidence{
		Ran: false, MandatoryPassed: true, Commit: coordinatorTestCommit,
	}, publisher)

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if harness.store.row.State != "published" {
		t.Errorf("state = %q, want published — an empty mandatory set does not block delivery", harness.store.row.State)
	}
	if harness.store.row.LastErrorCode.Valid {
		t.Errorf("last_error_code = %q, want none", harness.store.row.LastErrorCode.String)
	}
}

// TestSealedManifestEvidenceRidesTheCorrectionChain: after the seal,
// AddEvidence is refused and the publication section is written as a
// correction with the publication_updated reason — the sealed body stays the
// snapshot of seal time.
func TestSealedManifestEvidenceRidesTheCorrectionChain(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{
		id:      "github",
		refusal: &publish.Refusal{Code: publish.ReasonNetworkUnreachable, Message: "dial timeout"},
	}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)
	harness.mfst.sealed = true
	harness.mfst.addErr = manifest.ErrSealed

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if len(harness.mfst.corrects) != 1 {
		t.Fatalf("corrections = %d, want 1", len(harness.mfst.corrects))
	}
	corrected := harness.mfst.corrects[0]
	if corrected.Publication == nil || corrected.Publication.State != "failed" {
		t.Errorf("correction publication = %+v; want failed", corrected.Publication)
	}
}

// TestLeaseLossEndsTheJobQuietly: a worker that loses the lease writes
// nothing — no events, no outcome — because the lease holder is delivering.
func TestLeaseLossEndsTheJobQuietly(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{id: "github", result: publish.Result{BranchPushed: true, PullRequest: 5}}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)
	harness.store.claimLost = true

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v; losing the lease is not a job failure", err)
	}
	if len(harness.store.events) != 0 {
		t.Errorf("events = %v, want none", harness.store.events)
	}
	if harness.store.row.State != "pending" {
		t.Errorf("state = %q, want pending — the lease holder owns the attempt", harness.store.row.State)
	}
}

// TestMissingRowIsCreatedThenHandled: a publish job for a run whose gate ran
// while publication was disabled creates the row under the default provider
// and proceeds — the explicit retry is the row's creator.
func TestMissingRowIsCreatedThenHandled(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{id: "github", result: publish.Result{BranchPushed: true, PullRequest: 8}}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)
	harness.store.hasRow = false
	harness.store.row = sqlc.RunPublication{}

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if !harness.store.hasRow {
		t.Fatal("row was not created")
	}
	if harness.store.row.Provider != "github" {
		t.Errorf("created row provider = %q, want the default registry entry's id", harness.store.row.Provider)
	}
	if harness.store.row.State != "published" {
		t.Errorf("state = %q, want published", harness.store.row.State)
	}
}

// TestRowWriteFailureFailsTheJob: the one error the queue sees is a failure
// to write the publication row — there the retry and poison bound apply.
func TestRowWriteFailureFailsTheJob(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{
		id:      "github",
		refusal: &publish.Refusal{Code: publish.ReasonNetworkUnreachable, Message: "dial timeout"},
	}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)
	harness.store.failureWrite = fmt.Errorf("db gone")

	err := harness.handle(t)
	if err == nil {
		t.Fatal("Handle returned nil; a row-write failure must fail the job")
	}
	if !errors.Is(err, harness.store.failureWrite) {
		t.Errorf("error = %v, want the row-write failure wrapped", err)
	}
}

// TestManifestReadFailureIsATransientRefusalNotAGateVerdict: a manifest read
// that does not complete is a failure on this side, not a fact about the run.
// It lands as provider_error in the failed state — the retry re-reads — and
// never as checks_not_passed, which would name the run's evidence and block.
func TestManifestReadFailureIsATransientRefusalNotAGateVerdict(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{id: "github", result: publish.Result{BranchPushed: true, PullRequest: 4}}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)
	harness.mfst.getErr = errors.New("db down")

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v; a manifest read failure is an outcome, not a job failure", err)
	}
	if harness.store.row.State != "failed" {
		t.Errorf("state = %q, want failed — the attempt is re-run, not reconfigured", harness.store.row.State)
	}
	if harness.store.row.LastErrorCode.String != "provider_error" {
		t.Errorf("last_error_code = %q, want provider_error", harness.store.row.LastErrorCode.String)
	}
	if count := harness.store.eventCount(EvPublicationFailed); count != 1 {
		t.Errorf("publication_failed events = %d, want 1", count)
	}
}

// TestUnresolvedProviderIdIsBlocked: a provider id the registry cannot
// resolve is blocked, not failed — the row's id is frozen, so a retry with
// the configuration unfixed resolves the same nothing, and the next action
// is a corrected configuration, not another attempt.
func TestUnresolvedProviderIdIsBlocked(t *testing.T) {
	t.Parallel()
	harness := newCoordinatorHarness(t, passingChecks(), scriptedPublisher{id: "github"})
	harness.service = New(Deps{
		Store: harness.store, Manifest: harness.mfst,
		Registry: scriptedRegistry{resolveErr: errors.New(`unknown publication provider "gihub" (known: github, noop)`)},
		Log:      slog.New(slog.DiscardHandler),
	})

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if harness.store.row.State != "blocked" {
		t.Errorf("state = %q, want blocked — a retry cannot clear an unresolved id", harness.store.row.State)
	}
	if harness.store.row.LastErrorCode.String != "provider_unknown" {
		t.Errorf("last_error_code = %q, want provider_unknown", harness.store.row.LastErrorCode.String)
	}
	if !strings.Contains(harness.store.row.LastErrorMessage.String, "gihub") {
		t.Errorf("last_error_message = %q; want the registry's own text naming the id", harness.store.row.LastErrorMessage.String)
	}
}

// TestRefusedAttemptKeepsTheProviderProfileLabel: a failed attempt still
// names the boundary it ran under — the label lets a reviewer reconstruct
// which provider version refused, and a refusal is exactly when that
// question comes up.
func TestRefusedAttemptKeepsTheProviderProfileLabel(t *testing.T) {
	t.Parallel()
	publisher := scriptedPublisher{
		id:      "github",
		refusal: &publish.Refusal{Code: publish.ReasonNetworkUnreachable, Message: "dial timeout"},
	}
	harness := newCoordinatorHarness(t, passingChecks(), publisher)

	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if len(harness.mfst.added) != 1 || harness.mfst.added[0].Publication == nil {
		t.Fatalf("evidence writes = %d, want one publication section", len(harness.mfst.added))
	}
	if label := harness.mfst.added[0].Publication.Profile; label != "publisher-github-9.9.9" {
		t.Errorf("profile = %q, want publisher-github-9.9.9 on the refusal path too", label)
	}
}

// TestGateRefusalCarriesNoProviderProfileLabel: a refusal that happened
// before any provider was resolved carries no label — nothing ran under a
// boundary, and an empty label says so instead of guessing one.
func TestGateRefusalCarriesNoProviderProfileLabel(t *testing.T) {
	t.Parallel()
	harness := newCoordinatorHarness(t, nil, scriptedPublisher{id: "github"})
	if err := harness.handle(t); err != nil {
		t.Fatalf("Handle returned %v", err)
	}
	if len(harness.mfst.added) != 1 || harness.mfst.added[0].Publication == nil {
		t.Fatalf("evidence writes = %d, want one publication section", len(harness.mfst.added))
	}
	if label := harness.mfst.added[0].Publication.Profile; label != "" {
		t.Errorf("profile = %q, want empty — no provider was reached", label)
	}
}
