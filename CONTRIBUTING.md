# Contributing to Agentum

Thanks for your interest in contributing. This is a small, early project; the
short version: open an issue before a PR, keep changes focused, and follow the
conventions below.

## Development setup

1. Go 1.25+ and Docker.
2. Fork and clone.
3. Start Postgres: `make docker-up`.
4. Resolve deps and verify the baseline:

   ```sh
   go mod tidy
   go build ./...
   go vet ./...
   go test ./...
   ```

5. (Optional) Install `sqlc` if you change queries:
   `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`, then `make sqlc-gen`.

See `AGENTS.md` for the architecture seams that must not be violated.

## Database-backed tests

Tests that need Postgres (migrations up/down, store behavior) sit behind the
`AGENTUM_TEST_DATABASE_URL` environment variable:

```sh
make test-db
```

This starts the compose Postgres, waits for its healthcheck, and runs the full
test suite with the variable set. A plain `go test ./...` skips those tests
with a message pointing here — that skip is a developer convenience, not a
verdict: in CI the same tests fail when the variable is unset, so they cannot
quietly stop running. Each test gets its own database, copied from a migrated
template and dropped afterwards, so tests can neither observe each other's
rows nor depend on cleanup order.

The template itself is migrated once per test binary and left behind when it
exits — rebuilding it per test would cost a full migration run every time,
which is the cost the template exists to avoid. Leftovers (that template, plus
whatever a Ctrl-C stranded: an interrupted `go test` runs no cleanup) are
dropped by a later run once they are an hour old, which is long enough that a
database another `go test` process is using right now is never touched.

If port 5432 is already taken — another project's Postgres, usually — publish a
different one. The variable reaches both compose and the DSN:

```sh
AGENTUM_PG_PORT=55432 make test-db
```

On Windows there is no `make`; start Postgres and run the suite directly. Keep
the DSN quoted — unquoted, the `&` in it splits the command:

```powershell
docker compose up -d --wait
$env:AGENTUM_TEST_DATABASE_URL = "postgres://agentum:agentum@localhost:5432/agentum?sslmode=disable&search_path=agentum"
go test ./...
```

In `cmd.exe` the second line is
`set "AGENTUM_TEST_DATABASE_URL=postgres://agentum:agentum@localhost:5432/agentum?sslmode=disable&search_path=agentum"`.

## Workflow

- Branch from `main`, name it `<type>/<short-slug>` (e.g. `feat/memory-push`,
  `fix/sse-reconnect`, `chore/deps`).
- One concern per branch. Keep PRs small and reviewable.
- Conventional Commits for all commit messages — the **squash-merge** strategy
  turns each PR into a single commit on `main`, so your PR title should be a
  valid Conventional Commit subject line:

  ```
  feat: add keyword pull to project memory
  fix: handle empty Last-Event-ID on SSE attach
  chore: bump pgx to 5.10.1
  docs: clarify single-front-door rule in AGENTS.md
  refactor: collapse duplicate resume transitions in FSM
  ```

  Scope is allowed: `feat(memory): ...`. Keep the subject ≤ 72 chars,
  imperative mood. The PR body explains *why*; branch-commit history is
  squashed away on merge.

- Before requesting review, confirm locally:

  ```sh
  go build ./... && go vet ./... && go test ./... && gofmt -s -l .
  ```

  The last command should print nothing.

## Changelog

Add a bullet under `## [Unreleased]` in `CHANGELOG.md` for every PR that changes
behavior a user or operator would notice — a new endpoint, a schema change, a
format, a fix. Pure refactors and CI-only changes can skip it. Keep the bullet
to one line and reference the PR number (`(#42)`); group under `Added`,
`Changed`, `Fixed`, or `Removed` per [Keep a Changelog]. The bullet is the input
to release notes when a version is cut, so write what changed and why it
matters, not the implementation detail.

[Keep a Changelog]: https://keepachangelog.com/en/1.1.0/

When documenting a stable public contract (an extension surface like the pack
format), add or update the matching page under `docs/` in the same PR. Internal
packages do not need `docs/` pages.

## Code conventions

- Follow the existing style in the package you're editing.
- Comments capture *why*; don't restate what the code does.
- Errors wrap with `%w`. The entrypoint logs and exits; handlers speak HTTP.
- IDs are `uuid` in Postgres, strings in Go (see `sqlc.yaml`).
- Every new DB query and table carries `tenant_id` and `user_id`. No exceptions,
  even for local-only features — the multi-tenant seam is load-bearing.
- Tests are table-driven and live alongside the package they cover
  (`foo_test.go` next to `foo.go`).

## Reporting issues

Open a GitHub issue with: what you expected, what happened, minimal repro, and
Agentum version (`git rev-parse --short HEAD` if built from source).

## Security

See [SECURITY.md](SECURITY.md). Do not open public issues for security problems.

## License

By contributing, you agree your contributions are licensed Apache-2.0, the
project's license. See [LICENSE](LICENSE).
