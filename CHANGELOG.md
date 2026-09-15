# Changelog

All notable changes to Agentum are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Once tagged releases begin, this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until then the
`[Unreleased]` section accumulates change.

## [Unreleased]

### Fixed
- **The model-check registry is tenant-scoped.** Before this, keys were keyed
  by the bare header and check reads by the bare id, so a second tenant could
  hit a foreign key's `409`, receive a foreign check on a same-body replay,
  and read a foreign check's results. Keys are now `(tenant, key)`, and a
  foreign id reads as the same `404` as an unknown one; nothing to do.
- **The boot-time catalog check covers the tiers the process will actually
  run on, and the shipped defaults were refreshed.** Before this, two of the
  three baked-in tier models no longer existed upstream and the check skipped
  them, so a clean install refused every run at start on `strong`. If you run
  without a `models.yaml`, the new defaults apply on the next boot:
  `fast` → `opencode/nemotron-3.5-lightning-free`, `strong` →
  `opencode/muse-spark-1.3-contributor-free`; `reasoning` is unchanged.
- **A model-check request carrying `variant` returns `400 bad_input` naming
  the field.** Before this, the response and events echoed the requested
  variant while the check ran without it. If you send `variant` to
  `POST /api/v1/models/test`, drop the field until the adapter accepts one.
- **The accepted-check queue is bounded per tenant.** Before this, every
  `202` queued a goroutine with no backpressure, so a hundred keys were a
  hundred paid calls queued. Past 8 unfinished checks the answer is
  `429 too_many_requests` naming the cap, and replays of accepted checks
  still return `202`; nothing to do.
- **The catalog's unreadable-records warning reaches the structured log.**
  Before this, it went to the stdlib text handler on stderr instead of the
  process logger. Nothing to do.
- **The registry TTL counts from a check's completion, and pending checks stop
  with the process.** Before this, a long queue could retire a still-running
  check into a `404`, and a shutdown left running checks and their subprocesses
  behind. A shutdown now cancels pending checks and kills their subprocesses;
  nothing to do.
- **The orchestrator commits each stage's work, so agent output survives
  teardown.** Before this, every checkpoint was the base SHA: a task could
  complete, pass its gates, and deliver an empty diff while the teardown
  discarded everything the agent produced. Worktree cleanliness is still
  sampled before the checkpoint commit, so the `auto_if_clean` gate keeps its
  signal; nothing to do.
- **The delivery checks verify the commit the evidence names.** Before this,
  the executor ran against the working tree — uncommitted and untracked files
  included — and stamped a SHA it had not verified, so checking out the
  recorded commit reproduced a different tree. A dirty tree at the boundary
  now fails the task; nothing to do.
- **Reaching the delivery boundary without a resolved base commit fails the
  task.** Before this, an absent base commit resolved to an empty check set
  that passed vacuously — fail-open at the one boundary meant to be
  fail-closed. A project that defines no checks is still a legitimate empty
  run, recorded with `ran: false`.
- **A `result_commit` that diverges from the verified commit is recorded.**
  Before this, nothing compared the two, so a sealed manifest could assert
  "checks passed at X" alongside "delivered Y". A mismatch now records an
  evidence gap — `evidence_complete` reads false — and emits a
  `task.delivery_commit_diverged` event naming both SHAs; the task is not
  failed, because the human already approved.
- **`evidence_complete` no longer counts a checks section that ran nothing.**
  Before this, the completeness predicate and the missing-sections list were
  maintained in parallel and could drift, and a checks section with no run
  satisfied completeness. Nothing to do.
- **Cancelling a task with a sealed or missing manifest completes instead of
  returning `500`.** Before this, the strict decision-recording policy rolled
  the transaction back, leaving the task un-cancellable and forever
  non-terminal. Cancel now absorbs a sealed or missing manifest; a real write
  error still fails.
- **`evidence_complete` can become true.** Before this, completeness counted
  the not-yet-wired `memory` subsystem as a missing section, so the flag was
  permanently false. The `missing` list still reports `memory`; nothing to do.
- **A transient store error no longer disables the artifact-edit
  precondition.** Before this, any error reading the current revision read as
  "no current revision", so a PUT without `expected_revision_id` could
  overwrite a revision it never confirmed was absent. Any other error now
  fails the request with `500` before the precondition branches.
- **The manifest-corrections list and the latest-correction lookup agree on
  ordering.** Before this, at equal timestamps the two queries could disagree
  and a read could take a body that was not the chain head. Nothing to do.
- **A human artifact edit records a gate decision only while a gate is
  active.** Before this, every successful PUT wrote an `edited` decision, so
  an edit far from any gate read as "a human passed a gate". The artifact
  revision row remains the durable record of the edit itself.
- **Concurrent manifest evidence writes no longer lose each other.** Before
  this, two stages finishing close together merged onto the same stale base,
  and the SQL-level merge replaced the first writer's body at every top-level
  key. The merge base is now the body read under the row lock inside the
  write transaction; nothing to do.
- **A second manifest correction keeps the first.** Before this, each
  correction merged onto the sealed body, so correction 2's snapshot dropped
  correction 1's changes. Corrections now chain under the parent row's lock;
  nothing to do.
- **The manifest records which artifact revisions a task produced and
  consumed.** Before this, the outputs section was always empty and the
  cross-run diff compared two nils. Nothing to do.
- **Human gate decisions are recorded.** Before this, every gate action
  transitioned the FSM but wrote no decision, so the section answering "who
  approved this, and when" was always empty. Advance, approve, and continue
  now fail the request if the decision cannot be recorded; cancel tolerates a
  sealed or missing manifest.
- **A failed evidence write is itself recorded.** Before this, failures were
  logged and the task continued, so a task could reach `done` with a sealed
  manifest asserting nothing about the missing evidence. A failure in the
  initial evidence fails the task; later failures record an `EvidenceGap`
  and `evidence_complete` reads false.
- **The manifest's `missing` list is derived at seal time.** Before this, it
  was written once at task start, so sections that had since landed stayed
  listed as missing. Nothing to do.
- **The manifest records the model that served each stage.** Before this, a
  single pointer was overwritten per stage, so a pipeline running different
  tiers per stage recorded only whichever ran last. Nothing to do.
- **The human artifact-edit endpoints exist (`GET`/`PUT` on
  `/tasks/{id}/invocations/{iid}/artifacts/{name}`).** Before this, the
  `human_edit` gate had no edit path, and the store's error cases would have
  surfaced as 500s. The PUT maps them to `409` / `422` / `404`, requires
  `expected_revision_id` when a current revision exists (`428` otherwise),
  and records the edit as the approval.
- **An agent can no longer exfiltrate host files into the evidence store.**
  Before this, a declared artifact path like `/etc/passwd` or
  `../../.ssh/id_rsa` made the orchestrator read the file with its own
  privileges and store it as a durable, API-readable revision. All worktree
  file access now goes through an `os.Root`-backed container; a declared path
  that escapes fails the stage with `artifact_rejected` and pauses the task
  for review.
- **Concurrent artifact writes no longer fork the revision chain.** Before
  this, two writers for the same name could chain two siblings off the same
  revision, and a transient store error could turn an edit into a create that
  left the existing chain behind. The read now happens under a row lock
  inside the transaction; a racing editor gets a conflict instead of a lost
  update.
- **Secret scanning covers binary artifacts, credential-shaped names, and a
  policy knob.** Before this, binaries were not scanned, a name like
  `.ssh/id_rsa` or `.env` was accepted, and findings were always redacted.
  `AGENTUM_ARTIFACT_SCAN_POLICY` selects `redact` (default) or `reject`; set
  `reject` if a credential in an artifact should stop the write, and note
  that an unrecognized value fails at startup.
- **Capability profiles are enforced by the opencode runtime, not merely
  rendered for it.** Before this, the generated permission config had five
  independent defects — absolute path scopes that never matched, permission
  keys never set, a config the repository under test could override, bash
  deny patterns that matched only argument-less commands, and rule order
  correct by coincidence — each confirmed against a real opencode 1.18.10.
  An `mcp.*` grant now refuses the invocation instead of shipping a config
  that appears to be an allowlist and is not; nothing to do.
- **Agent invocations can complete.** Before this, the run context was
  cancelled milliseconds into the agent's work, so every real run ended in
  "cancelled" regardless of the configured timeout. Termination now
  escalates SIGTERM → SIGKILL after a grace period. (#19)
- **The repository builds on Windows.** Before this, the adapter's
  process-group calls used POSIX-only `syscall` members, and CI runs on
  Linux, so nothing caught it. CI now cross-builds windows and darwin. (#19)
- **Contract failures report what the agent did.** Before this, a missing
  `result.json` said only that the file was absent, and "the write was
  refused" and "the agent never attempted it" need opposite fixes. The error
  now carries the observed tool calls.
- **`store.Close` and SSE write errors are reported instead of discarded.**
  Before this, both were dropped. (#1)

### Added
- **Model strings are checked against the runtime's own catalog before they
  can hang a run.** Before this, a typo'd or retired model string surfaced
  only as a run that never produced output. The refusal runs at process
  start, run start, and `Invoke`, names the tier, the model, and the adapter,
  suggests near spellings, and names the listing command; a catalog that
  could not be obtained validates as nil, recorded in evidence as the
  `model_catalog` label (`ok (N models)` / `failed: <reason>` /
  `unsupported`), and runs proceed with models unchecked.
- **New setting `AGENTUM_FIRST_EVENT_TIMEOUT_SECONDS`** (default 120, zero
  disables): it bounds how long an invocation may produce no output at all —
  the failure mode of a hung model is no error, no exit code, and a worker
  slot held forever. The watchdog is disabled permanently once the first
  output line arrives, so legitimate long work never falls under it. Set it
  to zero only if you accept a hung model holding a worker slot.
- **The model surface is readable and testable over the API: `GET
  /api/v1/models`, `POST /api/v1/models/test`, and `GET
  /api/v1/models/test/{id}`** (actions `model:read` / `model:test`). Before
  this, a remote client could not see the configured tiers or probe a model.
  A check is accepted with `202` and reports `models.test_started` /
  `models.test_model_checked` / `models.test_finished` events;
  `Idempotency-Key` is mandatory — the same key and body return the same
  check, a different body is a `409` — targets are deduplicated by
  `(model, variant)`, and execution is serialized. Results live in-process
  for `AGENTUM_MODEL_TEST_RETENTION_MINUTES` (default 60); `timeout_seconds`
  is capped by `AGENTUM_MODEL_TEST_MAX_SECONDS` (default 120).
- **A project is keyed by repository identity, not by its local path, and a
  run pins its working copy.** Before this, moving a directory on disk forged
  a second project and stranded run history; identity is now a fingerprint
  of the repository's own history (`git-roots:v1:<sha256>` over the sorted
  root commits), never accepted from a request body. Registration refuses
  what cannot serve as a working copy — no commits, a shallow clone, a linked
  work tree — each refusal naming its fix. A run executes in the copy it
  pinned at first start; if that copy is gone or holds a different
  repository, the run pauses with `checkout_unavailable` and stays resumable,
  and re-registration reports what happened to unfinished runs
  (`runs_rebound_to_new_checkout`, `previous_repo_path`,
  `runs_awaiting_previous_checkout`). Nothing to do.
- **Decisions and events name their actor.** `actor` (`human | agent |
  system`) with `user_id` beside it answers "who did this, on whose behalf";
  before this, a system-written event could read on the stream as the author
  acting, and an automatic gate that passed read as incomplete evidence. An
  all-automatic run now carries its system decisions; nothing to do.
- **`tenant_id` is `uuid` in every table.** Before this, one seam column
  carried it as `text`, so joins needed a cast that bypassed the index. A
  static test now fails on any seam column that is not uuid; nothing to do.
- **The execution target is a registry entry, and evidence is
  per-invocation (manifest schema `"2"`).** The adapter is selected by
  `AGENTUM_EXECUTION_ADAPTER`; the runtime binary is
  `AGENTUM_RUNTIME_BINARY`, and the retired `AGENTUM_OPENCODE_BINARY` is
  refused at boot naming its replacement. Model options are typed and
  validated at boot, run start, and `Invoke` — nothing is dropped or
  substituted — and an empty `models.yaml` or an `AGENTUM_MODELS_CONFIG`
  naming a missing file is an error, not a fall-back. The manifest carries
  one record per stage attempt (keyed by `invocation_id`, both prompt
  hashes, telemetry), and the cross-run diff indexes attempts by `(stage,
  cycle, ordinal)` with new `adapter-runtime-version` and
  `capability-effective` axes. Schema-1 manifests stay readable.
- **The task request is a typed contract — `description` + `overrides`
  replace the `input` blob — and it reaches the agent.** Before this, the
  title and description never reached any agent prompt. The body is decoded
  strictly (an unknown key is a `400`), `description` is required and
  ≤ 32 KiB, credential material in either field is a `422 bad_input` before
  any row exists, and the request text is fenced as data in the routing
  block. **Breaking**: `POST /api/v1/tasks` and the `Task` response drop
  `input`; consumers must send `description` (plus `overrides` when needed).
  Nothing in `overrides` is ever rendered to an agent.
- **The Backend Development pack ships: plan → human approval → implement →
  review ⇄ fix → final human review, with the plan-approval lock.** Source
  writes depend on a durable approval: while it is pending, every stage's
  profile withholds `{fs.write, git.write, exec.bash}`, an implementer or
  fixer stage is refused (`plan_not_approved`), and an approved plan edited
  afterwards pauses the run (`plan_revision_drift`). Reviewers read an
  orchestrator-produced `diff.patch` + `diff.stat` with `fs.read` alone, and
  prior-stage artifacts reach the next stage. Use the pack from
  `packs/backend-development`; a human approves the plan artifact before
  implementation starts.
- **Project context is a declared channel: pinned instructions and
  enumerated skills.** `.agentum.yaml` gains an `instructions:` list
  (repo-relative, validated) whose bytes are pinned from the run's base
  commit beside the runtime-injected `AGENTS.md`; a tampered copy is
  restored before each stage and the restoration recorded. Skills the
  runtime can load are enumerated once per run — name, location, and content
  hash — and the manifest's `context` section plus a new diff axis make two
  runs differing only in their skill set distinguishable. Projects that want
  agents to follow repo rules should declare the files under
  `instructions:`.
- **Transitions can carry conditions, and the review ⇄ fix loop is
  bounded.** A condition is one term in a closed grammar (`verdict` /
  `status` / `fix_cycles`) — no boolean operators, no scripts — resolved
  first-match-wins by one resolver shared by both advance paths.
  `budgets.fix_cycles: N` caps fixer entries; the `N+1`-th is a controlled
  stop (`fix_budget_exhausted`) that tears nothing down, and an unparseable
  reviewer verdict pauses for a human (`verdict_unreadable`). Pack authors
  declare the conditions and the budget.
- **Project checks are orchestrator-owned: the project ships a versioned
  registry (`.agentum.yaml`), and Agentum runs the resolved set at the
  delivery boundary.** Commands live only in the registry — argument
  vectors, no shell — and packs or task input add checks by name; `required`
  is monotonic, and an agent's claim that checks passed is ignored. A
  mandatory failure blocks delivery, and per-check evidence lands in the
  sealed manifest. Projects should register their build/test/lint commands;
  `AGENTUM_CHECK_TIMEOUT_SECONDS` and `AGENTUM_CHECK_MAX_OUTPUT_BYTES` set
  executor defaults.
- **Every agent invocation runs under a code-enforced capability profile.**
  The effective profile is `host ∩ pack ∩ stage ∩ (role ∪ invocation-grant)`,
  deny by default, applied as a real permission config rather than a prompt
  instruction; `git.delivery` is orchestrator-only and no agent role carries
  it. `AGENTUM_HARD_TIMEOUT_SECONDS` / `AGENTUM_IDLE_TIMEOUT_SECONDS` layer
  per-invocation caps (zero = no cap). Packs select templates with
  `stage.role` (`analyst` / `reviewer` / `implementer` / `fixer`) and may
  narrow with `stage.capabilities`; an unenforceable profile refuses to
  start. Nothing to do.
- **Artifacts are immutable revisions, and every task carries an evidence
  manifest.** Produced or edited artifacts become content-addressed
  revisions stored outside the worktree (`AGENTUM_ARTIFACT_ROOT`, default
  `.agentum/artifacts`), chained by `prev_revision_id` with one current
  pointer per name; the manifest records every input that shaped the run, is
  append-only in flight, seals at terminal state, and takes linked
  corrections afterwards — the sealed row is never edited. Read surface:
  `/tasks/{id}/artifacts` (plus `/revisions/{rid}` and `/content`),
  `/tasks/{id}/manifest`, `/tasks/{id}/manifest/diff?other=<task-id>`, and
  `POST /tasks/{id}/manifest/corrections`. Nothing to do.
- **The task lifecycle separates pause, terminal abort, teardown, and
  cleanup, with orchestrator-owned checkpoints.** `base_ref` resolves once
  to an immutable `base_commit`; each successful stage gets a checkpoint
  commit on `agentum/<task-id>`, and `base_commit..result_commit` is the
  review diff that survives teardown. `cancel` aborts but preserves the
  branch; `cleanup` deletes it explicitly. A crashed worktree is classified
  (`clean` / `resumable` / `restorable` / `needs_attention`) and reconciled
  before any retry, and a periodic reconciler re-queues stale jobs and
  pauses orphaned tasks for a human. Nothing to do.
- **The foundation: Go engine, Postgres store with embedded auto-applied
  migrations, single-front-door HTTP API, multi-tenant schema, explicit FSM,
  memory and event-log tables, docker-compose, CI.** Everything later builds
  on it; nothing to do. (#1)
- **The task API: `POST /api/v1/tasks`, `GET /api/v1/tasks/{id}`,
  `GET /api/v1/tasks`, `POST /api/v1/tasks/{id}/start`.** State transitions
  route through the engine; an illegal transition returns `409`. (#2)
- **Pack format v1: a directory with `manifest.yaml` + `prompts/*.md`.**
  Named-map stages with explicit transitions, a six-value gate vocabulary,
  declared memory scopes and capabilities, fix and ask-to-edit budgets, tier
  policy, semver versioning; the loader and validator reject invalid packs.
  Write pipelines as packs. (#3)
- **Pack overrides: all four layers — lock-major base resolution, fork
  metadata, prompt swaps, stage/budget patches.** The resolved pack is
  re-validated; an override that breaks the contract is rejected. Customize
  a pack with an `overrides.yaml` instead of forking it. (#4)
- **The agent contract and the opencode reference adapter.** Agents write a
  strict `result.json` (status, summary, open questions, artifacts, memory
  writes, edit targets) at the documented artifact path; the adapter runs
  `opencode run --format json` as a subprocess, streams events, strict-parses
  the result, honors cancellation by killing the process group, and resumes
  non-destructively by session id. (#6)
- **The HTTP API contract and the SSE event streams are documented at
  `docs/api.md`.** Structured errors, a declared endpoint table, and two SSE
  streams — tenant-global `/events` and per-task `/tasks/{id}/events` — both
  with `Last-Event-ID` replay; unimplemented endpoints return
  `501 not_implemented`. (#7)
- **Tier→model resolution with per-agent defaults.** A pack's tier (`fast` /
  `strong` / `reasoning`) maps to the model string passed to the agent
  binary's `--model` flag; built-in defaults cover the no-configuration case,
  and a `models.yaml` overrides. Agentum does not manage credentials —
  configure the agent binary directly. (#8)
- **Projects: a project binds a local git repository to a project id (one
  repo = one project per tenant).** `POST` / `GET /api/v1/projects` with
  idempotent registration and real-git-repo validation; tasks reference a
  project, and an inert `related_projects` seam is stored for later
  cross-project access. Nothing to do.
- **The runner: a Postgres-backed job queue and a worker drive a task's
  stages.** Handlers enqueue and return; the worker claims jobs, invokes the
  adapter per stage, persists `stage_invocations`, and evaluates stop
  conditions into the FSM. Heartbeats and boot recovery re-queue what a dead
  worker left behind, bounded by `AGENTUM_JOB_MAX_ATTEMPTS` (default 3).
  Nothing to do.
- **Per-task git worktrees and the orchestrator-owned routing block.** Each
  task executes in its own worktree on branch `agentum/<task-id>`, idempotent
  on re-create and removed at terminal state; the routing block renders
  role/stage/gate context, the `result.json` preamble, and prior-stage
  artifact references. Nothing to do.

### Changed
- **The launch entity's physical names are `run` across the schema, queries,
  and Go identifiers.** Five tables rename — `tasks` → `runs`, with the
  approval, checkpoint, and manifest companions — plus their `task_id`
  columns, indexes, and constraints; a schema guard now fails when any
  relation reintroduces the reserved token, so a missed reference is a loud
  failure. No API surface changes here; nothing to do.
- **The launch entity is named `run` on every human-facing surface.** HTTP
  routes move from `/api/v1/tasks…` to `/api/v1/runs…` (no aliases), JSON
  fields rename `task_id` → `run_id`, event types rename their `task.*`
  prefix to `run.*`, and the manifest's `input.task_id` becomes
  `input.run_id` with no schema bump. API consumers must switch paths and
  field names.
- **The permission vocabulary is declared, not spelled at call sites.**
  Every checked permission is an `authz.Action*` constant, and the check is
  reached from exactly two places, so an unauthenticated, forbidden, or
  absent resource answers identically on every route. No endpoint, status
  code, or payload changes; nothing to do.
- **Blob storage sits behind an `artifacts.ObjectStore` interface** instead
  of the concrete local blob store, so an object-storage backend is a
  drop-in. Nothing to do.
- **Postgres tables live in a dedicated `agentum` schema** (created on boot)
  instead of `public`. Operators pointing tooling at the database should use
  the `agentum` schema. (#1)
- **Error responses are structured**: `{"error":{"code":"…","message":"…"}}`
  replaces the flat `{"error":"..."}`, and the codes (`not_found`,
  `illegal_transition`, `bad_input`, `unauthorized`, `forbidden`,
  `not_implemented`, `internal`) are stable machine identifiers the UI
  branches on. Clients must parse the envelope; pre-0.1 break. (#7)
