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
| Vet | `go vet ./...` |
| Format | `gofmt -s -w .` |
| Generate sqlc | `make sqlc-gen` (needs `sqlc` installed) |

Before any change is considered done: run `go vet ./...` and `go build ./...`.
There are no tests yet; when adding them, write table-driven tests alongside the
package they cover.

## Architecture seams — do not violate

- **Single front door.** Every external call hits the HTTP boundary in
  `internal/server` (`middleware.go`). Internal callers (future workers, any
  future CLI) must also traverse `internal/authz`. Never bypass it.
- **One authz function.** All permission decisions go through `authz.Can`. Today
  it returns true for the single owner; SSO/RBAC grow inside that function, not
  around it.
- **Multi-tenancy seam.** Every DB row carries `tenant_id` and `user_id`. Never
  write a query that omits them; never assume single-tenant outside `authz`.
- **Explicit FSM.** Task lifecycle transitions live in `internal/engine/fsm.go`.
  Add states/events there and route changes through `engine.Next`; never mutate
  task state ad hoc.
- **Memory commits at task-done.** Rows in `memory_entries` are inserted only on
  final approval. Retrieval is recency-ordered; keyword pull must exist before
  the flywheel test window closes (around task ~20).

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
