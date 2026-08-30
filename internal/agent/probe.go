package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// Readiness is the outcome of an execution adapter's readiness probe. A probe
// returns a value and never an error: an absent or broken runtime is a fact to
// record (Ready=false, Reason), not a boot failure — it surfaces where it
// belongs, in the run that tries to invoke and in a future readiness endpoint.
type Readiness struct {
	AdapterID AdapterID
	Ready     bool
	// RuntimeVersion is the external runtime's own version string (e.g. the
	// opencode CLI's). Empty when the probe failed.
	RuntimeVersion string
	// Reason says why the adapter is not ready; empty when Ready.
	Reason string
	// CheckedAt is when the probe ran (once per process — see Probe).
	CheckedAt time.Time
}

// Label renders the readiness as the manifest's runtime_probe value: "ok", or
// "failed: <reason>" — the same vocabulary the skills probe uses, so evidence
// readers see one shape for probed runtime facts.
func (readiness Readiness) Label() string {
	if readiness.Ready {
		return "ok"
	}
	return "failed: " + readiness.Reason
}

// Probe runs the opencode runtime's `--version` and memoizes the whole
// Readiness for the process lifetime: one subprocess per process, for every
// consumer. Evidence writing calls Probe on every invocation; the memoized
// value means the second call and the ten-thousandth cost nothing. The
// memoized outcome is sticky, including a failure — a probe that silently
// re-ran would make the recorded version unreproducible across a run, and
// installing the runtime after boot is a restart, which is the honest trade.
//
// The probe reuses the context probe's machinery verbatim — the scrubbed child
// environment from buildChildEnv, probeTimeout, and the process-group kill —
// because the no-output hang is a property of the binary itself, not of the
// `debug skill` subcommand.
//
// ctx contributes its values but not its cancellation (see runVersionProbe):
// the memoized answer outlives the caller that happened to ask first, so it
// must not be decided by that caller's lifetime. The cost is that a hanging
// runtime holds the first caller for up to probeTimeout even if that caller is
// cancelled — bounded, and the reason probeTimeout exists.
func (adapter *OpencodeAdapter) Probe(ctx context.Context) Readiness {
	adapter.probeOnce.Do(func() {
		adapter.readiness = adapter.runVersionProbe(ctx)
	})
	return adapter.readiness
}

// runVersionProbe performs one `<binary> --version` subprocess and classifies
// the outcome. The start/watcher/wait order mirrors Invoke and runDebugSkill:
// cmd.Start() runs synchronously (writing cmd.Process) BEFORE watchCancellation's
// goroutine can read it, so starting the watcher first would be a data race.
func (adapter *OpencodeAdapter) runVersionProbe(ctx context.Context) Readiness {
	descriptor := adapter.Describe()
	readiness := Readiness{AdapterID: descriptor.ID, CheckedAt: time.Now().UTC()}

	// The probe's result is PROCESS-scoped, not request-scoped: it is memoized
	// for the lifetime of the process, so it must not inherit the cancellation
	// of whichever caller happened to reach it first. Without this, cancelling
	// the task that triggered the first probe would pin "runtime not ready"
	// for every later run until a restart. Values are kept (tracing); only
	// cancellation is dropped, and probeTimeout remains the bound.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	defer cancel()

	bin, err := exec.LookPath(adapter.binary)
	if err != nil {
		readiness.Reason = "binary not found"
		return readiness
	}

	cmd := exec.CommandContext(probeCtx, bin, "--version")
	setProcessGroup(cmd)
	// The same credential-scrubbed environment an invocation runs under: the
	// empty profile grants nothing, so every credential-shaped variable is
	// dropped — a version probe has no reason to see the operator's secrets.
	// No permission config is injected; none is needed to report a version.
	cmd.Env = buildChildEnv(caps.Profile{}, "", nil)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if startErr := cmd.Start(); startErr != nil {
		readiness.Reason = fmt.Sprintf("start: %v", startErr)
		return readiness
	}
	reaped := make(chan struct{})
	watcherDone := watchCancellation(probeCtx, cmd, reaped)
	runErr := cmd.Wait()
	close(reaped)
	<-watcherDone

	if ctxErr := probeCtx.Err(); ctxErr != nil {
		// Only the deadline can fire now (the parent's cancellation is
		// detached above), but the two are still reported apart: a recorded
		// reason is evidence, and "timeout" for something that was cancelled
		// would send a reader looking for a slow runtime that never existed.
		readiness.Reason = "timeout"
		if !errors.Is(ctxErr, context.DeadlineExceeded) {
			readiness.Reason = fmt.Sprintf("cancelled: %v", ctxErr)
		}
		return readiness
	}
	if runErr != nil {
		reason := fmt.Sprintf("exit: %v", runErr)
		if errText := strings.TrimSpace(stderr.String()); errText != "" {
			reason += ": " + errText
		}
		readiness.Reason = reason
		return readiness
	}

	version, parseErr := parseVersionOutput(stdout.String())
	if parseErr != nil {
		readiness.Reason = parseErr.Error()
		return readiness
	}
	readiness.Ready = true
	readiness.RuntimeVersion = version
	return readiness
}

// parseVersionOutput extracts the runtime version from `--version` output:
// trim whitespace, take the first line, require a version-shaped token. Output
// that is empty, or whose first line does not look like a version, is an
// unparseable probe result rather than a recorded lie.
func parseVersionOutput(output string) (string, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return "", fmt.Errorf("empty output")
	}
	firstLine := trimmed
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	firstLine = strings.TrimSpace(firstLine)
	if !isVersionShaped(firstLine) {
		return "", fmt.Errorf("unparseable output %q", firstLine)
	}
	return firstLine, nil
}

// isVersionShaped reports whether the token plausibly is a version: it starts
// with a digit (1.18.11) or a v-prefixed digit (v1.2.3), and carries no
// whitespace. Anything else — a banner, a name, an error printed on stdout —
// must not be recorded as the runtime version.
func isVersionShaped(token string) bool {
	if token == "" || strings.ContainsAny(token, " \t") {
		return false
	}
	if token[0] >= '0' && token[0] <= '9' {
		return true
	}
	return len(token) > 1 && token[0] == 'v' && token[1] >= '0' && token[1] <= '9'
}
