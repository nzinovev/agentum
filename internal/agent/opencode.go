package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// OpencodeAdapter drives the `opencode` CLI as a subprocess: one invocation
// per stage, NDJSON events on stdout, result.json read from ArtifactDir after
// the run. Resume via --session. Cancellation kills the process group.
//
// Construction: NewOpencodeAdapter("opencode") resolves the binary via PATH;
// pass an absolute path to pin it.
type OpencodeAdapter struct {
	binary string // path to the opencode executable

	// firstEventTimeout bounds how long an invocation may stay totally
	// silent from start (zero disables the watchdog). Host-level operator
	// configuration, applied by the registry at construction; deliberately
	// not part of the capability profile, which answers a different question.
	firstEventTimeout time.Duration

	// probeOnce / readiness memoize the runtime version probe: one subprocess per
	// process lifetime, shared by every consumer.
	probeOnce sync.Once
	readiness Readiness

	// catalogOnce / catalog memoize the runtime model listing the same way:
	// one subprocess per process, sticky including a failure.
	catalogOnce sync.Once
	catalog     models.Catalog
}

// NewOpencodeAdapter returns an adapter that invokes the named binary. An
// empty name selects the descriptor's default — "which executable am I" is
// the adapter's own knowledge, so a caller with no operator override passes
// nothing rather than looking the default up first. The binary is resolved
// lazily via exec.LookPath on each Invoke.
func NewOpencodeAdapter(binary string) *OpencodeAdapter {
	if binary == "" {
		binary = opencodeDescriptor.Binary
	}
	return &OpencodeAdapter{binary: binary}
}

// errPrefix is the "<id> adapter: " lead-in for this adapter's errors. The id
// comes from the descriptor, never a literal in a message — the executor is
// named only in its own declaration, and an error message is calling code like
// anywhere else.
func (adapter *OpencodeAdapter) errPrefix() string {
	return string(adapter.Describe().ID) + " adapter: "
}

// Invoke implements Adapter. It starts `opencode run --format json`, forwards
// stream events on the returned channel, and on completion reads + parses
// ArtifactDir/result.json. On ctx cancellation it kills the process group and
// emits EventError.
//
// Enforcement is applied before the subprocess starts: the model selection is
// confirmed to carry only options this adapter declared, the effective profile
// is confirmed enforceable, a per-invocation opencode permission config is
// rendered outside the worktree and handed to the child through its
// environment, the environment is credential-scrubbed, and the profile's
// hard/idle timeouts wrap ctx. A profile that grants a capability the adapter
// cannot enforce — or a selection carrying an undeclared option — returns an
// error here; the invocation does not start.
func (adapter *OpencodeAdapter) Invoke(ctx context.Context, inv Invocation) (<-chan Event, error) {
	descriptor := adapter.Describe()
	// Defence in depth: re-check what was handed in, regardless of what the
	// caller believed. Inconsistent options (a variant without a model, a
	// whitespace value) and an option outside the descriptor's set are both
	// refused here and no subprocess starts.
	if err := inv.Model.Options.Validate(); err != nil {
		return nil, fmt.Errorf("execution adapter %q: %w", descriptor.ID, err)
	}
	if err := inv.Model.Options.SupportedBy(descriptor.ModelOptions); err != nil {
		return nil, fmt.Errorf("execution adapter %q: %w", descriptor.ID, err)
	}
	// The model string is the one input that reaches the runtime verbatim, so
	// it is checked against the catalog here too — a selection assembled by any
	// path, not only run-start resolution. The check costs one memoized probe
	// per process, and an unavailable catalog validates as nil: "could not
	// check" never becomes "does not exist". The same call grounds the variant
	// vocabulary check when the catalog read it.
	if err := adapter.Catalog(ctx).Validate(inv.Model); err != nil {
		return nil, fmt.Errorf("execution adapter %q: %w", descriptor.ID, err)
	}
	if err := adapter.validateInvocation(inv); err != nil {
		return nil, err
	}
	// A runtime whose version is known to be too old would fail on the first
	// flag it no longer has, after the run had already been recorded as
	// started. A probe that could not read the version does not block: "could
	// not check" never becomes "unsupported".
	if readiness := adapter.Probe(ctx); readiness.Unsupported {
		return nil, fmt.Errorf("%s%s", adapter.errPrefix(), readiness.Reason)
	}
	plan, err := adapter.prepareEnforcement(inv)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(adapter.binary)
	if err != nil {
		plan.cleanup()
		return nil, fmt.Errorf("%sbinary %q not found: %w", adapter.errPrefix(), adapter.binary, err)
	}

	// The run outlives Invoke: the subprocess is still working when we hand the
	// channel back. Ownership of the run context therefore transfers to run(),
	// which releases it once the stream is drained and the process reaped.
	// Releasing it here — the shape a plain `defer` would give — cancels the
	// context cmd is bound to and kills the agent the moment Invoke returns.
	control := newRunControl(ctx, plan.timeout.hard)

	args := buildOpencodeArgs(bin, inv, plan.attachments)
	cmd := exec.CommandContext(control.ctx, args[0], args[1:]...)
	setProcessGroup(cmd)
	cmd.Dir = inv.Workdir
	// Credential-scrubbed environment: every var in the deny list is dropped
	// unless the profile grants the matching secret.<name>. Passing the env
	// explicitly also means the child never inherits interactive-shell extras
	// the parent may have carried, and it is how the permission config reaches
	// opencode (OPENCODE_CONFIG_CONTENT).
	cmd.Env = withWorkingDirectory(plan.env, inv.Workdir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		control.release()
		plan.cleanup()
		return nil, fmt.Errorf("%sstdout pipe: %w", adapter.errPrefix(), err)
	}
	// Only the tail of stderr is kept: it is where opencode explains an exit
	// it reports nowhere else (an unknown flag, a broken config), and without
	// it the run records a bare exit status.
	stderrTail := newTailBuffer(stderrTailLimit)
	cmd.Stderr = stderrTail

	if err := cmd.Start(); err != nil {
		control.release()
		plan.cleanup()
		return nil, fmt.Errorf("%sstart: %w", adapter.errPrefix(), err)
	}

	ch := make(chan Event, 16)
	go adapter.run(control, cmd, stdout, stderrTail, inv, ch, plan)
	return ch, nil
}

// runControl owns the lifetime of one invocation: the context every part of the
// run shares, the cancel that releases it, and why the run was stopped. It
// exists because that lifetime spans two functions — Invoke starts the
// subprocess, run() finishes it — and a context released at the wrong seam
// terminates the agent mid-work.
type runControl struct {
	ctx    context.Context
	cancel context.CancelFunc
	idle   atomic.Bool
	// firstEvent records that the run was stopped by the first-event
	// watchdog — a distinct stop cause from the idle cap, so the terminal
	// error can say which silence it was.
	firstEvent atomic.Bool
}

// newRunControl wraps the parent context with the profile's hard cap.
func newRunControl(parent context.Context, limits caps.Profile) *runControl {
	ctx, cancel := applyTimeouts(parent, limits)
	return &runControl{ctx: ctx, cancel: cancel}
}

// release cancels the run context. Called exactly once, by whoever holds
// ownership at the time: Invoke on a start failure, run() at the end.
func (control *runControl) release() { control.cancel() }

// stopIdle cancels the run because the idle cap elapsed. The flag outlives the
// cancellation so the terminal event names the real reason rather than a
// generic "cancelled".
func (control *runControl) stopIdle() {
	control.idle.Store(true)
	control.cancel()
}

// stopFirstEvent cancels the run because nothing at all was emitted within the
// first-event bound. Same flag pattern as stopIdle, for the same reason.
func (control *runControl) stopFirstEvent() {
	control.firstEvent.Store(true)
	control.cancel()
}

// stopReason renders why a cancelled run ended, for the terminal EventError.
// The timeouts name the model: a silence has very different fixes depending
// on whether the model is misbehaving or the agent is legitimately working,
// and the model string is the one fact this error can add that the stop
// reason vocabulary deliberately does not carry.
func (control *runControl) stopReason(model string, idleTimeout, firstEventTimeout time.Duration) error {
	switch {
	case control.firstEvent.Load():
		return fmt.Errorf("opencode run stopped: no output for %s before the first stream event, model %q", firstEventTimeout, model)
	case control.idle.Load():
		return fmt.Errorf("opencode run stopped: no output for %s (idle cap), model %q", idleTimeout, model)
	case errors.Is(control.ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("opencode run stopped: hard timeout exceeded: %w", control.ctx.Err())
	default:
		return fmt.Errorf("opencode run cancelled: %w", control.ctx.Err())
	}
}

// Supported reports the capability categories the opencode adapter can
// technically enforce. The runner intersects this with pack / stage / role
// inputs; the adapter re-checks the effective profile at Invoke time as
// defense-in-depth (a profile built by any other path must still be enforceable
// before the subprocess may start).
func (adapter *OpencodeAdapter) Supported() []caps.Category {
	return append([]caps.Category(nil), opencodeSupported...)
}

func (adapter *OpencodeAdapter) run(control *runControl, cmd *exec.Cmd, stdout io.Reader, stderrTail *tailBuffer, inv Invocation, ch chan<- Event, plan enforcementPlan) {
	// Ownership of the run context and of the materialized enforcement plan
	// transfers here from Invoke. Both are released only after the process is
	// reaped: the rendered config backs the child's permission set for as long
	// as it runs, and the context is what keeps the child alive at all.
	defer close(ch)
	defer plan.cleanup()
	defer control.release()

	state := &invokeState{}
	// reaped closes once cmd.Wait has returned. Both watchers below retire on
	// it, so neither can signal a pid the OS has already recycled.
	reaped := make(chan struct{})

	cancelWatcher := watchCancellation(control.ctx, cmd, reaped)
	// First-event watchdog: a runtime that has produced NOTHING since it
	// started is not working, it is hung — a working one emits its first event
	// in fractions of a second. The watchdog is disabled forever after the
	// first line, so legitimate long work (which begins with an event) never
	// falls under it; silence mid-work belongs to the idle cap below. No-op
	// when the bound is zero.
	retireFirstEvent := startFirstEventWatcher(control, adapter.firstEventTimeout, reaped)
	// Idle timeout: a watcher resets a timer on every observed stream chunk; if
	// no chunk arrives within the profile's IdleTimeout the run is cancelled,
	// which the cancellation watcher turns into a process-group termination.
	// No-op when IdleTimeout is zero.
	idleReset := startIdleWatcher(control, plan.timeout.hard.IdleTimeout, reaped)

	scanner := bufio.NewScanner(stdout)
	// opencode tool input can be large; raise the per-line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawFirstLine := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !sawFirstLine {
			sawFirstLine = true
			if retireFirstEvent != nil {
				retireFirstEvent()
			}
		}
		if idleReset != nil {
			idleReset()
		}
		if ev, ok := state.ingest(line); ok && ev != nil {
			ch <- *ev
		}
	}
	// stdout EOF: the agent has finished (or been terminated). Wait closes the
	// pipe, so it must come after the last read — and it must happen exactly
	// once, which is why this is the only Wait call in the adapter.
	waitErr := cmd.Wait()
	close(reaped)
	<-cancelWatcher

	if control.ctx.Err() != nil {
		ch <- Event{Kind: EventError, Err: control.stopReason(inv.Model.Options.Model, plan.timeout.hard.IdleTimeout, adapter.firstEventTimeout)}
		return
	}
	if err := state.scannerErr(scanner); err != nil {
		ch <- Event{Kind: EventError, Err: fmt.Errorf("opencode stream read: %w", err)}
		return
	}
	if state.runtimeErr != nil {
		ch <- Event{Kind: EventError, Err: fmt.Errorf("opencode run model %q: %w", inv.Model.Options.Model, state.runtimeErr)}
		return
	}
	if waitErr != nil {
		ch <- Event{Kind: EventError, Err: withStderr(fmt.Errorf("opencode run model %q: %w", inv.Model.Options.Model, waitErr), stderrTail)}
		return
	}

	res, err := assembleResult(inv, state)
	if err != nil {
		ch <- Event{Kind: EventError, Err: err}
		return
	}
	ch <- Event{Kind: EventResult, Result: res}
}

// invokeState accumulates stream-derived fields across the run.
type invokeState struct {
	sessionID   string
	telemetry   Telemetry
	snapshot    string
	activity    []Activity
	scannerErr_ error
	runtimeErr  error
}

func (s *invokeState) scannerErr(sc *bufio.Scanner) error { return sc.Err() }

// activitySummary renders the observed tool calls as "tool(target)=status",
// for the contract-failure error. Returns "(none observed)" when the agent
// called no tools at all — itself the answer to "was the write denied, or never
// attempted?".
func (s *invokeState) activitySummary() string {
	if len(s.activity) == 0 {
		return "(none observed)"
	}
	parts := make([]string, 0, len(s.activity))
	for _, call := range s.activity {
		entry := call.Tool
		if call.Target != "" {
			entry += "(" + call.Target + ")"
		}
		parts = append(parts, entry+"="+call.Status)
	}
	return strings.Join(parts, ", ")
}

// ingest parses one NDJSON line, updates state, and returns an event to emit
// (ok=false means "drop the line", e.g. malformed lines that aren't worth
// failing on — though we currently fail on the first unparseable line).
func (s *invokeState) ingest(line []byte) (*Event, bool) {
	var env struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionID"`
		Part      json.RawMessage `json:"part"`
		Error     json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		// Not JSON at all — surface as a stream chunk so the UI still shows it.
		text := strings.TrimSpace(string(line))
		if text == "" {
			return nil, false
		}
		return &Event{Kind: EventStream, Chunk: text}, true
	}
	if env.SessionID != "" && s.sessionID == "" {
		s.sessionID = env.SessionID
	}

	switch env.Type {
	case stepStart:
		var p stepStartPart
		if err := json.Unmarshal(env.Part, &p); err == nil && p.Snapshot != "" {
			s.snapshot = p.Snapshot
		}
		return nil, true
	case textEvent:
		var p textPart
		if err := json.Unmarshal(env.Part, &p); err != nil {
			return nil, true
		}
		return &Event{Kind: EventStream, Chunk: p.Text}, true
	case toolUse:
		var p toolPart
		if err := json.Unmarshal(env.Part, &p); err == nil {
			s.activity = append(s.activity, Activity{
				Tool:   p.Tool,
				Status: p.State.Status,
				Target: p.target(),
			})
		}
		return nil, true
	case stepFinish:
		var p stepFinishPart
		if err := json.Unmarshal(env.Part, &p); err == nil {
			s.accumulateFinish(p)
		}
		return nil, true
	case "error":
		if s.runtimeErr == nil {
			s.runtimeErr = parseRuntimeError(env.Error)
		}
		return nil, true
	default:
		// Unknown event type: forward its raw form as an opaque stream chunk
		// (forward-compatible — opencode will add types over time).
		return &Event{Kind: EventStream, Chunk: fmt.Sprintf("[event:%s]", env.Type)}, true
	}
}

func parseRuntimeError(raw json.RawMessage) error {
	var reported struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message    string `json:"message"`
			StatusCode int    `json:"statusCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &reported); err != nil {
		return errors.New("opencode reported an error")
	}
	message := strings.TrimSpace(reported.Data.Message)
	if message == "" {
		message = strings.TrimSpace(reported.Message)
	}
	if message == "" {
		message = strings.TrimSpace(reported.Name)
	}
	if message == "" {
		message = "opencode reported an error"
	}
	if reported.Data.StatusCode != 0 {
		return fmt.Errorf("%s (HTTP %d)", message, reported.Data.StatusCode)
	}
	return errors.New(message)
}

func (s *invokeState) accumulateFinish(p stepFinishPart) {
	s.telemetry.Tokens.Total += p.Tokens.sum()
	s.telemetry.Tokens.Input += p.Tokens.Input
	s.telemetry.Tokens.Output += p.Tokens.Output
	s.telemetry.Tokens.Reasoning += p.Tokens.Reasoning
	s.telemetry.Tokens.CacheRead += p.Tokens.Cache.Read
	s.telemetry.Tokens.CacheWrite += p.Tokens.Cache.Write
	s.telemetry.Cost += p.Cost
	if p.Snapshot != "" {
		s.snapshot = p.Snapshot
	}
}

func assembleResult(inv Invocation, s *invokeState) (*Result, error) {
	path := filepath.Join(inv.ArtifactDir, "result.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Include what the agent actually did. "No result.json" has two very
		// different causes — the write was refused, or the agent never attempted
		// it — and they need opposite fixes. Without the tool trace neither an
		// operator nor a test can tell them apart, because the agent's prose is
		// not evidence.
		return nil, fmt.Errorf("read result.json at %s: %w (agent did not produce the required contract file; tool calls: %s)",
			path, err, s.activitySummary())
	}
	rj, err := ParseResultJSON(data)
	if err != nil {
		return nil, err
	}
	return &Result{
		SessionID:  s.sessionID,
		Telemetry:  s.telemetry,
		Snapshot:   s.snapshot,
		Activity:   s.activity,
		ResultJSON: rj,
	}, nil
}

// buildOpencodeArgs assembles the argv: ["opencode", "run", "--standalone",
// "--format", "json", "--auto", ...optional, <message>]. Model options are read
// from the typed selection field by field — there is no path where a caller
// appends a string to argv.
//
// --standalone is the enforcement, not a tuning flag. Without it opencode
// hands the run to its shared background service, which was started with its
// own environment and ignores the OPENCODE_CONFIG_CONTENT this child carries:
// the permission config and the credential scrub would both be void while the
// audit trail still recorded them. The private server lives inside this
// process, so the process-group kill stops it too.
//
// The working directory is the process's own (cmd.Dir); opencode has no flag
// for it. Each attachment is a pinned instruction file staged outside the
// worktree, passed with --file because opencode 2 parses the config's
// `instructions` key but never loads the files it names.
func buildOpencodeArgs(bin string, inv Invocation, attachments []string) []string {
	args := []string{bin, "run", "--standalone", "--format", "json", "--auto"}
	if inv.ResumeSession != "" {
		args = append(args, "--session", inv.ResumeSession)
	}
	args = appendModelOptionArgs(args, inv.Model.Options)
	for _, attachment := range attachments {
		args = append(args, "--file", attachment)
	}
	args = append(args, inv.Prompt+"\n\n"+inv.RoutingBlock)
	return args
}

// appendModelOptionArgs appends the model option flag. opencode spells the
// variant as a suffix of the model (`provider/model#variant`), so a variant
// without a model has nowhere to go and the two render as one argument. An
// empty variant never yields a suffix, not even a bare `#`. The ONE
// caller-facing contract: options must already be validated and declared
// (models.Options.Validate, SupportedBy); this helper only renders argv and is
// shared by Invoke and the model check so the two cannot drift apart.
func appendModelOptionArgs(args []string, options models.Options) []string {
	if options.Model == "" {
		return args
	}
	model := options.Model
	if options.Variant != "" {
		model += "#" + options.Variant
	}
	return append(args, "--model", model)
}

func (adapter *OpencodeAdapter) validateInvocation(inv Invocation) error {
	if inv.Workdir == "" {
		return fmt.Errorf("%sWorkdir is required", adapter.errPrefix())
	}
	if inv.ArtifactDir == "" {
		return fmt.Errorf("%sArtifactDir is required", adapter.errPrefix())
	}
	if strings.TrimSpace(inv.Prompt) == "" {
		return fmt.Errorf("%sPrompt is required", adapter.errPrefix())
	}
	return nil
}

// killGrace is how long a cancelled run has to exit on its own before it is
// terminated outright. Long enough for opencode to flush a partial result,
// short enough that a stuck agent cannot hold a worktree indefinitely. A var
// rather than a const so the subprocess tests can shrink it; nothing in
// production reassigns it.
var killGrace = 5 * time.Second

// terminateProcessGroup asks the child's process group to stop, then forces it
// if the process has not been reaped within killGrace. The escalation is
// platform-neutral; the signalling primitive is per-GOOS (process_unix.go /
// process_windows.go).
func terminateProcessGroup(cmd *exec.Cmd, reaped <-chan struct{}) {
	signalProcessGroup(cmd, false)
	select {
	case <-reaped:
	case <-time.After(killGrace):
		signalProcessGroup(cmd, true)
	}
}

// watchCancellation terminates the process group when the run context is
// cancelled (caller stop, hard timeout, idle cap). exec.CommandContext would
// kill only the group leader, and opencode's helpers are not the leader.
// Returns a channel closed once the watcher has retired, so run() never leaves
// a goroutine holding a stale pid.
func watchCancellation(ctx context.Context, cmd *exec.Cmd, reaped <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			terminateProcessGroup(cmd, reaped)
		case <-reaped:
		}
	}()
	return done
}

// applyTimeouts wraps ctx with the profile's HardTimeout when non-zero. The
// returned cancel func releases the timer's resources; callers MUST defer it.
// IdleTimeout is enforced stream-side in run() (startIdleWatcher) since it
// depends on observed progress, not wall-clock.
func applyTimeouts(ctx context.Context, profile caps.Profile) (context.Context, context.CancelFunc) {
	if profile.HardTimeout > 0 {
		return context.WithTimeout(ctx, profile.HardTimeout)
	}
	return context.WithCancel(ctx)
}

// startFirstEventWatcher launches a goroutine that cancels the run when no
// output at all arrives within firstEventTimeout of start. It returns a retire
// function the scanner loop calls on the first non-empty line — the watchdog's
// job ends there, and everything after is the idle cap's territory. Calling
// retire after the run ends is harmless (the goroutine has retired on reaped).
// Returns nil when firstEventTimeout is zero (watchdog disabled).
func startFirstEventWatcher(control *runControl, firstEventTimeout time.Duration, reaped <-chan struct{}) func() {
	if firstEventTimeout <= 0 {
		return nil
	}
	timer := time.NewTimer(firstEventTimeout)
	retired := make(chan struct{})
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C:
			control.stopFirstEvent()
		case <-retired:
		case <-reaped:
		case <-control.ctx.Done():
		}
	}()
	return func() { close(retired) }
}

// startIdleWatcher launches a goroutine that cancels the run when no stream
// chunk arrives within idleTimeout. It returns a reset function the scanner
// loop calls on every chunk; calling reset after the run ends is harmless.
// Returns nil when idleTimeout is zero (no idle cap configured).
//
// The watcher only cancels — terminating the process group is the cancellation
// watcher's job, so there is exactly one path from "this run must stop" to
// "this process tree is gone".
func startIdleWatcher(control *runControl, idleTimeout time.Duration, reaped <-chan struct{}) func() {
	if idleTimeout <= 0 {
		return nil
	}
	timer := time.NewTimer(idleTimeout)
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C:
			control.stopIdle()
		case <-reaped:
		case <-control.ctx.Done():
		}
	}()
	return func() { timer.Reset(idleTimeout) }
}

// NDJSON event type strings (top-level).
const (
	stepStart  = "step_start"
	stepFinish = "step_finish"
	textEvent  = "text"
	toolUse    = "tool_use"
)

// part structs (the inner `part` object).

type stepStartPart struct {
	Type     string `json:"type"`
	Snapshot string `json:"snapshot"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolPart struct {
	Type  string `json:"type"`
	Tool  string `json:"tool"`
	State struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
	} `json:"state"`
}

// target extracts a human-useful target from the tool input (filePath / path /
// command). Best-effort; empty when nothing recognizable.
func (p toolPart) target() string {
	var m map[string]any
	if err := json.Unmarshal(p.State.Input, &m); err != nil {
		return ""
	}
	for _, k := range []string{"filePath", "path", "command"} {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

type stepFinishPart struct {
	Type     string     `json:"type"`
	Reason   string     `json:"reason"`
	Tokens   tokensPart `json:"tokens"`
	Cost     float64    `json:"cost"`
	Snapshot string     `json:"snapshot"`
}

// sum is the step's token total. opencode 2 no longer reports `total`, so it
// is the sum of the parts; a reported total still wins, because it is the
// runtime's own figure.
func (tokens tokensPart) sum() int64 {
	if tokens.Total != 0 {
		return tokens.Total
	}
	return tokens.Input + tokens.Output + tokens.Reasoning + tokens.Cache.Read + tokens.Cache.Write
}

type tokensPart struct {
	Total     int64 `json:"total"`
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// stderrTailLimit bounds how much of a runtime's stderr an error carries:
// enough for the last message, not enough to copy a transcript into a run's
// error column.
const stderrTailLimit = 2048

// tailBuffer keeps the last limit bytes written to it. A subprocess writes
// stderr from its own goroutine inside os/exec, so writes and reads are
// serialized.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (tail *tailBuffer) Write(chunk []byte) (int, error) {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	tail.data = append(tail.data, chunk...)
	if overflow := len(tail.data) - tail.limit; overflow > 0 {
		tail.data = append([]byte(nil), tail.data[overflow:]...)
	}
	return len(chunk), nil
}

func (tail *tailBuffer) String() string {
	tail.mu.Lock()
	defer tail.mu.Unlock()
	return strings.TrimSpace(string(tail.data))
}

// withStderr appends the runtime's last stderr output to err, when there is
// any.
func withStderr(err error, stderrTail *tailBuffer) error {
	if stderrTail == nil {
		return err
	}
	if text := stderrTail.String(); text != "" {
		return fmt.Errorf("%w: %s", err, text)
	}
	return err
}
