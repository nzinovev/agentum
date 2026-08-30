package checks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Status is the outcome of one check.
type Status string

const (
	// StatusPass means the command exited 0.
	StatusPass Status = "pass"
	// StatusFail means the command exited non-zero.
	StatusFail Status = "fail"
	// StatusTimeout means the per-check deadline elapsed before the command
	// exited.
	StatusTimeout Status = "timeout"
	// StatusError means the command could not be started or failed to produce a
	// result for a reason other than exit code or timeout.
	StatusError Status = "error"
)

// Outcome is the result of running one check: the resolved item, the status,
// the exit code (when available), the wall-clock duration, and the capped
// stdout/stderr. Reason carries the cause for a non-pass status (e.g.
// "timeout after 2s", "start: exec: not found"). DefinitionRevision is the
// content hash of the definition that ran, so a result is tied to the exact
// contract that produced it.
type Outcome struct {
	Item               Item
	Status             Status
	ExitCode           int
	Duration           time.Duration
	Stdout             string
	Stderr             string
	Reason             string
	DefinitionRevision string
}

// Report is the result of running a whole Set. Commit is the verified commit the
// checks ran against (the worktree HEAD at run time); Profile is the audit label
// of the executor's enforced boundary.
type Report struct {
	Set      *Set
	Commit   string
	Outcomes []Outcome
	Profile  string
}

// MandatoryPassed reports whether every required (baseline/pack/task-mandatory)
// check passed. Optional checks do not affect this — their failures are recorded
// as evidence but do not block delivery.
func (report Report) MandatoryPassed() bool {
	for _, outcome := range report.Outcomes {
		if outcome.Item.Required && outcome.Status != StatusPass {
			return false
		}
	}
	return true
}

// FailedMandatory returns the names of mandatory checks that did not pass, for
// the runner's failure reason and the event payload.
func (report Report) FailedMandatory() []string {
	var failed []string
	for _, outcome := range report.Outcomes {
		if outcome.Item.Required && outcome.Status != StatusPass {
			failed = append(failed, outcome.Item.Definition.Name)
		}
	}
	return failed
}

// ExecutorDeps bundles Executor construction.
type ExecutorDeps struct {
	// DefaultTimeout is applied when a check declares no timeout. Zero means a
	// 5 minute cap so a misbehaving check cannot hang delivery indefinitely.
	DefaultTimeout time.Duration
	// DefaultMaxOutput is the per-stream cap when a check declares none. Zero
	// means 64 KiB — enough to diagnose a failure without ballooning the
	// manifest.
	DefaultMaxOutput int
	// Log is the structured logger; defaults to slog.Default() when nil.
	Log *slog.Logger
}

// Executor runs a resolved Set against a worktree under a fixed minimal
// boundary. It is the orchestrator's own runner, not an agent adapter, so it is
// not subject to the caps intersection — instead it is bound by code:
//
//   - commands come only from the project registry (arg vector; no shell);
//   - the working directory is scoped under the worktree root and cannot escape;
//   - the environment is scrubbed of provider credentials before each run;
//   - each check runs under a timeout and its stdout/stderr are capped.
//
// ProfileLabel is recorded on every Report as audit evidence of this boundary.
type Executor struct {
	defaultTimeout   time.Duration
	defaultMaxOutput int
	log              *slog.Logger
}

// ProfileLabel is the audit label recorded on each Report. It identifies the
// executor's enforced boundary (arg-vector commands, worktree scope, scrubbed
// env, no provider credentials) so a later review can reconstruct what the
// checks could and could not do.
const ProfileLabel = "checks-executor-v1"

// NewExecutor builds an Executor with sensible defaults when Deps is zero.
func NewExecutor(deps ExecutorDeps) *Executor {
	timeout := deps.DefaultTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	maxOutput := deps.DefaultMaxOutput
	if maxOutput <= 0 {
		maxOutput = 64 * 1024
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &Executor{defaultTimeout: timeout, defaultMaxOutput: maxOutput, log: log}
}

// Run executes every item in set against the worktree rooted at workdirRoot.
// Items run sequentially in set order; a failure does not short-circuit the rest
// (every result is evidence). Returns the Report; the caller decides whether a
// mandatory failure blocks delivery. An empty set returns an empty report.
func (executor *Executor) Run(ctx context.Context, set *Set, workdirRoot string) (Report, error) {
	report := Report{Set: set, Profile: ProfileLabel}
	if set.Empty() {
		return report, nil
	}
	for _, item := range set.Items {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		outcome := executor.runOne(ctx, item, workdirRoot)
		outcome.DefinitionRevision = DefinitionRevision(item.Definition)
		report.Outcomes = append(report.Outcomes, outcome)
		executor.log.Info("project check",
			"check", item.Definition.Name, "status", string(outcome.Status),
			"required", item.Required, "duration_ms", outcome.Duration.Milliseconds())
	}
	return report, nil
}

// runOne runs a single check. The workdir is resolved under the worktree root
// and validated not to escape it; the env is scrubbed; a per-check deadline
// bounds the run; output is captured into capped buffers.
func (executor *Executor) runOne(parent context.Context, item Item, workdirRoot string) Outcome {
	definition := item.Definition
	start := time.Now()
	outcome := Outcome{Item: item}

	workdir, dirErr := resolveWorkdir(workdirRoot, definition.Workdir)
	if dirErr != nil {
		outcome.Status = StatusError
		outcome.Reason = "workdir: " + dirErr.Error()
		outcome.Duration = time.Since(start)
		return outcome
	}

	timeout := time.Duration(definition.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = executor.defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, definition.Command[0], definition.Command[1:]...)
	cmd.Dir = workdir
	cmd.Env = scrubbedEnv()

	stdout := newLimitedBuffer(executor.outputCap(definition))
	stderr := newLimitedBuffer(executor.outputCap(definition))
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()
	outcome.Duration = time.Since(start)
	outcome.Stdout = stdout.String()
	outcome.Stderr = stderr.String()

	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome.Status = StatusTimeout
		outcome.Reason = fmt.Sprintf("timeout after %s", timeout)
	case runErr != nil:
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			outcome.Status = StatusFail
			outcome.ExitCode = exitErr.ExitCode()
		} else {
			outcome.Status = StatusError
			outcome.Reason = "start: " + runErr.Error()
		}
	default:
		outcome.Status = StatusPass
	}
	return outcome
}

// resolveWorkdir joins root and rel (when rel is set) and rejects a rel that
// escapes root. An empty rel means the worktree root itself.
func resolveWorkdir(root, rel string) (string, error) {
	if rel == "" {
		return root, nil
	}
	if pathEscapes(rel) {
		return "", fmt.Errorf("workdir %q escapes the worktree root", rel)
	}
	return filepath.Join(root, rel), nil
}

// outputCap picks the per-stream byte cap for a check: the check's own value, or
// the executor default.
func (executor *Executor) outputCap(definition Definition) int {
	if definition.MaxOutputBytes > 0 {
		return definition.MaxOutputBytes
	}
	return executor.defaultMaxOutput
}

// limitedBuffer is a byte buffer that stops storing after cap bytes. The process
// keeps running (and writing) once the cap is hit; the excess is dropped so a
// check that emits gigabytes cannot balloon memory or the manifest. Truncated is
// true when output was dropped.
type limitedBuffer struct {
	cap       int
	truncated bool
	buf       bytes.Buffer
}

func newLimitedBuffer(cap int) *limitedBuffer {
	if cap <= 0 {
		cap = 0
	}
	return &limitedBuffer{cap: cap}
}

func (buffer *limitedBuffer) Write(chunk []byte) (int, error) {
	if buffer.cap > 0 && buffer.buf.Len() >= buffer.cap {
		buffer.truncated = true
		return len(chunk), nil
	}
	if buffer.cap <= 0 {
		return buffer.buf.Write(chunk)
	}
	remaining := buffer.cap - buffer.buf.Len()
	if len(chunk) > remaining {
		buffer.truncated = true
	}
	written, err := buffer.buf.Write(chunk[:min(remaining, len(chunk))])
	if err != nil {
		return written, err
	}
	return len(chunk), nil
}

func (buffer *limitedBuffer) String() string {
	out := buffer.buf.String()
	if buffer.truncated {
		out += "\n...[truncated]"
	}
	return out
}

// scrubbedEnv returns the current environment with provider credentials and
// generic secret-bearing variables removed. The executor must never receive
// provider credentials — checks are project tooling (go test, ruff, …), not an
// agent invocation. The scrub is best-effort like the artifact redactor: it
// strips anything whose key looks like a credential, and is conservative
// (drop-on-match).
func scrubbedEnv() []string {
	raw := os.Environ()
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if isCredentialKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// isCredentialKey reports whether the env var key (case-insensitive) looks like a
// provider credential. Matches suffixes (_KEY, _TOKEN, _SECRET, _CREDENTIAL,
// _PASSWORD, _PRIVATE_KEY) and known provider prefixes. Best-effort.
func isCredentialKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, suffix := range []string{"_API_KEY", "_KEY", "_TOKEN", "_SECRET", "_CREDENTIAL", "_PASSWORD", "_PRIVATE_KEY"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	if upper == "API_KEY" || upper == "TOKEN" || upper == "SECRET" || upper == "PASSWORD" {
		return true
	}
	for _, prefix := range []string{
		"ANTHROPIC", "OPENAI", "MISTRAL", "GROQ", "TOGETHER", "REPLICATE",
		"DEEPSEEK", "COHERE", "PERPLEXITY", "FIREWORKS", "GOOGLE_GENAI",
		"GEMINI", "AZURE_OPENAI", "VERCEL", "NETLIFY", "STRIPE",
	} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
