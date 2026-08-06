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
directory inside it; the routing block the agent receives tells it the absolute
path. Prior stages' artifact directories are referenceable by path (later
stages read earlier stages' outputs that way).

```
<worktree-root>/.agentum/<worktree-id>/.ag-artifacts/<stage>/
  result.json          # the agent MUST write this
  verdict.json         # reviewer stages MUST write this (see below)
  ...                  # plus whatever artifacts the stage produces
```

## Project context the agent sees

Beyond the role-pure prompt from the pack, a stage receives two pieces of
project context that are **not** pack-owned (ADR 0002):

- **Pinned instruction files.** The repository's `AGENTS.md` and any files
  declared under `instructions:` in `.agentum.yaml`, pinned from the task's
  `base_commit`. These are read-only from the agent's perspective: the `edit`
  tool is denied on them, and a `bash`-side rewrite is caught and reversed by a
  pre-stage hash check. The agent learns the project's rules from these; it
  cannot change them mid-run.
- **Resolved project checks.** The routing block lists the project's named
  checks (build, test, lint, …) with their commands and required markers. The
  orchestrator runs these itself at the delivery boundary; the agent is told
  what they are so it can run them to check its own work. The agent's own claim
  that a check passed is **not** evidence — Agentum reads the result from its
  own executor. The agent cannot change which checks gate delivery.

The enumerated set of skills the runtime has available (including the user's
own in `~/.claude/skills/`) is recorded in the manifest but not surfaced to the
agent as a list — skills reach the agent through the runtime's own mechanism.

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

## verdict.json v1 (reviewer stages only)

A **reviewer stage** — one whose transitions include a `verdict` condition —
writes a second file, `verdict.json`, next to its `result.json` at
`<artifact-dir>/verdict.json`. This is the routing input the orchestrator parses
to decide whether the pipeline advances or loops back to a fixer. The
orchestrator routes on the parsed `verdict` field **only**; `result.json.summary`
and stream text cannot move a verdict-conditioned pipeline.

```json
{
  "schema_version": "1",
  "verdict": "approved | changes_requested",
  "summary": "one-line rationale",
  "findings": [
    {"id": "F1", "severity": "blocker|major|minor", "path": "internal/x/y.go", "line": 42, "detail": "..."}
  ]
}
```

Parsing rules (a violation pauses the run with `verdict_unreadable`, the same
retryable shape as a `parse_error`):

- `schema_version` and `verdict` are required; `verdict` ∈ {`approved`,
  `changes_requested`}.
- `findings[].severity` ∈ {`blocker`, `major`, `minor`}.
- `changes_requested` with zero findings is a contract violation: a fixer with
  no findings has nothing to act on. `approved` with no findings is the normal
  clean-pass shape.
- Unknown fields are ignored (forward-compatible).
- A missing `verdict.json` (the reviewer stage produced none) also pauses with
  `verdict_unreadable` — the orchestrator never defaults to `approved`.

The orchestrator captures `verdict.json` as an immutable artifact revision
(`<stage>/verdict.json`, kind `verdict_json`), so a transition resolved on the
`advance` job path (after a worktree restore or a worker restart) reads the
verdict from the store rather than a file Restore may have reset. A fixer stage
entered through a `changes_requested` transition is pointed at the findings in
its routing block.

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
