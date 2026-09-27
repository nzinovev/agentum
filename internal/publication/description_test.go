package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/sqlc-dev/pqtype"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/publish"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type memoryDescriptions struct {
	revisions       map[string]artifacts.Revision
	bodies          map[string][]byte
	readIDs         []string
	putErr          error
	readErr         error
	corruptRead     bool
	invalidRevision bool
	getErr          error
}

func newMemoryDescriptions() *memoryDescriptions {
	return &memoryDescriptions{revisions: make(map[string]artifacts.Revision), bodies: make(map[string][]byte)}
}

func (store *memoryDescriptions) Put(_ context.Context, params artifacts.PutParams) (artifacts.Revision, error) {
	if store.putErr != nil {
		return artifacts.Revision{}, store.putErr
	}
	if store.invalidRevision {
		return artifacts.Revision{}, nil
	}
	hash := artifacts.Hash(params.Bytes)
	revision := artifacts.Revision{ID: hash, TenantID: params.TenantID, RunID: params.RunID, Name: params.Name, Kind: params.Kind, ContentHash: hash, Actor: params.Actor}
	store.revisions[revision.ID] = revision
	store.bodies[revision.ID] = params.Bytes
	return revision, nil
}

func (store *memoryDescriptions) Get(_ context.Context, tenantID, revisionID string) (artifacts.Revision, error) {
	if store.getErr != nil {
		return artifacts.Revision{}, store.getErr
	}
	revision, exists := store.revisions[revisionID]
	if !exists || revision.TenantID != tenantID {
		return artifacts.Revision{}, sql.ErrNoRows
	}
	return revision, nil
}

func (store *memoryDescriptions) GetBytes(_ context.Context, _, revisionID string) ([]byte, error) {
	store.readIDs = append(store.readIDs, revisionID)
	if store.readErr != nil {
		return nil, store.readErr
	}
	if store.corruptRead {
		return []byte("corrupted stored bytes"), nil
	}
	body, exists := store.bodies[revisionID]
	if !exists {
		return nil, sql.ErrNoRows
	}
	return body, nil
}

func TestDescriptionFailuresPreventProviderWrites(test *testing.T) {
	for _, scenario := range []struct {
		name            string
		putErr          error
		readErr         error
		code            publish.ReasonCode
		state           string
		corruptRead     bool
		invalidRevision bool
	}{
		{name: "missing revision", invalidRevision: true, code: publish.ReasonDescriptionInvalid, state: "blocked"},
		{name: "integrity", corruptRead: true, code: publish.ReasonDescriptionInvalid, state: "blocked"},
		{name: "secret", putErr: artifacts.ErrSecretDetected, code: publish.ReasonSecretInDescription, state: "blocked"},
		{name: "store unavailable", putErr: errors.New("db unavailable"), code: publish.ReasonProviderError, state: "failed"},
		{name: "blob unavailable", readErr: errors.New("blob unavailable"), code: publish.ReasonProviderError, state: "failed"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{onPublish: func(publish.Delivery) { test.Fatal("provider called without stored description") }})
			store := newMemoryDescriptions()
			store.putErr, store.readErr, store.corruptRead, store.invalidRevision = scenario.putErr, scenario.readErr, scenario.corruptRead, scenario.invalidRevision
			harness.service.artifacts = store
			if err := harness.handle(test); err != nil {
				test.Fatal(err)
			}
			if harness.store.row.State != scenario.state || harness.store.row.LastErrorCode.String != string(scenario.code) {
				test.Fatalf("outcome = %+v", harness.store.row)
			}
		})
	}
}

func TestDeliveryMetadataUsesPinnedRevisions(test *testing.T) {
	harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{})
	store := newMemoryDescriptions()
	harness.service.artifacts = store
	approvedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store.revisions["approved-plan"] = artifacts.Revision{ID: "approved-plan", TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Kind: "plan_md", Name: "plan/plan.md", ContentHash: "plan-hash"}
	store.bodies["approved-plan"] = []byte("PRIVATE PLAN BODY")
	verdictBytes := []byte(`{"schema_version":"1","verdict":"approved","summary":"PRIVATE REVIEW SUMMARY","findings":[]}`)
	store.revisions["reviewed-verdict"] = artifacts.Revision{ID: "reviewed-verdict", TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Kind: "verdict_json", ContentHash: artifacts.Hash(verdictBytes)}
	store.bodies["reviewed-verdict"] = verdictBytes
	harness.store.approvals = []sqlc.RunApproval{{Decision: "approved", ArtifactRevisionID: sql.NullString{String: "approved-plan", Valid: true}, UserID: "approver", CreatedAt: approvedAt}}
	body := manifest.Body{Checks: &manifest.CheckEvidence{Ran: true, Results: []manifest.CheckResult{{Name: "unit", Required: true, Status: "passed", Stdout: "PRIVATE CHECK OUTPUT", Stderr: "PRIVATE CHECK ERROR"}}}, Schema: "2", Invocations: []manifest.InvocationEvidence{
		{InvocationID: "old-review", Sequence: 1, Cycle: 1, Capabilities: manifest.InvocationCaps{Role: "reviewer"}},
		{InvocationID: "fix", Stage: "fix", Sequence: 2, Cycle: 1, Capabilities: manifest.InvocationCaps{Role: "fixer"}, Telemetry: &manifest.InvocationTelemetry{}},
		{InvocationID: "fix-resume", Stage: "fix", Sequence: 3, Cycle: 1, Capabilities: manifest.InvocationCaps{Role: "fixer"}, Telemetry: &manifest.InvocationTelemetry{}},
		{InvocationID: "review", Sequence: 4, Cycle: 2, Capabilities: manifest.InvocationCaps{Role: "reviewer"}},
		{InvocationID: "unfinished-fix", Stage: "fix", Sequence: 5, Cycle: 2, Capabilities: manifest.InvocationCaps{Role: "fixer"}},
	}, Artifacts: &manifest.ArtifactEvidence{Outputs: []manifest.ArtifactRef{
		{Kind: "verdict_json", RevisionID: "stale-missing", InvocationID: "old-review"},
		{Kind: "verdict_json", RevisionID: "reviewed-verdict", ContentHash: artifacts.Hash(verdictBytes), InvocationID: "review"},
		{Kind: "diff", RevisionID: "unreadable-diff"},
	}}}
	harness.store.invocations = []sqlc.StageInvocation{finishedFix("fix-resume", "fix", 1, 3, "complete")}
	delivery, err := harness.service.assembleDelivery(test.Context(), harness.store.run, harness.store.row, body, manifest.SealInfo{})
	if err != nil {
		test.Fatal(err)
	}
	if delivery.Plan.RevisionID != "approved-plan" || delivery.Plan.ApprovedBy != "approver" || !delivery.Plan.ApprovedAt.Equal(approvedAt) || delivery.Review.Verdict != "approved" || delivery.Review.FixCycles != 1 {
		test.Fatalf("plan=%+v review=%+v", delivery.Plan, delivery.Review)
	}
	if len(store.readIDs) != 1 || store.readIDs[0] != "reviewed-verdict" {
		test.Fatalf("read bodies: %v", store.readIDs)
	}
	stored, err := harness.service.prepareDescription(test.Context(), delivery)
	if err != nil {
		test.Fatal(err)
	}
	if strings.Contains(stored.Text, "PRIVATE") {
		test.Fatal("source artifact body leaked")
	}
}

func finishedFix(id, stage string, cycle, sequence int32, status string) sqlc.StageInvocation {
	return sqlc.StageInvocation{ID: id, Stage: stage, Cycle: cycle, Sequence: sequence, FinishedAt: sql.NullTime{Valid: true}, Result: pqtype.NullRawMessage{Valid: true, RawMessage: json.RawMessage(`{"status":"` + status + `"}`)}}
}

func TestCompletedFixCycles(test *testing.T) {
	for _, scenario := range []struct {
		name string
		rows []sqlc.StageInvocation
		want int
	}{
		{"blocked", []sqlc.StageInvocation{finishedFix("fix-1", "fix", 0, 1, "blocked")}, 0},
		{"partial", []sqlc.StageInvocation{finishedFix("fix-1", "fix", 0, 1, "partial")}, 0},
		{"two stages one cycle", []sqlc.StageInvocation{finishedFix("fix-1", "fix-api", 0, 1, "complete"), finishedFix("fix-2", "fix-db", 0, 2, "complete")}, 1},
		{"unfinished cycle", []sqlc.StageInvocation{finishedFix("fix-1", "fix-api", 0, 1, "complete"), finishedFix("fix-2", "fix-db", 0, 2, "blocked")}, 0},
		{"resume", []sqlc.StageInvocation{finishedFix("fix-1", "fix", 0, 1, "blocked"), finishedFix("fix-2", "fix", 0, 2, "complete")}, 1},
		{"two cycles", []sqlc.StageInvocation{finishedFix("fix-1", "fix", 0, 1, "complete"), finishedFix("fix-2", "fix", 1, 2, "complete")}, 2},
		{"still running", []sqlc.StageInvocation{{ID: "fix-1", Stage: "fix"}}, 0},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			body := manifest.Body{}
			for _, row := range scenario.rows {
				body.Invocations = append(body.Invocations, manifest.InvocationEvidence{InvocationID: row.ID, Capabilities: manifest.InvocationCaps{Role: "fixer"}})
			}
			if count := completedFixCycles(body, scenario.rows); count != scenario.want {
				test.Fatalf("cycles=%d want=%d", count, scenario.want)
			}
		})
	}
}

func TestInvalidOptionalMetadataDoesNotBlockPublication(test *testing.T) {
	for _, content := range []string{`{"schema_version":"1","verdict":"other"}`, `{"schema_version":"2","verdict":"approved"}`, `{"schema_version":1,"verdict":"approved"}`, `{`, "hash mismatch", "missing revision"} {
		test.Run(content, func(test *testing.T) {
			called := false
			harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{result: publish.Result{BranchPushed: true, PullRequest: 42}, onPublish: func(delivery publish.Delivery) {
				called = true
				if delivery.Review.Verdict != "" || delivery.Plan.RevisionID != "" {
					test.Fatalf("invalid metadata published: %+v", delivery)
				}
			}})
			store := newMemoryDescriptions()
			harness.service.artifacts = store
			harness.store.approvals = []sqlc.RunApproval{{Decision: "approved", ArtifactRevisionID: sql.NullString{String: "missing-plan", Valid: true}}}
			hash := artifacts.Hash([]byte(content))
			if content == "hash mismatch" {
				hash = "wrong"
			}
			if content != "missing revision" {
				store.revisions["verdict"] = artifacts.Revision{ID: "verdict", TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Kind: "verdict_json", ContentHash: hash}
			}
			store.bodies["verdict"] = []byte(content)
			harness.mfst.body.Invocations = []manifest.InvocationEvidence{{InvocationID: "review", Capabilities: manifest.InvocationCaps{Role: "reviewer"}}}
			harness.mfst.body.Artifacts = &manifest.ArtifactEvidence{Outputs: []manifest.ArtifactRef{{Kind: "verdict_json", RevisionID: "verdict", InvocationID: "review", ContentHash: hash}}}
			if err := harness.handle(test); err != nil {
				test.Fatal(err)
			}
			if !called || harness.store.row.State != "published" {
				test.Fatalf("row=%+v", harness.store.row)
			}
		})
	}
}

func TestPlanApprovalSelectionIgnoresEditableKind(test *testing.T) {
	harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{})
	store := newMemoryDescriptions()
	harness.service.artifacts = store
	for _, id := range []string{"old", "new"} {
		store.revisions[id] = artifacts.Revision{ID: id, TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Name: "plan/plan.md", Kind: "file", ContentHash: "hash"}
	}
	older := sqlc.RunApproval{ID: "a", Decision: "approved", ArtifactRevisionID: sql.NullString{String: "old", Valid: true}, CreatedAt: time.Unix(1, 0)}
	newer := sqlc.RunApproval{ID: "b", Decision: "approved", ArtifactRevisionID: sql.NullString{String: "new", Valid: true}, CreatedAt: time.Unix(2, 0)}
	for _, approvals := range [][]sqlc.RunApproval{{newer, older}, {older, newer}} {
		harness.store.approvals = approvals
		delivery, err := harness.service.assembleDelivery(test.Context(), harness.store.run, harness.store.row, manifest.Body{}, manifest.SealInfo{})
		if err != nil || delivery.Plan.RevisionID != "new" {
			test.Fatalf("plan=%+v err=%v", delivery.Plan, err)
		}
	}
}

func TestOptionalMetadataStorageFailuresRemainRetryable(test *testing.T) {
	for _, scenario := range []string{"approval query", "revision query", "blob read"} {
		test.Run(scenario, func(test *testing.T) {
			harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{onPublish: func(publish.Delivery) { test.Fatal("provider called after storage failure") }})
			store := newMemoryDescriptions()
			harness.service.artifacts = store
			failure := errors.New("temporary storage failure")
			switch scenario {
			case "approval query":
				harness.store.approvalsErr = failure
			case "revision query":
				harness.store.approvals = []sqlc.RunApproval{{Decision: "approved", ArtifactRevisionID: sql.NullString{String: "plan", Valid: true}}}
				store.getErr = failure
			case "blob read":
				store.revisions["verdict"] = artifacts.Revision{ID: "verdict", TenantID: coordinatorTestTenant, RunID: coordinatorTestRun, Kind: "verdict_json", ContentHash: "hash"}
				store.readErr = failure
				harness.mfst.body.Invocations = []manifest.InvocationEvidence{{InvocationID: "review", Capabilities: manifest.InvocationCaps{Role: "reviewer"}}}
				harness.mfst.body.Artifacts = &manifest.ArtifactEvidence{Outputs: []manifest.ArtifactRef{{Kind: "verdict_json", InvocationID: "review", RevisionID: "verdict", ContentHash: "hash"}}}
			}
			if err := harness.handle(test); err != nil {
				test.Fatal(err)
			}
			if harness.store.row.State != "failed" || harness.store.row.LastErrorCode.String != string(publish.ReasonProviderError) {
				test.Fatalf("row=%+v", harness.store.row)
			}
		})
	}
}
