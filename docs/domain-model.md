# Domain model

The written semantics of the entities Agentum stores, and the vocabulary the
API, the UI, and this documentation share. When a screen, a route, or a table
disagrees with this file, one of the two is wrong and gets fixed — the
dictionary is not allowed to fork.

## The four levels

The target shape of the model is a hierarchy:

```
workspace (tenant) → project (product) → repository → local checkout
```

In the MVP exactly one of each level exists per project line: one project
holds one repository with one local checkout. No tables were added for the
levels — an empty `repositories` table with a guaranteed 1:1 row would be a
promise, not a model. What the MVP changes instead is that the single
`projects` row stops using one name for three levels:

| Level | What it is | Where it lives in the MVP |
| --- | --- | --- |
| workspace / tenant | the tenant, the visibility boundary of everything | `tenant_id` on every table |
| project | the product: what run history, memory, and decisions attach to | `projects.id` |
| repository | the repository itself, wherever it currently is on disk | `projects.repo_identity` |
| local checkout | one working copy of the repository on this machine | `projects.repo_path` |

The review rule that follows: **no name denotes two levels at once.**
`projects.id` does not mean "the repository", `repo_identity` does not mean
"the project", and `repo_path` means nothing except "where the working copy
currently lives on this machine". When the hierarchy becomes real tables, each
column moves to its own table without a rename.

The identifiers do not assume this is the only shape: identity is a value with
its own scheme (below), not a filesystem fact, so additional sources of
context can appear later without renaming anything.

## Repository identity: `repo_identity`

A project's identity key is a fingerprint of the repository itself, not of
where it sits:

```
git-roots:v1:<sha256 hex>
```

The hash is taken over the sorted list of root commits reachable from `HEAD`
(`git rev-list --max-parents=0 HEAD`). Three properties drove the choice: it
does not change when the directory is moved or cloned, it needs no network and
no remote (a repository without an `origin` is a normal case here), and it
writes nothing into the user's repository. The prefix names the rule that
derived the value, so a future scheme (a marker in `.git/`, a normalized
remote URL, a server-issued identity) is a new prefix, not a type migration.

Registration computes the identity at the boundary (`internal/repoid`) and
never accepts it from a request body — the same rule `tenant_id` and `user_id`
follow. Uniqueness is `UNIQUE (tenant_id, repo_identity)`; `repo_path` is a
non-unique attribute of the checkout, updated by every registration.

Deriving the fingerprint walks the entire reachable history, so it happens
once, at registration, which also stores the root commits the fingerprint was
folded from (`repo_root_commits`). Every later question — "does this path
still hold the recorded repository", asked by each run job and by each
re-registration's previous-path probe — is answered by confirming those roots
exist, an object lookup that does not depend on the repository's size.

Accepted limitations of `git-roots:v1`: two clones of one history share the
identity (that is what makes a relocation the same project); a repository
whose reachable root set changes (merging unrelated histories) gets a new
identity — the same price a relocation used to cost permanently; and shallow
clones are refused at registration, because their fingerprint would sit at the
cut boundary and move on `git fetch --unshallow`.

## Registration

Registering a repository probes git once (`repoid.Resolve`) and refuses what
cannot serve as a project's working copy — each refusal names the fix:

- not inside a git work tree;
- no commits — there is no history to fingerprint, and no base a run could
  branch from (make at least one commit first);
- a shallow clone — run `git fetch --unshallow` and register again;
- a linked work tree — the error names the main work tree; register that
  instead (Agentum parks its own worktrees under the repository, so a linked
  tree is one typo away).

The stored `repo_path` is the working-tree root (`git rev-parse
--show-toplevel`), never the raw client input: a subdirectory of the repo, a
trailing slash, and the root itself all resolve to the same project row.

## A run executes in the working copy it started in

The path stopped being the project's key, so it can change under a run that is
already in flight. Therefore a run pins its working copy once, exactly the way
it pins `base_commit`: `tasks.checkout_path` is set at first start and never
re-resolved. Everything a run does — worktree creation, checks, instruction
pinning, evidence, teardown — goes to the pinned copy, not to whatever the
project points at now.

Re-registration resolves the project's *previous* path and distinguishes two
worlds:

- the previous path no longer holds this repository (it moved, or something
  else lives there): the working copy is one and it relocated — unfinished
  runs' `checkout_path` is rebound to the new path; terminal runs keep theirs
  as a historical fact;
- the previous path still resolves to the same identity: there are now two
  working copies — unfinished runs stay where they started, and the
  registration response names the previous path and how many runs remain in
  it.

A run whose pinned copy is unavailable, or turns out to hold a different
repository, pauses with stop reason `checkout_unavailable` (the event names
the path). A pause, not a failure: the condition is lifted from outside, and
the run must never rebuild its worktree in some other copy — that is the
silent loss of an entire commit line. If the repository moved with its
worktrees, `git worktree repair` rewires the absolute links before the run
continues.

## Runs, tasks, and stage invocations

- **run** — one execution of a pipeline against a project's working copy. It
  has a state ([the FSM](../internal/engine/fsm.go)), stages, evidence, the
  `agentum/<id>` branch, and decisions. **This is the only execution entity
  that exists in the MVP.**
- **task (work item)** — the business job, which over time may have several
  runs. **It is not an entity in the MVP**; today its content lives in the
  run's fields: `title` and `description`. The word `task` inside pack prompts
  keeps this, correct, meaning — a prompt's `## Task` section describes the
  requested work (the work item), not the execution.
- **stage invocation** — one request to an agent inside a run. The term is
  unambiguous and used identically everywhere; it is not renamed.

In the schema and the internal packages, `task` is the **physical name** of
the run — a leftover from the first migration; it denotes no second concept.
The one product term is **run**. The physical rename happens together with
the run / work-item split, where the schema changes anyway.

## Actor, creator, owner

Every recorded action carries two facts beside it:

| Role | What it answers | Where |
| --- | --- | --- |
| **owner** | who owns the project | `projects.user_id` |
| **creator** | who created the run | `tasks.user_id` |
| **actor** | who performed *this* action | `actor` + `user_id` on the action's row |

`actor` is one shared vocabulary — `human | agent | system` (`internal/authz`)
— used by artifact revisions, gate decisions (`task_approvals.actor`), and
events (`events.actor`) alike. There is no second vocabulary for the same
thing. `user_id` answers "on whose behalf"; the two name the same person only
when `actor = human`. When the orchestrator passes an automatic gate or pauses
a run, the record says `system` with the tenant's `user_id` — it never
presents a system action as the task author clicking something.

An actor value is a statement about what happened, not a permission: nothing
branches on it to allow or deny — that decision lives in `authz.Can`.

A gate that could have stopped the work and did not is a decision, and it is
recorded: `auto_if_clean` (the orchestrator verified a clean tree and passed)
and `auto_on_approval` (it applied a recorded human approval) each write one
decision with `actor = system` into the manifest's `gate_decisions`. A stage
with gate `auto` has no gate and writes nothing; a gate that *did* stop the
work is decided by the human, and the API handlers record that decision.

## `related_projects` is an inert seam

`related_projects uuid[]` stores a value and enables nothing. It is **not** a
model of relations between projects; a normalized model (an edge table, a
relation type, direction) is a separate decision, and its absence is not a
bug. When the seam comes alive it becomes the source of a path-scoped
`fs.read` capability derived from a **user-configured** set — the security
boundary is the configuration, never auto-discovery of neighboring
directories. Any change that puts `related_projects` into a join, a
validation, or a capability derivation is out of scope until that decision is
made.
