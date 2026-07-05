# Agent contract

The contract between Agentum and the coding agents it orchestrates. Every
adapter enforces this contract regardless of which agent (opencode,
claude-code, …) or pack is running. Pack authors and adapter authors build
against this document.

There are **two channels** between orchestrator and agent — do not confuse them:

1. **The event stream** — live progress (text output, tool calls, telemetry)
   emitted by the agent during the run. Used for the live UI, audit, and
   session-id capture. **Not the structured result.**
2. **`result.json`** — a file the **agent itself writes** at a known path. This
   is the structured result the orchestrator reads to decide what happens next.

## Where the agent writes

Each stage runs in a task worktree. The orchestrator owns a per-stage artifact
directory inside it:

```
<worktree-root>/.agentum/<worktree-id>/.ag-artifacts/<stage>/
  result.json          # the agent MUST write this
  ...                  # plus whatever artifacts the stage produces
```

The routing block the agent receives tells it the absolute path of its artifact
directory. Prior stages' artifact directories are referenceable by path (later
stages read earlier stages' outputs that way).

The agent MUST write `result.json` to `<artifact-dir>/result.json` before it
finishes. A run that exits without a readable `result.json` is a contract
violation and surfaces as a retryable stop-point — not a silent skip.

## result.json v1

```json
{
  "schema_version": "1",
  "status": "complete",
  "summary": "Short human-readable summary of what this stage did.",
  "open_questions": [],
  "artifacts": [
    {"path": "specs/auth.md", "kind": "spec"}
  ],
  "memory_writes": [
    {"kind": "decision", "title": "Auth via session cookies", "body": "...", "keywords": ["auth", "session"]}
  ],
  "edit_targets": [],
  "notes": "Optional free-form text."
}
```

### Fields

| Field | Required | Type | Notes |
|---|---|---|---|
| `schema_version` | yes | string | Must be `"1"`. |
| `status` | yes | enum | `complete` \| `partial` \| `blocked`. |
| `summary` | no | string | Short summary of what the stage did. Surfaces in the UI/audit. |
| `open_questions` | no | string[] | Questions for a human. Presence drives the gate: any open question stops for a human, regardless of the stage's gate value. |
| `artifacts` | no | object[] | Produced artifacts. `path` is relative to the worktree root (or absolute within it); `kind` is an optional free-form label (`spec`, `code`, `adr`, …). |
| `memory_writes` | no | object[] | Proposed memory entries. Committed only when the whole task is finally approved. `kind` ∈ `{decision, convention, spec_ref, fix, note}`; `keywords` is a string array. |
| `edit_targets` | no | string[] | Scoped-edit targets for the ask-to-edit gate action (e.g. `"src/auth.ts:session"`). |
| `notes` | no | string | Free-form text. |

### Parsing rules

- **Required fields missing → error.** `schema_version` and `status` must be
  present and valid. A missing or malformed `result.json` is a contract
  violation, surfaced as a retryable stop-point.
- **Known fields are strictly typed when present.** If `memory_writes` appears,
  each entry must have a valid `kind`; if `status` appears, it must be one of
  the enum values. Malformed known fields → error.
- **Unknown fields are ignored.** Agents and the schema both evolve; unknown
  keys are forward-compatible and silently dropped.
- **Absent optionals default empty** (`[]`, `""`, no entries).

## How the orchestrator uses result.json

- `status` + `open_questions` → the gate decision (any open question, or
  `status` ≠ `complete`, stops for a human; otherwise the stage's gate value
  decides auto-advance vs human review).
- `memory_writes` → staged, committed at final task approval (not at this gate).
- `artifacts` → the next stage reads them by path; the UI lists them.
- `edit_targets` → scope the ask-to-edit gate action.
- `summary` + `notes` → UI and audit trail.

## Event stream (reference: opencode)

The live stream is one JSON object per line on the agent's stdout. (For
opencode this is `opencode run --format json`.) The orchestrator depends on
these fields and forwards everything else as opaque stream events:

| Top-level `type` | Carries | Orchestrator use |
|---|---|---|
| `step_start` | git `snapshot` (before) | progress marker |
| `text` | assistant text + timing | live SSE to UI |
| `tool_use` | tool name, state, file path | Activity record; live SSE |
| `step_finish` | `reason`, `tokens`, `cost`, `snapshot` | accumulate telemetry; detect run completion (`reason == "stop"`) |

Every event carries `sessionID`. The orchestrator captures it from the first
event and persists it, enabling non-destructive resume on a later continuation.

### Cancellation

The orchestrator may cancel an in-flight invocation (user stop, shutdown, or a
timeout policy). On cancellation the agent subprocess is terminated; the run
surfaces as a retryable stop-point (session-id resume preserves the agent's
reasoning thread). This is the single "stop" mechanism — there is no separate
abort channel.

## Reserved for later

These are NOT in result.json v1; they'll be added when their consumers land:

- `partial_success` (a stage that produces partial output with a follow-up spec).
- `capability_use` (the traced MCP capability usage record).

When added, they'll follow the same rules: requiredness decided per field,
strict typing when present, unknown fields ignored, and `schema_version` bumped.
