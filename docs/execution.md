# Execution model

How a task actually runs: a project binds a repo, the runner drives a pack's
stages through an agent adapter, stop conditions route into the FSM, events flow
into the durable log, and a Postgres-backed queue decouples HTTP handlers from
multi-minute agent runs. This is **F.6** — the loop Epics 1–4 and 6 wire into
(the second keystone after the foundation specs). Build design lives in
`reference/04 §7`; this page is the user-facing companion.

Everything here goes through the single HTTP front door (`docs/api.md`); state
transitions go through `engine.Next` only (`internal/engine/fsm.go`); every row
carries `tenant_id` + `user_id`.

## The lifecycle in one pass

```
POST /projects          → register repo (one repo = one project)
POST /tasks             → created (project + pack + input)
POST /tasks/{id}/start  → running  (enqueues a `run` job)
   worker: stage loop — invoke adapter, parse result.json, evaluate stop
   ▸ paused_open_questions  → POST .../continue (resume, same session)
   ▸ paused_gate            → POST .../advance  (next stage, fresh session)
   ▸ paused_user_stop       → POST .../continue (resume)
   ▸ awaiting_memory_commit → POST .../approve  (task done)
POST /tasks/{id}/cancel → cancelled (aborts in-flight run)
```

Every `→` is the worker enqueuing or completing a job; HTTP handlers never
execute task work inline.

## Projects

A project binds a local git repository to an Agentum project id. One repo =
one project per tenant; registration is idempotent on `(tenant_id, repo_path)`.

- `POST /api/v1/projects` validates `repo_path` is a real git work tree
  (`git -C <path> rev-parse --is-inside-work-tree`) at registration time.
- `tasks.project_id` is a real FK; a task cannot exist without a project.
- `related_projects` is an **inert seam**: stored now, grants nothing.
  Cross-project / sibling-folder access lands in Epic 6 as a path-scoped
  `fs.read` capability derived from this set — the configured relation is the
  security boundary, never auto-discovered.

See `docs/api.md#projects` for the endpoint surface.

## Worktrees

Each task runs in its own git worktree off the project's repo (C5 — isolated
workspace per task). Created by `internal/worktree` on the first stage of a
run; reused across stages and resumes; torn down at terminal state.

- **Location:** `<repo>/.agentum/worktrees/<task-id>/`
- **Branch:** `agentum/<task-id>` (off the repo's current HEAD)
- **Artifacts:** `<worktree>/.agentum/<task-id>/.ag-artifacts/<stage>/result.json`
  (the per-stage path convention from `04 §6.4`; filesystem-as-bus, C1/C4)
- **`.agentum/` is gitignored** locally (`.git/info/exclude`, never a tracked
  `.gitignore`) so worktrees and artifacts don't pollute the user's working tree.

All git operations shell out to the `git` binary on PATH; there is no libgit2
dependency. The project repo must be a real work tree (validated at project
registration).

## The runner

`internal/runner` is the job worker's `Handler`. The worker claims a job and
calls `Runner.Handle`, which dispatches by `kind`:

| Job kind | Entry point | Triggered by |
|---|---|---|
| `run` | fresh run, first stage | `POST /tasks/{id}/start` |
| `continue` | resume after `open_questions` / `user_stop` | `POST .../continue` |
| `advance` | next stage, fresh session | `POST .../advance` |
| `cancel` | no-op (cancel handler aborts ctx + drives FSM directly) | `POST /tasks/{id}/cancel` |
| `teardown` | remove worktree at terminal state | enqueued by `approve` / `cancel` / `failTask` |

`run` / `continue` / `advance` enter the shared **stage loop** (`04 §7.2`):

1. **Resolve** the pack + current stage (or `pack.Entry` on first run) → stage
   def (gate, prompt, tier).
2. **Prepare the worktree** — created once per task; reused thereafter.
3. **Render the routing block** (`internal/routing.Render`) with role/stage/gate
   context, the artifact-dir, the result.json preamble, and memory/capability
   stubs (inert until Epic 1 / Epic 6).
4. **Resolve the model** via `internal/models.Resolve(cfg, agent, tier)` and
   pass as `Invocation.Model` (`docs/models.md`).
5. **Invoke the adapter** (`agent.Invoke(ctx, inv)`), forwarding stream chunks
   to live SSE subscribers (ephemeral) and accumulating telemetry.
6. **Persist the `stage_invocations` row** — `session_id`, telemetry,
   `stop_reason`, parsed `result.json`. Emit `stage.started` / `stage.stopped`
   / `stage.telemetry` events.
7. **Evaluate the stop condition** (`04 §7.4`) → FSM event → `engine.Next`.
   - On a pause event → loop completes; task stays paused.
   - On advance → read the pack's transition; loop to step 1 with the next stage.
   - On `reach_final_gate` → task moves to `awaiting_memory_commit`; loop completes.
   - On terminal → worker tears down the worktree; loop completes.

The loop honors `ctx` cancellation throughout: a cancel job or shutdown
cancels the active stage's ctx, the adapter kills the subprocess, the loop
transitions to a paused/cancelled state.

### Stop conditions → FSM

Driven by the parsed `result.json` (or its absence). The FSM table is unchanged
from F.1 — no new states; new `stop_reason` values distinguish *why* a pause
happened (`04 §7.1.6`).

| Outcome | FSM event | Resulting state | `stop_reason` |
|---|---|---|---|
| `status: blocked` + non-empty `open_questions` | `stop_open_questions` | `paused_open_questions` | `open_questions` |
| `status: complete`, gate ∈ {human_approval, human_final, human_edit, auto_on_approval} | `stop_gate` | `paused_gate` | `gate` |
| `status: complete`, gate ∈ {auto, auto_if_clean (and clean)} | `advance` | `running` | — |
| final stage + final gate reached | `reach_final_gate` | `awaiting_memory_commit` | — |
| `result.json` missing / invalid | `stop_user` | `paused_user_stop` | `parse_error` |
| adapter returned `EventError` | `stop_user` | `paused_user_stop` | `adapter_error` |
| ctx cancelled by user | `cancel` | `cancelled` | — |

Multi-branch transitions are unconditional in F.6 (a stage declares one
`transitions:` target). The condition evaluator for conditional-linear
pipelines lands with Epic 4.

## Job queue

The runner is a **Postgres-backed job queue + worker**, not goroutine-per-task
(`04 §7.5`). No Redis, no new infra; transactional with task state.

- **Table:** `jobs` (migration `0003_runner.sql`) — `kind`, `status`
  (`pending | running | done | failed`), `worker_id`, `heartbeat_at`,
  `attempts`, `last_error`.
- **Claim** is one atomic query — `FOR UPDATE SKIP LOCKED`:
  ```sql
  UPDATE jobs SET status='running', worker_id=$1, heartbeat_at=now(), attempts=attempts+1
  WHERE id = (
    SELECT id FROM jobs WHERE status='pending'
    ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
  )
  RETURNING *;
  ```
- **Worker** (started on boot): a configurable pool, default 1 — the single-host
  MVP rarely benefits from >1 concurrent agent stage. Polls every 500ms for MVP;
  `LISTEN/NOTIFY` wake is a clean fast-follow, not F.6.
- **Heartbeat:** the worker bumps `heartbeat_at` every 5s during a run. A
  boot-time recovery pass uses this to detect a worker that died mid-run.
- **Poison bound:** `config.Config.JobMaxAttempts` (default 3) — over the bound
  the job moves to `failed` and the task to `paused_user_stop` with
  `stop_reason='interrupted'`. Config-driven, not a magic constant.

### Enqueue points

All HTTP-driven enqueues are transactional with the FSM transition (a handler
that can't enqueue rolls back the transition):

| Endpoint | FSM transition | Enqueued kind |
|---|---|---|
| `POST /tasks/{id}/start` | `created → running` | `run` |
| `POST .../continue` | `paused_*→ running` | `continue` |
| `POST .../advance` | `paused_gate → running` | `advance` |
| `POST .../approve` | `awaiting_memory_commit → done` | `teardown` |
| `POST /tasks/{id}/cancel` | `*→ cancelled` | `teardown` |

A `run` / `continue` / `advance` job is "advance until pause/terminal." Only one
such job per task should be live at a time — enforced by the FSM (you can't
enqueue `continue` from a non-paused state).

### Crash recovery

On boot, before the worker starts (`04 §7.6`):

1. **Re-queue stale jobs** — `status='running' AND heartbeat_at < now() - 60s`.
   Set `status='pending'`, `worker_id=NULL`, increment `attempts`. The poison
   bound caps retries.
2. **Recover orphaned tasks** — `state='running'` with no live job (the job was
   lost between enqueue and claim, or the process died mid-FSM-transition).
   Transition to `paused_user_stop` with `stop_reason='interrupted'` and emit
   `task.state_changed`. The user explicitly continues — safer than auto-resume,
   which could re-run a half-completed stage. Session-id resume makes the
   re-run cheap if a session was captured.

Recovery is best-effort and conservative: it prefers a human-visible pause over
silent re-execution.

## Worktree teardown

Worktrees are torn down by Agentum on **terminal state** — `done`, `cancelled`,
or `failed` — not by a TTL and not manually (`04 §7.1.3`). Teardown is a runner
job (`kind=teardown`) that runs `git worktree remove --force` + deletes the
task branch. It is enqueued by:

- `handleInvocationApprove` — after the task moves to `done`.
- `handleCancelTask` — after the task moves to `cancelled`.
- `failTask` (best-effort) — when a run moves the task to `failed`.

Enqueuing (rather than removing inline) serializes teardown with the still-running
driving job — it never races the runner. The teardown job is idempotent: a
missing worktree is a no-op.

**F.6 gap (until F.7):** artifact *files* live inside the worktree. Teardown
discards them; only the parsed `result.json` (already persisted as jsonb on
`stage_invocations`) and git history survive. F.7 (object-storage seam) makes
artifacts durable independently of worktree lifecycle.

## Events

Only **meaningful** events are persisted to the durable `events` log (`04 §7.1.5`).
Live stream text/tool chunks are forwarded to SSE subscribers but never written
to the DB; `Last-Event-ID` replay reconstructs state changes, stage boundaries,
stop reasons, telemetry, and errors — not the full transcript. This keeps write
volume sane and matches the audit-trail intent.

F.6 emits: `task.state_changed`, `stage.started`, `stage.stopped`,
`stage.telemetry`, `task.worktree_created`, `task.worktree_removed`.

See `docs/api.md#events-sse` for the SSE contract.

## End-to-end proof

A gated integration test drives the full path with a real agent —
`internal/runner/runner_live_test.go`, build tag `integration`. It is excluded
from CI (no `opencode` binary or credentials there); run locally:

```
go test -tags integration ./internal/runner/ -run TestRunnerLive -v -timeout 5m
```

It proves: `POST /tasks/{id}/start` runs `packs/minimal` via the real opencode
adapter to a stop point (the `spec` stage's `human_approval` gate pauses the
task at `paused_gate`) and a `session_id` is captured. This is the F.6 proof
that the loop works with a live agent, not just fakes.

## What F.6 does not do yet

These land with their epics — the seams exist, the behavior does not:

- **Artifact durability** beyond the DB-stored `result.json` → **F.7**
  (object-storage interface; survives worktree teardown).
- **Memory push/pull** → **Epic 1** (the routing block's "Project decisions"
  section is an inert stub until 1.2/1.3 land).
- **MCP capability pass-through** → **Epic 6** (the routing block's
  "Capabilities available" section is an inert stub).
- **Conditional-linear pipelines** → **Epic 4** (transitions are unconditional
  here; the condition evaluator lands with 4.1).
- **Fix-loops** → **Epic 3** (the runner does not yet honor a fix-cycle budget).
- **Multi-step delivery / handoff** → **F.8** (one task = one step today).
- **Idle/hard timeout values** — the ctx seam is used, but no idle timer ships
  (`04 §5.2`).
- **`LISTEN/NOTIFY` low-latency wake** — poll is fine for MVP.
- **Per-project pack roots** — pack root is server-wide config
  (`config.Config.PacksDir`, default `./packs`).
