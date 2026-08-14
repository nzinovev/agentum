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
| `POST` | `/tasks` | ✅ | `{project_id, pipeline_pack, title, input?, base_ref?}` → `201 Task` |
| `GET` | `/tasks` | ✅ | `?project_id=&limit=&offset=` → `200 Task[]` |
| `GET` | `/tasks/{id}` | ✅ | → `200 Task` / `404 not_found` |
| `POST` | `/tasks/{id}/start` | ✅ | `created → running` (enqueues a run job) → `200 Task` / `409 illegal_transition` |
| `POST` | `/tasks/{id}/reject` | ✅ | terminal reject at either human gate (plan `paused_gate` or final `awaiting_final_review`). Reuses cancel semantics (lands in `cancelled`, branch survives) but records a `rejected` decision and seals the manifest `SealRejected`. Idempotent: a repeat reject matching the recorded decision returns `200`. → `200 Task` / `409 illegal_transition` |
| `POST` | `/tasks/{id}/cancel` | ✅ | any non-terminal → `cancelled` (terminal abort; branch survives) → `200 Task` / `409 illegal_transition` |
| `GET` | `/tasks/{id}/final-review` | ✅ | the reviewable payload — `200` in `awaiting_final_review` **and** in terminal states (`done` / `cancelled` / `failed`); `409 illegal_transition` before the gate. Carries `plan` / `git` / `diff` / `stages` / `review` / `checks` / `manifest` / `decisions`. |
| `POST` | `/tasks/{id}/cleanup` | ✅ | terminal task → branch deleted (idempotent, audited) → `202 Task` / `409 illegal_transition` (if not terminal) |

`base_ref` is the git ref the task builds against (branch / tag / SHA / `HEAD`).
It is resolved once to an immutable `base_commit` before the worktree is
created; omitted defaults to `HEAD`. See `docs/execution.md` § "Safe lifecycle,
checkpoints, and code egress" for the full lineage / abort / cleanup model.

`cancel` is a **terminal abort**: the in-flight run is aborted and the worktree
is torn down, but the `agentum/<task-id>` branch and any committed recovery work
survive for review. `cleanup` is the **explicit, post-terminal disposal** that
deletes the branch; it is a distinct verb because cancel and cleanup must not be
ambiguous with each other or with pause.

`reject` is a **terminal reject at a human gate** — the plan gate
(`paused_gate`) or the final gate (`awaiting_final_review`). It reuses `cancel`'s
FSM event (the task lands in `cancelled`, branch preserved) but records a
`rejected` decision on `task_approvals` and seals the manifest with
`SealRejected`, so a sealed record cannot describe a rejected result as a plain
abort. At the plan gate nothing ever unlocked source-write, so there is no
source change to undo. Idempotent: a repeat reject matching the recorded
decision returns `200`, a conflicting one returns `409`.

The pre-final-review state is `awaiting_final_review` (the previous
memory-commit state name was retired; migration 0009 rewrites existing rows).

`result_commit` is now pinned at the final gate (the commit the human reviews),
not only at teardown — see `docs/execution.md` § "`result_commit` at the final
gate".

### Task

```json
{
  "id": "uuid",
  "project_id": "uuid",
  "pipeline_pack": "java-spring@1",
  "title": "Add auth to /settings",
  "input": {},
  "state": "created | running | paused_open_questions | paused_gate | paused_user_stop | awaiting_final_review | done | failed | cancelled",
  "base_ref": "main",
  "base_commit": "a1b2... full SHA the task branched from, set on first run",
  "result_commit": "c3d4... full SHA pinned at the final gate (the commit the human reviews); empty before the gate",
  "branch": "agentum/<task-id>",
  "created_at": "2026-07-05T...",
  "updated_at": "2026-07-05T..."
}
```

## Stage invocations

A stage invocation is one agent run within a task (`stage_invocations` row).
Each carries a `session_id` (for non-destructive resume), `stop_reason`
(`open_questions | gate | user_stop | fix_budget_exhausted | verdict_unreadable`),
`cycle` (the 0-based repeat index of the stage within the task — distinguishes
retries from resumes), and `pending_edits`.

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/tasks/{id}/invocations` | ✅ | list invocations for a task, ordered by sequence. `200 Invocation[]` / `404 not_found` (unknown task). |
| `GET` | `/tasks/{id}/invocations/{iid}` | ✅ | one invocation. `200 Invocation` / `404 not_found`. |

`Invocation` shape: `{id, stage, sequence, cycle, stop_reason?, session_id?,
resume_of?, started_at, finished_at?}`. `sequence` is the global run order;
`cycle` is the per-stage repeat index (0 = first entry, increments on a fresh
re-entry, inherited on a resume).

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
| `POST` | `/tasks/{id}/invocations/{iid}/continue` | ✅ | resume after `open_questions` / `user_stop` (session-id resume; enqueues a `continue` job) |
| `POST` | `/tasks/{id}/invocations/{iid}/advance` | ✅ | pass a `gate` → next stage runs (enqueues an `advance` job) |
| `POST` | `/tasks/{id}/invocations/{iid}/approve` | ✅ | final approval at `awaiting_final_review` → task done + memory commits. Pins `result_commit` at the gate. Idempotent. |
| `POST` | `/tasks/{id}/invocations/{iid}/edit` | stub | edit-and-approve: the human edits the artifact directly; the edit is the approval. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/ask-to-edit` | stub | scoped agent-mediated edit; re-stops for review. Epic 2 |
| `POST` | `/tasks/{id}/invocations/{iid}/add-context` | stub | additive guidance; agent resumes (does not regenerate). Epic 2 |

> `continue` / `advance` are implemented but operate on the **task**, not the
> invocation id: they enqueue a job that drives the runner. The `{iid}` path
> parameter is accepted for contract stability but the runner resumes from the
> task's current state. `cancel` is `POST /tasks/{id}/cancel`.

## Artifacts

Two surfaces: the per-invocation edit surface under
`/tasks/{id}/invocations/{iid}/artifacts/{name...}` and the F.7 immutable
revisions surface.

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/tasks/{id}/artifacts` | ✅ | list revisions for a task. `?current=true` narrows to current revisions. |
| `GET` | `/tasks/{id}/artifacts/revisions/{rid}` | ✅ | one revision (metadata only). |
| `GET` | `/tasks/{id}/artifacts/revisions/{rid}/content` | ✅ | streams the blob bytes. |
| `GET` | `/tasks/{id}/invocations/{iid}/artifacts/{name...}` | ✅ | current revision of `(task, name)` + its content. `X-Revision-Id` header carries the revision id to use as `expected_revision_id` on a PUT. 404 `not_found` when no current revision. |
| `PUT` | `/tasks/{id}/invocations/{iid}/artifacts/{name...}` | ✅ | edit-and-approve via artifact write — creates a new revision (`actor = human`, no source invocation). The edit IS the approval at a `human_edit` gate. |

The artifact route uses `{name...}` (multi-segment) so orchestrator-built names
like `plan/plan.md`, `review/verdict.json`, and `<stage>/result.json` are
addressable. Purely additive: single-segment names keep resolving identically.

#### Artifact edit request

```json
{ "content": "…", "kind": "spec", "expected_revision_id": "uuid" }
```

`kind` is optional (defaults to the prior revision's kind, or `file` for a
create). `expected_revision_id` is the optimistic-concurrency precondition:
**required when the artifact already has a current revision** (the value from a
prior GET's `X-Revision-Id`), so two editors racing produce a 409 for the loser
rather than a silent lost update. A first create (no current revision) omits it.

| Store outcome | Status | Code |
|---|---|---|
| revision created | `200` artifact revision | — |
| `ErrRevisionConflict` (precondition failed, or two creates raced) | `409` | `conflict` |
| `ErrSecretDetected` (reject-on-secret policy refused the content) | `422` | `bad_input` |
| `ErrNoCurrentRevision` (GET on an artifact with no revision yet) | `404` | `not_found` |
| current revision exists but `expected_revision_id` was omitted | `428` | `precondition_missing` |

The revisions store is content-addressed and lives outside any worktree. Each
edit creates a new immutable revision that chains to the prior one; the
`current` pointer is the single mutable bit per `(task, name)`.

### Artifact revision

```json
{
  "id": "uuid",
  "task_id": "uuid",
  "name": "specs/auth.md",
  "kind": "spec | code | adr | result_json | …",
  "content_hash": "sha256 hex",
  "content_size": 1234,
  "action_type": "create | edit",
  "prev_revision_id": "uuid (empty for create)",
  "source_invocation_id": "uuid (empty for human edits)",
  "delivery_step": "optional — empty for single-unit runs",
  "execution_unit": "optional — empty for single-unit runs",
  "phase": "optional — empty for single-unit runs",
  "actor": "human | agent | system",
  "is_current": true,
  "created_at": "2026-07-09T..."
}
```

## Evidence manifest

One manifest per task. Records the inputs that shaped the run (input task +
revision, project + base commit, pack + version + hash, prompt revisions,
adapter + declared capabilities, model + tier, effective capability profile,
memory slice, input/output artifact revisions, check set version + results,
human gate decisions, branch + checkpoints + result commits).

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/tasks/{id}/manifest` | ✅ | manifest body + seal info + corrections |
| `GET` | `/tasks/{id}/manifest/diff?other=<task-id>` | ✅ | input-level diff vs another task's manifest |
| `POST` | `/tasks/{id}/manifest/corrections` | ✅ | add a post-seal correction (body: `{reason, body?}`) |

The manifest body is filled append-only while the run is in flight and sealed
at terminal state (`done | failed | cancelled | interrupted`). Corrections
after sealing are linked rows with a `reason` and a fresh body snapshot; the
sealed row is never edited.

### Manifest body shape

```json
{
  "schema_version": "1",
  "input":            { "task_id": "…", "title": "…", "input": {}, "revision": "…", "pipeline_pack": "…" },
  "project":          { "project_id": "…", "repo_path": "…", "name": "…", "base_ref": "…", "base_commit": "…" },
  "pack":             { "ref": "…", "name": "…", "version": "1.0.0", "content_hash": "…", "forked": false },
  "prompts":          [{ "stage_id": "spec", "hash": "…" }],
  "adapter":          { "name": "opencode", "version": "…", "declared_capabilities": [] },
  "model":            { "tier": "strong", "model": "…", "agent_name": "opencode" },
  "capabilities":     { "declared": [], "granted": [] },
  "memory":           { "scope": "project", "hashes": [], "entries": 0 },
  "artifacts":        { "inputs": [], "outputs": [] },
  "checks":           { "set_version": "", "results": [] },
  "human_gates":      [{ "stage": "…", "gate": "…", "decision": "approved | rejected | edited | continued", "actor": "…", "timestamp": "…" }],
  "git":              { "branch": "agentum/…", "base_commit": "…", "result_commit": "…", "checkpoints": [] },
  "execution_coordinate": { "delivery_step": "", "execution_unit": "", "phase": "" },
  "missing":          ["memory", "checks", "capabilities", "human_gates"]
}
```

### Diff response

The diff is one SectionDelta per axis that differs (or `null` for axes that
match):

```json
{
  "input":               { "reason": "input-revision", "summary": "…" },
  "pack":                { "reason": "pack-version", "summary": "…" },
  "model":               null,
  "execution_coordinate": null
}
```

The diff is **input-level only**. Outputs (artifacts produced) and human
decisions are not compared — those are results, not inputs.

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
| `stage.artifact_rejected` | `{task_id, stage, path, reason}` | runner when a declared artifact is refused (`reason` ∈ `escapes_worktree`, `unresolvable`, `secret_detected`) |
| `task.delivery_commit_diverged` | `{task_id, result_commit, checks_commit, checkpoint_label}` | runner at teardown when `result_commit` differs from the commit the delivery checks verified; the task is not failed, but the sealed manifest reads `evidence_complete: false` |
| `memory.committed` | `{task_id, entries:[...]}` | memory layer at final approval |
| `run.log` | `{task_id, level, message}` | runner / adapter diagnostics |

Pre-release: the `payload` shapes are stable in shape but may gain fields; the
UI must ignore unknown payload fields.

## Health

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/healthz` | ✅ | liveness — process up. `200 {status:"ok"}` |
| `GET` | `/readyz` | ✅ | readiness — DB reachable. `200` / `503` |
