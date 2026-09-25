# Handoff: isolated-publisher — PR1

**Branch:** agent/isolated-publisher-pr1
**Date:** 2026-09-25

## What Was Merged

The publication pipeline's skeleton, end to end and network-free: the
provider contract and registry in `internal/publish` (one entry — a
placeholder that refuses every attempt with `credentials_missing`), the
`run_publications` table and its six queries, the queue's kind-table dispatch
(`jobs.Mux`) that makes the publication coordinator a peer of the runner, the
coordinator itself in `internal/publication` (delivery assembly, the delivery
gate, the lease, the outcome recording, the three events, the manifest
section with the correction path after seal), the final-gate hook in the
runner (best-effort row + job, off by default), the read/write API surface
(`GET /api/v1/runs/{id}/publication`, `POST /api/v1/runs/{id}/publish`, the
`publication` block in final-review, `run:publish` in the permission
vocabulary), the removal of the Git-provider token grants from the adapter's
secret map, and the recovery probe for lost publications in the reconciler.
Nothing in this PR touches the network: a publication that runs today records
`failed` with `credentials_missing`.

## DB State

Goose migration `0013_run_publications.sql` is part of this PR and applies on
boot through the usual `store.Open` path. One table, one index; the target
columns default to the empty string because the destination is derived at
the first attempt and then frozen.

## Intentional Stubs / Incomplete Interfaces

| Stub | Location | What PR2 must do |
|------|----------|------------------|
| `NoopPublisher` (registry id `noop`) | `internal/publish/noop.go` | Add the GitHub provider as a second registry entry (push by result-commit SHA, draft PR, the closed operation table without merge); the placeholder stays or goes per the plan's provider table |
| `PublicationHook` zero value wired in the server | `internal/server/server.go` | Wire the hook from the six configuration variables (`AGENTUM_PUBLISH_ENABLED` etc.): enabled flag + provider id, with `Load` refusing enabled-without-token at boot |
| `publicationConfig{enabled: false}` wired in the server | `internal/server/server.go` | Same source: the API's disabled/not-attempted answers and the POST precondition follow the configuration |
| Target derivation absent | `internal/publication/service.go` | Derive host/owner/repository/base branch from the pinned checkout's remote at the first attempt (both URL forms, the three base-branch rules), and freeze the derived target in the row; `RecordPublicationSuccess` already keeps an existing target via `COALESCE(NULLIF(...))` |
| `Delivery.Plan` left zero-valued; `Delivery.Review.Verdict` empty | `internal/publication/service.go` (`assembleDelivery`) | Fill when the pull request body is rendered (PR3): plan ref from the pack's approval artifact revision + approval row, verdict from the reviewer's verdict.json revision; `FixCycles` is already assembled |

## Gotchas Discovered

- The `run_publications` target columns are `NOT NULL` — without
  `DEFAULT ''` the gate-time insert (which carries no target yet) fails with
  a not-null violation. The default is in the migration; keep it when the
  columns change.
- `httptest` + direct handler dispatch does not populate `r.PathValue("id")`
  — tests must call `request.SetPathValue` themselves (see
  `internal/api/publication_test.go`).
- `sqlc` is not on PATH on this machine; it lives at `~/go/bin/sqlc`
  (v1.31.1, needs Go ≥ 1.26 which `go install` switches to automatically).
- Port 5432 on this host is held by an unrelated compose project; use
  `AGENTUM_PG_PORT=55432 make test-db`.
- The fake store in the runner tests (`runner_loop_test.go`) now carries the
  `publications` slice and the two error-injection fields; runner job tests
  call the per-kind methods (`HandleRun`, `HandleAdvance`, …) — the runner's
  old kind switch is gone.

## What PR2 Must Read Before Starting

- [ ] This file
- [ ] **The pull request body sequencing decision**: PR2 creates real draft
  pull requests, and the full body template with its artifact revision lands
  in PR3. Between the two, pull requests would go out with an empty
  description. Decision to carry into PR2: it renders a MINIMAL body from the
  fields the coordinator already assembles (request title and description,
  `base_commit..result_commit`, the checks table with its "no checks
  declared" line, the evidence line, the closing line that merge is a human
  action); PR3 replaces that with the full template and stores the published
  text as an artifact revision. The alternative — releasing PR2 and PR3
  together — is on the table only if the minimal body turns out to duplicate
  the template's structure.
- [ ] The plan document: `/home/nikita/Documents/urbanVault/проекты/Agentum/ADR/0010-isolated-publisher-push-and-draft-pr.md` — sections «План поставки» (PR2), «Решения» on the git surface (two commands, push by SHA), the askpass token transport and allow-list environment, the target derivation and freezing, the GitHub client's closed operation table, and idempotency (the three paths to one pull request)
- [ ] `internal/publish/publish.go` — the contract shapes PR2 implements
- [ ] `internal/publication/service.go` — where the provider is resolved and the outcome recorded; PR2's target derivation slots in beside `assembleDelivery`
- [ ] `internal/store/queries/run_publications.sql` — the freeze semantics already in `RecordPublicationSuccess`, and the claim predicate that matches the recovery probe on expired AND NULL leases
- [ ] `internal/config/config.go` — the refusal pattern PR2's six variables follow (`AGENTUM_OPENCODE_BINARY` refusal is the model)
