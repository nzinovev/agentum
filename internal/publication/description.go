package publication

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/publish"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// DescriptionStore gives the coordinator revision reads and scanned writes.
// The provider receives the resulting text without access to the store.
type DescriptionStore interface {
	Put(context.Context, artifacts.PutParams) (artifacts.Revision, error)
	Get(context.Context, string, string) (artifacts.Revision, error)
	GetBytes(context.Context, string, string) ([]byte, error)
}

// NewDescriptionStore uses prose rules for the orchestrator's rendered text.
// Stage artifacts continue to use the general artifact scanner.
func NewDescriptionStore(deps artifacts.SQLStoreDeps) *artifacts.SQLStore {
	deps.Scanner = artifacts.NewProseScanner(deps.ScanPolicy)
	return artifacts.NewSQLStore(deps)
}

func (service *Service) prepareDescription(ctx context.Context, delivery publish.Delivery) (publish.DescriptionRef, error) {
	if service.artifacts == nil {
		return publish.DescriptionRef{}, errors.New("publication: artifact store unavailable")
	}
	rendered, err := publish.RenderDescription(delivery)
	if err != nil {
		return publish.DescriptionRef{}, fmt.Errorf("render publication description: %w", err)
	}
	revision, err := service.artifacts.Put(ctx, artifacts.PutParams{
		TenantID: delivery.Run.TenantID, UserID: delivery.Run.CreatorUserID, RunID: delivery.Run.ID,
		Name: "publication/pr-description.md", Kind: "pr_description", Bytes: rendered, Actor: artifacts.ActorSystem,
	})
	if err != nil {
		return publish.DescriptionRef{}, fmt.Errorf("store publication description: %w", err)
	}
	if revision.ID == "" || revision.RunID != delivery.Run.ID || revision.TenantID != delivery.Run.TenantID {
		return publish.DescriptionRef{}, &publish.Refusal{Code: publish.ReasonDescriptionInvalid}
	}
	// Put can redact the rendered text. Only the immutable stored bytes may
	// reach the provider, including on a retry that reuses this revision.
	stored, err := service.artifacts.GetBytes(ctx, delivery.Run.TenantID, revision.ID)
	if err != nil {
		return publish.DescriptionRef{}, fmt.Errorf("read publication description: %w", err)
	}
	description := publish.DescriptionRef{Text: string(stored), RevisionID: revision.ID, ContentHash: revision.ContentHash}
	if err := description.Validate(); err != nil {
		return publish.DescriptionRef{}, err
	}
	return description, nil
}

func (service *Service) attachReviewMetadata(ctx context.Context, record sqlc.Run, body manifest.Body, delivery *publish.Delivery) error {
	if service.artifacts == nil {
		return errors.New("publication: artifact store unavailable")
	}
	approvals, err := service.store.ListApprovalsForRun(ctx, sqlc.ListApprovalsForRunParams{TenantID: record.TenantID, RunID: record.ID})
	if err != nil {
		return fmt.Errorf("read publication approvals: %w", err)
	}
	// Artifact-bound approvals are source-write plan approvals. Select the
	// newest decision explicitly; editable revision kinds do not identify it.
	var selected *sqlc.RunApproval
	for _, approval := range approvals {
		if approval.Decision == "approved" && approval.ArtifactRevisionID.Valid && approval.ArtifactRevisionID.String != "" &&
			(selected == nil || approval.CreatedAt.After(selected.CreatedAt) || approval.CreatedAt.Equal(selected.CreatedAt) && approval.ID > selected.ID) {
			selected = &approval
		}
	}
	if selected != nil {
		revision, err := service.artifacts.Get(ctx, record.TenantID, selected.ArtifactRevisionID.String)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, artifacts.ErrNoCurrentRevision) {
			return fmt.Errorf("read approved plan revision: %w", err)
		}
		if err == nil && revision.ID == selected.ArtifactRevisionID.String && revision.RunID == record.ID && revision.TenantID == record.TenantID && revision.Name != "" && revision.ContentHash != "" {
			delivery.Plan = publish.PlanRef{Name: revision.Name, RevisionID: revision.ID, ContentHash: revision.ContentHash,
				ApprovedBy: selected.UserID, ApprovedAt: selected.CreatedAt}
		}
	}

	var reviewer manifest.InvocationEvidence
	for _, invocation := range body.InvocationRecords() {
		if invocation.Capabilities.Role == "reviewer" && (reviewer.InvocationID == "" || invocation.Sequence > reviewer.Sequence) {
			reviewer = invocation
		}
	}
	invocations, err := service.store.ListStageInvocationsForRun(ctx, sqlc.ListStageInvocationsForRunParams{TenantID: record.TenantID, RunID: record.ID})
	if err != nil {
		return fmt.Errorf("read fix cycle outcomes: %w", err)
	}
	delivery.Review.FixCycles = completedFixCycles(body, invocations)
	if reviewer.InvocationID == "" || body.Artifacts == nil {
		return nil
	}
	for _, reference := range body.Artifacts.Outputs {
		if reference.Kind != "verdict_json" || reference.InvocationID != reviewer.InvocationID {
			continue
		}
		revision, err := service.artifacts.Get(ctx, record.TenantID, reference.RevisionID)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, artifacts.ErrNoCurrentRevision) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read reviewer revision: %w", err)
		}
		if revision.RunID != record.ID || revision.TenantID != record.TenantID || revision.Kind != "verdict_json" || revision.ContentHash != reference.ContentHash {
			return nil
		}
		stored, err := service.artifacts.GetBytes(ctx, record.TenantID, revision.ID)
		if err != nil {
			return fmt.Errorf("read reviewer verdict: %w", err)
		}
		if artifacts.Hash(stored) == revision.ContentHash {
			delivery.Review.Verdict = descriptionVerdict(stored)
		}
		break
	}
	return nil
}

const descriptionVerdictSchema = "1"

var descriptionVerdicts = []string{"approved", "changes_requested"}

func descriptionVerdict(stored []byte) string {
	var verdict struct {
		SchemaVersion string `json:"schema_version"`
		Verdict       string `json:"verdict"`
	}
	if json.Unmarshal(stored, &verdict) != nil || verdict.SchemaVersion != descriptionVerdictSchema {
		return ""
	}
	for _, known := range descriptionVerdicts {
		if verdict.Verdict == known {
			return known
		}
	}
	return ""
}

func completedFixCycles(body manifest.Body, invocations []sqlc.StageInvocation) int {
	fixers := make(map[string]bool)
	for _, invocation := range body.InvocationRecords() {
		if invocation.Capabilities.Role == "fixer" {
			fixers[invocation.InvocationID] = true
		}
	}
	cycles := make(map[int32]map[string]sqlc.StageInvocation)
	for _, invocation := range invocations {
		if !fixers[invocation.ID] {
			continue
		}
		if cycles[invocation.Cycle] == nil {
			cycles[invocation.Cycle] = make(map[string]sqlc.StageInvocation)
		}
		previous, exists := cycles[invocation.Cycle][invocation.Stage]
		if !exists || invocation.Sequence > previous.Sequence {
			cycles[invocation.Cycle][invocation.Stage] = invocation
		}
	}
	completed := 0
	for _, stages := range cycles {
		allComplete := true
		for _, invocation := range stages {
			var result struct {
				Status string `json:"status"`
			}
			if !invocation.FinishedAt.Valid || invocation.StopReason.String != "" || !invocation.Result.Valid || json.Unmarshal(invocation.Result.RawMessage, &result) != nil || result.Status != "complete" {
				allComplete = false
			}
		}
		if allComplete {
			completed++
		}
	}
	return completed
}
