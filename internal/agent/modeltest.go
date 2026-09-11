package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// Model check outcomes. Stable strings the API surface and its events carry.
const (
	ModelCheckOK           = "ok"
	ModelCheckUnknownModel = "unknown_model"
	ModelCheckTimeout      = "timeout"
	ModelCheckError        = "error"
)

// modelTestDefaultDeadline is the check's own deadline when the caller passes
// none: long enough for a slow first event, short enough that a hanging model
// does not hold the operator's attention for minutes.
const modelTestDefaultDeadline = 60 * time.Second

// modelTestPrompt is the trivial prompt one check runs with. The check asks
// exactly one question — does this model answer at all — so the prompt is
// chosen to cost tokens, not attention.
const modelTestPrompt = "Say hi"

// ModelCheck is the outcome of one on-demand model check: a structure, not
// text, because an API surface and a future screen consume it. Latency is the
// time to the FIRST stream line (zero when there was none) — a working model
// produces its first event in fractions of a second, and a model that emits
// one line and goes quiet passes this check by design; catching that
// mid-stream silence is the idle cap's job on real runs, not the check's.
type ModelCheck struct {
	Model     string        // "provider/model"
	Variant   string        // from the checked tier, empty when it declares none
	Tier      string        // when the check ran against a configured tier
	Outcome   string        // ok | unknown_model | timeout | error
	Latency   time.Duration // to the first line; zero when there was none
	Reason    string        // text for any non-ok outcome
	CheckedAt time.Time
}

// ModelTester is the optional adapter capability of checking one model by
// actually invoking the runtime with it — the one answer no free signal
// gives: a model can sit in the catalog, declared active, and still hang
// every run that uses it. Discovered by comma-ok type assertion, like
// ContextProber, so the Adapter interface grows no method most runtimes
// cannot honor.
type ModelTester interface {
	// TestModel invokes the runtime once with the selection and reports
	// whether it produced output. It is NOT memoized: each call honestly goes
	// to the model, or "checked again after a fix" stops working. It writes
	// no database rows, emits no events, and touches no manifest — it is a
	// diagnostic, not a run; delivery belongs to the layer that calls it.
	TestModel(ctx context.Context, selection models.Selection, deadline time.Duration) ModelCheck
}

// TestModel implements ModelTester. The catalog is consulted first and for
// free: a model the runtime does not even list is unknown_model with no
// invocation and no bill. Otherwise one `run` with a trivial prompt starts in
// a temporary directory under the probes' scrubbed environment; the first
// stdout line is the success signal and the process group is stopped the
// moment it arrives, so the check costs a few tokens rather than a whole
// answer. A deadline with no line is a timeout, and the group is killed.
func (adapter *OpencodeAdapter) TestModel(ctx context.Context, selection models.Selection, deadline time.Duration) ModelCheck {
	check := ModelCheck{
		Model:     selection.Options.Model,
		Tier:      selection.Tier,
		CheckedAt: time.Now().UTC(),
	}
	if check.Model == "" {
		check.Outcome = ModelCheckError
		check.Reason = "no model configured"
		return check
	}
	if catalogErr := adapter.Catalog(ctx).Validate(selection); catalogErr != nil {
		check.Outcome = ModelCheckUnknownModel
		check.Reason = catalogErr.Error()
		return check
	}
	if deadline <= 0 {
		deadline = modelTestDefaultDeadline
	}

	workdir, workdirErr := os.MkdirTemp("", "agentum-model-check-")
	if workdirErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("create workdir: %v", workdirErr)
		return check
	}
	defer os.RemoveAll(workdir)

	testCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	bin, lookErr := exec.LookPath(adapter.binary)
	if lookErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("binary %q not found", adapter.binary)
		return check
	}
	args := []string{bin, "run", "--format", "json", "--auto", "--model", selection.Options.Model, modelTestPrompt}
	cmd := exec.CommandContext(testCtx, args[0], args[1:]...)
	setProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.Env = buildChildEnv(caps.Profile{}, "", nil)

	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("stdout pipe: %v", pipeErr)
		return check
	}
	cmd.Stderr = io.Discard

	started := time.Now()
	if startErr := cmd.Start(); startErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("start: %v", startErr)
		return check
	}
	reaped := make(chan struct{})
	watcherDone := watchCancellation(testCtx, cmd, reaped)

	// firstLine fires once, on the first non-empty stdout line; scanDone
	// closes when stdout ends (EOF or read error) — the process-exited
	// signal for a run that produced nothing at all.
	firstLine := make(chan struct{}, 1)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		sawLine := false
		for scanner.Scan() {
			if !sawLine && len(scanner.Bytes()) > 0 {
				sawLine = true
				firstLine <- struct{}{}
			}
		}
	}()

	stopped := false
	select {
	case <-firstLine:
		stopped = true
		check.Outcome = ModelCheckOK
		check.Latency = time.Since(started)
		// Success is the first line; everything after it is an answer nobody
		// asked to pay for. Terminate in its own goroutine: the escalation
		// path waits for the reap that only the epilogue below performs.
		go terminateProcessGroup(cmd, reaped)
	case <-testCtx.Done():
		check.Outcome = ModelCheckTimeout
		check.Reason = fmt.Sprintf("no output within %s", deadline)
		// The cancellation watcher terminates the group on this same ctx.
	case <-scanDone:
		// stdout ended before any line and before the deadline: the process
		// exited (or the stream broke). Classified by the wait error below.
	}

	<-scanDone
	waitErr := cmd.Wait()
	close(reaped)
	<-watcherDone

	if !stopped && check.Outcome != ModelCheckTimeout {
		check.Outcome = ModelCheckError
		if waitErr != nil {
			check.Reason = fmt.Sprintf("exit: %v", waitErr)
		} else {
			check.Reason = "no output before exit"
		}
	}
	return check
}
