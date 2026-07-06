# Agentum

Self-hostable orchestrator for AI engineering pipelines, built in Go.

Agentum coordinates AI coding-agent stages against a target codebase —
assembling prompts, running agents, gating output for humans, accumulating
memory across tasks, and enforcing tool-capability boundaries. You install and
configure your coding agent (opencode, claude-code, …) yourself; Agentum
coordinates runs, governance, memory, and audit.

> **Status:** early. The engine foundation, multi-tenant schema, explicit task
> FSM, single-front-door HTTP API, and the project-memory schema are scaffolded.
> See `AGENTS.md` for the build agreement and architecture seams.

## Quick start

1. Install Go 1.25+ and Docker.
2. Start Postgres: `make docker-up`.
3. Resolve deps and run: `go mod tidy && make run`.
4. Check health: `curl http://localhost:8080/healthz`.
5. (Optional) Generate the data layer: install `sqlc`, then `make sqlc-gen`.

## Documentation

- [`docs/pack-format.md`](docs/pack-format.md) — the pipeline-pack format, the
  primary extension surface (manifest schema, gates, override layers,
  validation rules).
- [`docs/agent-contract.md`](docs/agent-contract.md) — the result.json contract
  agents must write, and the event-stream model.
- [`docs/api.md`](docs/api.md) — the HTTP API: endpoint table, error model, SSE
  event types + replay.
- [`docs/models.md`](docs/models.md) — how Agentum resolves a tier to a model
  string and passes `--model`; built-in defaults for opencode + claude-code.
- [`CHANGELOG.md`](CHANGELOG.md) — what's landed, under `[Unreleased]`.
- [`AGENTS.md`](AGENTS.md) — build agreement and architecture seams.
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — how to contribute.

## License

Apache-2.0. See [LICENSE](LICENSE).
