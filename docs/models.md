# Models

Agentum is a **coordinator, not a credential manager.** It does not handle API
keys, provider endpoints, or base URLs. You install and configure the coding
agent (`opencode`, `claude-code`, …) yourself, exactly as you would if you were
running it standalone. Agentum's only model-handling job is to decide which
**model string** to pass to the agent binary's `--model` flag.

The intended UX: clone, `make run`, and Agentum works — because your opencode or
claude-code is already configured on your machine.

## How it works

1. A pack's stage names a **tier** (`fast`, `strong`, `reasoning`) — a portable
   label, not a concrete model. (See `docs/pack-format.md`.)
2. At run time Agentum resolves the tier to a model string and passes
   `--model <string>` to the agent subprocess.
3. The **agent binary** resolves that string to a real provider + endpoint +
   credentials using **your** configuration (`opencode auth`,
   `~/.claude/settings.json`, env vars, …).

Agentum never touches credentials. If your agent is configured so that the model
string `"sonnet"` routes to z.ai's GLM, that's where it routes — Agentum just
handed it `"sonnet"`.

## Defaults (no configuration needed)

Agentum ships per-agent defaults so the common case needs no `models.yaml`:

| Agent | `fast` | `strong` | `reasoning` | default |
|---|---|---|---|---|
| `opencode` | `anthropic/claude-haiku` | `anthropic/claude-sonnet` | `anthropic/claude-opus` | `strong` |
| `claude-code` | `haiku` | `sonnet` | `opus` | `strong` |

If your agent has Anthropic configured, this works out of the box. The
`claude-code` short aliases (`haiku`/`sonnet`/`opus`) are intentionally
remappable — your `~/.claude/settings.json` can point them at any compatible
provider.

## Override (optional)

Drop a `models.yaml` next to the binary (or at `$XDG_CONFIG_HOME/agentum/`, or
point `AGENTUM_MODELS_CONFIG` at it) to override the defaults:

```yaml
# models.yaml (gitignored — copy from models.example.yaml)
tiers:
  fast: anthropic/claude-haiku
  strong: anthropic/claude-sonnet
  reasoning: zai/glm-5.2     # any string your agent understands
default: strong
```

When `models.yaml` is present it **replaces** the built-in defaults for all
agents. Per-agent overrides are a future addition; today the file applies
globally, so pick strings your active agent understands.

## Resolution rules

- Operator override (`models.yaml`) wins when present.
- Otherwise the built-in default for the active agent is used.
- An empty tier falls back to the configured default.
- An unknown tier is an **error** — Agentum never silently picks a model.
- An unknown agent with no override is also an error.

## What's explicitly not Agentum's job

- API keys, OAuth tokens, refresh tokens.
- Provider base URLs / custom endpoints.
- Generating or placing `opencode.json`, `.claude/settings.json`, auth files.
- Per-task credential isolation (the agent binary owns its own auth).

If your agent binary needs configuration to reach a provider, configure that
binary directly. Agentum will pass the tier's model string and get out of the
way.
