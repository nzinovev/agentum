package agent

import (
	"bufio"
	"context"
	"errors"
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
// text, because an API surface and a future screen consume it.
type ModelCheck struct {
	// Model is the id exactly as configured — "provider/model" for a runtime
	// that shapes its names that way, a bare name for one that does not.
	Model string
	// Variant is the reasoning variant of the checked tier, empty when the
	// tier declares none.
	Variant string
	// Tier is set when the check ran against a configured tier.
	Tier string
	// Outcome is one of the ModelCheck* constants.
	Outcome string
	// Latency is the time to the first observable output from the runtime,
	// zero when there was none. What counts as output is the adapter's
	// knowledge — a stream line for a subprocess runtime, a first token or
	// response for one reached another way.
	Latency   time.Duration
	Reason    string // text for any non-ok outcome
	CheckedAt time.Time
}

// ModelTester is the optional adapter capability of checking one model by
// actually invoking the runtime with it — the one answer no free signal
// gives: a model can sit in the catalog, declared active, and still hang
// every run that uses it. Discovered by comma-ok type assertion, like
// ContextProber, so the Adapter interface grows no method most runtimes
// cannot honor.
//
// Three rules bind every implementation, because the outcomes are compared
// and rendered side by side and must mean the same thing whichever runtime
// produced them:
//
//  1. SUCCESS IS THE FIRST OBSERVABLE OUTPUT, not a complete answer. A model
//     that answers is distinguishable from one that hangs by the very first
//     thing it emits; waiting for the whole answer buys no new information
//     and bills for tokens nobody asked for. An implementation stops the
//     runtime as soon as that first output arrives. The known cost is stated
//     rather than hidden: a model that emits once and then stalls passes this
//     check, and catching THAT silence on real runs belongs to the
//     first-event watchdog and the idle cap.
//  2. THE CHECK IS NO WIDER THAN A REAL INVOCATION. It starts the runtime
//     under the same capability boundary an invocation gets — a profile that
//     grants nothing, and whatever the adapter uses to enforce it. A
//     diagnostic is not a reason to hand a model a machine that an invocation
//     would not get.
//  3. NO MEMOIZATION AND NO SIDE EFFECTS. Each call actually reaches the
//     model, or "checked again after a fix" stops working; and it writes no
//     database rows, emits no events, and touches no manifest — delivery
//     belongs to the layer that calls it.
type ModelTester interface {
	TestModel(ctx context.Context, selection models.Selection, deadline time.Duration) ModelCheck
}

// TestModel implements ModelTester for opencode. The selection is validated
// and the catalog consulted first and for free: an inconsistent option set, a
// model the runtime does not list, or a variant outside the model's declared
// vocabulary are answered with no invocation and no bill. Otherwise one `run`
// with a trivial prompt starts in a temporary directory under a
// deny-everything permission config and the scrubbed environment; the first
// stdout line is this runtime's observable output, so the process group is
// stopped the moment it arrives and the check costs a few tokens rather than
// a whole answer. A deadline with no line is a timeout, and the group is
// killed.
func (adapter *OpencodeAdapter) TestModel(ctx context.Context, selection models.Selection, deadline time.Duration) ModelCheck {
	descriptor := adapter.Describe()
	check := ModelCheck{
		Model:     selection.Options.Model,
		Variant:   selection.Options.Variant,
		Tier:      selection.Tier,
		CheckedAt: time.Now().UTC(),
	}
	// The same three refusals an invocation gets, paid before any subprocess:
	// consistency, the declared option set, the catalog. A check that started
	// the runtime anyway would bill for an answer the configuration can never
	// use. All three speak Invoke's "execution adapter %q" lead-in — one fact,
	// one wording, whichever entry refused.
	if validateErr := selection.Options.Validate(); validateErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("execution adapter %q: %v", descriptor.ID, validateErr)
		return check
	}
	if optionErr := selection.Options.SupportedBy(descriptor.ModelOptions); optionErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("execution adapter %q: %v", descriptor.ID, optionErr)
		return check
	}
	if catalogErr := adapter.Catalog(ctx).Validate(selection); catalogErr != nil {
		// Only an unknown MODEL is the unknown_model outcome — the diagnostic's
		// own question, "is this model reachable"; every other catalog refusal
		// (a variant outside the vocabulary) is the generic error with its
		// cause.
		if errors.Is(catalogErr, models.ErrUnknownModel) {
			check.Outcome = ModelCheckUnknownModel
		} else {
			check.Outcome = ModelCheckError
		}
		check.Reason = fmt.Sprintf("execution adapter %q: %v", descriptor.ID, catalogErr)
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

	// The check runs under the same boundary an invocation gets: a config
	// rendered from a profile that grants nothing, handed to the child through
	// its environment, plus the credential scrub. --auto below auto-approves
	// what is not explicitly denied, and this config denies everything — it is
	// there because a permission request in a non-interactive run has nobody
	// to answer it, and the check would hang instead of reporting.
	checkProfile := caps.Profile{}
	permissionConfig, configErr := buildOpencodeConfig(
		checkProfile, scopeSubst{worktree: workdir, artifact: workdir}, nil)
	if configErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("render check config: %v", configErr)
		return check
	}
	compactConfig, _, renderErr := renderOpencodeConfigBytes(permissionConfig)
	if renderErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("render check config: %v", renderErr)
		return check
	}

	testCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	bin, lookErr := exec.LookPath(adapter.binary)
	if lookErr != nil {
		check.Outcome = ModelCheckError
		check.Reason = fmt.Sprintf("binary %q not found", adapter.binary)
		return check
	}
	args := []string{bin, "run", "--format", "json", "--auto"}
	args = appendModelOptionArgs(args, selection.Options)
	args = append(args, modelTestPrompt)
	cmd := exec.CommandContext(testCtx, args[0], args[1:]...)
	setProcessGroup(cmd)
	cmd.Dir = workdir
	cmd.Env = buildChildEnv(checkProfile, "", compactConfig)

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
	// terminateDone joins the ok-path terminator before the check returns, so
	// no goroutine of this call outlives it.
	var terminateDone chan struct{}
	select {
	case <-firstLine:
		stopped = true
		check.Outcome = ModelCheckOK
		check.Latency = time.Since(started)
		// Success is the first line; everything after it is an answer nobody
		// asked to pay for. Terminate in its own goroutine: the escalation
		// path waits for the reap that only the epilogue below performs.
		terminateDone = make(chan struct{})
		go func() {
			defer close(terminateDone)
			terminateProcessGroup(cmd, reaped)
		}()
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
	if terminateDone != nil {
		<-terminateDone
	}

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
