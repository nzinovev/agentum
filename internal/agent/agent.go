// Package agent is the single seam between the orchestrator and an external
// coding agent (opencode, claude-code, …). The runner hands an Adapter an
// Invocation; the Adapter returns a stream of Events ending in a parsed
// Result. Memory injection, gate evaluation, and fix-loops plug in around this
// seam.
//
// There are two channels between orchestrator and agent:
//
//   - The event stream: live progress (text, tool calls, telemetry) emitted by
//     the agent during the run. Used for SSE/UX, audit, and session-id capture.
//   - result.json: a file the agent itself writes at ArtifactDir/result.json.
//     This is the structured result the orchestrator reads to decide the next
//     move (see docs/agent-contract.md).
//
// The Adapter synthesizes its returned Result from both.
package agent

import "context"

// Adapter is the boundary to an external coding agent. Implementations MUST
// honor ctx cancellation: abort the agent subprocess, release resources, and
// close the returned channel. This is the cancellation seam — every "stop the
// invocation" reason (user stop, timeout, shutdown) flows through ctx.
type Adapter interface {
	// Invoke starts the agent for one stage invocation and returns a channel
	// of Events. The channel closes when the run ends (cleanly or by error);
	// the final meaningful event is an EventResult or EventError. The channel
	// must be drained by the caller. A non-nil error means the run could not
	// start at all; errors during the run arrive on the channel.
	Invoke(ctx context.Context, inv Invocation) (<-chan Event, error)
}

// Invocation is one stage's run. Identity (tenant/user) is NOT here — it lives
// on the Principal in the caller's context; the adapter is identity-agnostic.
type Invocation struct {
	Workdir       string   // the task worktree root (the agent's working directory)
	ArtifactDir   string   // per-stage dir; result.json is read here after the run
	Prompt        string   // role-pure system prompt, loaded from the pack
	RoutingBlock  string   // rendered: stage/gate/memory/capabilities + result.json contract
	Capabilities  []string // pack∩stage capability subset (declared = passed at MVP)
	ResumeSession string   // non-empty → resume that session id; empty → fresh invocation
	Model         string   // optional provider/model override (BYO-models, F.4)
}

// EventKind distinguishes the things an adapter emits on its stream.
type EventKind int

const (
	// EventStream is a live output chunk forwarded to the UI (assistant text,
	// tool activity). Chunk holds the rendered fragment.
	EventStream EventKind = iota
	// EventResult is the terminal success: the parsed result.json plus the
	// stream-derived fields (session id, telemetry). The channel closes after.
	EventResult
	// EventError is the terminal failure: the run could not produce a valid
	// result. Err carries the reason. The channel closes after.
	EventError
)

// Event is one item on the adapter stream.
type Event struct {
	Kind   EventKind
	Chunk  string  // EventStream: the fragment to forward
	Result *Result // EventResult
	Err    error   // EventError
}

// Result is the parsed outcome of one stage invocation: stream-derived fields
// (SessionID, Telemetry, Snapshot, Activity) joined with file-derived fields
// (the parsed result.json).
type Result struct {
	// Stream-derived.
	SessionID string     // captured from the first stream event
	Telemetry Telemetry  // accumulated across step-finish events
	Snapshot  string     // latest git snapshot sha the agent committed to
	Activity  []Activity // tool calls observed in-stream

	// File-derived (parsed from ArtifactDir/result.json).
	ResultJSON
}

// Telemetry is the per-invocation cost summary accumulated from step-finish
// events. Carryover C10, tagged with tenant/user/pipeline/tier at the caller.
type Telemetry struct {
	Tokens TotalTokens
	Cost   float64
}

// TotalTokens is the token breakdown. All fields are cumulative across the run.
type TotalTokens struct {
	Total      int64
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
}

// Activity is one observed tool call from the stream (a record of what the
// agent did, not a control signal).
type Activity struct {
	Tool   string // tool name, e.g. "write", "bash", "read"
	Status string // "completed" | "running" | "error"
	Target string // file path or command, when available
}
