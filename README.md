# Agentum

Self-hostable orchestrator for AI engineering pipelines, built in Go.

Agentum coordinates AI coding-agent stages against a target codebase —
assembling prompts, running agents, gating output for humans, accumulating
memory across tasks, and enforcing tool-capability boundaries. You bring your
own model credentials; Agentum handles coordination, governance, memory, and
audit.

> **Status:** early. The engine foundation, multi-tenant schema, explicit task
> FSM, single-front-door HTTP API, and the project-memory schema are scaffolded.
> See `AGENTS.md` for the build agreement and architecture seams.

## Quick start

1. Install Go 1.25+ and Docker.
2. Start Postgres: `make docker-up`.
3. Resolve deps and run: `go mod tidy && make run`.
4. Check health: `curl http://localhost:8080/healthz`.
5. (Optional) Generate the data layer: install `sqlc`, then `make sqlc-gen`.

## License

Apache-2.0. See [LICENSE](LICENSE).
