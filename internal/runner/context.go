package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/instructions"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/taskinput"
)

// prepareProjectContext wires the project-context channel for the run (ADR
// 0002): it reads .agentum.yaml from base_commit once (the agent-immutability
// seam), pins the declared + auto-injected instruction files, probes the
// runtime's skills, and resolves the check set for rendering. Everything is
// stashed on stageRun so the loop and invokeStage share one source of truth.
//
// A failure to pin or probe degrades evidence, it does not fail the run: the
// task still proceeds with whatever context was pinnable, and the gaps are
// recorded. Only a malformed .agentum.yaml that fails to parse, or a check-set
// resolution error, returns an error — drive turns those into failTask. The
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
	// cached set here never reaches the gate, only the routing block.
	// The strict overrides decode lives here (ADR 0004 D7): after the API
	// boundary guarantees well-formed overrides, a malformed column is an
	// invariant break and this error reaches failTask through drive.
	taskOverrides, overridesErr := taskinput.ParseOverrides(run.task.Overrides)
	if overridesErr != nil {
		return fmt.Errorf("project context: parse task overrides: %w", overridesErr)
	}
	packRequests := packCheckRequests(run.taskPack)
	taskRequests := taskCheckRequests(taskOverrides)
	set, resolveErr := checks.Resolve(registry, packRequests, taskRequests)
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
				AutoInstructions: append([]string(nil), "AGENTS.md"),
				SkillsProbe:      agent.ContextProbeFailedPrefix + "error",
				SkillsError:      probeErr.Error(),
			}
		} else {
			report = probed
		}
	}
	if len(report.AutoInstructions) == 0 {
		// Defensive: a prober that returned no baseline still gets the static
		// default, so the pin's auto list is never empty.
		report.AutoInstructions = []string{"AGENTS.md"}
	}
	run.contextReport = report

	// Pin the instruction set: declared (from the registry) ∪ auto (from the
	// probe's baseline), bytes read from base_commit via the worktree manager
	// (which satisfies instructions.Reader through FileAtCommit).
	declared := registry.InstructionPaths()
	pinned, pinErr := instructions.Pin(ctx, runner.wt, run.project.RepoPath, baseCommit, declared, report.AutoInstructions)
	if pinErr != nil {
		// A read failure (not a missing file) is recorded as an evidence gap;
		// the run proceeds with whatever was pinnable. A missing file is already
		// captured per-entry as MissingAtCommit.
		runner.recordEvidenceGap(ctx, run.task, "context.instructions", "", pinErr)
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
// restore that fails to write is a task failure, matching the precedent that a
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
		// A restore IO error is a task failure — we cannot stand behind a run
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
		runner.emit(ctx, run.task, EvInstructionsRestored, map[string]any{
			"stage":      stageID,
			"path":       restoration.Path,
			"action":     action,
			"found_hash": restoration.FoundHash,
		})
	}
	runner.recordRestorationEvidence(ctx, run, restorations)
	return nil
}

// recordContextEvidence writes the context section into the manifest: the
// instruction refs (from the pinned set), the enumerated skills (from the
// probe), the skills probe label, and the missing list. Called once per
// successful stage invocation; merge is append-unique so a re-pin under a retry
// collapses and a skill-set change between jobs surfaces.
//
// A failed skill probe additionally records an EvidenceGap(context.skills),
// which makes evidence_complete false — the honest reading that we do not know
// what knowledge was in play. An unsupported probe (adapter has no prober)
// records no gap: it is a permanent capability gap, like memory.
func (runner *Runner) recordContextEvidence(ctx context.Context, run stageRun, stageID string) {
	if runner.mfst == nil {
		return
	}
	section := &manifest.ContextEvidence{
		SkillsProbe: run.contextReport.SkillsProbe,
	}
	section.Instructions = instructionRefsForEvidence(run.instructionFiles)
	section.Skills = skillRefsForManifest(run.contextReport.Skills)
	for _, file := range run.instructionFiles {
		if file.MissingAtCommit {
			section.Missing = append(section.Missing, file.RepoPath)
		}
	}
	patch := manifest.Body{Context: section}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		if errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("record context evidence: manifest sealed", "task", run.task.ID)
			return
		}
		runner.log.Warn("record context evidence", "task", run.task.ID, "error", err)
		runner.recordEvidenceGap(ctx, run.task, "context", stageID, err)
		return
	}
	// A failed probe is an evidence gap (degraded this run). An unsupported
	// probe is not — it is a permanent capability gap recorded in the section.
	if probe := run.contextReport.SkillsProbe; probeFailed(probe) {
		runner.recordEvidenceGap(ctx, run.task, "context.skills", stageID,
			fmt.Errorf("skill probe failed: %s", run.contextReport.SkillsError))
	}
}

// recordRestorationEvidence appends the tamper-reversal history to the
// manifest's context section. Merge is append-unique by (stage, path, at).
func (runner *Runner) recordRestorationEvidence(ctx context.Context, run stageRun, restorations []manifest.InstructionRestoration) {
	if runner.mfst == nil || len(restorations) == 0 {
		return
	}
	patch := manifest.Body{Context: &manifest.ContextEvidence{Restorations: restorations}}
	if err := runner.mfst.AddEvidence(ctx, run.task.TenantID, run.task.ID, patch); err != nil {
		if !errors.Is(err, manifest.ErrSealed) {
			runner.log.Warn("record restoration evidence", "task", run.task.ID, "error", err)
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
