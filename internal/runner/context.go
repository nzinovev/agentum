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
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/engine"
	"github.com/nzinovev/agentum/internal/instructions"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/publish"
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
	registry, registryErr := runner.loadRegistryAtBaseCommit(ctx, run.record, run.project, checkoutPathOf(run.record, run.project))
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

// maxPackDriftEntries caps how many pack-directory entries reach the event
// payload and the evidence: enough to show a whole drifted pack, small enough
// that a wide untracked tree does not turn the pause record into a listing.
const maxPackDriftEntries = 20

// pauseOnProjectPackDrift stops the run (stop_reason project_pack_drift) when
// the directory of the pack the run executes — .agentum/packs/<name>/ —
// carries uncommitted changes in the source checkout, relative to the
// checkout's HEAD. Every porcelain code except "!!" pauses: untracked files
// and all modifications of committed files, in the worktree or the index. An
// ignored entry pauses only when it is the pack's manifest.yaml or
// overrides.yaml (the pack is structurally uncommittable); other ignored
// entries — editor junk under global excludes — are recorded without pausing.
//
// The comparison is against HEAD, not base_commit, deliberately: an
// uncommitted edit is the accident this pause exists for, whatever branch the
// checkout stands on, and committing the edit (or reverting it) clears it —
// after a commit the run simply continues executing the pinned base_commit
// pack, which is also why a committed difference never pauses (recorded as
// BaseDiverged evidence instead). Unlike the .agentum.yaml comparison, which
// is a warning: a pack change alters what every stage of the run does, so
// proceeding silently would apply something the operator visibly did not
// choose. Returns true when the pause was applied.
func (runner *Runner) pauseOnProjectPackDrift(ctx context.Context, record sqlc.Run, runPack *pack.Pack, checkoutPath, baseCommit string) bool {
	packDir := pack.ProjectPacksDir + "/" + runPack.Pack.Name
	// Upgrade the exclude rules first: a bare ".agentum/" line — the legacy
	// form or an operator's own — hides the packs directory from status, and
	// the drift it causes would be invisible exactly when this check exists
	// to catch it. Non-fatal by design; a failed write leaves the entries
	// visible as "!!" instead.
	if err := runner.wt.EnsureExcludes(ctx, checkoutPath); err != nil {
		runner.log.Warn("upgrade .agentum excludes before pack drift check", "run", record.ID, "error", err)
	}
	changes, changesErr := runner.wt.UncommittedChanges(ctx, checkoutPath, packDir, maxPackDriftEntries)
	if changesErr != nil {
		// A failed status read must not fail the run: the pack still comes
		// from base_commit, so the comparison is advisory. The gap is logged;
		// no pause is applied on a reading we could not take.
		runner.log.Warn("read pack dir status", "run", record.ID, "dir", packDir, "error", changesErr)
		return false
	}

	evidence := &manifest.ProjectPacksEvidence{Dir: packDir}
	for _, change := range changes {
		entry := strings.TrimSpace(change.Code + " " + change.Path)
		if change.Code == "!!" {
			base := change.Path
			if index := strings.LastIndexByte(base, '/'); index >= 0 {
				base = base[index+1:]
			}
			if base == "manifest.yaml" || base == "overrides.yaml" {
				evidence.Uncommitted = append(evidence.Uncommitted, entry)
			} else {
				evidence.Ignored = append(evidence.Ignored, entry)
			}
			continue
		}
		evidence.Uncommitted = append(evidence.Uncommitted, entry)
	}
	if head, headErr := runner.wt.HeadCommit(ctx, checkoutPath); headErr != nil {
		runner.log.Warn("read checkout HEAD for pack drift evidence", "run", record.ID, "error", headErr)
	} else if head != baseCommit {
		// A committed difference is the documented model — the run applies the
		// base_commit pack — so it is recorded, never a pause.
		evidence.BaseDiverged = true
	}
	runner.recordProjectPacksEvidence(ctx, record, evidence)

	if len(evidence.Uncommitted) == 0 {
		return false
	}
	runner.log.Warn("project pack directory has uncommitted changes; the run executes the base_commit version",
		"run", record.ID, "dir", packDir,
		"uncommitted", evidence.Uncommitted, "ignored", len(evidence.Ignored))
	runner.emit(ctx, record, EvProjectPackDrift, map[string]any{
		"dir": packDir, "base_commit": baseCommit,
		"uncommitted": evidence.Uncommitted, "ignored": evidence.Ignored,
		"hint": "commit or revert the pack directory changes, then continue; the run always executes the base_commit version of the pack",
	})
	return runner.applyStop(ctx, record, runPack, "project_pack_drift")
}

// recordProjectPacksEvidence writes the pack-directory comparison into the
// manifest's context section (first write wins, like the config comparison).
func (runner *Runner) recordProjectPacksEvidence(ctx context.Context, record sqlc.Run, evidence *manifest.ProjectPacksEvidence) {
	if runner.mfst == nil || evidence == nil {
		return
	}
	patch := manifest.Body{Context: &manifest.ContextEvidence{ProjectPacks: evidence}}
	if err := runner.mfst.AddEvidence(ctx, record.TenantID, record.ID, patch); err != nil && !errors.Is(err, manifest.ErrSealed) {
		runner.log.Warn("record project packs evidence", "run", record.ID, "error", err)
	}
}

// pauseOnOffTargetBase verifies that a publishable run's base_commit belongs
// to the publication target branch's history, using the remote-tracking ref
// as the last verifiable comparison point, and stops the run in
// paused_user_stop when it does not. Returns true when the pause was applied.
//
// The target branch is the configured publication base branch; when none is
// configured (the provider resolves the default branch at attempt time), the
// run's own base_ref names the candidate through publish.BaseBranchFromRef —
// the same rule the publisher resolves its destination with, so
// `refs/remotes/origin/main` and `main` name the same target. A base_ref of
// HEAD, a bare SHA, or a tag names no target branch, so the comparison point
// cannot be established and the run stops asking for an explicit ref or a
// configured base, never a silent HEAD.
//
// Both stop reasons are the resumable shape: after `git fetch` (refreshing
// the tracking ref) or after re-creating the run from the proper target ref,
// the precondition holds. An already-pinned base_commit of an existing run is
// never rewritten by this check.
func (runner *Runner) pauseOnOffTargetBase(ctx context.Context, record sqlc.Run, runPack *pack.Pack, checkoutPath, baseCommit string) bool {
	if !runner.publication.Enabled {
		return false
	}
	remote := runner.publication.Remote
	if remote == "" {
		remote = "origin"
	}
	targetBranch := runner.publication.BaseBranch
	if targetBranch == "" {
		candidate, named := publish.BaseBranchFromRef(record.BaseRef, remote)
		if !named {
			runner.log.Warn("publication base target unverifiable from base_ref; pausing",
				"run", record.ID, "base_ref", record.BaseRef)
			runner.emit(ctx, record, EvRunBaseOffTarget, map[string]any{
				"cause": "target_unverifiable", "base_ref": record.BaseRef,
				"hint": "set AGENTUM_PUBLISH_BASE_BRANCH or start the run from the target branch (e.g. refs/remotes/" + remote + "/main)",
			})
			return runner.applyStop(ctx, record, runPack, "base_target_unverifiable")
		}
		targetBranch = candidate
	}
	trackingRef := "refs/remotes/" + remote + "/" + targetBranch
	tip, tipErr := runner.wt.ResolveRef(ctx, checkoutPath, trackingRef)
	if tipErr != nil {
		runner.log.Warn("publication target comparison point unavailable; pausing",
			"run", record.ID, "ref", trackingRef, "error", tipErr)
		runner.emit(ctx, record, EvRunBaseOffTarget, map[string]any{
			"cause": "target_unavailable", "ref": trackingRef,
			"hint": "git fetch " + remote + " to establish the comparison point, or configure AGENTUM_PUBLISH_BASE_BRANCH",
		})
		return runner.applyStop(ctx, record, runPack, "base_target_unverifiable")
	}
	onBranch, ancestryErr := runner.wt.IsAncestor(ctx, checkoutPath, baseCommit, tip)
	if ancestryErr != nil {
		runner.log.Warn("publication base ancestry check failed; pausing",
			"run", record.ID, "base_commit", baseCommit, "ref", trackingRef, "error", ancestryErr)
		runner.emit(ctx, record, EvRunBaseOffTarget, map[string]any{
			"cause": "ancestry_unreadable", "base_commit": baseCommit, "ref": trackingRef, "reason": ancestryErr.Error(),
		})
		return runner.applyStop(ctx, record, runPack, "base_target_unverifiable")
	}
	if onBranch {
		return false
	}
	ahead, aheadErr := runner.wt.CountAhead(ctx, checkoutPath, tip, baseCommit)
	if aheadErr != nil {
		runner.log.Warn("count commits ahead of target; reporting without count", "run", record.ID, "error", aheadErr)
	}
	runner.log.Warn("run base is not on the publication target branch; pausing",
		"run", record.ID, "base_commit", baseCommit, "ref", trackingRef, "commits_ahead", ahead)
	runner.emit(ctx, record, EvRunBaseOffTarget, map[string]any{
		"cause": "base_not_on_target", "base_commit": baseCommit, "ref": trackingRef,
		"commits_ahead_of_target": ahead,
		"hint":                    "start the run from the target branch's ref, or publish the base branch's commits first; the run's pull request must carry only its own commits",
	})
	return runner.applyStop(ctx, record, runPack, "base_not_on_target")
}

// applyStop applies the shared paused_user_stop decision used by the
// base-ancestry pauses. A pause-application failure fails the run (the FSM
// refused a transition this code relies on); true means the pause landed.
func (runner *Runner) applyStop(ctx context.Context, record sqlc.Run, runPack *pack.Pack, stopReason string) bool {
	pauseErr := runner.applyPauseDecision(ctx, record, Decision{
		Action:     ActionPause,
		FSMEvent:   engine.EventStopUser,
		StopReason: stopReason,
	}, currentStageOrFallback(record.CurrentStage, runPack.Entry))
	if pauseErr != nil {
		runner.failRun(ctx, record, pauseErr)
	}
	return true
}
