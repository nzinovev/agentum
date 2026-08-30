# AGENTS.md

Working agreement for agents (and humans) building Agentum. This file is the
build-side companion to the code; it captures the stack, commands, and the
non-negotiable architecture seams.

## Stack

- **Language:** Go 1.25+. API on the stdlib `net/http`; logs on `log/slog`.
- **Store:** Postgres. Schema in `internal/store/migrations` (goose). Access via
  **sqlc** (queries in `internal/store/queries`, generated into
  `internal/store/sqlc`) over **pgx** driven through `database/sql`.
- **Migrations:** goose, embedded and **applied automatically on boot**
  (`internal/store.Open`). A manual goose CLI path exists via `make migrate-*`.
- **Deploy:** single-host `docker compose` (Postgres + app).

## Commands

| Task | Command |
|---|---|
| Resolve deps | `go mod tidy` |
| Build | `go build ./...` |
| Run (needs Postgres) | `make docker-up && make run` |
| Tests | `go test ./...` |
| Integration tests (needs `opencode` + credentials) | `go test -tags integration ./...` |
| Vet | `go vet ./...` |
| Format | `gofmt -s -w .` |
| Generate sqlc | `make sqlc-gen` (needs `sqlc` installed) |

Before any change is considered done: run `go vet ./...`, `go build ./...`, and
`go test ./...`. Tests live alongside the package they cover and are
table-driven by default. The tests that drive a real `opencode` subprocess sit
behind the `integration` build tag — they need the binary and provider
credentials, so a plain `go test ./...` excludes them; run them locally with
`go test -tags integration ./...`.

## Architecture seams — do not violate

- **Single front door.** Every external call hits the HTTP boundary in
  `internal/server` (`middleware.go`). Internal callers (future workers, any
  future CLI) must also traverse `internal/authz`. Never bypass it.
- **One authz function, one action vocabulary.** All permission decisions go
  through `authz.Can`. Today it returns true for the single owner; SSO/RBAC grow
  inside that function, not around it. The `action` argument is ALWAYS an
  `authz.Action*` constant — never a string literal at the call site. A new
  permission is declared in that `const` block in `internal/authz/authz.go`
  first, then used; a literal makes the permission surface unenumerable, and
  RBAC then has no fixed set of rules to attach to. Handlers do not hand-roll
  the check either: the guards live in `internal/api/access.go`, and every
  handler enters through `requireAccess` (principal + permission) or through one
  built on it — `requireTaskRead` for `task:read` on `{id}`,
  `requireTaskForAction` when the task row is needed too, `authorize` when a
  second resource must be cleared. `authz.Can` is therefore reached from exactly
  two places, `authorize` and the route gate in `internal/server`; a third call
  site is a bug. That is what makes every route answer an unauthenticated,
  forbidden, or missing resource with the same status and code. Adding a route
  means reusing a guard or adding one beside them, never copying a preamble.
- **Multi-tenancy seam.** Every DB row carries `tenant_id` and `user_id`. Never
  write a query that omits them; never assume single-tenant outside `authz`.
- **Explicit FSM.** Task lifecycle transitions live in `internal/engine/fsm.go`.
  Add states/events there and route changes through `engine.Next`; never mutate
  task state ad hoc.
- **Memory commits at task-done.** Rows in `memory_entries` are inserted only on
  final approval. Retrieval is recency-ordered; keyword pull must exist before
  the flywheel test window closes (around task ~20).
- **The task request reaches an agent only through the routing block's `Task`
  section**. `tasks.title` + `tasks.description` are the request and
  are rendered there by `internal/routing`; `tasks.overrides` configures the
  run and is orchestrator-only — never render it into any prompt, and the
  resolved `## Project checks` section is the only check information an agent
  sees.
- **Code-enforced capability profiles.** Every agent invocation runs under an
  effective `caps.Profile` computed as host ∩ pack ∩ stage ∩ role, deny by
  default. Profiles live in `internal/caps`; the opencode adapter materializes
  them in `internal/agent/enforce.go` (permission config + env scrub + ctx
  timeouts). Never bypass the adapter to run an agent — the profile is the
  boundary. `git.delivery` is orchestrator-only; no agent role includes it.
  Skills are allowed-and-recorded, not denied: a skill grants knowledge, not
  reach (it still meets the same `bash`/`edit`/`net` rules), so the runtime
  loads the operator's own skills and Agentum records each one's name,
  location, and content hash in the manifest (`internal/agent` `ContextProber`).
  `skill.*` is an inert capability token (no role grants it; a grant is
  unenforceable, like `mcp.*`) kept as a seam so narrowing becomes a config
  change later.
- **Plan-approval gates source-write.** A pack may declare an `approvals` block
  (ADR 0003). While a `source_write` approval is pending, the runner withholds
  `{fs.write, git.write, exec.bash}` from EVERY stage's capability profile (a
  subtractive input applied after the four-way intersection), refuses to enter
  implementer/fixer stages, and detects `plan_revision_drift`. The shipped
  `packs/backend-development` pack is the worked example: plan → human approval
  → implement → review ⇄ fix → final human review.
- **Orchestrator-owned project checks.** The project ships a versioned registry
  of named checks (`.agentum.yaml`, tracked in the repo). Commands live ONLY in
  that registry (an arg vector; no shell injection). Packs and task input add
  checks by name only — `checks.Resolve` rejects unknown names, never accepts a
  command, and cannot weaken the mandatory baseline (required is monotonic). The
  runner enforces the resolved set itself, against the post-stage checkpoint
  commit, before the result reaches the review gate; a mandatory failure blocks
  delivery. The check executor runs under a fixed minimal boundary (scrubbed
  env, no provider credentials). Never trust an agent's claim that checks
  passed — Agentum reads the result from its own executor. The project's
  instruction set (`instructions:` in `.agentum.yaml` ∪ the runtime-injected
  `AGENTS.md`) is pinned from `base_commit` the same way: the agent cannot
  change which instructions or checks gate a run.
- **The execution target is a registry entry.** Adapters are selected by id
  through `internal/agent`'s registry and describe themselves (`Describe()`:
  id, implementation version, binary, understood model options, baked-in tier
  defaults; `Probe()`: the runtime's own version, memoized per process).
  Adapter id and version are never literals in calling code, an option the
  descriptor does not declare is refused (boot, run start, Invoke), and
  manifest evidence is keyed by invocation — one record per stage attempt,
  never merged by stage.

## Conventions

- Comments capture *why*; don't restate what the code already says.
- IDs are `uuid` in Postgres and strings in Go (sqlc override in `sqlc.yaml`).
- Errors wrap with `%w`; the entrypoint logs and exits, handlers speak HTTP.
- Module path: `github.com/nzinovev/agentum`.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `chore:`,
  `docs:`, `refactor:`). PRs land via **squash-merge**, so the PR title is the
  commit on `main`.
- **No magic variable names.** Every identifier carries a descriptive name — no
  `a`, `b`, `p`, `d`, `t`, `m`, `q` or other single-letter/short-cryptic
  variables. The only single-letter names allowed are the universal Go idioms
  the style guide sanctions: `w`/`r` in `net/http` handlers, `ctx`, `err`, `i`
  in index loops, and `k`/`v` in map range. Receiver names are descriptive too
  (`api *API`, `runner *Runner`, `queue QueueStore`) and consistent across all
  methods of a type. When wrapping errors from a sub-call, expand the short
  form (`cerr` → `completeErr`, `ferr` → `failErr`) so the cause is readable.
