# Changelog

All notable changes to Agentum are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Once tagged releases begin, this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until then the
`[Unreleased]` section accumulates change.

## [Unreleased]

### Added
- **Foundation** (#1): Go engine, Postgres store (goose migrations embedded and
  auto-applied on boot), single-front-door HTTP API with middleware boundary,
  multi-tenant schema (`tenant_id` + `user_id` on every row), explicit task FSM,
  project-memory and durable event-log tables, docker-compose for local
  Postgres, Apache-2.0 license, CI (build/vet/test/gofmt).
- **Task API** (#2): `POST /api/v1/tasks`, `GET /api/v1/tasks/{id}`,
  `GET /api/v1/tasks`, `POST /api/v1/tasks/{id}/start`. State transitions route
  through `engine.Next`; an illegal transition returns `409`. Table-driven FSM
  tests. sqlc-generated data layer committed so a fresh clone builds.
- **Pack format v1** (#3): the versioned pipeline-pack schema — a directory with
  `manifest.yaml` + `prompts/*.md`. Named-map stages with explicit transitions,
  six-value gate vocabulary, declared memory scopes and MCP capabilities,
  per-pack fix-loop and ask-to-edit budgets, tier policy, semver versioning.
  Loader, validator, and a minimal reference pack. Keystone for the gate,
  fix-loops/tier, conditional-linear, and MCP epics.
- **Pack override resolver** (#4): all four override layers — lock-major base
  resolution via a filesystem `Source`, fork metadata, prompt swaps, and
  stage/budget param patches. `Resolve` deep-copies the base, applies the
  layers, and re-validates the result; an override that breaks the contract is
  rejected. Completes the pack format work (F.5).
- **Agent adapter contract + opencode reference adapter** (#6): the
  orchestrator↔agent seam. Defines a strict `result.json` v1 contract (the file
  agents must write: `schema_version`, `status`, `summary`, `open_questions`,
  `artifacts`, `memory_writes`, `edit_targets`, `notes`) at
  `.agentum/<worktree>/.ag-artifacts/<stage>/result.json`, documented publicly.
  The opencode adapter runs `opencode run --format json` as a subprocess,
  streams NDJSON events (session-id, telemetry, activity), reads + strict-parses
  result.json, and honors context cancellation by killing the process group.
  Session-id resume (`--session`) is non-destructive. Completes F.2.
- **HTTP API contract + SSE event streams** (#7): the full single-front-door
  surface is documented at `docs/api.md` — endpoint table for tasks, stage
  invocations, the six gate actions, artifacts, memory, packs; structured error
  model `{error:{code,message}}`; and an SSE event taxonomy with Last-Event-ID
  replay over the durable `events` table. SSE ships two streams (tenant-global
  `/events` + per-task `/tasks/{id}/events`), both with replay + live tail +
  heartbeats. Every contract endpoint is declared; unimplemented ones return
  `501 not_implemented` so the surface is real for the UI today. Completes F.3.
- **Tier→model resolver with per-agent defaults** (#8): Agentum maps a pack's
  tier name (`fast`/`strong`/`reasoning`) to the model string passed to the
  agent binary's `--model` flag. Built-in defaults for `opencode`
  (`anthropic/claude-*`) and `claude-code` (`haiku`/`sonnet`/`opus`) so the
  common case needs no configuration — clone, `make run`, works if the agent is
  already configured. Optional `models.yaml` overrides. Agentum deliberately
  does NOT manage credentials, provider endpoints, or agent config files; the
  operator configures opencode/claude-code directly. Completes F.4 — and with
  it, Epic F (foundation & contracts) is done.
- **Projects** (F.6 PR1): a `projects` entity binds a local git repo to a
  project id (one repo = one project per tenant); `tasks.project_id` is now a
  real foreign key (was a dangling column). `POST/GET /api/v1/projects` with
  idempotent registration on `(tenant_id, repo_path)` and real-git-repo
  validation at registration. Carries an inert `related_projects` seam for the
  deferred cross-project / sibling-folder access (stored now, enforced in a
  later epic). First piece of F.6 (execution model); the runner lands next.
- **Runner + job queue + worker** (F.6 PR3): the loop that makes a task actually
  run. A Postgres-backed job queue (`FOR UPDATE SKIP LOCKED`, no Redis) decouples
  HTTP from execution — handlers enqueue and return; a worker claims and drives.
  `internal/runner` composes the adapter, pack loader, worktree service, routing
  block, models resolver, and engine FSM: it walks a pack's stages, invokes the
  adapter per stage, persists `stage_invocations`, evaluates stop conditions
  (`open_questions`/`gate`/`adapter_error`/`parse_error`) into the FSM via a pure,
  table-tested evaluator, and emits meaningful events into the durable log.
  `start`/`continue`/`advance`/`cancel` handlers enqueue the driving jobs; cancel
  aborts the in-flight run via a per-task cancel registry (the §5.1 seam). A
  heartbeat + boot recovery re-queue jobs a dead worker left behind, bounded by
  `AGENTUM_JOB_MAX_ATTEMPTS` (default 3). Terminal stages fire `reach_final_gate`
  without invoking the adapter. Nullable-uuid columns are now `sql.NullString`
  (was a broken plain-string override that couldn't represent NULL).
- **Worktree service + routing-block renderer** (F.6 PR2): `internal/worktree`
  creates per-task git worktrees off a project's repo at
  `<repo>/.agentum/worktrees/<task-id>/` on branch `agentum/<task-id>` (C5),
  idempotent on re-create (resume/retry safe), and removes them at terminal
  state. Ensures the repo ignores its own `.agentum/` dir so worktrees and
  artifacts never pollute the user's working tree. `internal/routing` renders
  the orchestrator-owned routing block (C2): role/stage/gate context, the
  result.json contract preamble (identical for every pack/agent), prior-stage
  artifact references, plus inert memory and capabilities sections that their
  epics fill in. Both pure, unit-tested with temp git repos; no runner wiring
  yet — that is PR3.

### Changed
- Postgres tables live in a dedicated `agentum` schema (created on boot before
  migrations run) instead of the default `public` schema (#1).
- **Error responses are now structured**: `{"error":{"code":"...","message":"..."}}`
  replaces the previous flat `{"error":"..."}` (#7). Codes are stable machine
  identifiers the UI branches on (`not_found`, `illegal_transition`, `bad_input`,
  `unauthorized`, `forbidden`, `not_implemented`, `internal`). Pre-0.1 break.

### Fixed
- `store.Close` and SSE write errors are no longer silently dropped (#1).
