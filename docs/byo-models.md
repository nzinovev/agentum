# Bring-your-own-models config

Agentum is a coordinator, not a model gateway. You bring the credentials; the
config below tells the orchestrator how to authenticate each provider you use
and how a **tier** name (e.g. `fast`, `strong`) maps to a concrete model id
(`provider/model`). Packs reference tiers by name; the runner resolves them at
invocation time.

This is the operator-facing contract for `models.yaml`. The package is
`internal/models`.

## Location

The config file is resolved in order:

1. `$AGENTUM_MODELS_CONFIG` (if set) — explicit path
2. `<cwd>/models.yaml`
3. `$XDG_CONFIG_HOME/agentum/models.yaml` (or `~/.config/agentum/models.yaml`)

A missing file is allowed: the orchestrator then requires literal model ids at
call sites and the agent binary's own auth. Tier resolution in that mode errors
loudly — no silent model pick.

Copy `models.example.yaml` to `models.yaml` to start. **`models.yaml` is
gitignored** — never commit a real one.

## Schema

```yaml
providers:
  anthropic:
    api_key_env: ANTHROPIC_API_KEY   # required: canonical env var name
  zai:
    api_key_env: ZAI_API_KEY
    base_url: https://api.z.ai/v     # optional endpoint override
  local-dev:
    api_key_env: LOCAL_API_KEY
    api_key: sk-inline-dev-only      # optional inline value (see Security)

tiers:
  fast: anthropic/claude-haiku
  strong: anthropic/claude-sonnet
  reasoning: zai/glm-5.2

default: strong
```

### `providers`

| Field | Required | Notes |
|---|---|---|
| `api_key_env` | **yes** | The env var name the provider's SDK reads (`ANTHROPIC_API_KEY`, …). The orchestrator sets this var on the agent subprocess. |
| `api_key` | no | Inline value for `api_key_env` — dev convenience. Warned on; masked in logs. Production should export the env var instead. |
| `base_url` | no | Endpoint override; surfaced to the subprocess as `<PROVIDER>_BASE_URL`. |

A provider without `api_key_env` fails to parse — the variable name is mandatory
(the orchestrator never invents one).

### `tiers`

A map from a tier name to a `provider/model` id. Packs reference tiers in their
manifest; the runner resolves a tier here to get the concrete id the adapter
passes through (e.g. opencode's `--model provider/model`).

### `default`

The tier used when a pack/stage doesn't name one. Optional; if absent, every
call site must name a tier explicitly.

## Resolution

- `Resolve(tier)` → `"provider/model"`. Empty tier falls back to `default`. An
  unknown tier (or empty-with-no-default) is an error — the orchestrator never
  silently picks a model.
- `EnvForProvider(provider)` → the env vars the runner sets on the agent
  subprocess for that provider: the key (inline value if set, else read from the
  live process env) plus `<PROVIDER>_BASE_URL` when configured. Returns an error
  if the credential can't be resolved (unknown provider, or env var unset and no
  inline value).

## Security

- **Credentials live in the environment**, not in committed files. `api_key_env`
  names the variable; the value is read from the process env at invocation time.
- **Inline `api_key` is a dev escape hatch**, warned on at load. `Provider`
  implements `slog.LogValuer` so the key is masked if a struct is ever logged —
  but the intended discipline is: production exports the env var and leaves
  `api_key` empty.
- **`models.yaml` is gitignored.** The committed `models.example.yaml` uses env
  references only.
- The orchestrator **never logs credential values**. Tier resolutions and env
  var *names* are logged; values are not.

## Adapter integration

The adapter (`internal/agent`) already takes `agent.Invocation.Model` as a
literal `provider/model`. The runner (Epic 3 / 5.1) is the consumer of this
config: it resolves a pack's tier to a model id and, when spawning the agent
subprocess, sets the provider's env vars from `EnvForProvider`. Wiring lands with
the runner; this package delivers the config contract and resolver.
