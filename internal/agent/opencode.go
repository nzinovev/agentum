package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// OpencodeAdapter drives the `opencode` CLI as a subprocess: one invocation
// per stage, NDJSON events on stdout, result.json read from ArtifactDir after
// the run. Resume via --session. Cancellation kills the process group.
//
// Construction: NewOpencodeAdapter("opencode") resolves the binary via PATH;
// pass an absolute path to pin it.
type OpencodeAdapter struct {
	binary string // path to the opencode executable
}

// NewOpencodeAdapter returns an adapter that invokes the named binary. The
// binary is resolved lazily via exec.LookPath on each Invoke.
func NewOpencodeAdapter(binary string) *OpencodeAdapter {
	return &OpencodeAdapter{binary: binary}
}

// Invoke implements Adapter. It starts `opencode run --format json`, forwards
// stream events on the returned channel, and on completion reads + parses
// ArtifactDir/result.json. On ctx cancellation it kills the process group and
// emits EventError.
func (a *OpencodeAdapter) Invoke(ctx context.Context, inv Invocation) (<-chan Event, error) {
	if err := validateInvocation(inv); err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(a.binary)
	if err != nil {
		return nil, fmt.Errorf("opencode adapter: binary %q not found: %w", a.binary, err)
	}

	args := buildOpencodeArgs(bin, inv)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	// Put the child in its own process group so cancellation can kill the
	// whole tree (opencode may spawn subagents, formatters, LSP servers).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = inv.Workdir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opencode adapter: stdout pipe: %w", err)
	}
	// stderr is discarded for MVP; the print-logs flag is the debug path later.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("opencode adapter: start: %w", err)
	}

	ch := make(chan Event, 16)
	go a.run(ctx, cmd, stdout, inv, ch)
	return ch, nil
}

func (a *OpencodeAdapter) run(ctx context.Context, cmd *exec.Cmd, stdout io.Reader, inv Invocation, ch chan<- Event) {
	defer close(ch)

	state := &invokeState{}

	// If ctx is cancelled, kill the process group. exec.CommandContext already
	// kills the process on cancel, but only the leader — we need the group.
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-ctx.Done():
			killProcessGroup(cmd)
		case <-waitDoneAsync(cmd):
		}
	}()

	scanner := bufio.NewScanner(stdout)
	// opencode tool input can be large; raise the per-line limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if ev, ok := state.ingest(line); ok && ev != nil {
			ch <- *ev
		}
	}
	// stdout EOF: the agent has finished (or been killed). Wait for the process
	// to reap and for the cancel watcher to retire.
	_ = cmd.Wait()
	<-cancelDone

	if ctx.Err() != nil {
		ch <- Event{Kind: EventError, Err: fmt.Errorf("opencode run cancelled: %w", ctx.Err())}
		return
	}
	if err := state.scannerErr(scanner); err != nil {
		ch <- Event{Kind: EventError, Err: fmt.Errorf("opencode stream read: %w", err)}
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
}

func (s *invokeState) scannerErr(sc *bufio.Scanner) error { return sc.Err() }

// ingest parses one NDJSON line, updates state, and returns an event to emit
// (ok=false means "drop silently", e.g. malformed lines that aren't worth
// failing on — though we currently fail on the first unparseable line).
func (s *invokeState) ingest(line []byte) (*Event, bool) {
	var env struct {
		Type      string          `json:"type"`
		SessionID string          `json:"sessionID"`
		Part      json.RawMessage `json:"part"`
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
	default:
		// Unknown event type: forward its raw form as an opaque stream chunk
		// (forward-compatible — opencode will add types over time).
		return &Event{Kind: EventStream, Chunk: fmt.Sprintf("[event:%s]", env.Type)}, true
	}
}

func (s *invokeState) accumulateFinish(p stepFinishPart) {
	s.telemetry.Tokens.Total += p.Tokens.Total
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
		return nil, fmt.Errorf("read result.json at %s: %w (agent did not produce the required contract file)", path, err)
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

// buildOpencodeArgs assembles the argv: ["opencode", "run", "--format", "json",
// "--auto", "--dir", workdir, ...optional, <message>].
func buildOpencodeArgs(bin string, inv Invocation) []string {
	args := []string{bin, "run", "--format", "json", "--auto", "--dir", inv.Workdir}
	if inv.ResumeSession != "" {
		args = append(args, "--session", inv.ResumeSession)
	}
	if inv.Model != "" {
		args = append(args, "--model", inv.Model)
	}
	args = append(args, inv.Prompt+"\n\n"+inv.RoutingBlock)
	return args
}

func validateInvocation(inv Invocation) error {
	if inv.Workdir == "" {
		return fmt.Errorf("opencode adapter: Workdir is required")
	}
	if inv.ArtifactDir == "" {
		return fmt.Errorf("opencode adapter: ArtifactDir is required")
	}
	if strings.TrimSpace(inv.Prompt) == "" {
		return fmt.Errorf("opencode adapter: Prompt is required")
	}
	return nil
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	// SIGTERM first; SIGKILL after grace is the job of exec.CommandContext's
	// own reaping. We send to -pgid to hit the whole group.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
}

func waitDoneAsync(cmd *exec.Cmd) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return done
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
