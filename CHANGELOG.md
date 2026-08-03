# Changelog

All notable changes to Agentum are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Once tagged releases begin, this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until then the
`[Unreleased]` section accumulates change.

## [Unreleased]

### Added
- **Orchestrator-owned project checks**: the project ships a versioned registry
  of named checks (`.agentum.yaml`, tracked in the repo) and Agentum runs the
  resolved set itself at the delivery boundary — never trusting an agent's claim
  that tests passed. A mandatory (baseline) failure blocks delivery.
  - **Project registry** (`internal/checks`): `.agentum.yaml` defines named
    checks as argument vectors (no shell injection), each with a worktree-scoped
    `workdir`, `timeout_seconds`, `max_output_bytes`, and a `required` flag
    (project baseline). Validated and content-hashed at load so a changed
    contract is detectable across runs. The registry is read from the task's
    `base_commit` (agent-immutable) via `worktree.FileAtCommit`, so an agent
    editing the file in its worktree cannot weaken the checks gating its own
    delivery.
  - **Monotonic resolution** (`checks.Resolve`): the effective set is the union
    of the project baseline + the pack's `checks.required`/`optional` + the
    task's `checks` input. Pack and task add checks **by name only** — unknown
    names are rejected before execution, commands are never accepted from them,
    and `required` is monotonic (once mandatory, always mandatory; no layer can
    weaken the baseline).
  - **Executor** (`checks.Executor`): runs the set under a fixed minimal
    boundary — arg-vector commands from the registry only, a workdir that cannot
    escape the worktree, a scrubbed environment (provider credentials like
    `OPENAI_API_KEY` / `*_TOKEN` stripped), per-check timeouts, and capped
    stdout/stderr. Its profile label is recorded as audit evidence.
  - **Delivery gating**: the runner enforces the set after the final stage's
    checkpoint and before the review gate, against the checkpoint commit (the SHA
    that becomes `result_commit`). A mandatory failure fails the task; the
    sealed manifest carries the per-check evidence for review.
  - **Manifest evidence**: the `body.checks` section now carries the set version,
    registry revision, verified commit, executor profile, mandatory-passed flag,
    and per-check results (status, exit code, duration, capped output, reason,
    definition revision, source layer). Append-only merge with `required`/pass
    monotonicity.
  - **Pack `checks`** field (`pack.CheckPolicy`): `required`/`optional` name
    lists, validated for non-empty and unique names. Documented in
    `docs/pack-format.md`.
  - **Config**: `AGENTUM_CHECK_TIMEOUT_SECONDS` /
    `AGENTUM_CHECK_MAX_OUTPUT_BYTES` set executor defaults (a check's own values
    take precedence).
  - **Dogfooding**: the agentum repo ships its own `.agentum.yaml` baseline
    (`go build`, `go vet`, `gofmt -l`, `go test`), all required.
  - Table-driven tests in `internal/checks` (registry validation/hashing,
    monotonic resolution, unknown-name rejection, executor statuses/timeout/
    output-cap/env-scrub) and an integration suite in
    `internal/runner/checks_test.go` (delivery gating on pass/fail/optional/
    unknown-name).
- **Code-enforced capability profiles** (Epic 6): every agent invocation now
  runs under an effective `caps.Profile` that is computed, persisted, and
  applied as a real boundary — not as a prompt instruction. v1 protects against
  accidental agent actions in the single-owner local runtime (a malicious
  process, kernel/container escape, compromised adapter, and prompt injection
  are explicitly out of scope and documented as escape paths in
  `docs/capabilities.md`).
  - **Provider-neutral model** (`internal/caps`): a capability taxonomy
    (`fs.read`, `fs.write`, `artifact.write`, `exec.bash`, `git.read`,
    `git.write`, `git.delivery`, `net.fetch`, `secret.<name>`, `mcp.<server>`)
    with role templates (`analyst`, `reviewer`, `implementer`, `fixer`,
    `system`). The effective profile is `host ∩ pack ∩ stage ∩ (role ∪
    invocation-grant)`, deny by default. `git.delivery` is orchestrator-only;
    no agent role includes it. Time/resource limits ride as `HardTimeout` /
    `IdleTimeout` profile fields.
  - **Opencode adapter enforcement** (`internal/agent/enforce.go`): before the
    subprocess starts, the adapter confirms the profile is enforceable
    (`EnforceableBy`), renders a per-invocation deny-by-default opencode
    permission config, scrubs credential-bearing env vars (re-added only for
    granted `secret.<name>`), applies deny patterns for delivery-ref mutation
    (`git push*`, `git reset --hard*`, `git branch -D*`, `git update-ref*`,
    `git rebase*`, …), and wraps ctx with the hard/idle timeouts. An
    unenforceable profile refuses to start with
    `stop_reason=capability_unenforceable`. The config's shape, delivery, and
    verification against a real opencode landed in the follow-up under *Fixed*;
    `mcp.<server>` remains part of the capability vocabulary but is not
    enforceable by this adapter and is refused rather than silently accepted.
  - **Pack role + stage capabilities**: optional `stage.role` (one of analyst /
    reviewer / implementer / fixer; defaults to a convention derived from the
    stage id) and optional `stage.capabilities` (a per-stage subset that
    narrows the pack ceiling; absent → inherit). Documented in
    `docs/pack-format.md`.
  - **Saved effective profile**: `stage_invocations.capability_profile` jsonb
    (migration `0007`) is the authoritative per-invocation record; a
    `stage.capability_enforced` event and a per-stage snapshot in the manifest's
    `body.capabilities.effective` section mirror it for audit and cross-run
    diff.
  - **Routing block** now renders the exact granted set with a deny-by-default
    notice (replacing the old "native defaults" stub).
  - **Config**: `AGENTUM_HARD_TIMEOUT_SECONDS` / `AGENTUM_IDLE_TIMEOUT_SECONDS`
    layer per-invocation caps onto every profile (zero = no cap).
  - Table-driven tests in `internal/caps` (intersection, role templates,
    enforceability) and `internal/agent` (config rendering, env scrub, scope
    substitution, unenforceable refusal); an integration suite in
    `internal/runner/capabilities_test.go` proves role isolation (analyst /
    reviewer / implementer), deny-by-default, the unenforceable-profile refusal
    path, and that the profile is saved + emitted as evidence — all without the
    opencode binary.
- **Immutable artifact revisions and evidence manifest** (F.7): the worktree is
  disposable, the artifacts an agent produces during a stage and the inputs that
  shaped the run are the durable record. Two pieces:
  - **Artifact revisions** (`artifact_revisions` table + content-addressed FS
    blob store): every produced or edited artifact variant becomes a new
    immutable revision stored outside the worktree at
    `<ArtifactRoot>/<hash[:2]>/<hash>` (default
    `<cwd>/.agentum/artifacts`, overridable via `AGENTUM_ARTIFACT_ROOT`). Edits
    chain via `prev_revision_id`; the single `is_current` pointer per
    `(task, name)` is the only mutable bit (enforced by a partial unique index).
    Revisions are linked to their producing `stage_invocation` via
    `source_invocation_id`. An optional execution coordinate
    (`delivery_step` / `execution_unit` / `phase`) rides along as provenance —
    inert when NULL, single-unit runs never fill it. A `Syncer` materializes
    the current revisions back into the worktree on `continue` / `advance` so
    the agent starts from the same content the prior invocation produced. A
    best-effort `DefaultRedactor` scrubs Bearer / Authorization headers, AWS
    AKIA keys, GitHub PATs, labeled `token|secret|password|api_key` values, and
    PEM private key blocks before bytes enter the store.
  - **Evidence manifest** (`task_manifests` table, one per task): records every
    input that shaped the run — input task + revision, project + base_commit,
    pack + version + content hash, prompt revisions the adapter saw, adapter +
    declared capabilities, model + tier, effective capability profile, memory
    slice, input/output artifact revisions, check set version + results, human
    gate decisions, and the git lineage (branch, checkpoints, base_commit,
    result_commit). Append-only while in flight; sealed at terminal state
    (`done | failed | cancelled | interrupted`). Corrections after sealing land
    in `task_manifest_corrections` as linked rows with a fresh body snapshot —
    the sealed row is never edited. Subsystems not yet wired (project memory,
    project checks, capability enforcement) are listed explicitly under
    `body.missing` rather than hidden.
  - **Read surface**: `GET /tasks/{id}/artifacts[?current=true]`,
    `GET /tasks/{id}/artifacts/revisions/{rid}`,
    `GET /tasks/{id}/artifacts/revisions/{rid}/content` (streamed),
    `GET /tasks/{id}/manifest`, `GET /tasks/{id}/manifest/diff?other=<task-id>`
    (input-level diff — outputs and human decisions are not compared, those are
    results not inputs), `POST /tasks/{id}/manifest/corrections`.
  - Table-driven tests for the blob store (roundtrip, idempotent, concurrent,
    invalid hash), the redactor (Bearer / AWS / GitHub / labeled / PEM /
    binary), the syncer (write / skip / escape / pinned), the manifest body
    merge semantics, and the per-axis input-level diff (order-independent,
    output-ignoring).
- **Safe lifecycle, checkpoints, and code egress** (F.6.1): the primitive Epic 8
  reuses for every execution unit. Separates four concepts that were previously
  conflated: **pause** (non-terminal, resumable), **terminal abort**
  (`POST /tasks/{id}/cancel` — worktree torn down, branch + recovery work
  preserved), **worktree teardown** (disposable working tree only — the
  `agentum/<task-id>` branch and its commits are the durable delivery output),
  and **cleanup** (`POST /tasks/{id}/cleanup` — explicit, idempotent, audited
  branch deletion on a terminal task). A generic `cancel` cannot ambiguously
  mean all three.
  - Task input records `base_ref` (branch/tag/SHA/`HEAD`, default `HEAD`);
    Agentum resolves it **once** to an immutable `base_commit` before creating
    the worktree (`worktree.Manager.Create` branches off the resolved SHA, so a
    later move of `base_ref` cannot retcon lineage). Terminal teardown captures
    `result_commit` (the `agentum/<task-id>` tip); `base_commit..result_commit`
    is the review/handoff diff and survives teardown. The task response exposes
    `base_ref` / `base_commit` / `result_commit` / `branch`.
  - **Orchestrator-owned checkpoints** (`task_checkpoints` table): immutable
    commit SHAs at stage boundaries (`base` + `post-<stage>`). Agents may edit
    and inspect git but cannot create/delete/reset/rebase delivery refs —
    Agentum owns boundary checkpoints.
  - **Reconciliation before retry/resume**: a crashed worktree is classified as
    `clean`, `resumable`, `restorable`, or `needs_attention`; a restorable tree
    is reset to the last checkpoint (`Restore` = `git reset --hard` +
    `git clean -fd`) so a side-effectful stage is never blindly replayed against
    a half-modified tree. A needs-attention tree fails the task for a human
    rather than guessing.
  - **Transactional outbox**: every HTTP FSM transition that carries a
    runnable-job intent enqueues the job in the same database transaction
    (`api.runInTx`); a failed enqueue rolls back the transition, so a task can
    never be left `running` with no driver intent.
  - **Periodic reconciler** (`internal/jobs.Reconciler`): runs at boot and on a
    ticker (not only process startup) to re-queue stale job leases and repair
    orphaned running tasks (state=`running` with no live job) by pausing them
    for explicit human resume. Bounded by `AGENTUM_JOB_MAX_ATTEMPTS`.
  - Lifecycle tests prove a committed change survives terminal teardown and is
    diffable as `base_commit..result_commit`; that pause/resume, abort, crash,
    and cleanup preserve their documented invariants; and that the reconciler
    repairs orphaned tasks without replaying side effects.
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
- **Blob storage sits behind an `artifacts.ObjectStore` interface** instead of
  the concrete `*BlobStore`. The revisions index and the byte store were welded
  together through a struct field, which pinned the durable layer to the local
  filesystem; a deployment where the orchestrator is not the only reader of the
  blobs needs an object-storage backend, and that is now a drop-in.
- Postgres tables live in a dedicated `agentum` schema (created on boot before
  migrations run) instead of the default `public` schema (#1).
- **Error responses are now structured**: `{"error":{"code":"...","message":"..."}}`
  replaces the previous flat `{"error":"..."}` (#7). Codes are stable machine
  identifiers the UI branches on (`not_found`, `illegal_transition`, `bad_input`,
  `unauthorized`, `forbidden`, `not_implemented`, `internal`). Pre-0.1 break.

### Fixed
- **An agent could exfiltrate host files into the evidence store**: artifact
  paths in `result.json` are declared by the agent, and the runner resolved them
  by joining or — for an absolute path — using them verbatim, with no
  containment check. `{"path": "/etc/passwd"}` or `{"path": "../../.ssh/id_rsa"}`
  made the orchestrator read the file with its own privileges and store it as a
  durable, API-readable artifact revision attributed to the task. All worktree
  file access now goes through `artifacts.Container`, an `os.Root`-backed handle
  that confines reads and writes to one tree.
  - **A link the agent planted is an escape a path comparison cannot see**, and
    resolving the path by hand does not close it either: `filepath.EvalSymlinks`
    follows POSIX symlinks but returns Windows junctions unresolved, so a
    junction inside a worktree reads as contained. Delegating to `os.Root` makes
    the containment check the OS's, performed as part of the open — which also
    removes the window between validating a path and using it.
  - **The write side had the same hole.** `artifacts.Syncer` materializes
    revisions back into a worktree the agent had write access to, guarded only
    by a lexical check, so a planted link turned a sync into an overwrite of a
    host file. It writes through the same container now.
  - **Fail-closed, not skip-and-continue.** A declared path that escapes fails
    the stage: the capture is all-or-nothing, the invocation is finalized with
    `stop_reason = artifact_rejected`, a `stage.artifact_rejected` event records
    the path and reason, and the task pauses for review. Recording the stage as
    complete would have meant accepting output the orchestrator refused to read.
    A declared-but-unwritten file remains an ordinary contract gap.
- **Concurrent artifact writes could fork the revision chain**: the current
  revision was read *before* the transaction that demoted it, so two writers for
  the same `(task, name)` could both read revision N and chain two siblings off
  it — the partial unique index then rejected one at random, after both had
  written blobs. The read now happens inside the transaction under a row lock,
  and the demotion targets that exact revision id so a zero-row update is a
  conflict rather than a silent no-op. Two racing first-creates have no row to
  lock; the index still serializes them, and the loser's constraint violation
  now surfaces as `ErrRevisionConflict` instead of an opaque driver error.
  - A related swallow is gone: `lookupCurrent` returned "no current revision" for
    *any* store error, so a transient DB failure turned an edit into a create
    that silently orphaned the existing chain.
  - `PutParams.ExpectedCurrentRevision` adds an optimistic-concurrency
    precondition for callers that composed an edit against a revision they were
    looking at — the seam the human-edit API needs to answer 409 rather than
    losing an update. Checked before the identical-content shortcut: a caller
    whose pinned revision is gone has lost the race whatever the bytes say.
- **Secret scanning had three gaps** (`artifacts.Scanner`, formerly `Redactor`):
  - **Binary artifacts were not scanned at all**, so a credential inside one
    entered the store unnoticed. They are now matched against the context-free
    rules (AKIA, `ghp_`, PEM headers) but never rewritten — substituting a
    placeholder would change the blob's length and corrupt it — so detection is
    reported and `reject` is what actually stops the write.
  - **There was no policy knob**: findings were always redacted, which is the
    wrong default for a deployment where a credential in an artifact is an
    incident. `AGENTUM_ARTIFACT_SCAN_POLICY` selects `redact` (default) or
    `reject`; an unrecognized value fails at startup rather than falling back, so
    an operator who asked for rejection never silently gets redaction.
  - **A credential-shaped *name* was accepted.** `.ssh/id_rsa`,
    `.aws/credentials`, `.env`, `.netrc`, `*.pem` and similar are now refused
    under every policy — a path has nothing to redact. `.env.example` and its
    siblings stay allowed.
- **Capability profiles are now enforced by opencode, not merely rendered for
  it**: the generated permission config was a boundary in name only. Five
  independent defects, each confirmed against a real opencode 1.18.10 rather
  than inferred from its documentation.
  - **Path scopes were absolute, and absolute patterns never match.** opencode
    normalises a tool's target path to a project-root-relative form before
    matching, so every scoped rule was dead. The failure was silent: the deny
    baseline refused the write while the config still read correctly, which
    meant an analyst or reviewer could not produce `result.json` at all —
    analytical stages of any pipeline could not complete. Scopes are now emitted
    relative to the worktree (`${worktree}/**` → `**`, an artifact scope →
    `.agentum/<task>/.ag-artifacts/<stage>/**`), and a scope that cannot be
    expressed relative to the worktree is an error rather than a silently
    dropped grant. The audit profile keeps absolute paths — it is evidence, not
    configuration.
  - **`write` is not an opencode permission key** (write and patch are governed
    by `edit`), and eleven documented keys were never set at all. Since opencode
    merges config sources and overrides only conflicting keys, every omitted key
    fell through to the operator's global config or the repository's own
    `opencode.json` — and `--auto` approves whatever is not explicitly denied.
    The config now opens with a `"*": "deny"` catch-all and sets every
    documented key, including `external_directory`, `task`, `skill`, `websearch`
    and `lsp`.
  - **The config could be overridden by the repository under test.** It was
    written to `<worktree>/.opencode/opencode.json`: editable by the very agent
    it constrains, and below a project's own `opencode.json` in precedence. It
    now renders to a per-invocation directory outside the worktree (as
    `OPENCODE_CONFIG`, for the audit trail) and is inlined into
    `OPENCODE_CONFIG_CONTENT`, which loads above every project source; ambient
    `OPENCODE_CONFIG*` variables are dropped from the child environment. As a
    side effect the worktree is no longer dirtied on every run — which had made
    `isClean()` permanently false and the `auto_if_clean` gate unreachable.
  - **Every bash deny pattern was inert.** opencode matches bash rules as
    wildcard patterns against the parsed command, not as substrings, so
    `"git push"` matched only the argument-less command and `git push origin
    HEAD` passed. All patterns now end in `*`; the common network clients are
    additionally denied when `net.fetch` is not granted.
  - **Rule order was correct by coincidence.** opencode resolves rules with the
    last match winning, and the rendering used a Go map, which `encoding/json`
    sorts. Rules are now an ordered list with an explicit deny-first invariant.
  - `mcp` is removed from `adapter.Supported()`: opencode addresses MCP tools by
    per-tool permission names this adapter cannot enumerate for an arbitrary
    server, so "this server and nothing else" is not expressible. An `mcp.*`
    grant now refuses the invocation instead of shipping a config that looks
    like an allowlist and is not.
  - Verified at two levels: `TestPermissionScope_*` pins the relative-scope rule
    against a port of opencode's matcher (deterministic, no model in the loop),
    and the opt-in `TestOpencodeLive_*` suite drives the real binary — an
    analyst writes its own artifact, an analyst is refused a source edit, an
    implementer is permitted one — asserting on the bytes on disk rather than on
    the agent's account of what it did. `docs/capabilities.md` records the
    version that passed.
- **Agent invocations could not complete** (#19): the adapter created the run
  context in `Invoke` and released it with a deferred cancel in the same
  function. `Invoke` returns as soon as the subprocess starts, so the cancel
  fired milliseconds into the agent's work and `exec.CommandContext` killed it —
  every real run ended in "cancelled" regardless of the configured timeout.
  Ownership now transfers to the goroutine draining the stream. Two adjacent
  defects went with it: `cmd.Wait` was called twice (unsafe, and the async call
  raced the stdout reads it must follow), and the idle watcher killed the
  process without cancelling the context, surfacing an idle stop as a confusing
  parse failure. Termination now escalates SIGTERM → SIGKILL after a grace
  period. Pinned by subprocess tests that re-exec the test binary as a fake
  agent, so they run in ordinary CI with no opencode binary.
- **The repository did not build on Windows** (#19): the adapter's process-group
  calls used POSIX-only `syscall` members, and CI runs on Linux, so nothing
  caught it. They now sit behind per-GOOS implementations, and CI cross-builds
  windows and darwin. This also exposed a Windows-only test failure that had
  never been runnable there (a rendered-config assertion compared against a raw
  path, while the path is backslash-escaped inside JSON).
- **Contract failures now report what the agent did**: a missing `result.json`
  used to say only that the file was absent. "The write was refused" and "the
  agent never attempted it" need opposite fixes, and the agent's prose is not
  evidence for either, so the error now carries the observed tool calls.
- `store.Close` and SSE write errors are no longer silently dropped (#1).
