# Handoff: isolated-publisher — PR2

**Branch:** agent/isolated-publisher-pr2
**Date:** 2026-09-27

## What Was Merged

This branch implements the GitHub provider, publication configuration, target
resolution and freezing, authenticated push of the pinned result commit, and
creation or update of a draft pull request. Pull requests carry the minimal
body agreed in the PR1 handoff. PR3 adds the full template, stores the published body as an
artifact revision, and completes the user-facing documentation.

The default registry entry is now `github`; `noop` remains available for
network-free refusal. Publication remains disabled by default. The server
passes no credential to the provider while publication is disabled, including
when processing an older queued job.

## DB State

Migration `0015_publication_draft_rejected.sql` adds `draft_rejected` with a
false default. It backfills recorded `draft_unsupported` refusals that carry a
PR number. The marker survives unrelated failures and clears after successful
delivery. A retry must observe the previously refused PR as draft before it
can succeed; later human removal of draft remains allowed. The migration has
an up/down/up test and a backfill test.

`FreezePublicationTarget` writes the destination before push. It checks the
tenant, run, creator, attempt, lease owner, and lease expiry. Retries retain the
first destination even after a failed push or a crash before recording the PR
number. `RecordPublicationFailure` now preserves an observed PR number, URL,
and state, including `closed` or `merged`. The sqlc output was regenerated and
a second generation produced identical bytes.

## Intentional Stubs / Incomplete Interfaces

| Remaining work | Location | What PR3 must do |
|---|---|---|
| Minimal PR body | `internal/publish/body.go` | Replace `renderMinimalBody` with the full embedded template; retain the explicit no-checks line and human-merge statement. |
| Plan reference and review verdict remain unfilled | `internal/publication/service.go`, `assembleDelivery` | Assemble the approved plan revision, approval actor/time, and reviewer verdict from durable records. |
| Body publication has no artifact revision yet | `internal/publish/github.go`, create/update payloads | Store the rendered body as `pr_description`, publish exactly the returned bytes, and implement the reject-policy refusal. Both creation and update must consume those bytes. |
| Final-review documentation and changelog | `docs/api.md`, `docs/execution.md`, `docs/capabilities.md`, `docs/domain-model.md`, `AGENTS.md`, `CHANGELOG.md` | Document publication outcomes, events, retry and cleanup behavior, and the publisher boundary. The six publication variables are already in `.env.example`; expand it to cover the rest of the application configuration. Its header currently states that it is publication-only. |

## Gotchas Discovered

### Agreed additions to the provider boundary

The closed HTTP operation table includes one additional read:
`GET /repos/{owner}/{repo}/branches/{branch}`. Repository metadata alone cannot
confirm that `base_ref` names an existing branch. The table still contains no
`PUT` or merge operation.

A temporary `HOME` does not disable repository-local hooks or credential
helpers. The git environment therefore includes fixed `GIT_CONFIG_*` settings
that disable helpers, hooks, fsmonitor, signed pushes, recursive submodule
pushes, tag following, local push options, automatic upstream setup, HTTP
redirects, and ambient proxy/header configuration. Only HTTPS
transport is enabled. The two delivery commands retain their specified argv.

Before target resolution or any authenticated request, the coordinator calls
the provider's checkout preflight. The provider repeats this check before
push. Git parses the common config and optional per-worktree config with
`--name-only --null --no-includes`; values and included files are not read into
the diagnostic. Ordinary `notes`, `pull`, `submodule`, `maintenance`, `filter`,
and `alias` settings are accepted, including multiline values.

The key filter refuses `include.*`, `includeif.*`, `url.*`, `http.*`, scoped
`credential` keys, `remote.*.promisor`, `remote.*.partialclonefilter`,
`core.sshcommand`, `core.hookspath`, and `core.fsmonitor`. The earlier guards
against URL-shaped remote names and `extensions.partialclone` remain: both can
redirect authenticated git execution. The refusal names the key, replacing
URL/condition/name subsections with placeholders so credentials in them cannot
reach records or events. Config read/start failures use retryable
`provider_error`. Successful commit verification compares stdout only;
stderr warnings do not cause a commit mismatch.

### Refusal vocabulary added during review

| Code | Row state | Meaning |
|---|---|---|
| `unsafe_git_config` | `blocked` | A local key can redirect or extend authenticated git execution; the diagnostic names the key. |
| `provider_rate_limited` | `failed` | GitHub returned a rate limit response; wait for the provider limit to reset before explicit retry. |
| `pull_request_not_found` | `blocked` | The recorded number was not found; no replacement is created and closure is not inferred. |
| `lease_budget_exhausted` | `failed` | Less than one second of usable lease budget remains; no network attempt starts. |

All four codes are included in `allReasonCodes`, `SafeRefusal`, and the retry
partition tests. `SafeError` retains the package's structured config-key
diagnostic while discarding provider prose. Rate limits are recognized from
`Retry-After`, `X-RateLimit-Remaining: 0`, status `429`, or a rate-limit message
on `403`. Repository/list `404` becomes `remote_unknown`; branch lookup `404`
still permits default-branch fallback. Recorded `failed` and `blocked` rows
are never scheduled by the recovery probe, including rate limits.

### Target and retry behavior

| Case | Behavior |
|---|---|
| Explicit base override | Use the configured branch name. |
| Branch-valued `base_ref` found by the provider | Use that branch. `refs/heads/` is removed before lookup. |
| SHA, tag, or missing branch | Read the repository's default branch. |
| Remote host differs from the API host | Refuse before sending credentials; `api.github.com` maps to `github.com`, while Enterprise uses the configured API host. |
| PR number is recorded | Find that number in the paginated head-filtered list and update only title/body. Missing numbers yield `pull_request_not_found`. |
| Number was lost after creation | Find the existing PR by head and retain its number. A duplicate-creation response repeats that lookup. |
| Existing PR is closed or merged | Record the observation and block; never create a replacement or reopen it. |

The PR lookup compares owner labels without case sensitivity and preserves
case-sensitive branch comparison. `AGENTUM_PUBLISH_REMOTE` and
`AGENTUM_PUBLISH_BASE_BRANCH` are validated on boot.

The PR lookup includes closed requests. This also prevents a replacement when
a PR was closed between remote creation and recovery of its lost local number.
Provider response URLs and error prose are not stored: the URL is constructed
from the pinned target and number, and failures use fixed diagnostics.

Each provider attempt has a two-minute timeout. The coordinator also bounds
network work to less than the remaining publication lease and refuses budgets
below one second before creating a context. Target resolution
uses the claimed row, and the existing attempt/owner fence protects outcome
writes.

`Probe` reads a repository only when registry construction supplies
`ProbeTarget`. The process-wide registry has no project target and reports
not ready with that reason. Project readiness can construct a target-scoped
registry later; the probe verifies read access, not publication permissions.

### Validation

Passed on the final code:

- `gofmt -s -w .`
- `go vet ./...`
- `go build ./...`
- `go test ./...`
- `AGENTUM_PG_PORT=55432 make test-db`
- `go test -race ./internal/publish ./internal/publication`
- `make sqlc-gen` with reproducibility verification
- `git diff --check`

Tests use local bare repositories and TLS `httptest` servers. A live read of
[the encoded slash branch](https://api.github.com/repos/llvm/llvm-project/branches/release%2F20.x)
returned HTTP `200`, name `release/20.x`, and commit
`87f0227cb60147a26a1eeb4fb06e3b505e9c7261` during review. `PathEscape` is retained;
the slash-branch regression test asserts both the escaped path and selected
base. The actual Agentum linked checkout also passed the revised preflight.
`.env.example` is explicitly unignored by `.gitignore`. No live GitHub
push or pull request was performed. The external-runtime integration suite
was not run; it requires `opencode` and provider credentials.

## What PR3 Must Read Before Starting

- [ ] This file and the external delivery plan's PR3 section.
- [ ] `internal/publish/publish.go` and `internal/publish/outcome.go` — delivery references, existing PR number, and partial outcomes.
- [ ] `internal/publish/body.go` and `internal/publish/github.go` — both PR payloads currently render the minimal body.
- [ ] `internal/publication/service.go` — delivery assembly, target freeze before push, and fenced outcome recording.
- [ ] `internal/store/queries/run_publications.sql` — target and PR observation persistence.
- [ ] `internal/api/publication.go` — distinguish API-only `disabled`, `not_attempted`, and `unavailable` from row states.
- [ ] Repository `docs-writing` and `go-comments` skills before editing prose or comments.
