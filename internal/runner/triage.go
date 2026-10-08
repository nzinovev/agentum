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
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/policy"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

const routeDefaultPack = pack.DefaultPipelinePack

const defaultTriageCloseTimeout = 10 * time.Second

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

// selectRoute records one route choice after the read-only triage attempt.
// The stored choice is authoritative for every later worker and recovery.
func (runner *Runner) selectRoute(ctx context.Context, record sqlc.Run, checkoutPath, baseCommit string) (sqlc.Run, error) {
	if err := runner.closeInterruptedTriage(ctx, record); err != nil {
		return record, err
	}
	catalog, available := runner.packs.(routeCatalog)
	if !available {
		return record, nil
	}
	project, err := runner.store.GetProject(ctx, sqlc.GetProjectParams{ID: record.ProjectID, TenantID: record.TenantID})
	if err != nil {
		return record, fmt.Errorf("load project for triage: %w", err)
	}
	candidates, catalogErr := runner.routeCandidates(ctx, record, project, catalog, checkoutPath, baseCommit)
	choice := routeChoice{}
	failure := routeFailure{}
	invocationID := ""
	if catalogErr != nil {
		failure = routeFailure{code: "triage_catalog_error", message: catalogErr.Error()}
	} else {
		choice, invocationID, failure = runner.invokeTriage(ctx, record, project.Name, candidates)
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
	if err := ctx.Err(); err != nil {
		return record, err
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

// closeInterruptedTriage records an interrupted attempt before a recovered
// worker invokes triage again. The prior attempt remains visible in evidence.
func (runner *Runner) closeInterruptedTriage(ctx context.Context, record sqlc.Run) error {
	unfinished, err := runner.store.ListUnfinishedTriageInvocationsForRun(ctx, sqlc.ListUnfinishedTriageInvocationsForRunParams{
		RunID: record.ID, TenantID: record.TenantID,
	})
	if err != nil {
		return fmt.Errorf("list interrupted triage attempts: %w", err)
	}
	for _, invocation := range unfinished {
		if err := runner.store.FinishStageInvocation(ctx, sqlc.FinishStageInvocationParams{
			ID: invocation.ID, TenantID: record.TenantID,
			SessionID: invocation.SessionID, StopReason: nullStr("interrupted"),
		}); err != nil {
			return fmt.Errorf("close interrupted triage invocation %s: %w", invocation.ID, err)
		}
		runner.closeInvocationEvidence(ctx, record, invocation.ID, "interrupted", nil)
	}
	return nil
}

// routeCandidates omits broken project packs independently so one bad layer
// cannot disable triage for the other routes.
func (runner *Runner) routeCandidates(ctx context.Context, record sqlc.Run, project sqlc.Project, catalog routeCatalog, checkoutPath, baseCommit string) (map[string]pack.Meta, error) {
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
	if len(projectEntries) == 0 {
		return candidates, nil
	}
	record.BaseCommit = sql.NullString{String: baseCommit, Valid: true}
	registry, err := runner.loadRegistryAtBaseCommit(ctx, record, project, checkoutPath)
	if err != nil {
		return nil, fmt.Errorf("load project checks for route catalog: %w", err)
	}
	for _, entry := range projectEntries {
		resolved, resolveErr := catalog.ResolveForFloor(ctx, entry.Name, checkoutPath, baseCommit)
		if resolveErr != nil {
			delete(candidates, entry.Name)
			runner.log.Warn("skip unreadable project route", "route", entry.Name, "error", resolveErr)
			continue
		}
		violations := runner.floor.Check(policy.Target{
			Pack: resolved.Pack, Origin: resolved.Origin, FieldOrigins: resolved.FieldOrigins,
			HostCaps: runner.hostCaps, Registry: registry,
		})
		validationErr := resolved.ValidateErr
		if validationErr == nil {
			validationErr = resolved.Pack.Validate()
		}
		if validationErr != nil || len(violations) != 0 {
			delete(candidates, entry.Name)
			runner.log.Warn("skip invalid project route", "route", entry.Name, "validation_error", validationErr, "floor_violations", violations)
			continue
		}
		candidates[entry.Name] = resolved.Pack.Pack
	}
	return candidates, nil
}

// invokeTriage runs the normal adapter under a read-only capability profile.
// The routing template is the only path for title and description to the agent.
func (runner *Runner) invokeTriage(ctx context.Context, record sqlc.Run, projectName string, candidates map[string]pack.Meta) (routeChoice, string, routeFailure) {
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
	selection, err := models.ResolveTriage(runner.models, runner.adapter.Describe().DefaultTiers)
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
	routes := make([]routing.RouteRef, 0, len(names))
	for _, name := range names {
		routes = append(routes, routing.RouteRef{Name: name, Description: strings.Join(strings.Fields(candidates[name].Description), " ")})
	}
	block := routing.Render(routing.Block{
		RunID: record.ID, ProjectName: projectName, Stage: "triage", Gate: "auto",
		ArtifactDir: artifactDir, Title: record.Title, Description: record.Description,
		RouteCatalog: routes,
	})
	invocation, err := runner.store.CreateStageInvocation(ctx, sqlc.CreateStageInvocationParams{
		TenantID: record.TenantID, UserID: record.UserID, RunID: record.ID,
		Stage: "triage", Sequence: 0, Kind: "triage", CapabilityProfile: toNullRaw(marshalProfile(profile)),
	})
	if err != nil {
		return routeChoice{}, "", routeFailure{code: "triage_record_error", message: err.Error()}
	}
	finish := func(sessionID, stopReason string, resultJSON *agent.ResultJSON, telemetry *agent.Telemetry) {
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), runner.triageCloseTimeout)
		defer finishCancel()
		// A nil *agent.ResultJSON inside an interface is not a nil interface;
		// finalize would store the JSON literal null instead of leaving the
		// column empty.
		var stored any
		if resultJSON != nil {
			stored = resultJSON
		}
		runner.finalize(finishCtx, invocation, record, sessionID, stopReason, stored)
		runner.closeInvocationEvidence(finishCtx, record, invocation.ID, stopReason, telemetry)
	}
	runner.recordTriageEvidence(ctx, record, invocation, selection, block, profile)
	stream, err := runner.adapter.Invoke(ctx, agent.Invocation{
		Workdir: tempDir, ArtifactDir: artifactDir, Prompt: triagePrompt,
		RoutingBlock: block, Model: selection, Profile: profile,
	})
	if err != nil {
		finish("", "adapter_error", nil, nil)
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
		stopReason := "adapter_error"
		if ctx.Err() != nil {
			stopReason = "cancelled"
		}
		finish("", stopReason, nil, nil)
		return routeChoice{}, invocation.ID, routeFailure{code: "triage_adapter_error", message: message}
	}
	stopReason := string(result.Status)
	finish(result.SessionID, stopReason, &result.ResultJSON, &result.Telemetry)
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

// recordTriageEvidence opens the same manifest record used by stage attempts.
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
