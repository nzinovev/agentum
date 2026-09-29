package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/instructions"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/taskinput"
)

// prepareProjectContext wires the project-context channel for the run (ADR
// 0002): it reads .agentum.yaml from base_commit once (the agent-immutability
// seam), pins the declared + auto-injected instruction files, probes the
// runtime's skills, and resolves the check set for rendering. Everything is
// stashed on stageRun so the loop and invokeStage share one source of truth.
//
// A failure to pin or probe degrades evidence, it does not fail the run: the
// run still proceeds with whatever context was pinnable, and the gaps are
// recorded. Only a malformed .agentum.yaml that fails to parse, or a check-set
// resolution error, returns an error — drive turns those into failRun. The
// missing-file case is (nil, nil) from loadRegistryAtBaseCommit, not an error.
func (runner *Runner) prepareProjectContext(ctx context.Context, run *stageRun, baseCommit string) error {
	registry, registryErr := runner.loadRegistryAtBaseCommit(ctx, *run)
	if registryErr != nil {
		// A parse error is fatal (the project's config is unreadable). A missing
		// file is not — loadRegistryAtBaseCommit returns (nil, nil) for that, so
		// this branch is a genuine malformed-config failure.
		return fmt.Errorf("project context: load registry: %w", registryErr)
	}

	// Resolve the check set ONCE for rendering. enforceProjectChecks keeps its
	// own independent load+resolve at the delivery boundary (D8 / PR #23): the
	// cached set here never reaches the gate, only the routing block. The strict
	// overrides decode lives here: after the API boundary guarantees well-formed
	// overrides, a malformed column is an invariant break and this error reaches
	// failRun through drive.
	runOverrides, overridesErr := taskinput.ParseOverrides(run.record.Overrides)
	if overridesErr != nil {
		return fmt.Errorf("project context: parse run overrides: %w", overridesErr)
	}
	packRequests := packCheckRequests(run.runPack)
	runRequests := runCheckRequests(runOverrides)
	set, resolveErr := checks.Resolve(registry, packRequests, runRequests)
	if resolveErr != nil {
		return fmt.Errorf("project context: resolve checks: %w", resolveErr)
	}
	run.resolvedChecks = resolvedChecksForRender(set)

	// Probe the runtime's non-prompt context (auto-injected instructions +
	// enumerated skills). Comma-ok on the adapter so a typed-nil adapter is not
	// a trap, and an adapter with no prober records "unsupported" rather than
	// failing. AutoInstructions is the adapter-owned baseline that also feeds
	// the pin's auto list — sourced from the report so the two cannot drift.
	report := agent.ContextReport{SkillsProbe: agent.ContextProbeUnsupported}
	if prober, ok := runner.adapter.(agent.ContextProber); ok {
		probeInv := agent.Invocation{
			Workdir:     run.worktree.Root,
			ArtifactDir: run.worktree.Root, // unused by the probe; a non-empty stand-in
		}
		probed, probeErr := prober.ProbeContext(ctx, probeInv)
		if probeErr != nil {
			// ProbeContext returns a report with a failed label, not an error,
			// except on a programming fault. Treat an error as a failed probe.
			report = agent.ContextReport{
				AutoInstructions: runner.autoInstructionBaseline(),
				SkillsProbe:      agent.ContextProbeFailedPrefix + "error",
				SkillsError:      probeErr.Error(),
			}
		} else {
			report = probed
		}
	}
	if len(report.AutoInstructions) == 0 {
		// Defensive: a prober that returned no baseline still gets the
		// declared one, so the pin's auto list is never empty.
		report.AutoInstructions = runner.autoInstructionBaseline()
	}
	run.contextReport = report

	// Pin the instruction set: declared (from the registry) ∪ auto (from the
	// probe's baseline), bytes read from base_commit via the worktree manager
	// (which satisfies instructions.Reader through FileAtCommit).
	declared := registry.InstructionPaths()
	pinned, pinErr := instructions.Pin(ctx, runner.wt, checkoutPathOf(run.record, run.project), baseCommit, declared, report.AutoInstructions)
	if pinErr != nil {
		// A read failure (not a missing file) is recorded as an evidence gap;
		// the run proceeds with whatever was pinnable. A missing file is already
		// captured per-entry as MissingAtCommit.
		runner.recordEvidenceGap(ctx, run.record, "context.instructions", "", pinErr)
	}
	run.instructionFiles = pinned
	return nil
}

// resolvedChecksForRender maps the resolved check set onto the routing block's
// plain CheckRef shape (no checks-package import in routing). Each Item already
// carries its project-owned Definition (the only source of a command), so no
// registry lookup is needed. The set is rendering-only; the delivery gate
// re-resolves independently (D8 / PR #23).
func resolvedChecksForRender(set *checks.Set) []routing.CheckRef {
	if set == nil {
		return nil
	}
	refs := make([]routing.CheckRef, 0, len(set.Items))
	for _, item := range set.Items {
		refs = append(refs, routing.CheckRef{
			Name:        item.Definition.Name,
			Command:     item.Definition.Command,
			Description: item.Definition.Description,
			Required:    item.Required,
		})
	}
	return refs
}

// restoreInstructions is ADR 0002 D4 layer 2: before every stage invocation,
// compare each instruction path's worktree content to its pinned bytes and
// rewrite/remove the drift. The edit deny (D4 layer 1) does not cover bash, and
// bash is an acknowledged escape path — so the pin is only worth something if
// the worktree copy is controlled at the source.
//
// This runs STRICTLY BEFORE the invocation, never between the isClean sample
// and the checkpoint commit (the load-bearing ordering in processStage). A
// restore that fails to write is a run failure, matching the precedent that a
// broken invariant at the delivery boundary fails rather than proceeds on a
// claim we cannot stand behind (ErrDirtyTreeAtDeliveryBoundary).
//
// Restorations are recorded as manifest evidence and as
// EvInstructionsRestored events; orchestrator-authored rewrites land in the
// next checkpoint commit and show in the delivery diff as reverts — the tamper
// and its reversal are both in the git lineage.
func (runner *Runner) restoreInstructions(ctx context.Context, run stageRun, stageID string) error {
	if len(run.instructionFiles) == 0 {
		return nil
	}
	plan := instructions.Verify(run.worktree.Root, run.instructionFiles)
	if len(plan) == 0 {
		return nil
	}
	done, execErr := instructions.Execute(plan, run.instructionFiles, run.worktree.Root)
	if execErr != nil {
		// A restore IO error is a run failure — we cannot stand behind a run
		// whose reviewer might be reading rewritten rules.
		return fmt.Errorf("restore instruction files for stage %q: %w", stageID, execErr)
	}
	if len(done) == 0 {
		return nil
	}
	restorations := make([]manifest.InstructionRestoration, 0, len(done))
	now := time.Now().UTC()
	for _, restoration := range done {
		action := "restored"
		if restoration.Action == instructions.ActionRemove {
			action = "removed"
		}
		restorations = append(restorations, manifest.InstructionRestoration{
			Stage:     stageID,
			Path:      restoration.Path,
			Action:    action,
			FoundHash: restoration.FoundHash,
			At:        now,
		})
		runner.emit(ctx, run.record, EvInstructionsRestored, map[string]any{
			"stage":      stageID,
			"path":       restoration.Path,
			"action":     action,
			"found_hash": restoration.FoundHash,
		})
	}
	runner.recordRestorationEvidence(ctx, run, restorations)
	return nil
}

// contextEvidenceSection builds the context section for one stage: the
// instruction refs (from the pinned set), the enumerated skills (from the
// probe), the skills probe label, and the paths declared but absent at
// base_commit. Pure — the write happens in completeStageEvidence, folded into
// the one manifest transaction that closes a successful attempt.
//
// The section is built on every successful stage, even when it is empty: a
// project with no AGENTS.md and no skills gets a section that says exactly
// that, which is what lets an empty project still seal evidence_complete.
// Merge is append-unique, so a re-pin under a retry collapses and a skill set
// that changed between jobs surfaces.
func contextEvidenceSection(run stageRun) *manifest.ContextEvidence {
	section := &manifest.ContextEvidence{
		SkillsProbe:  run.contextReport.SkillsProbe,
		Instructions: instructionRefsForEvidence(run.instructionFiles),
		Skills:       skillRefsForManifest(run.contextReport.Skills),
	}
	for _, file := range run.instructionFiles {
		if file.MissingAtCommit {
			section.Missing = append(section.Missing, file.RepoPath)
		}
	}
	return section
}

// recordRestorationEvidence appends the tamper-reversal history to the
// manifest's context section. Merge is append-unique by (stage, path, at).
func (runner *Runner) recordRestorationEvidence(ctx context.Context, run stageRun, restorations []manifest.InstructionRestoration) {
	if runner.mfst == nil || len(restorations) == 0 {
		return
	}
	patch := manifest.Body{Context: &manifest.ContextEvidence{Restorations: restorations}}
	if err := runner.mfst.AddEvidence(ctx, run.record.TenantID, run.record.ID, patch); err != nil {
		if !errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("record restoration evidence", "run", run.record.ID, "error", err)
		}
	}
}

// instructionRefsForEvidence maps the pinned instruction files to the manifest
// evidence shape. SourceHash is over the original base_commit bytes (identity);
// DeliveredHash is over the post-truncate bytes the model saw.
func instructionRefsForEvidence(files []instructions.File) []manifest.InstructionRef {
	refs := make([]manifest.InstructionRef, 0, len(files))
	for _, file := range files {
		if file.MissingAtCommit {
			// A missing path is recorded in the section's Missing list, not as
			// an instruction ref with empty hashes.
			continue
		}
		refs = append(refs, manifest.InstructionRef{
			Path:           file.RepoPath,
			Source:         string(file.Source),
			SourceHash:     file.SourceHash,
			DeliveredHash:  file.DeliveredHash,
			DeliveredBytes: file.DeliveredBytes,
			Truncated:      file.Truncated,
		})
	}
	return refs
}

// skillRefsForManifest maps the agent package's SkillRef to the manifest-local
// shape (the manifest never imports the agent package).
func skillRefsForManifest(skills []agent.SkillRef) []manifest.SkillRef {
	out := make([]manifest.SkillRef, 0, len(skills))
	for _, skill := range skills {
		out = append(out, manifest.SkillRef{
			Name:        skill.Name,
			Location:    skill.Location,
			Description: skill.Description,
			Hash:        skill.Hash,
			Bytes:       skill.Bytes,
		})
	}
	return out
}

// probeFailed reports whether a skills probe label is a failure prefix
// ("failed: ..."). Mirrors the manifest's startsWithFailed so the runner does
// not import the manifest helper.
func probeFailed(probe string) bool {
	const failedPrefix = "failed:"
	return len(probe) >= len(failedPrefix) && probe[:len(failedPrefix)] == failedPrefix
}

// agentInstructionFiles maps the pinned instruction files to the agent
// package's Invocation.Instructions shape: the runner owns reading/capping, the
// adapter only stages and lists. Content is the (already capped) bytes that
// will reach the model.
func agentInstructionFiles(files []instructions.File) []agent.InstructionFile {
	out := make([]agent.InstructionFile, 0, len(files))
	for _, file := range files {
		if file.MissingAtCommit || len(file.SourceContent) == 0 {
			continue
		}
		out = append(out, agent.InstructionFile{
			RepoPath: file.RepoPath,
			Content:  file.SourceContent,
		})
	}
	return out
}

// contextPinnedPayload builds the EvContextPinned event payload: the stage, the
// counts of delivered instruction files (excluding missing), truncated files,
// and enumerated skills. Counts only — hashes live in the manifest body.
func contextPinnedPayload(stageID string, run stageRun) map[string]any {
	deliveredCount := 0
	truncatedCount := 0
	for _, file := range run.instructionFiles {
		if file.MissingAtCommit {
			continue
		}
		deliveredCount++
		if file.Truncated {
			truncatedCount++
		}
	}
	return map[string]any{
		"stage":             stageID,
		"instruction_count": deliveredCount,
		"truncated_count":   truncatedCount,
		"skill_count":       len(run.contextReport.Skills),
		"skills_probe":      run.contextReport.SkillsProbe,
	}
}

// autoInstructionBaseline returns the instruction files the execution runtime
// loads by itself, as the adapter declares them. The runner names no file of
// its own: which one a runtime auto-loads is a fact about that runtime, and a
// literal here would pin the wrong file for every executor but the one it was
// written for, with no error and only in the paths where the context probe
// could not answer.
func (runner *Runner) autoInstructionBaseline() []string {
	return runner.adapter.Describe().AutoInstructions
}

// configDriftChange names how the source checkout's .agentum.yaml differs
// from the one pinned at base_commit. Absence is a value, not a failure: a
// config can be added or removed, and "absent on both sides" is the project's
// empty-registry configuration.
type configDriftChange string

const (
	configDriftAdded    configDriftChange = "added"
	configDriftRemoved  configDriftChange = "removed"
	configDriftModified configDriftChange = "modified"
	// configDriftUnreadable: the checkout copy exists or may exist but could
	// not be read, so the comparison has no result.
	configDriftUnreadable configDriftChange = "unreadable"
)

// recordProjectConfigAtStart compares .agentum.yaml in the source checkout
// with the run's pinned base_commit version when the run's worktree is first
// created, and records the result in the manifest's context section. A
// difference is a warning, never a stop: checks and instructions come from
// base_commit regardless, so the comparison only tells a reviewer that the
// operator's local config was not the one applied. Pausing on it stopped runs
// for ordinary situations — a checkout on another branch than the run's base,
// a config being edited — and on continuations, where the checkout's state no
// longer bears on a run that pinned its config long before.
//
// The comparison is by hash and change kind only; file contents never reach an
// event payload or the manifest. Absence is a value: absent at base_commit
// means the run's check registry is empty, which the evidence states plainly.
func (runner *Runner) recordProjectConfigAtStart(ctx context.Context, record sqlc.Run, checkoutPath, baseCommit string) {
	atCommit, commitReadErr := runner.wt.FileAtCommit(ctx, checkoutPath, baseCommit, checks.ConfigFile)
	if commitReadErr != nil && !errors.Is(commitReadErr, os.ErrNotExist) {
		// The registry load in prepareProjectContext surfaces this as a failed
		// config read; recording a guess here would contradict it.
		return
	}
	evidence := &manifest.ProjectConfigEvidence{File: checks.ConfigFile, PresentAtBase: commitReadErr == nil}
	if evidence.PresentAtBase {
		evidence.BaseHash = contentHash(atCommit)
	}
	onDisk, diskReadErr := os.ReadFile(filepath.Join(checkoutPath, checks.ConfigFile))
	diskPresent := diskReadErr == nil
	switch {
	case diskReadErr != nil && !errors.Is(diskReadErr, fs.ErrNotExist):
		// The file may exist; the comparison has no answer. Recording it as
		// "removed" would put a false fact into first-write-wins evidence.
		runner.log.Warn("project config comparison: read checkout copy", "run", record.ID, "error", diskReadErr)
		evidence.CheckoutChange = string(configDriftUnreadable)
	case diskPresent && !evidence.PresentAtBase:
		evidence.CheckoutChange = string(configDriftAdded)
	case !diskPresent && evidence.PresentAtBase:
		evidence.CheckoutChange = string(configDriftRemoved)
	case diskPresent && evidence.PresentAtBase && sha256.Sum256(onDisk) != sha256.Sum256(atCommit):
		evidence.CheckoutChange = string(configDriftModified)
	}
	if diskPresent {
		evidence.CheckoutHash = contentHash(onDisk)
	}

	if evidence.CheckoutChange != "" {
		runner.log.Warn("source checkout config differs from base_commit; the run applies the base_commit version",
			"run", record.ID, "file", checks.ConfigFile, "change", evidence.CheckoutChange, "base_commit", baseCommit)
		runner.emit(ctx, record, EvProjectConfigDrift, map[string]any{
			"file":          checks.ConfigFile,
			"base_commit":   baseCommit,
			"change":        evidence.CheckoutChange,
			"base_hash":     evidence.BaseHash,
			"checkout_hash": evidence.CheckoutHash,
		})
	}
	if runner.mfst == nil {
		return
	}
	patch := manifest.Body{Context: &manifest.ContextEvidence{ProjectConfig: evidence}}
	if err := runner.mfst.AddEvidence(ctx, record.TenantID, record.ID, patch); err != nil && !errors.Is(err, manifest.ErrSealed) {
		runner.log.Warn("record project config evidence", "run", record.ID, "error", err)
	}
}

// contentHash renders the sha256 of config bytes for event payloads — the
// drift comparison material that is safe to record (no file contents).
func contentHash(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
