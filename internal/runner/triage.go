package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

const routeDefaultPack = "backend-development"

const triagePrompt = `Choose the best pipeline pack for the task. Use only the task and catalog below. Do not edit source. Write result.json with schema_version "1", status "complete", and summary containing ONLY a JSON object with keys "pack" and "reason". The pack must be one catalog name. The reason must be a brief explanation specific to the task. If no choice is possible, use status "blocked".`

type routeCatalog interface {
	ListBuiltin(context.Context) ([]pack.Meta, error)
	ListProjectPacks(context.Context, string, string) ([]pack.ProjectPackEntry, error)
	ResolveForFloor(context.Context, string, string, string) (*pack.Resolved, error)
}

type routeChoice struct {
	Pack   string `json:"pack"`
	Reason string `json:"reason"`
}

type routeFailure struct {
	code    string
	message string
}

func (runner *Runner) selectRoute(ctx context.Context, record sqlc.Run, checkoutPath, baseCommit string) (sqlc.Run, error) {
	catalog, available := runner.packs.(routeCatalog)
	if !available {
		return record, nil
	}
	candidates, catalogErr := runner.routeCandidates(ctx, catalog, checkoutPath, baseCommit)
	choice := routeChoice{}
	failure := routeFailure{}
	invocationID := ""
	if catalogErr != nil {
		failure = routeFailure{code: "triage_catalog_error", message: catalogErr.Error()}
	} else {
		choice, invocationID, failure = runner.invokeTriage(ctx, record, candidates)
	}
	if failure.code == "" {
		if _, known := candidates[choice.Pack]; !known {
			failure = routeFailure{code: "triage_unknown_pack", message: fmt.Sprintf("triage returned %q, which is outside the route catalog", choice.Pack)}
		}
	}
	source := "triage"
	if failure.code != "" {
		source = "fallback"
		choice = routeChoice{Pack: routeDefaultPack, Reason: "The full route was selected because triage did not return a usable route."}
	}
	selected, err := runner.store.SelectRunRoute(ctx, sqlc.SelectRunRouteParams{
		ID: record.ID, TenantID: record.TenantID, PipelinePack: choice.Pack,
		RouteSource:       sql.NullString{String: source, Valid: true},
		RouteReason:       choice.Reason,
		RouteFallbackCode: failure.code, RouteFallbackMessage: failure.message,
		RouteTriageInvocationID: nullStr(invocationID),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return runner.store.GetRun(ctx, sqlc.GetRunParams{ID: record.ID, TenantID: record.TenantID})
	}
	return selected, err
}

func (runner *Runner) routeCandidates(ctx context.Context, catalog routeCatalog, checkoutPath, baseCommit string) (map[string]pack.Meta, error) {
	builtin, err := catalog.ListBuiltin(ctx)
	if err != nil {
		return nil, fmt.Errorf("list builtin routes: %w", err)
	}
	candidates := make(map[string]pack.Meta, len(builtin))
	for _, meta := range builtin {
		candidates[meta.Name] = meta
	}
	projectEntries, err := catalog.ListProjectPacks(ctx, checkoutPath, baseCommit)
	if err != nil {
		return nil, fmt.Errorf("list project routes: %w", err)
	}
	for _, entry := range projectEntries {
		resolved, resolveErr := catalog.ResolveForFloor(ctx, entry.Name, checkoutPath, baseCommit)
		if resolveErr != nil {
			return nil, fmt.Errorf("read project route %q: %w", entry.Name, resolveErr)
		}
		candidates[entry.Name] = resolved.Pack.Pack
	}
	return candidates, nil
}

func (runner *Runner) invokeTriage(ctx context.Context, record sqlc.Run, candidates map[string]pack.Meta) (routeChoice, string, routeFailure) {
	tempDir, err := os.MkdirTemp("", "agentum-triage-")
	if err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_adapter_error", message: err.Error()}
	}
	defer os.RemoveAll(tempDir)
	artifactDir := filepath.Join(tempDir, "artifacts")
	if err := os.Mkdir(artifactDir, 0o700); err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_adapter_error", message: err.Error()}
	}
	profile := withArtifactFloor(caps.Effective(caps.Input{
		Host:        runner.hostCaps,
		Pack:        []caps.Token{"fs.read", "git.read"},
		Stage:       []caps.Token{"fs.read", "git.read"},
		Role:        caps.RoleAnalyst,
		HardTimeout: runner.hardTimeout, IdleTimeout: runner.idleTimeout,
	}))
	selection, err := models.Resolve(runner.models, runner.adapter.Describe().DefaultTiers, "fast")
	if err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_model_error", message: err.Error()}
	}
	if err := selection.Options.SupportedBy(runner.adapter.Describe().ModelOptions); err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_model_error", message: err.Error()}
	}
	if runner.adapter.Describe().EnumeratesModels {
		if err := runner.adapter.Catalog(ctx).Validate(selection); err != nil {
			return routeChoice{}, "", routeFailure{code: "triage_model_error", message: err.Error()}
		}
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	var block strings.Builder
	block.WriteString("## Task\n\nTitle: ")
	block.WriteString(record.Title)
	block.WriteString("\n\nDescription:\n")
	block.WriteString(record.Description)
	block.WriteString("\n\n## Route catalog\n")
	for _, name := range names {
		block.WriteString("\n- ")
		block.WriteString(name)
		block.WriteString(": ")
		block.WriteString(strings.TrimSpace(candidates[name].Description))
	}
	block.WriteString(agent.ResultContractPreamble(artifactDir))
	invocation, err := runner.store.CreateStageInvocation(ctx, sqlc.CreateStageInvocationParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Stage: "triage", Sequence: 0, CapabilityProfile: toNullRaw(marshalProfile(profile)),
	})
	if err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_record_error", message: err.Error()}
	}
	runner.recordTriageEvidence(ctx, record, invocation, selection, block.String(), profile)
	stream, err := runner.adapter.Invoke(ctx, agent.Invocation{
		Workdir: tempDir, ArtifactDir: artifactDir, Prompt: triagePrompt,
		RoutingBlock: block.String(), Model: selection, Profile: profile,
	})
	if err != nil {
		runner.finalize(ctx, invocation, record, "", "adapter_error", nil)
		runner.closeInvocationEvidence(ctx, record, invocation.ID, "adapter_error", nil)
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_adapter_error", message: err.Error()}
	}
	var result *agent.Result
	var invokeErr error
	for event := range stream {
		if event.Kind == agent.EventResult {
			result = event.Result
		}
		if event.Kind == agent.EventError {
			invokeErr = event.Err
		}
	}
	if result == nil {
		message := "triage returned no result"
		if invokeErr != nil {
			message = invokeErr.Error()
		}
		runner.finalize(ctx, invocation, record, "", "adapter_error", nil)
		runner.closeInvocationEvidence(ctx, record, invocation.ID, "adapter_error", nil)
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_adapter_error", message: message}
	}
	runner.finalize(ctx, invocation, record, result.SessionID, "complete", result.ResultJSON)
	runner.closeInvocationEvidence(ctx, record, invocation.ID, "complete", &result.Telemetry)
	if result.Status != agent.StatusComplete {
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_refused", message: fmt.Sprintf("triage status was %q", result.Status)}
	}
	var choice routeChoice
	if err := json.Unmarshal([]byte(result.Summary), &choice); err != nil || strings.TrimSpace(choice.Pack) == "" || strings.TrimSpace(choice.Reason) == "" {
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_invalid_response", message: "triage summary must be a JSON object with nonempty pack and reason"}
	}
	choice.Pack = strings.TrimSpace(choice.Pack)
	choice.Reason = strings.TrimSpace(choice.Reason)
	if len(choice.Reason) > 2000 {
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_invalid_response", message: "triage reason exceeds 2000 bytes"}
	}
	return choice, invocation.ID, routeFailure{}
}

func (runner *Runner) recordTriageEvidence(ctx context.Context, record sqlc.Run, invocation sqlc.StageInvocation, selection models.Selection, block string, profile caps.Profile) {
	if runner.mfst == nil {
		return
	}
	descriptor := runner.adapter.Describe()
	readiness := runner.adapter.Probe(ctx)
	evidence := manifest.InvocationEvidence{
		InvocationID: invocation.ID, Stage: "triage", Sequence: invocation.Sequence,
		Adapter:      manifest.InvocationAdapter{ID: manifest.AdapterID(descriptor.ID), AdapterVersion: descriptor.AdapterVersion, RuntimeVersion: readiness.RuntimeVersion},
		Model:        selection,
		Prompt:       manifest.InvocationPrompt{StagePromptHash: hashForEvidence(triagePrompt), RenderedHash: hashForEvidence(triagePrompt + "\n\n" + block)},
		Capabilities: manifest.InvocationCaps{Role: string(profile.Source.Role), Profile: marshalProfile(profile)},
	}
	if err := runner.mfst.AddEvidence(ctx, record.TenantID, record.ID, manifest.Body{Invocations: []manifest.InvocationEvidence{evidence}}); err != nil {
		runner.log.Warn("record triage evidence", "run", record.ID, "error", err)
		runner.recordEvidenceGap(ctx, record, "invocations", "triage", err)
	}
}
