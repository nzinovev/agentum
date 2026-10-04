# Agentum

Self-hostable orchestrator for AI engineering pipelines, built in Go.

Agentum coordinates AI coding-agent stages against a target codebase —
assembling prompts, running agents, gating output for humans, accumulating
memory across runs, and enforcing tool-capability boundaries. You install and
configure your coding agent (opencode, …) yourself; Agentum coordinates runs,
governance, memory, and audit. The executor is a registry entry, not a
hard-coded name: Agentum ships the opencode adapter today, and the model, its
parameters, and the runtime's version are evidence on every recorded
invocation.

> **Status:** early. The browser UI covers project registration, run creation,
> run status, and artifact viewing. The engine and HTTP API are under active development.
> See `AGENTS.md` for the build agreement and architecture seams.

## Quick start

1. Install Go 1.25+ and Docker.
2. Start Postgres: `make docker-up`.
3. Resolve deps and run: `go mod tidy && make run`.
4. Open `http://localhost:8080/projects` to register a repository and create a run.
5. Check health: `curl http://localhost:8080/healthz`.
6. (Optional) Generate the data layer: install `sqlc`, then `make sqlc-gen`.

## Browser UI development

Install Node.js 22 and run `npm ci --prefix web` once. After editing `web/src`,
run `make web-build` and commit the generated files in `internal/server/web`.
CI rebuilds the UI and checks that those files match. A Go build and the running
server use the committed files and do not require Node.js.

## Documentation

- [`docs/pack-format.md`](docs/pack-format.md) — the pipeline-pack format, the
  primary extension surface (manifest schema, gates, override layers,
  validation rules).
- [`docs/agent-contract.md`](docs/agent-contract.md) — the result.json contract
  agents must write, and the event-stream model.
- [`docs/api.md`](docs/api.md) — the HTTP API: endpoint table, error model, SSE
  event types + replay.
- [`docs/models.md`](docs/models.md) — how Agentum resolves a tier to a model
  string and passes `--model`; built-in defaults shipped with the active
  execution adapter.
- [`CHANGELOG.md`](CHANGELOG.md) — what's landed, under `[Unreleased]`.
- [`AGENTS.md`](AGENTS.md) — build agreement and architecture seams.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — how to contribute.

## License

Apache-2.0. See [LICENSE](LICENSE).
