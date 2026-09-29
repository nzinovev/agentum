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
  `not_implemented`, `internal`, `conflict`, `too_many_requests`,
  `publication_disabled`, `publication_not_ready`.
- **Identity is implicit.** Every write carries `tenant_id` and `user_id` from
  the resolved Principal, never the request body.
- **State transitions** route through `engine.Next`. An illegal transition
  returns `409 illegal_transition`; the write does not happen.

## Projects

A project binds a repository to an Agentum project id (one repository = one
project per tenant). The project's key is the repository's **identity** — a
fingerprint of its own history (`repo_identity`, computed at registration from
the repository itself and never accepted from a request body) — not its local
path. Runs reference a project; the runner creates a per-run worktree off
the project's working copy. Registration is idempotent: registering the same
repository — the same copy, a moved directory, or a clone of the same history —
returns the same project, updating `repo_path` / `name` / `related_projects`
rather than failing, so a relocated repository keeps its run history.

`repo_path` must point inside a usable git working copy. Registration probes
git and refuses, each with a `400 bad_input` naming the fix:

- the path is not inside a git work tree;
- the repository has no commits (make at least one commit first — a run needs
  a base to branch from);
- the repository is a shallow clone (run `git fetch --unshallow` first: a
  shallow history would fingerprint at the cut boundary, and unshallowing
  would change the identity with no failure);
- the path is a linked work tree (the message names the main work tree —
  register that instead).

The stored `repo_path` is the working-tree root, so a subdirectory of the
repository, the root, and the root with a trailing slash all resolve to the
same project.

When a registration moves the project's working copy, the response says what
happened to runs that already started. `previous_repo_path` is always set on a
move. `runs_rebound_to_new_checkout` counts the unfinished runs that moved
with the copy — the relocation branch, taken when the previous path is
verifiably gone (absent from disk, or holding a different repository).
`runs_awaiting_previous_checkout` counts the unfinished runs that stay in the previous
copy: the second-working-copy branch, and the could-not-tell branch — an
unreadable or unreachable previous copy is not proven gone, and rebinding on
a guess is irreversible while staying is recoverable (a run whose copy really
is gone pauses itself with `checkout_unavailable`). Terminal runs keep their
checkout as history in every branch.

`related_projects` is an **inert seam**: stored now, it will grant
cross-project read access (a path-scoped `fs.read` capability) in a later
epic — never auto-discovered, the configured set is the security boundary. See
`docs/domain-model.md` for the full model vocabulary (workspace / project /
repository / checkout, run vs work item, actor / creator / owner).

| Method | Path | Status | Body / Query → Response |
|---|---|---|---|
| `POST` | `/projects` | ✅ | `{repo_path, name, related_projects?}` → `201 Project` / `400 bad_input` (not a usable working copy — see the refusals above) |
| `GET` | `/projects` | ✅ | `?limit=&offset=` → `200 Project[]` |
| `GET` | `/projects/{id}` | ✅ | → `200 Project` / `404 not_found` |

### Project

```json
{
  "id": "uuid",
  "repo_identity": "git-roots:v1:9f86d0...",
  "repo_path": "/home/me/repos/my-app",
  "name": "My App",
  "related_projects": [],
  "created_at": "2026-07-09T...",
  "updated_at": "2026-07-09T...",
  "previous_repo_path": "/home/me/old/path",
  "runs_rebound_to_new_checkout": 2,
  "runs_awaiting_previous_checkout": 1
}
```

`repo_identity` is read-only. The three trailing fields appear only on a
registration that moved the working copy and are omitted otherwise; at most
one of the two counters is non-zero on any given registration.

## Runs

| Method | Path | Status | Body / Query → Response |
|---|---|---|---|
| `POST` | `/runs` | ✅ | `{project_id, pipeline_pack, title, description, overrides?, base_ref?}` → `201 Run`. The body is decoded strictly (`DisallowUnknownFields`): an unknown key — including the legacy `input` blob — is a `400 bad_input`, not an ignored field. `description` is required, non-blank, ≤ 32 KiB; `title` ≤ 200 bytes (over-budget is a `400`, not a truncation). `title` and `description` are both scanned for credential material at the boundary and a match is a `422 bad_input`; the scan runs only the self-identifying rules (AWS key ids, GitHub PATs, PEM private-key blocks, `aws_secret_access_key` with its value), so prose that merely discusses credentials — "Add Bearer authentication to /settings" — is accepted. A body over the transport cap is a `400` naming the limit, not a JSON parse error. `overrides` is the orchestrator-facing half of the request; `overrides.checks.{required,optional}` name registered checks — a command is never accepted, and a typo'd key is a `400`. |
| `GET` | `/runs` | ✅ | `?project_id=&limit=&offset=` → `200 Run[]` |
| `GET` | `/runs/{id}` | ✅ | → `200 Run` / `404 not_found` |
| `POST` | `/runs/{id}/start` | ✅ | `created → running` (enqueues a run job) → `200 Run` / `409 illegal_transition` |
| `POST` | `/runs/{id}/reject` | ✅ | terminal reject at either human gate (plan `paused_gate` or final `awaiting_final_review`). Reuses cancel semantics (lands in `cancelled`, branch survives) but records a `rejected` decision and seals the manifest `SealRejected`. Idempotent: a repeat reject matching the recorded decision returns `200`. → `200 Run` / `409 illegal_transition` |
| `POST` | `/runs/{id}/cancel` | ✅ | any non-terminal → `cancelled` (terminal abort; branch survives) → `200 Run` / `409 illegal_transition` |
| `GET` | `/runs/{id}/final-review` | ✅ | the reviewable payload — `200` in `awaiting_final_review` **and** in terminal states (`done` / `cancelled` / `failed`); `409 illegal_transition` before the gate. Carries `plan` / `git` / `diff` / `stages` / `review` / `checks` / `manifest` / `decisions` / `publication`. Each decision carries `actor` (`human \| agent \| system`) and `user_id` (whose name it was taken under) — "who let this through" is the question the section answers, and a system-passed automatic gate must never read as the run author approving. |
| `GET` | `/runs/{id}/publication` | ✅ | → `200 Publication`; requires `run:read`. See [Publication](#publication). |
| `POST` | `/runs/{id}/publish` | ✅ | no body → `202 Publication` after enqueue; requires `run:publish`; `409` on a failed precondition. |
| `POST` | `/runs/{id}/cleanup` | ✅ | terminal run → branch deleted (idempotent, audited) → `202 Run` / `409 illegal_transition` (if not terminal) |
| `POST` | `/runs/{id}/worktree/reconcile` | ✅ | resolve a `worktree_uncommitted_changes` pause: `{mode: "resume_session" \| "keep_as_checkpoint" \| "discard_to_checkpoint", expected_head, confirm_uncommitted_loss?}` → `200 Run` / `400 bad_input` / `409 illegal_transition`. Requires `run:reconcile`. |
| `POST` | `/runs/{id}/worktree/discard` | ✅ | remove a terminal or explicitly-stopped run's working tree — the tree only, never the branch: `{expected_head, discard_uncommitted?}` → `202 Run` / `400 bad_input` / `409 illegal_transition`. Requires `run:discard-worktree`. |

`base_ref` is the git ref the run builds against (branch / tag / SHA / `HEAD`).
It is resolved once to an immutable `base_commit` before the worktree is
created; omitted defaults to `HEAD`. See `docs/execution.md` § "Safe lifecycle,
checkpoints, and code egress" for the full lineage / abort / cleanup model.

A run executes in the working copy it pinned at first start (the project's
`repo_path` at that moment), and keeps that copy even if the project is later
re-registered from another clone — its branch, checkpoints, and worktree stay
together. If the pinned copy becomes unavailable or holds a different
repository, the run pauses with stop reason `checkout_unavailable` (the
`run.checkout_unavailable` event names the path) and stays resumable: restore
the directory or re-register the project, then continue. The run never rebuilds
its worktree in a different copy.

`cancel` is a **terminal abort**: the in-flight run is aborted and the worktree
is torn down, but the `agentum/<run-id>` branch and any committed recovery work
survive for review. `cleanup` is the **explicit, post-terminal disposal** that
deletes the branch; it is a distinct verb because cancel and cleanup must not be
ambiguous with each other or with pause.

A **failure never tears the worktree down**. `failed` keeps the working tree,
the branch, and the checkpoints exactly as the error left them — the uncommitted
files may be the only copy of a partially executed stage's work, and a person
decides what to salvage. Removing that tree afterwards is the explicit,
audited `POST /runs/{id}/worktree/discard` (tree only; the branch survives;
`cleanup` remains the branch-deleting verb). The endpoint requires a terminal
or explicitly stopped run with no active job, the worktree's current HEAD
(`expected_head`), and — when the tree is dirty — an explicit
`discard_uncommitted: true`; the runner re-verifies every precondition at
execution time and refuses (with a diagnostic event) rather than remove a tree
the request does not describe. A discarded stopped run can still be continued
or advanced: the runner checks the surviving branch out again, but only while
its tip is the HEAD the discard confirmed — otherwise the run pauses with
`worktree_branch_unconfirmed`.

A run resumed over a worktree holding **uncommitted changes** does not get its
tree wiped: the runner pauses with stop reason `worktree_uncommitted_changes`
(the `run.worktree_recovery_required` event carries the HEAD, the restore
target, and the dirty paths) and waits for an explicit decision via
`POST /runs/{id}/worktree/reconcile`:

- `resume_session` — leave the tree as the captured session left it and resume
  that session;
- `keep_as_checkpoint` — commit the tree on the run branch as an
  orchestrator-authored checkpoint, then resume;
- `discard_to_checkpoint` — reset the tree back to the last checkpoint
  (requires `confirm_uncommitted_loss: true`).

Every decision names the `expected_head` it applies to; the runner refuses a
decision whose HEAD no longer matches. The transition, the driving job, and the
human-decision evidence commit atomically, exactly as a `continue` does.

`reject` is a **terminal reject at a human gate** — the plan gate
(`paused_gate`) or the final gate (`awaiting_final_review`). It reuses `cancel`'s
FSM event (the run lands in `cancelled`, branch preserved) but records a
`rejected` decision on `run_approvals` and seals the manifest with
`SealRejected`, so a sealed record cannot describe a rejected result as a plain
abort. At the plan gate nothing ever unlocked source-write, so there is no
source change to undo. Idempotent: a repeat reject matching the recorded
decision returns `200`, a conflicting one returns `409`.

The pre-final-review state is `awaiting_final_review` (the previous
memory-commit state name was retired; migration 0009 rewrites existing rows).

`result_commit` is now pinned at the final gate (the commit the human reviews),
not only at teardown — see `docs/execution.md` § "`result_commit` at the final
gate".

### Run

```json
{
  "id": "uuid",
  "project_id": "uuid",
  "pipeline_pack": "java-spring@1",
  "title": "Add auth to /settings",
  "description": "Problem. … What to do. …",
  "overrides": {},
  "state": "created | running | paused_open_questions | paused_gate | paused_user_stop | awaiting_final_review | done | failed | cancelled",
  "base_ref": "main",
  "base_commit": "a1b2... full SHA the run branched from, set on first run",
  "result_commit": "c3d4... full SHA pinned at the final gate (the commit the human reviews); empty before the gate",
  "branch": "agentum/<run-id>",
  "created_at": "2026-07-05T...",
  "updated_at": "2026-07-05T..."
}
```

## Publication

`GET /api/v1/runs/{id}/publication` reads stored delivery state without calling
the provider. `GET /api/v1/runs/{id}/final-review` includes the same object as
`publication`. Publication failure leaves the implementation review available.

| `state` | Meaning |
|---|---|
| `disabled` | No publication row; publication is disabled in configuration. |
| `not_attempted` | No publication row; publication is enabled. |
| `unavailable` | The publication row could not be read. |
| `pending` | A publication attempt is requested. |
| `publishing` | A worker claimed the publication lease. |
| `published` | The checked commit was pushed and the PR was created or updated. |
| `failed` | The last attempt has a retryable failure. |
| `blocked` | The last attempt requires a change to the delivery or destination. |

Every state above returns `200` for an accessible run. The absence forms contain
only `run_id` and `state`. An existing row remains visible when publication is
switched off. An unknown or foreign run returns `404 not_found`; the usual
`401 unauthorized` and `403 forbidden` guards apply to both endpoints.

```json
{
  "run_id": "uuid",
  "state": "published",
  "provider": "github",
  "target": {
    "provider": "github",
    "host": "github.com",
    "owner": "example",
    "repository": "project",
    "base_branch": "main",
    "remote_branch": "agentum/uuid"
  },
  "published_commit": "full-result-commit-sha",
  "pull_request": {"number": 42, "url": "https://github.com/example/project/pull/42", "state": "open"},
  "branch_pushed_at": "2026-09-27T12:00:00.000000000Z",
  "published_at": "2026-09-27T12:00:01.000000000Z",
  "attempts": 1,
  "created_at": "2026-09-27T11:59:59.000000000Z",
  "updated_at": "2026-09-27T12:00:01.000000000Z"
}
```

Unset target fields and timestamps are omitted; `attempts` is omitted at zero.
`published_commit` names the intended checked commit even while publication is
pending or failed. `pull_request` appears once a number is recorded; its state
is the last observed `open`, `closed`, or `merged` value. Failures add
`last_error: {code, message}` with a safe diagnostic. A failed attempt can still
carry a pushed branch or a recorded PR number.

`POST /api/v1/runs/{id}/publish` queues an attempt and returns `202` with the
current Publication object. It does not wait for network work. A repeated
request retains the frozen destination and the recorded PR identity.

| Precondition failure | Response |
|---|---|
| Publication disabled | `409 publication_disabled` |
| Run outside `awaiting_final_review` / `done`, missing passing mandatory checks, or mismatched checked/result commit | `409 publication_not_ready` |
| An attempt holds an unexpired lease | `409 conflict` |
| Store or manifest read/write failure | `500 internal` |

A recorded `failed` or `blocked` outcome permits explicit retry after its cause
has been addressed. The recovery probe does not automatically retry either
state. The closed `last_error.code` vocabulary is:

| Code | State | Meaning / operator action |
|---|---|---|
| `credentials_missing` | `failed` | Configure the publication token. |
| `credentials_rejected` | `failed` | Correct the token or its permissions. |
| `network_unreachable` | `failed` | Restore provider connectivity. |
| `provider_rate_limited` | `failed` | Wait for the provider limit to reset. |
| `lease_budget_exhausted` | `failed` | Retry with time remaining in the lease. |
| `provider_error` | `failed` | An unclassified provider or local storage/execution failure prevented completion. |
| `unsafe_git_config` | `blocked` | Remove the local Git setting named by the diagnostic. |
| `remote_unknown` | `blocked` | Configure an accessible publication repository. |
| `base_branch_unknown` | `blocked` | Supply a valid base branch. |
| `non_fast_forward` | `blocked` | A human must resolve the remote branch divergence. |
| `push_rejected` | `blocked` | Resolve the provider's push restriction. |
| `draft_unsupported` | `blocked` | The provider must support draft PRs; no ordinary PR fallback is accepted. |
| `pull_request_closed` | `blocked` | The PR was closed or merged; it is neither reopened nor replaced. |
| `pull_request_not_found` | `blocked` | The recorded PR number was not found; no replacement is created. |
| `checks_not_passed` | `blocked` | The mandatory-check gate did not pass. |
| `commit_mismatch` | `blocked` | The result commit is missing or differs from the checked commit. |
| `description_invalid` | `blocked` | The stored description or revision reference is missing or fails its integrity check; repair the data before retrying. |
| `secret_in_description` | `blocked` | The artifact scanner rejected the rendered PR description. |
| `provider_unknown` | `blocked` | The frozen provider id is not registered. |

The published description is an artifact of kind `pr_description`, named
`publication/pr-description.md`. Read its immutable revisions through the
[artifact endpoints](#artifacts). It is excluded from the final-review `stages`
array. See [Publication execution](execution.md#publication) for the template,
scanning, lease, and retention rules.

## Stage invocations

A stage invocation is one agent session within a run (`stage_invocations` row).
Each carries a `session_id` (for non-destructive resume), `stop_reason`
(`open_questions | gate | user_stop | fix_budget_exhausted | verdict_unreadable`),
`cycle` (the 0-based repeat index of the stage within the run — distinguishes
retries from resumes), and `pending_edits`.

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/runs/{id}/invocations` | ✅ | list invocations for a run, ordered by sequence. `200 Invocation[]` / `404 not_found` (unknown run). |
| `GET` | `/runs/{id}/invocations/{iid}` | ✅ | one invocation. `200 Invocation` / `404 not_found`. |

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
| `POST` | `/runs/{id}/invocations/{iid}/continue` | ✅ | resume after `open_questions` / `user_stop` (session-id resume; enqueues a `continue` job). Optional body `{"text": …}` carries new user text to the resumed session — see [Continue request](#continue-request) |
| `POST` | `/runs/{id}/invocations/{iid}/advance` | ✅ | pass a `gate` → next stage runs (enqueues an `advance` job) |
| `POST` | `/runs/{id}/invocations/{iid}/approve` | ✅ | final approval at `awaiting_final_review` → run done + memory commits. Pins `result_commit` at the gate. Idempotent. |
| `POST` | `/runs/{id}/invocations/{iid}/edit` | stub | edit-and-approve: the human edits the artifact directly; the edit is the approval. Epic 2 |
| `POST` | `/runs/{id}/invocations/{iid}/ask-to-edit` | stub | scoped agent-mediated edit; re-stops for review. Epic 2 |
| `POST` | `/runs/{id}/invocations/{iid}/add-context` | stub | additive guidance; agent resumes (does not regenerate). Epic 2 |

> `continue` / `advance` are implemented but operate on the **run**, not the
> invocation id: they enqueue a job that drives the runner. The `{iid}` path
> parameter is accepted for contract stability but the runner resumes from the
> run's current state. `cancel` is `POST /runs/{id}/cancel`.

#### Continue request

```json
{ "text": "Use PostgreSQL 17. No new table is needed." }
```

The body is optional in full: an empty body, `{}`, `null`, and an absent or
whitespace-only `text` all continue without new text, exactly as a
pre-text continue did. The text is the user's answer to an open question or
extra context. It is rendered verbatim inside the routing block's Task section,
after the original request, between explicit markers, and it reaches only the
first invocation the continue job resumes — never the fresh sessions of later
stages. It grants no capability, approves no plan, and changes no checks.

| Input or state | Status | Code |
|---|---|---|
| `paused_open_questions` / `paused_user_stop`, non-empty `text` | `200` | run resumes; the `continue` job stores `{"text": …}` |
| empty body, `{}`, `null`, absent or whitespace-only `text` | `200` | run resumes; the job payload is `{}` |
| any other run state | `409` | `illegal_transition` |
| malformed JSON, unknown field, non-string `text` (null included), a second JSON object | `400` | `bad_input`; no state change, no enqueue |
| invalid UTF-8, or `text` over 32 KiB (decoded) or body over 256 KiB | `400` | `bad_input`; no truncation |
| credential-shaped `text` (scanner reject) | `422` | `bad_input`; the text is not stored |
| non-empty `text` with no captured session on the latest invocation | `409` | `illegal_transition`; no enqueue |

The text is not trimmed or rewritten on the way to the model; only the
emptiness check trims. `jobs.payload` keeps the accepted text across a process
restart, so delivery does not depend on the HTTP process that accepted it.

## Artifacts

Two surfaces: the per-invocation edit surface under
`/runs/{id}/invocations/{iid}/artifacts/{name...}` and the F.7 immutable
revisions surface.

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/runs/{id}/artifacts` | ✅ | list revisions for a run. `?current=true` narrows to current revisions. |
| `GET` | `/runs/{id}/artifacts/revisions/{rid}` | ✅ | one revision (metadata only). |
| `GET` | `/runs/{id}/artifacts/revisions/{rid}/content` | ✅ | streams the blob bytes. |
| `GET` | `/runs/{id}/invocations/{iid}/artifacts/{name...}` | ✅ | current revision of `(run, name)` + its content. `X-Revision-Id` header carries the revision id to use as `expected_revision_id` on a PUT. 404 `not_found` when no current revision. |
| `PUT` | `/runs/{id}/invocations/{iid}/artifacts/{name...}` | ✅ | edit-and-approve via artifact write — creates a new revision (`actor = human`, no source invocation). The edit IS the approval at a `human_edit` gate. |

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
rather than a lost update. A first create (no current revision) omits it.

| Store outcome | Status | Code |
|---|---|---|
| revision created | `200` artifact revision | — |
| `ErrRevisionConflict` (precondition failed, or two creates raced) | `409` | `conflict` |
| `ErrSecretDetected` (reject-on-secret policy refused the content) | `422` | `bad_input` |
| `ErrNoCurrentRevision` (GET on an artifact with no revision yet) | `404` | `not_found` |
| current revision exists but `expected_revision_id` was omitted | `428` | `precondition_missing` |

The revisions store is content-addressed and lives outside any worktree. Each
edit creates a new immutable revision that chains to the prior one; the
`current` pointer is the single mutable bit per `(run, name)`.

### Artifact revision

```json
{
  "id": "uuid",
  "run_id": "uuid",
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

One manifest per run. Records the inputs that shaped the run (input request +
revision, project + base commit, pack + version + hash, one invocation record
per stage attempt — adapter + runtime versions, model selection, both prompt
hashes, effective capability profile, telemetry — the adapter wiring and its
runtime probe, memory slice, input/output artifact revisions, check set
version + results, gate decisions (human and system alike, each with its
actor and user_id), branch + checkpoints + result commits).

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/runs/{id}/manifest` | ✅ | manifest body + seal info + corrections |
| `GET` | `/runs/{id}/manifest/diff?other=<run-id>` | ✅ | input-level diff vs another run's manifest |
| `POST` | `/runs/{id}/manifest/corrections` | ✅ | add a post-seal correction (body: `{reason, body?}`) |

The manifest body is filled append-only while the run is in flight and sealed
at terminal state (`done | failed | cancelled | interrupted`). Corrections
after sealing are linked rows with a `reason` and a fresh body snapshot; the
sealed row is never edited. The write path accepts schema `"2"` only; a
correction carrying the schema-1 sections (`prompts`, `model`,
`capabilities.effective`, `adapter.name`/`version`) is rejected with a `400`.

### Manifest body shape

Schema `"2"`. Schema-1 manifests stay readable forever — their
legacy sections are retained verbatim on read and synthesized into equivalent
invocation records for the diff.

```json
{
  "schema_version": "2",
  "input":            { "run_id": "…", "title": "…", "description": "…", "overrides": {}, "revision": "…", "pipeline_pack": "…" },
  "project":          { "project_id": "…", "repo_path": "…", "name": "…", "base_ref": "…", "base_commit": "…" },
  "pack":             { "ref": "…", "name": "…", "version": "1.0.0", "content_hash": "…", "forked": false },
  "adapter":          { "id": "opencode", "adapter_version": "1.0.0", "declared_capabilities": ["fs.read", "…"], "runtime_probe": "ok" },
  "invocations": [
    {
      "invocation_id": "6f1c…",
      "stage": "review",
      "sequence": 7,
      "cycle": 1,
      "adapter":  { "id": "opencode", "adapter_version": "1.0.0", "runtime_version": "1.18.11" },
      "model":    { "tier": "strong", "provider": "zai-coding-plan", "options": { "model": "zai-coding-plan/glm-5.3" } },
      "prompt":   { "stage_prompt_hash": "…", "rendered_hash": "…" },
      "capabilities": { "role": "reviewer", "profile": { "grants": [] } },
      "telemetry": { "tokens": { "total": 0, "input": 0, "output": 0, "reasoning": 0, "cache_read": 0, "cache_write": 0 }, "cost": 0.0 },
      "stop_reason": ""
    }
  ],
  "capabilities":     { "declared": [], "granted": [] },
  "memory":           { "scope": "project", "hashes": [], "entries": 0 },
  "artifacts":        { "inputs": [], "outputs": [{ "name": "review/result.json", "revision_id": "…", "content_hash": "…", "stage": "review", "invocation_id": "6f1c…" }] },
  "checks":           { "set_version": "", "results": [] },
  "gate_decisions":   [{ "stage": "…", "gate": "…", "decision": "approved | rejected | edited | continued", "actor": "human | agent | system", "user_id": "…", "timestamp": "…" }],
  "git":              { "branch": "agentum/…", "base_commit": "…", "result_commit": "…", "checkpoints": [] },
  "execution_coordinate": { "delivery_step": "", "execution_unit": "", "phase": "" },
  "missing":          ["memory", "checks", "gate_decisions"]
}
```

The unit of evidence is the invocation: one record per stage
ATTEMPT, keyed by `invocation_id`. The record opens before the adapter starts
and closes after the stream drains, so a crashed, timed-out, or refused
attempt still records the model, prompts, and profile it was going to run.
Output artifact refs carry the `invocation_id` that produced them.

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
decisions are not compared — those are results, not inputs. The per-attempt
axes (prompts / model / capabilities / adapter) index each run's invocation
records by `(stage, cycle, ordinal)` and compare shared keys before set
differences. The ordinal counts attempts sharing a `(stage, cycle)` in
`sequence` order — a resumed stage produces a second attempt at the same
cycle — and is derived when the diff is computed; it is not part of the
manifest body. Reason strings:

| Axis | Reasons |
|---|---|
| `prompts` | `prompt-hash` (differing `stage_prompt_hash` on a shared attempt), `prompt-set` |
| `model` | `model-id`, `model-tier`, `model-provider`, `model-variant`, `model-options`, `model-set` |
| `capabilities` | `capability-declared`, `capability-granted`, `capability-effective` |
| `adapter` | `adapter-id`, `adapter-version`, `adapter-runtime-version`, `adapter-capabilities` |

`adapter-runtime-version` compares the set of runtime versions observed
across each run's invocations: two runs on the same tier and model but
different runtime builds differ on this axis and no other. The rendered
prompt hash is never a diff axis — it embeds the run id and absolute paths,
so it never repeats across runs.

Independently of the value reasons above, any axis whose section is present on
one manifest and absent on the other reports `<axis>-missing`:
`input-missing`, `project-missing`, `pack-missing`, `adapter-missing`,
`capabilities-missing`, `memory-missing`, `context-missing`,
`artifacts-missing`, `checks-missing`, `git-missing`, `coordinate-missing`.
One run recorded the section and the other did not, so the two are not
comparable on that axis — this is a different statement from "the values
differ", and a client should say so rather than rendering a value delta.

## Models

The model surface: what the process is configured to run on, what the runtime's
catalog lists, and the on-demand check of whether a model responds.
A remote client cannot read the server's `models.yaml` off its disk — the
list handle is what it has to render a tier table or compose a test request.
Actions: `model:read` (both GET handles), `model:test` (the check) —
tenant-scoped, like `run:create`.

| Method | Path | Status | Body / Query → Response |
|---|---|---|---|
| `GET` | `/models` | ✅ | → `200` with the adapter id, the default tier, the catalog status, and the resolved tiers |
| `POST` | `/models/test` | ✅ | requires an `Idempotency-Key` header; body `{tier}` or `{model, variant?}` or empty (all tiers), optional `timeout_seconds` (1–120, default 60) → `202 {check_id, targets}`. A `variant` alongside a `tier`, or with no target at all, is `400 bad_input`: the tier carries its own variant, and a variant qualifies a model |
| `GET` | `/models/test/{id}` | ✅ | → `200 {check_id, state, results}` / `404 not_found` |

### `GET /models`

```json
{
  "adapter": "opencode",
  "default_tier": "strong",
  "catalog": {"status": "ok", "models": 30, "checked_at": "2026-09-09T12:00:00Z"},
  "tiers": [
    {"tier": "fast",   "model": "zai-coding-plan/glm-5.2-highspeed", "variant": "", "in_catalog": true},
    {"tier": "strong", "model": "zai-coding-plan/glm-5.3",           "variant": "high", "in_catalog": true}
  ]
}
```

`catalog.status` uses the probe label vocabulary: `ok`, `failed: <reason>`,
or `unsupported` (the adapter cannot list its runtime). `in_catalog` is
false whenever that question could not be answered — the status field records
which of the two it was. `variant` is the tier's own, empty when the tier
declares none. The tiers are the boot-resolved values runs use,
never a re-read of the file.

### `POST /models/test` — accept, don't hold the session

The check invokes the model, which can take up to the timeout, so the request
is **accepted** (`202`) and executed in the background; results arrive as
events on the tenant stream and `GET /models/test/{id}` serves state for a
reconnecting client. Targets are deduplicated by the exact `(model, variant)`
pair — three tiers naming one pair are one paid call, while the same model
under two variants is two — and execution is serialized, so accepted checks
queue rather than fan out.

**`Idempotency-Key` is required.** Without it, a retried request runs a second
check and pays for it. A request without the header returns `400 bad_input`
naming the header. Keys and check ids are **tenant-scoped**: a key another
tenant used is invisible (same key, same body from another tenant mints that
tenant's own check), and a foreign `check_id` reads as the same `404` as an
unknown one.

| Situation | Response |
|---|---|
| accepted | `202` + `check_id` |
| same `Idempotency-Key`, same body | `202` + the **same** `check_id`; no second check runs |
| same key, different body | `409 conflict` — a key matches one body |
| header absent | `400 bad_input` (header named) |
| unknown tier / broken body / `timeout_seconds` above the ceiling / a `variant` with a `tier` or without a target | `400 bad_input` (ceiling named; the variant refusals say which shape to send instead) |
| already `8` unfinished checks queued for the tenant | `429 too_many_requests` (cap named; replays of accepted checks still return `202`) |
| unknown, TTL-expired, or foreign `check_id` on GET | `404 not_found` (finished results remain in the tenant's event stream) |

A non-`ok` outcome is a **result**, not an error of the request: the endpoint
returned `202`, and the outcome (`ok` | `unknown_model` | `timeout` |
`error`) arrives in the events and in the GET. `unknown_model` means the
catalog was obtained and no call was made.

The check registry is in-process with a TTL (default 60 min,
`AGENTUM_MODEL_TEST_RETENTION_MINUTES`, counted from a check's completion, so
a long queue does not expire a running check). Diagnostics carry no durable
state: a restart discards keys and unfinished checks, and a client retries.
The ceiling for `timeout_seconds` is
`AGENTUM_MODEL_TEST_MAX_SECONDS` (default 120). Pending checks stop with the
process: they derive from the server's run context, so a shutdown cancels
them and kills their subprocesses rather than leaving them running.

### Model-check events

Delivered on the tenant stream (`GET /api/v1/events`, `Last-Event-ID`
replay), one event per model so a live consumer sees progress:

| `event` | When | Payload |
|---|---|---|
| `models.test_started` | request accepted | `{check_id, targets}` |
| `models.test_model_checked` | after each model | `{check_id, tier, model, variant, outcome, latency_ms, reason}` — `variant` is the pair the check ran with, empty when none was set |
| `models.test_finished` | all targets checked | `{check_id, results, duration_ms}` |

The events carry no `run_id` (a check is not a run) and the actor is `human`:
the check exists because a person asked for it through this API.

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
| `GET` | `/runs/{id}/events` | ✅ | per-run |

### Framing

Each event is one SSE block:

```
id: 42
event: stage.stopped
data: {"run_id":"...","stage":"implement","stop_reason":"gate"}

```

- `id` is the monotonic `events.id` (bigint). **`Last-Event-ID`** replays every
  row with `id > lastID`, scoped to the tenant (and run for per-run). A
  missing/invalid `Last-Event-ID` replays from the start.
- The `data` object carries two facts from the event row on every frame:
  `run_id` (present only when the event belongs to a run) and `actor`
  (`human | agent | system` — who produced the event). A payload's own key
  takes precedence over the mixed-in value.
- After replay completes, the connection live-tails new rows and emits a
  comment-frame keepalive (`: ping <unix>`) every 15s.
- The same durable log backs the audit trail, so reconnect semantics and audit
  are one schema.

### Event types

| `event` | Carries | Emitted by |
|---|---|---|
| `run.state_changed` | `{run_id, from, to, stop_reason?, stage?}` | engine on every transition |
| `run.checkout_unavailable` | `{run_id, checkout_path, reason}` | runner when a run's pinned working copy is gone or holds a different repository; the run pauses (`stop_reason: checkout_unavailable`) and stays resumable |
| `stage.invocation_started` | `{run_id, invocation_id, stage, sequence}` | runner |
| `stage.stream` | `{run_id, invocation_id, chunk}` | adapter (agent text → SSE) |
| `stage.tool` | `{run_id, invocation_id, tool, target, status}` | adapter (tool activity) |
| `stage.stopped` | `{run_id, invocation_id, stop_reason}` | runner at a stop point |
| `stage.result` | `{run_id, invocation_id, status, open_questions, ...}` | runner after result.json |
| `stage.artifact_rejected` | `{run_id, stage, path, reason}` | runner when a declared artifact is refused (`reason` ∈ `escapes_worktree`, `unresolvable`, `secret_detected`) |
| `run.delivery_commit_diverged` | `{run_id, result_commit, checks_commit, checkpoint_label}` | runner at teardown when `result_commit` differs from the commit the delivery checks verified; the run is not failed, but the sealed manifest reads `evidence_complete: false` |
| `run.publication_started` | `{run_id, attempts, state}` | publication worker after claiming the lease; actor `system` |
| `run.published` | `{run_id, pull_request, url, branch}` | publication worker after recording success; actor `system` |
| `run.publication_failed` | `{run_id, code, state, message}` | publication worker after recording refusal; actor `system` |
| `memory.committed` | `{run_id, entries:[...]}` | memory layer at final approval |
| `run.log` | `{run_id, level, message}` | runner / adapter diagnostics |
| `models.test_started` | `{check_id, targets}` | API when an on-demand model check is accepted (no `run_id`; see "Models") |
| `models.test_model_checked` | `{check_id, tier, model, variant, outcome, latency_ms, reason}` — `variant` carries the checked pair's variant, empty when none | API after each checked model |
| `models.test_finished` | `{check_id, results, duration_ms}` | API when the check completes |

Pre-release: the `payload` shapes are stable in shape but may gain fields; the
UI must ignore unknown payload fields.

## Health

| Method | Path | Status | Notes |
|---|---|---|---|
| `GET` | `/healthz` | ✅ | liveness — process up. `200 {status:"ok"}` |
| `GET` | `/readyz` | ✅ | readiness — DB reachable. `200` / `503` |
