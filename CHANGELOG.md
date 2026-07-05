# Changelog

All notable changes to Agentum are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Once tagged releases begin, this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); until then the
`[Unreleased]` section accumulates change.

## [Unreleased]

### Added
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

### Changed
- Postgres tables live in a dedicated `agentum` schema (created on boot before
  migrations run) instead of the default `public` schema (#1).

### Fixed
- `store.Close` and SSE write errors are no longer silently dropped (#1).
