package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// ContextProber is the optional adapter capability of reporting the non-prompt
// context the runtime will inject: the instruction files it loads by itself and
// the skills it has available (ADR 0002 D6). Enumeration is runtime-specific
// work — it needs the same binary, the same working directory, and the same
// scrubbed environment as the invocation, or it does not describe what the agent
// actually saw — so it belongs behind the adapter seam.
//
// The Adapter interface itself stays as it is: an adapter that cannot report is
// not an error, the evidence records the gap. Callers discover the capability
// with a comma-ok type assertion (no interface stored on the Runner) so a
// typed-nil adapter is not a trap.
type ContextProber interface {
	// ProbeContext reports the runtime-injected instruction paths (static
	// adapter knowledge) and the available skills (probed from the runtime).
	// AutoInstructions is returned even when the skill probe fails — the
	// baseline must not evaporate because a subprocess timed out.
	ProbeContext(ctx context.Context, inv Invocation) (ContextReport, error)
}

// ContextReport is the non-prompt context the runtime will inject for an
// invocation.
type ContextReport struct {
	// AutoInstructions are the repo-relative paths the runtime loads by itself,
	// regardless of any project declaration. For opencode that is AGENTS.md at
	// the project root. This is static adapter knowledge, returned even when the
	// skill subprocess fails.
	AutoInstructions []string

	// Skills is one entry per available skill, in the order the runtime reports
	// them. Empty (not nil) when the probe ran and found none — but note
	// opencode always ships at least one built-in skill, so an empty slice
	// usually means the probe failed rather than "no skills."
	Skills []SkillRef

	// SkillsError carries the reason the skill probe failed (timeout, bad JSON,
	// non-zero exit). Empty when the probe succeeded. AutoInstructions is still
	// populated in this case.
	SkillsError string

	// SkillsProbe is the outcome label the manifest records: "ok",
	// "unsupported" (the adapter has no prober), or "failed: <reason>". A failed
	// probe makes the run's evidence incomplete (an EvidenceGap is recorded).
	SkillsProbe string
}

// SkillRef is one available skill as the runtime reports it, with the body
// hashed and discarded (ADR 0002 D6: bodies never enter the manifest — hashes
// and sizes only, both to keep evidence compact and to keep third-party text out
// of a record that already has a secret-scanning story).
type SkillRef struct {
	Name        string `json:"name"`
	Location    string `json:"location"` // path, or "<built-in>"
	Description string `json:"description"`
	Hash        string `json:"hash"`  // sha256 hex of the skill body
	Bytes       int    `json:"bytes"` // body size
}

// probeTimeout caps a context probe. opencode can produce zero bytes on both
// streams and never exit (a known defect, ≈2 in 6 runs — ADR 0002 §1), so the
// probe carries its own deadline and its own process-group kill rather than
// relying on the caller's ctx alone. A probe that hangs must not hang the run.
// A var so the subprocess tests can shrink it; nothing in production reassigns
// it.
var probeTimeout = 10 * time.Second

// autoInstructionBaseline is the runtime-injected instruction baseline for the
// opencode adapter: AGENTS.md at the project root (measured 2026-08-05, zero
// tool calls — the file reaches the system prompt with no configuration). This
// is adapter-owned knowledge, not a constant the runner assumes: it is a fact
// about a runtime, and the codebase rule is that runtime facts live behind the
// adapter seam.
var autoInstructionBaseline = []string{"AGENTS.md"}

// ProbeContext implements ContextProber for the opencode adapter. It probes
// `opencode debug skill` with the invocation's working directory and a
// rendered deny-baseline + skill:allow config in OPENCODE_CONFIG_CONTENT, hashes
// each skill body and discards it, and returns the static AutoInstructions
// regardless of the subprocess outcome.
//
// The probe runs the same scrubbed environment as an invocation, so the
// enumeration is what the agent will see, not what the operator's shell would
// see. It carries its own timeout (probeTimeout) and its own process-group kill
// because the no-output hang is a known opencode defect, not a hypothetical.
func (adapter *OpencodeAdapter) ProbeContext(ctx context.Context, inv Invocation) (ContextReport, error) {
	report := ContextReport{
		AutoInstructions: append([]string(nil), autoInstructionBaseline...),
		SkillsProbe:      ContextProbeOK,
	}
	// A deny-baseline profile with skill allowed: the probe wants to enumerate
	// exactly what an invocation would see, and skills must be allowed for the
	// enumeration to be meaningful. No fs/net/bash grants are needed to list
	// skills.
	probeProfile := caps.Profile{}
	config, configErr := buildOpencodeConfig(probeProfile, scopeSubst{worktree: inv.Workdir, artifact: inv.ArtifactDir}, nil)
	if configErr != nil {
		// buildOpencodeConfig only errors on a path scope that cannot be
		// rendered; the empty profile has none. Treat as a probe failure rather
		// than panicking.
		report.SkillsError = fmt.Sprintf("render probe config: %v", configErr)
		report.SkillsProbe = ContextProbeFailedPrefix + "config"
		return report, nil
	}
	compact, _, renderErr := renderOpencodeConfigBytes(config)
	if renderErr != nil {
		report.SkillsError = fmt.Sprintf("render probe config: %v", renderErr)
		report.SkillsProbe = ContextProbeFailedPrefix + "config"
		return report, nil
	}
	env := buildChildEnv(probeProfile, "", compact)

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	skills, skillsErr := adapter.runDebugSkill(probeCtx, inv.Workdir, env)
	if skillsErr != nil {
		report.SkillsError = skillsErr.Error()
		report.SkillsProbe = classifyProbeError(skillsErr)
		return report, nil
	}
	report.Skills = skills
	return report, nil
}

// runDebugSkill runs `opencode debug skill` and parses the JSON array of
// {name, description, location, content} entries. There is no --dir flag
// (Step-0 finding), so the working directory is set on the command itself. The
// body of each skill is hashed and discarded; only the hash and byte size are
// returned. A timeout, a non-zero exit, or malformed JSON is returned as an
// error — the caller classifies it into SkillsProbe.
func (adapter *OpencodeAdapter) runDebugSkill(ctx context.Context, workdir string, env []string) ([]SkillRef, error) {
	args := []string{adapter.binary, "debug", "skill"}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	setProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	reaped := make(chan struct{})
	watcherDone := watchCancellation(ctx, cmd, reaped)
	runErr := cmd.Run()
	close(reaped)
	<-watcherDone

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("opencode debug skill: %w", ctxErr)
	}
	if runErr != nil {
		stderrText := stderr.String()
		if stderrText != "" {
			return nil, fmt.Errorf("opencode debug skill exited: %w: %s", runErr, stderrText)
		}
		return nil, fmt.Errorf("opencode debug skill exited: %w", runErr)
	}

	var raw []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Location    string `json:"location"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("opencode debug skill: decode: %w", err)
	}
	skills := make([]SkillRef, 0, len(raw))
	for _, entry := range raw {
		body := []byte(entry.Content)
		skills = append(skills, SkillRef{
			Name:        entry.Name,
			Location:    entry.Location,
			Description: entry.Description,
			Hash:        hashSkillBody(body),
			Bytes:       len(body),
		})
	}
	return skills, nil
}

// classifyProbeError turns a probe failure into a stable SkillsProbe label.
// "failed: timeout" makes the evidence gap attributable; "failed: exit" and
// "failed: json" distinguish a runtime crash from a shape change.
func classifyProbeError(err error) string {
	if err == nil {
		return ContextProbeOK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ContextProbeFailedPrefix + "timeout"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "decode") || strings.Contains(msg, "json"):
		return ContextProbeFailedPrefix + "json"
	case strings.Contains(msg, "exited"):
		return ContextProbeFailedPrefix + "exit"
	default:
		return ContextProbeFailedPrefix + "error"
	}
}

// hashSkillBody returns the sha256 hex of a skill body. Kept here rather than
// importing a shared hasher so the agent package stays self-contained for its
// evidence shape.
func hashSkillBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ContextProbe outcome labels. Stable strings the manifest records verbatim.
const (
	ContextProbeOK           = "ok"
	ContextProbeUnsupported  = "unsupported"
	ContextProbeFailedPrefix = "failed:"
)
