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
   ▸ awaiting_final_review → POST .../approve  (task done)
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
   - On `reach_final_gate` → task moves to `awaiting_final_review`; loop completes.
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
| final stage + final gate reached | `reach_final_gate` | `awaiting_final_review` | — |
| `result.json` missing / invalid | `stop_user` | `paused_user_stop` | `parse_error` |
| adapter returned `EventError` | `stop_user` | `paused_user_stop` | `adapter_error` |
| declared artifact path escapes the worktree | `stop_user` | `paused_user_stop` | `artifact_rejected` |
| resolved transition targets a fixer stage, but the fix budget is spent | `stop_user` | `paused_user_stop` | `fix_budget_exhausted` |
| a source-writing stage (effective role implementer or fixer) entered while the run's `source_write` approval is absent | `stop_gate` | `paused_gate` (pinned to the **approval stage**, not the refused stage) | `plan_not_approved` |
| the approved plan revision no longer matches the approval artifact's current revision (the plan was edited after approval) | `stop_gate` | `paused_gate` (pinned to the **approval stage**) | `plan_revision_drift` |
| a verdict-sourcing stage produced no parseable `verdict.json` | `stop_user` | `paused_user_stop` | `verdict_unreadable` |
| ctx cancelled by user | `cancel` | `cancelled` | — |

### Conditional transitions and the fix loop

A transition may carry a `condition` in the closed grammar (see
[docs/pack-format.md](pack-format.md#transition-conditions)). The runner resolves
the first matching edge in declaration order through one resolver, shared
between the auto-advance path and the `advance` job. A reviewer stage sources a
`verdict` condition and writes `verdict.json`; the orchestrator parses it (never
the agent's prose) and routes on the `verdict` field. A fixer stage is
budget-bound: `budgets.fix_cycles: N` ⇒ at most `N` fixer entries; the `N+1`-th
is refused with `fix_budget_exhausted`.

The fix-cycle counter is durable — derived from `stage_invocations.cycle` (the
0-based repeat index of a stage within the task), not process memory. It
survives a worker restart (recomputed from committed rows) and cannot be
inflated by a resume (a resume inherits its cycle). Each retry is a separate
invocation row with its own `sequence` and `cycle`.

`fix_budget_exhausted` and `verdict_unreadable` are controlled pauses, not
failures: nothing is torn down (the branch, checkpoint commits, artifact
revisions, and the unsealed manifest all stay). From `paused_user_stop` a human
can `continue` (the loop re-evaluates, hits the bound again, and stops again —
bounded, not a runaway) or `cancel` (terminal, branch preserved).

`plan_not_approved` and `plan_revision_drift` are the same controlled shape,
and belong to the plan-approval lock below.

### Plan-approval lock (ADR 0003)

A pack may declare a `source_write` approval (see [docs/pack-format.md](pack-format.md#approvals)).
While that approval is pending, no stage may write source. The lock is three
layers, in order of authority:

1. **Capability withholding (the guarantee).** While the run's `source_write`
   approval is absent, the runner passes `SourceWriteCategories`
   (`fs.write`, `git.write`, `exec.bash`) as `Input.Withheld` for **every**
   stage, after the four-way intersection. The runtime refuses source writes
   regardless of role — a pack that mislabels a source-writing stage as an
   analyst gains nothing. See [docs/capabilities.md](capabilities.md).
2. **Entry refusal + drift detection.** The runner refuses to *enter* a
   source-writing stage (effective role implementer or fixer) while the unlock
   is absent, as `plan_not_approved` — running a crippled implementer that
   writes nothing and reports `complete` would burn a cycle and mislead. When
   the unlock *is* granted, it checks the approval artifact's current revision
   against the one the human approved; a mismatch (the plan was edited after
   approval) is `plan_revision_drift`. This layer protects against a misleading
   result; layer 1 is what actually stops the write.
3. **Static validator.** At load time, the pack validator flags a graph where a
   source-writing stage is reachable from `entry` without passing through the
   approval stage. Advisory — it cannot see runtime state; layer 1 holds even
   if this check is wrong.

The guarantee is layer 1. Layers 2 and 3 turn a silent refusal into a named,
human-actionable stop and catch authoring mistakes early.

**Where the layer-2 stop pauses matters.** Both stops pin `current_stage` to
the **approval stage**, not the refused stage. The advance job resolves the
*current stage's* transition, so a `paused_gate` stop must sit at a stage that
already ran; pinning the never-invoked implementer would make advance resolve
the implementer's transition and silently skip it — reaching review against an
empty diff with no plan approval ever recorded. Pinned to the approval stage,
recovery is exactly the ordinary plan-gate advance:

- `plan_not_approved` → advance records the plan approval (the pause sits at
  the approval stage, so the handler's stage guard matches) and the
  implementer runs with the grant. Reject and cancel also work from this stop.
- `plan_revision_drift` → the approval row already exists, so advance's write
  is a no-op; the run re-checks, drifts again, and pauses at the same place —
  never a skip, and cancel is the exit (reject returns 409: the plan gate was
  already decided approved). Drift cannot be cleared by advance: re-editing the
  plan mints a revision the approval is not bound to. The retry is free only
  when the approval stage transitions straight into the source-writing stage
  (the shipped pack's shape); with intermediate stages between them —
  well-formed under the validator's pass-through rule — each advance re-runs
  those stages before re-hitting the refusal, so each retry costs real
  invocations. A pack author adding a stage between plan and implement is
  buying that.

### Orchestrator-produced delivery diff (ADR 0003)

Reviewer-role stages and the final gate do not run `git` themselves (the
reviewer role grants no `exec.bash`). The orchestrator produces the change set
for them: `diff.patch` (`git diff base..HEAD`, capped at a hunk boundary with a
truncation marker) and `diff.stat` are written into the reviewer's artifact dir
and captured as immutable revisions (`<stage>/diff.patch`, kind `diff`;
`<stage>/diff.stat`, kind `diff_stat`). The routing block's "Delivery diff"
section names the paths. The diff runs to the post-stage checkpoint the
orchestrator authored, so it describes a real commit range.

### `result_commit` at the final gate

`result_commit` is recorded when the task reaches `awaiting_final_review`, not
only at teardown — it names the commit the human is asked to review (the
checkpoint the delivery checks verified). Teardown re-records only if it was
unset. A divergence between the recorded `result_commit` and the live branch tip
at teardown (something moved the branch between review and teardown) is recorded
as an evidence gap + `task.delivery_commit_diverged` event; the human already
approved, so the task is not failed.

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
| `POST .../approve` | `awaiting_final_review → done` | `teardown` |
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
job (`kind=teardown`) that runs `git worktree remove --force` **only**. The
`agentum/<task-id>` branch and its commits are NOT deleted at teardown — they
are the durable delivery output that survives for review and Epic 8 handoff.
Branch deletion is a separate, explicit `cleanup` action (below). It is
enqueued by:

- `handleInvocationApprove` — after the task moves to `done`.
- `handleCancelTask` — after the task moves to `cancelled`.
- `failTask` (best-effort) — when a run moves the task to `failed`.

Before removing the worktree, the teardown job captures the tip of
`agentum/<task-id>` as `result_commit` on the task row — the immutable record of
what was delivered (done) or recovered (cancelled/failed). The branch survives
teardown, so `result_commit` is always resolvable after the fact; the
`base_commit..result_commit` range is the review/handoff surface.

Enqueuing (rather than removing inline) serializes teardown with the
still-running driving job — it never races the runner. The teardown job is
idempotent: a missing worktree is a no-op.

**F.7:** artifact *files* are now durable independently of the worktree —
`artifact_revisions` rows + the content-addressed blob store survive teardown.
The parsed `result.json` is still on `stage_invocations.result`; the revisions
store adds the bytes and the immutable edit chain.

## Safe lifecycle, checkpoints, and code egress (F.6.1)

This is the primitive Epic 8 reuses for every execution unit. It separates four
concepts that were previously conflated:

| Concept | Verb | Effect | Branch + commits |
|---|---|---|---|
| **Pause** | FSM `stop_*` events | Non-terminal; resumable via `continue`/`advance` | preserved |
| **Terminal abort** | `POST /tasks/{id}/cancel` (FSM `cancel`) | Terminal (`cancelled`); worktree torn down | **preserved** |
| **Worktree teardown** | `teardown` job | Removes the disposable working tree | **preserved** |
| **Cleanup** | `POST /tasks/{id}/cleanup` (`cleanup` job) | Explicit branch deletion; idempotent; audited | deleted |

A generic `cancel` cannot ambiguously mean all three — each is a distinct,
named action. Pause is non-terminal; abort is terminal-but-preserves-delivery;
cleanup is post-terminal disposal.

### Git lineage: base_ref, base_commit, result_commit

Every task records its git lineage explicitly:

- **`base_ref`** (input): the ref the task builds against — branch / tag / SHA /
  `HEAD`. Set at `POST /tasks`, defaults to `HEAD`. The input record is
  reproducible from this.
- **`base_commit`** (anchor): the full SHA `base_ref` resolved to, captured
  **once** before the worktree is created (`SetBaseCommit` is a `WHERE
  base_commit IS NULL` no-op after the first capture). The worktree branches
  from this SHA, so a later move of `base_ref` cannot retcon the task's lineage.
- **`result_commit`** (delivery): the tip of `agentum/<task-id>` captured at
  the final gate (`awaiting_final_review`), naming the commit the human reviews.
  Immutable; the branch survives teardown so this is always resolvable.
  `base_commit..result_commit` is the review/handoff diff.

`result_commit` is captured at the final gate (the commit the human is asked to
review) and re-confirmed at teardown, while the delivery checks run earlier
(verifying the commit recorded as `body.checks.commit`). If the two diverge — a
continue job, a human artifact edit, or a filesystem change moved the branch tip
in between — teardown does not silently seal a manifest asserting "checks passed
at X" alongside "delivered Y". The verified commit is read back from
`body.checks.commit` (not proxied through the latest checkpoint, whose
correctness would depend on an FSM property a future ask-to-edit feature could
break) and compared against `result_commit`. The divergence is recorded as an
evidence gap (so the sealed manifest reads `evidence_complete: false`) and
emitted as a `task.delivery_commit_diverged` event naming both SHAs. The task is
not failed: the human already approved, and the manifest's incompleteness is the
signal a reviewer acts on.

A comparison that *cannot* run is recorded too. If the verified commit cannot be
read back, teardown records an evidence gap rather than returning quietly —
"checked, no divergence" and "never checked" are different claims about a
delivery, and a manifest silent about both would be the fail-open shape this
comparison exists to remove. No divergence event is emitted in that case:
nothing was compared, so asserting a divergence would be equally unsupported.
An *absent* checks commit (a task that never reached delivery, or a project that
defines no checks) is an absence rather than a failure and records nothing.

The task response exposes all three plus `branch` (the canonical
`agentum/<task-id>` ref) so a UI or Epic 8 handoff can render and diff delivery
without touching git. Provider PR creation belongs to Epic P and is not required
for safe local egress.

### Checkpoints

The orchestrator owns boundary checkpoints (`task_checkpoints` table): immutable
SHAs recorded at stage boundaries. The runner captures `base` (the lineage
anchor, a commit that already exists) plus a `post-<stage>` checkpoint after
each successful stage invocation. `(task_id, label)` is unique, so a retry that
re-crosses a boundary upserts rather than duplicates.

The orchestrator authors the post-stage checkpoint commit itself
(`worktree.Manager.Commit`), staging the worktree's working state and committing
it on `agentum/<task-id>` under the identity `agentum <agentum@orchestrator>`
(passed inline via `git -c`, so it does not depend on ambient config and the
audit trail shows Agentum authored the boundary). This is the `git.delivery`
privilege the capability model reserves for the orchestrator and no agent role
carries; before this the orchestrator only *read* HEAD, so every checkpoint was
the base SHA and the agent's uncommitted work was discarded at teardown. A stage
that produced no change records the unchanged HEAD honestly with no empty commit
— an empty commit per stage would pollute the lineage a reviewer reads.

Agents may edit and inspect git but cannot create, delete, reset, or rebase
delivery refs — `agentum/<task-id>` and checkpoint SHAs are orchestrator-owned.
The routing block tells the agent this; Agentum enforces it by being the only
thing that touches those refs (and now, by being the thing that commits them).

#### Checkpoint commits and the `auto_if_clean` gate

The checkpoint commit destroys the signal the `auto_if_clean` gate reads, so the
order of the two is load-bearing: **worktree cleanliness is sampled before the
checkpoint commit, and that sample is what the gate evaluates.**

`auto_if_clean` exists to surface "the agent touched files beyond its declared
`edit_targets`" — a property of the working tree the agent left behind. Staging
and committing that tree makes it clean by construction, so a cleanliness check
taken after the commit is always true and the gate degenerates into `auto`,
auto-advancing exactly the runs it was meant to hold for review. The undeclared
files are committed into the delivery at the same time, so the signal is lost on
both sides.

This is a two-sided trap, and both sides have been hit:

| Sampled | `isClean` | Effect on the gate |
|---|---|---|
| While a per-invocation config file was written into the worktree | always false | gate unreachable; everything paused for review |
| After the checkpoint commit | always true | gate unreachable; nothing paused for review |

The first was fixed by moving the adapter's generated config out of the worktree
(see the opencode permission-config entry in `CHANGELOG.md`); the second by
sampling before the commit. A change to either the checkpoint or the gate should
re-check this ordering — the pure evaluator tests cannot catch it, because they
feed `Clean` directly to `Evaluate`; only a test that drives the stage loop can
(`TestRunner_AutoIfCleanGateFiresOnUndeclaredWrite`).

### Reconciliation before retry/resume

Before driving a side-effectful stage on a retry or resume, the runner
reconciles the worktree (`worktree.Manager.Reconcile`). A crashed worktree is
classified as one of:

| Class | Condition | Runner action |
|---|---|---|
| `clean` | HEAD at base_commit (or last checkpoint), tree clean | proceed |
| `resumable` | committed work beyond base, tree clean | proceed from HEAD |
| `restorable` | uncommitted changes | `Restore` to last checkpoint (or base), then proceed |
| `needs_attention` | worktree missing, or HEAD in an unexpected lineage | fail the task — surface for a human |

A side-effectful stage is never blindly replayed against a half-modified tree.
`Restore` is `git reset --hard <checkpoint>` + `git clean -fd`, so the
post-restore tree is byte-identical to the checkpoint (untracked agent-written
files from the crashed run are discarded).

### Transactional outbox + periodic reconciler

Every HTTP-driven FSM transition that carries a runnable-job intent enqueues the
job **inside the same database transaction** as the transition
(`api.runInTx`). A handler that cannot enqueue rolls back the transition — a
task can never be left `running` with no driver intent.

A periodic reconciler (`internal/jobs.Reconciler`, started on boot and on a
ticker) repairs what crashes still leave behind, not only at process startup:

- **Stale job leases** — `status='running' AND heartbeat_at < now() - stale` —
  re-queued, or failed past `AGENTUM_JOB_MAX_ATTEMPTS`.
- **Orphaned tasks** — `state='running'` with no live (pending/running) job —
  transitioned to `paused_user_stop` (reason `interrupted`) so a human resumes
  explicitly. Conservative by design: a half-run stage is never auto-replayed.

## Events

Only **meaningful** events are persisted to the durable `events` log (`04 §7.1.5`).
Live stream text/tool chunks are forwarded to SSE subscribers but never written
to the DB; `Last-Event-ID` replay reconstructs state changes, stage boundaries,
stop reasons, telemetry, and errors — not the full transcript. This keeps write
volume sane and matches the audit-trail intent.

F.6 emits: `task.state_changed`, `stage.started`, `stage.stopped`,
`stage.telemetry`, `task.worktree_created`, `task.worktree_removed`.
F.7 adds: `task.revisions_synced` (current revisions materialized into the
worktree at stage start).

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

## Project checks (orchestrator-owned)

An agent must not get to declare its own work "done" by claiming tests passed.
For honest dogfooding, Agentum runs the project's checks itself and reads the
result from its own executor. The commands are project-owned (they depend on the
stack), so they live in a versioned project file, not in a pack or an architect's
plan.

### The registry (`.agentum.yaml`)

A tracked file at the repo root carries the versioned registry of named checks:

```yaml
api: agentum/v1
instructions:               # optional: extra project-instruction files to pin
  - AGENTS.md
  - docs/conventions.md
checks:
  - name: build
    command: ["go", "build", "./..."]
    workdir: "."
    timeout_seconds: 240
    max_output_bytes: 1048576
    required: true          # project baseline: always runs, failure blocks delivery
  - name: lint
    command: ["golangci-lint", "run"]
    required: false         # registered but optional unless a pack/task requires it
```

Each check is an **argument vector** (first element is the binary; no shell
unless a check explicitly invokes one). Only this file supplies commands — it is
versioned with the code and read from the task's `base_commit` (the lineage
anchor, captured before the worktree is created), so the registry an agent is
gated against is the one the project committed, not one the agent could edit in
its worktree.

### Who can add, remove, or override what

| Source | Can do | Cannot do |
|---|---|---|
| Project registry | Define commands, mark a check `required` (baseline) | — |
| Pack (`checks.required` / `checks.optional`) | Add a check **by name**; mark it mandatory | Supply a command; remove a baseline check; weaken an already-mandatory check |
| Task input (`checks.required` / `checks.optional`) | Add a check **by name**; mark it mandatory | Supply a command; remove or weaken |
| Agent | nothing | Claim checks passed (its claim is ignored) |

`checks.Resolve` enforces all of this before any execution: an unregistered name
is rejected, a command is never accepted from pack/task input, and `required` is
monotonic (OR across baseline + pack + task — once mandatory, always mandatory).
The registry itself is loaded from `base_commit`, so an agent cannot weaken the
checks by editing `.agentum.yaml` inside its worktree.

### When checks run

The runner runs the resolved set once, at the **final delivery boundary**: after
the last stage's checkpoint is recorded and before the task reaches the review
gate (`awaiting_final_review`).

**Commit binding.** The checks must verify exactly the commit they claim to. The
runner resolves the worktree HEAD (the post-stage checkpoint commit the
orchestrator authored) *before* the executor runs and asserts the tree is clean
first: a dirty tree means something wrote after the checkpoint, and running the
checks against it would test content that exists in no commit while the manifest
asserts a specific SHA was verified. A dirty tree at this boundary fails the task
rather than claiming a verification it cannot stand behind. Only then does the
executor run, and the recorded `checks.commit` is that checkpoint SHA by
construction — not a pre-run HEAD read that could drift. The outcome is recorded
as manifest evidence:

- every per-check result (status, exit code, duration, capped stdout/stderr,
  stop reason, definition revision, which layer contributed it);
- the resolved set version + the registry revision;
- the verified commit and the executor's capability label;
- `ran`, which is true only when at least one check executed — an empty set (the
  project defines no checks) is a legitimate configuration recorded as
  `ran: false` so it is not misread as a gate that ran and cleared.

A **mandatory failure blocks delivery**: instead of reaching the review gate, the
task fails, and the check evidence in the sealed manifest is the record. Optional
check failures are recorded as evidence but do not block. A successful run is the
evidence available to the final reviewer. Reaching the delivery boundary without
a resolved `base_commit` also fails the task — the anchor is required to load the
registry and to gate delivery, and its absence there is a broken invariant, not
an early exit to an empty set.

### The executor's boundary

The executor is orchestrator code, not an agent adapter — it is not subject to
the `caps` intersection. It is bound by code instead: arg-vector commands from
the registry only, a worktree-scoped working directory that cannot escape, a
scrubbed environment (provider credentials like `OPENAI_API_KEY` / `*_TOKEN` are
stripped), per-check timeouts, and capped output. Its profile label
(`checks-executor-v1`) is recorded on every report.

## Project context channel (ADR 0002)

A pack is portable across projects, so it cannot carry stack knowledge — but a
stage still needs the project's rules. The project-context channel is the
answer: the repository's own instruction files and the runtime's available
skills, declared, pinned from `base_commit`, and recorded in the manifest.

**What is pinned.** The instruction set is the project declaration
(`instructions:` in `.agentum.yaml`, repo-relative, validated) ∪ the
runtime-injected baseline (`AGENTS.md`, which opencode loads itself with no
configuration). Bytes are read from `base_commit` through the worktree manager
(the same agent-immutability seam as the checks registry), capped at 64 KiB/file
and 192 KiB/set with a recorded truncation marker, and delivered to the adapter
through the same config channel as the permission map. A path absent at
`base_commit` is recorded in the manifest's `missing` list, not fatal — and the
pre-stage restore treats a worktree file at such a path as tampering (D4 row 4):
the agent authored an instruction file the project never declared at the anchor,
so the restore removes it rather than judging by agent-authored rules. The set
is built once in `drive()` (`prepareProjectContext`) and carried on `stageRun`.

**Tampering, two layers.** Delivery only adds context, so the pin is worth
nothing unless the worktree copy is controlled. An `edit` deny rule stops an
agent rewriting `AGENTS.md` (or any declared path) via the edit tool — plus a
`**/AGENTS.md` name guard, because the runtime injects that filename from
anywhere in the tree and a nested copy the project never declared could otherwise
reach the model. A pre-stage hash check stops a `bash`-side rewrite and restores
the pinned ORIGINAL bytes (not the truncated delivery form). The check runs
strictly before each stage invocation (`restoreInstructions`), never between the
`isClean` sample and the checkpoint commit (the load-bearing ordering for the
`auto_if_clean` gate). The compare is CRLF-normalised on BOTH sides so neither
an autocrlf checkout (CRLF in the worktree, LF in the object) nor a CRLF-committed
repo reads as tampering; the pin's `SourceHash` stays over the raw bytes for
evidence identity. Each restoration emits `task.instructions_restored` and lands
in the manifest context section; the rewrite is orchestrator-authored, so the
next checkpoint commit shows it as a revert — the tamper and its reversal are
both in the git lineage. A restore IO error fails the task, mirroring a dirty
tree at the delivery boundary.

**Skills, allowed and recorded.** `skill` resolves to `allow`: a skill grants
knowledge, not reach, and still meets the same `bash`/`edit`/`net` rules.
`opencode debug skill` is probed once per run (after the worktree exists), with
its own 10s timeout and process-group kill, and each skill's name, location, and
content hash land in the manifest context section (bodies never stored). A
failed probe records an `EvidenceGap("context.skills")`, making
`evidence_complete` false. `skill.<name>` is an inert capability token: no role
grants it, and a grant is unenforceable (the opencode adapter does not list it
in `Supported()`), the same answer `mcp.<server>` gets — narrowing becomes a
config change later.

**Resolved checks, rendered.** The routing block lists the resolved project
checks (name, command, required) so the agent knows the build/test commands and
can run them to check its own work — hiding them is unenforceable
(`.agentum.yaml` is `fs.read`-able) and harmful (an implementer that knows them
saves a review/fix cycle). The agent learns *what* the checks are; it still
cannot change *which* gate delivery. This cached set is rendering-only;
`enforceProjectChecks` keeps its own independent load+resolve at the boundary,
bound to the verified commit (PR #23).

**Evidence.** The manifest gains a `context` section (written every run, so an
empty project still seals `evidence_complete: true`): instruction refs (path,
source, source/delivered hashes, delivered bytes, truncated flag),
restorations, enumerated skills, the probe label, and the missing list.
`DiffManifests` gains a `context` axis comparing path→delivered_hash and
name→hash, so two runs differing only in their skill set are distinguishable.

## What F.6 does not do yet

These land with their epics — the seams exist, the behavior does not:

- **Memory push/pull** → **Epic 1** (the routing block's "Project decisions"
  section is an inert stub until 1.2/1.3 land).
- **MCP capability pass-through** → **Epic 6** (the routing block's
  "Capabilities available" section is an inert stub).
- **Multi-step delivery / handoff** → **F.8** (one task = one step today).
- **Idle/hard timeout values** — the ctx seam is used, but no idle timer ships
  (`04 §5.2`).
- **`LISTEN/NOTIFY` low-latency wake** — poll is fine for MVP.
- **Per-project pack roots** — pack root is server-wide config
  (`config.Config.PacksDir`, default `./packs`).

## Evidence manifest and artifact revisions (F.7)

The worktree is disposable; the artifacts an agent produces during a stage and
the inputs that shaped the run are the durable record. F.7 splits that record
into two pieces:

- **Artifact revisions** — every produced or edited artifact variant becomes
  an immutable, content-addressed revision stored outside the worktree. Edits
  chain via `prev_revision_id`; the worktree-independent blob store survives
  teardown.
- **Evidence manifest** — one row per task that records everything that went
  into the run: input task + revision, project + base commit, pack + version +
  hash, one invocation record per stage attempt (adapter + runtime versions,
  model selection, both prompt hashes, effective capability profile,
  telemetry), the adapter wiring + runtime probe, memory slice, input/output
  artifact revisions, check set version + results, human gate decisions,
  branch / checkpoints / result commits. The manifest is append-only while a
  run is in flight and sealed at terminal state; corrections after sealing
  are linked rows that supersede rather than rewrite.

### Storage layout

```
<ArtifactRoot>/                       # config.ArtifactRoot, default .agentum/artifacts
  <hash[:2]>/
    <hash>                            # the content-addressed blob
```

The revisions index lives in Postgres (`artifact_revisions`); the bytes live on
the FS in a canonical location independent of any worktree. Two revisions with
the same content share one blob.

### Revisions and the immutable chain

```sql
artifact_revisions(
  id, tenant_id, user_id, task_id,
  name, kind,                          -- identity within the task
  content_hash, content_size,          -- content addressing
  action_type,                         -- create | edit
  prev_revision_id,                    -- chain to the prior revision
  source_invocation_id,                -- NULL for human edits
  delivery_step, execution_unit, phase, -- optional execution coordinate
  actor,                               -- human | agent | system
  is_current                           -- the single mutable bit
)
```

- A new revision chains via `prev_revision_id`. Edits never overwrite.
- `is_current` is the single "current" pointer per `(task, name)`; the partial
  unique index `idx_artifact_rev_current` enforces one current per name.
- A `PUT /tasks/{id}/invocations/{iid}/artifacts/{name}` creates a new
  revision; a revision already referenced by an `invocation` is never
  modified, so a later edit cannot retroactively change what an agent saw.
- On `continue` / `advance`, the runner syncs the current revisions back into
  the worktree before the next stage runs (`artifacts.Syncer`).
- The execution coordinate (`delivery_step`, `execution_unit`, `phase`) is
  optional and inert when NULL. Single-unit runs leave it empty; Epic 8
  populates it.

### Artifact containment

Artifact paths in `result.json` are declared by the agent, which makes them
untrusted input the orchestrator then reads with its own privileges. Every
read and write against a worktree goes through `artifacts.Container`, an
`os.Root`-backed handle that confines file access to one tree. Three escapes
are closed:

| Escape | Shape | Closed by |
|---|---|---|
| Absolute | `/etc/passwd` | lexical check before the open |
| Traversal | `../../.ssh/id_rsa` | lexical check after `Join` + `Clean` |
| Reparse point | a symlink or junction the agent planted in its own worktree | the `os.Root` open itself |

The third is why the check is not a path comparison: `filepath.EvalSymlinks`
follows POSIX symlinks but returns Windows junctions unresolved, so a junction
inside the worktree reads as contained. Performing the check as part of the
open also removes the window between validating a path and using it.

A declared path that escapes **fails the stage**. The capture is
all-or-nothing — nothing is ingested — the invocation is finalized with
`stop_reason = artifact_rejected`, a `stage.artifact_rejected` event records
the path and reason, and the task pauses for review. A declared path that
simply was not written is a different thing: a contract gap, logged and
skipped, with the run continuing.

Delegating to the OS means accepting its refusals. `os.Root` follows a symlink
that stays inside the root, but rejects two shapes beyond a plain escape:

- a **Windows junction**, wherever it points — Go will not traverse one at all;
- a symlink whose **target is absolute**, even when that target is inside the
  root, because an absolute target cannot be resolved within the
  component-by-component walk that makes the check sound.

Both cost an artifact in a shape git does not produce inside a worktree, and
the alternative is re-deriving containment in code that has already been shown
not to work. An agent that reaches an in-tree file through an absolute symlink
gets `artifact_rejected`; declaring the file's own path works.

### Secret scanning

Before bytes enter the store, `artifacts.DefaultScanner` inspects both the
artifact's name and its content.

**Content.** Text is scanned for token patterns (Authorization / Bearer
headers, AWS AKIA keys, GitHub PATs, generic
`token|secret|password|api_key`-labeled values, PEM private key blocks).
Binary content is scanned too, but only against the context-free rules (AKIA,
`ghp_`, PEM headers) — a `token:` label is meaningless in a binary stream —
and it is **never rewritten**, because substituting a placeholder would change
its length and corrupt the blob.

**Name.** An artifact named `.ssh/id_rsa`, `.aws/credentials`, `.env`,
`.netrc`, `*.pem` and similar is refused under every policy: a path has
nothing to redact, and storing the bytes under a different name would only
hide where they came from. `.env.example` and its siblings are allowed —
templates are ordinary stage output.

`AGENTUM_ARTIFACT_SCAN_POLICY` decides what a finding does:

| Value | Effect |
|---|---|
| `redact` (default) | substitute `[REDACTED]` in text and store the result; report binary findings without altering them |
| `reject` | refuse the write with `ErrSecretDetected` — nothing is stored |

`reject` is the fail-closed choice and the only one that stops a credential
inside a binary artifact. An unrecognized value fails at startup rather than
falling back, so an operator who asked for rejection never silently gets
redaction. A refused write does not fail the stage — nothing was read that
should not have been — but it does emit `stage.artifact_rejected`, since an
absent revision alone is indistinguishable from an artifact the agent never
wrote.

The scanner is best-effort, not a security boundary — operators remain
responsible for what their agents emit.

### Concurrent revision writes

A revision write is `read current → demote it → insert the new one`, and all
three steps run in one transaction. The read takes a row lock
(`LockCurrentArtifactRevisionForName`), so a second writer for the same
`(task, name)` blocks until the first commits and then observes the new
current revision rather than chaining a sibling off the one it already read.
The demotion targets that exact revision id, so an affected-row count of zero
is a conflict rather than a silent no-op. Two racing *first* creates have no
row to lock; the partial unique index `idx_artifact_rev_current` serializes
them and the loser's constraint violation surfaces as a conflict too.

`PutParams.ExpectedCurrentRevision` adds an optimistic-concurrency
precondition on top. When set, the write commits only if that revision is
still current; otherwise it fails with `ErrRevisionConflict` and stores
nothing. A stage capture leaves it empty (the runner is the only writer for
the duration of an invocation); a human edit composed against a revision the
user was looking at should set it, so two editors racing produce a conflict
instead of a lost update. The precondition is checked before the
identical-content shortcut: a caller whose pinned revision is gone has lost the
race whatever the bytes say.

### Manifest lifecycle

| Phase | Action | Who |
|---|---|---|
| Init | `POST /tasks` creates a manifest row (empty body) | API |
| Add evidence | `internal/manifest.Service.AddEvidence` merges keys as the runner resolves pack / base_commit / prompts / model / artifacts / git lineage / human-gate decisions. The merge base is the body read under the row lock inside the write transaction, so two concurrent writes cannot lose each other's contribution. | runner / API |
| Seal | At terminal state, `Seal(reason)` derives `body.missing` and `body.evidence_complete` from the body under the row lock and freezes it. `reason` ∈ `{completed, interrupted, cancelled, failed}` | runner (`teardown` / `failTask`) |
| Correct | `POST /tasks/{id}/manifest/corrections` adds a linked correction row with a fresh body snapshot. Corrections chain: correction N's body is correction N-1's body with the patch merged in, so the newest correction is the authoritative state by construction. | API |

**Concurrency.** `AddEvidence` computes its merge from the body read under the
row lock (`GetManifestForUpdate`) inside the write transaction itself — not a
pre-transaction snapshot. The SQL is a full body replacement, not a JSONB
merge, because the deep merge happens once in Go against the locked bytes; a
SQL-level `||` would be a second, shallow merge whose top-level-key-only
semantics silently drop nested evidence a concurrent writer just committed.
`AddEvidenceTx` exposes the tx-scoped write so a human-gate decision can commit
atomically with the FSM transition it describes.

**Corrections chain.** Each correction chains onto the prior one (the latest
correction is the merge base, falling back to the sealed body for the first
correction). The parent manifest row is the lock target, which serializes
concurrent corrections even though there is no correction row to lock for the
first one.

**Evidence gaps and completeness.** A failed evidence write is itself recorded
as an `EvidenceGap` (section, stage, reason, time) on the body — a sealed
manifest that degraded silently is worse than one that is absent, because a
reviewer cannot tell the two apart. At seal time the manifest carries an
`evidence_complete` flag: false when any section is absent or degraded. The one
exception is the initial evidence (input, project, pack, base commit): a failure
there fails the run, because it is the provenance root every later piece of
evidence chains off.

**Derived missing.** `body.missing` is derived from the body at seal time
(`Body.MissingSections()`), not asserted once at init. A stale claim (e.g.
`capabilities` listed missing on a body that carries a populated capabilities
section) therefore cannot survive to seal time. `memory` stays reported as
missing because the memory subsystem is genuinely not wired yet.

**Per-invocation evidence (ADR 0005).** The unit of evidence is the
invocation: `body.invocations` carries one record per stage ATTEMPT, keyed by
`invocation_id` (`stage` / `sequence` / `cycle` are coordinates for a reader,
never merge keys). Each record opens before the adapter starts — invocation
id, adapter id + implementation version + probed runtime version, the model
selection, both prompt hashes, the effective capability profile — and closes
after the stream drains with telemetry and the stop reason. A fix cycle's
second `review` therefore leaves a second record with its own model, profile,
and rendered prompt hash; nothing is overwritten. Two prompt hashes are
recorded per attempt (D8): `stage_prompt_hash` (the pack's stage prompt — the
cross-run diff axis) and `rendered_hash` (prompt + routing block — what makes
two attempts at the same stage distinguishable; deliberately never a diff
axis, because the routing block embeds the task id and absolute paths).
Telemetry (tokens, cost) is recorded per invocation and only there. Output
artifact refs carry the `invocation_id` that produced them.

**Runtime version.** The external runtime's own version (e.g. the CLI's) is
probed once per process (`adapter.Probe`, memoized — sticky, including a
failure) and recorded on every invocation record. The run-level `adapter`
section carries the wiring instead: id, the adapter implementation's version,
declared capabilities, and the probe outcome label (`runtime_probe`: `ok` or
`failed: <reason>`; a failed probe additionally records an `adapter.runtime`
evidence gap). A run resumed in a new process after a runtime upgrade
genuinely has two runtime versions, which is exactly why the version is not
a run-level scalar.

### Comparing two runs

`GET /tasks/{id}/manifest/diff?other=<task-id>` returns the structural
comparison between two sealed manifest bodies. The diff surfaces *input-level*
differences only — the things that meaningfully change what an agent would do:

- input task + revision
- project + base_commit
- pack (name, version, content hash)
- per-attempt prompts (the `stage_prompt_hash` on each invocation record)
- adapter (id, adapter implementation version, the SET of runtime versions
  observed across the run's invocations, declared capabilities)
- model (per attempt: id, tier, provider, options)
- declared / granted capability sets and each shared attempt's effective profile
- memory slice (entry hashes)
- input artifact revisions
- check set version
- git base
- execution coordinate (when set)

The per-attempt axes index each run's invocation records by `(stage, cycle)` —
the semantic coordinate of an attempt, since invocation ids are UUIDs and
never repeat across runs — and compare shared keys before set differences: a
fix cycle whose second `review` ran a different model reports `model-id`
(the specific answer), while a run with an extra attempt reports `model-set`.
Two runs on the same tier and model but different runtime builds differ on
`adapter-runtime-version` and nothing else — the axis that answers
"identical configuration, different result".

Outputs (artifacts produced) and human decisions are **not** compared — those
are *results*, not inputs. Two runs that produced different output but had
identical inputs are the same comparable run; the diff is empty. The rendered
prompt hash is never an axis (it embeds the task id and absolute paths, so it
never repeats across runs). A schema-1 manifest against a schema-2 manifest
of the same run diffs empty: both go through the same invocation-record
accessor.

### Read surface

| Method | Path | Notes |
|---|---|---|
| `GET` | `/tasks/{id}/artifacts` | list revisions; `?current=true` narrows to current |
| `GET` | `/tasks/{id}/artifacts/revisions/{rid}` | one revision (no bytes) |
| `GET` | `/tasks/{id}/artifacts/revisions/{rid}/content` | streams the blob bytes |
| `GET` | `/tasks/{id}/manifest` | manifest body + seal info + corrections |
| `GET` | `/tasks/{id}/manifest/diff?other=<task-id>` | input-level diff |
| `POST` | `/tasks/{id}/manifest/corrections` | add a post-seal correction |
