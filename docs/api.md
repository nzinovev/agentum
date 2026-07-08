# HTTP API

The single external surface — every consumer (the UI, a future CLI, an external
system tagging the orchestrator) is a client of this API. All `/api/v1/*`
endpoints go through one boundary that resolves the caller to a Principal and
routes every permission decision through `authz.Can`. Identity (tenant_id,
user_id) is never read from the request body — it is implicit in the Principal.

Pre-release: the surface is versioned `v1` and will break before `1.0`.
Implemented endpoints are marked below; the rest return `501 not_implemented`
and land with the epic named in the table.

## Conventions

- **JSON bodies in, JSON bodies out.** Timestamps are RFC3339 nano, UTC.
- **Errors** are structured:

  ```json
  { "error": { "code": "illegal_transition", "message": "engine: illegal transition running --start-->" } }
  ```

  Codes are stable machine identifiers; the UI branches on them. Current codes:
  `not_found`, `illegal_transition`, `bad_input`, `unauthorized`, `forbidden`,
  `not_implemented`, `internal`.
- **Identity is implicit.** Every write carries `tenant_id` and `user_id` from
  the resolved Principal, never the request body.
- **State transitions** route through `engine.Next`. An illegal transition is
  `409 illegal_transition`, never a silent write.

## Projects

A project binds a local git repository to an Agentum project id (one repo = one
project per tenant). Tasks reference a project; the runner creates a per-task
worktree off the project's repo. Registration is idempotent: re-`POST`ing the
same `repo_path` updates `name` / `related_projects` rather than failing.

| Method | Path | Status | Body / Query → Response |
|---|---|---|---|
| `POST` | `/projects` | ✅ | `{repo_path, name, related_projects?}` → `201 Project` / `400 bad_input` (if `repo_path` is not a git work tree) |
| `GET` | `/projects` | ✅ | `?limit=&offset=` → `200 Project[]` |
| `GET` | `/projects/{id}` | ✅ | → `200 Project` / `404 not_found` |

`repo_path` must point inside a real git work tree (validated at registration).
`related_projects` is an **inert seam**: stored now, it will grant cross-project
read access (a path-scoped `fs.read` capability) in a later epic — never
auto-discovered, the configured set is the security boundary.

### Project

```json
{
  "id": "uuid",
  "repo_path": "/home/me/repos/my-app",
  "name": "My App",
  "related_projects": [],
  "created_at": "2026-07-09T...",
  "updated_at": "2026-07-09T..."
}
```

## Tasks

| Method | Path | Status | Body / Query → Response |
|---|---|---|---|
| `POST` | `/tasks` | ✅ | `{project_id, pipeline_pack, title, input?}` → `201 Task` |
| `GET` | `/tasks` | ✅ | `?project_id=&limit=&offset=` → `200 Task[]` |
| `GET` | `/tasks/{id}` | ✅ | → `200 Task` / `404 not_found` |
| `POST` | `/tasks/{id}/start` | ✅ | `created → running` → `200 Task` / `409 illegal_transition` |
| `POST` | `/tasks/{id}/cancel` | stub | any non-terminal → `cancelled`. Epic: foundation |

### Task

```json
{
  "id": "uuid",
  "project_id": "uuid",
  "pipeline_pack": "java-spring@1",
  "title": "Add auth to /settings",
  "input": {},
  "state": "created | running | paused_open_questions | paused_gate | paused_user_stop | awaiting_memory_commit | done | failed | cancelled",
  "created_at": "2026-07-05T...",
  "updated_at": "2026-07-05T..."
}
```

## Stage invocations

A stage invocation is one agent run within a task (`stage_invocations` row).
Each carries a `session_id` (for non-destructive resume), `stop_reason`
(`open_questions | gate | user_stop`), and `pending_edits`.

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/tasks/{id}/invocations` | stub | list invocations for a task. Epic 5.1 |
| `GET` | `/tasks/{id}/invocations/{iid}` | stub | one invocation. Epic 5.1 |

## Gate actions — stop-point → continue semantics

Humans act only at stop points. The three stop conditions and their
continue-semantics (from §3.2) map 1:1 to these endpoints:

| Stop reason | What happened | Endpoint to continue | Continues same session? |
|---|---|---|---|
| `open_questions` | agent asked; needs answers | `POST .../continue` | yes — resume |
| `gate` | gate passed | `POST .../advance` | **no** — next stage is a fresh invocation |
| `user_stop` | user paused it | `POST .../continue` | yes — resume |

The three gate **actions** from §3.4:

| Method | Path | Status | Action |
|---|---|---|---|
| `POST` | `/tasks/{id}/invocations/{iid}/continue` | stub | resume with answers / context. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/advance` | stub | pass the gate → next stage runs. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/approve` | stub | final approval → task done + memory commits. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/edit` | stub | edit-and-approve: the human edits the artifact directly; the edit is the approval. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/ask-to-edit` | stub | scoped agent-mediated edit; re-stops for review. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/add-context` | stub | additive guidance; agent resumes (does not regenerate). Epic 2 |

## Artifacts

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/tasks/{id}/invocations/{iid}/artifacts/{name}` | stub | fetch an artifact. Epic 2 |
| `PUT` | `/tasks/{id}/invocations/{iid}/artifacts/{name}` | stub | edit-and-approve via artifact write. Epic 2 |

## Memory

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/projects/{id}/memory?keyword=&limit=` | stub | keyword-pull handle (recency-ordered). Epic 1.3 |

## Packs

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/packs` | stub | list available packs. Epic 5.1 |
| `GET` | `/packs/{name}` | stub | pack manifest. Epic 5.1 |

## Events (SSE)

Two streams, both honoring `Last-Event-ID` replay:

| Method | Path | Status | Scope |
|---|---|---|---|
| `GET` | `/events` | ✅ | tenant-global (inbox / feed) |
| `GET` | `/tasks/{id}/events` | ✅ | per-task (run view) |

### Framing

Each event is one SSE block:

```
id: 42
event: stage.stopped
data: {"task_id":"...","stage":"implement","stop_reason":"gate"}

```

- `id` is the monotonic `events.id` (bigint). **`Last-Event-ID`** replays every
  row with `id > lastID`, scoped to the tenant (and task for per-task). A
  missing/invalid `Last-Event-ID` replays from the start.
- After replay completes, the connection live-tails new rows and emits a
  comment-frame keepalive (`: ping <unix>`) every 15s.
- The same durable log backs the audit trail, so reconnect semantics and audit
  are one schema.

### Event types

| `event` | Carries | Emitted by |
|---|---|---|
| `task.state_changed` | `{task_id, from, to}` | engine on every transition |
| `stage.invocation_started` | `{task_id, invocation_id, stage, sequence}` | runner |
| `stage.stream` | `{task_id, invocation_id, chunk}` | adapter (agent text → SSE) |
| `stage.tool` | `{task_id, invocation_id, tool, target, status}` | adapter (tool activity) |
| `stage.stopped` | `{task_id, invocation_id, stop_reason}` | runner at a stop point |
| `stage.result` | `{task_id, invocation_id, status, open_questions, ...}` | runner after result.json |
| `memory.committed` | `{task_id, entries:[...]}` | memory layer at final approval |
| `run.log` | `{task_id, level, message}` | runner / adapter diagnostics |

Pre-release: the `payload` shapes are stable in shape but may gain fields; the
UI must ignore unknown payload fields.

## Health

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/healthz` | ✅ | liveness — process up. `200 {status:"ok"}` |
| `GET` | `/readyz` | ✅ | readiness — DB reachable. `200` / `503` |
