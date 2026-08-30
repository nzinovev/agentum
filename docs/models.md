# Models

Agentum is a **coordinator, not a credential manager.** It does not handle API
keys, provider endpoints, or base URLs. You install and configure the coding
agent (`opencode`) yourself, exactly as you would if you were running it
standalone. Agentum's only model-handling job is to decide which **model string**
to pass to the agent binary's `--model` flag.

The intended UX: clone, `make run`, and Agentum works — because your opencode is
already configured on your machine.

## How it works

1. A pack's stage names a **tier** (`fast`, `strong`, `reasoning`) — a portable
   label, not a concrete model. (See `docs/pack-format.md`.)
2. At run time Agentum resolves the tier to a model string and passes
   `--model <string>` to the agent subprocess.
3. The **agent binary** resolves that string to a real provider + endpoint +
   credentials using **your** configuration (`opencode auth`, env vars, …).

Agentum never touches credentials. If your agent is configured so that the model
string `"zai-coding-plan/glm-5.1"` routes to your z.ai coding plan, that's where
it routes — Agentum just handed it the string.

## Defaults (no configuration needed)

Agentum ships per-agent defaults so the common case needs no `models.yaml`:

| Agent | `fast` | `strong` | `reasoning` | default |
|---|---|---|---|---|
| `opencode` | `opencode/deepseek-v4-flash-free` | `opencode/north-mini-code-free` | `opencode/nemotron-3-ultra-free` | `strong` |

The `opencode` defaults use the **free models on opencode Zen** (the `-free`
suffix is explicit), so a fresh install works without a paid provider once you
connect Zen (`/connect opencode` in the TUI, or `opencode auth login`).

Defaults belong to the execution adapter that runs them: opencode is the only
adapter Agentum ships, so it is the only set of defaults there is. A second
adapter brings its own tiers with it, in the same change that adds it.

## Override (optional)

Drop a `models.yaml` next to the binary (or at `$XDG_CONFIG_HOME/agentum/`, or
point `AGENTUM_MODELS_CONFIG` at it) to override the defaults. A common case is
routing tiers to a different provider you've configured in your agent — for
example, GLM via the z.ai coding plan:

```yaml
# models.yaml (gitignored — copy from models.example.yaml)
tiers:
  fast: zai-coding-plan/glm-5-turbo
  strong: zai-coding-plan/glm-5.1
  reasoning: zai-coding-plan/glm-5.2
default: strong
```

When `models.yaml` is present it **replaces** the built-in defaults. Per-adapter
overrides are a future addition; today the file applies globally, so pick strings
your active agent understands.

## Resolution rules

- Operator override (`models.yaml`) wins when present.
- Otherwise the active execution adapter's built-in defaults are used — the
  tier table travels with the adapter's descriptor, because "these model
  strings work with this runtime" is runtime knowledge.
- An empty tier falls back to the configured default.
- An unknown tier is an **error** — Agentum never silently picks a model, and
  never substitutes a default for a name it could not resolve. A task whose
  pack names an unresolvable tier fails at run start, before the first
  invocation.
- The resolved model selection is a typed struct (`tier`, derived `provider`,
  options). A model option the active adapter does not declare is an **error**
  naming the adapter and the option — at boot for configured tiers, at run
  start for the pack, and again at Invoke as defence in depth. Nothing is
  silently dropped.

## Strict loading

`models.yaml` is decoded strictly: an unknown key (a `teirs:` typo, a
misspelled `defualt:`), a tier whose model string is empty, or a nested object
where a string is expected are load **errors** that stop the process at boot
with the file named — never a silent fall-back to the defaults while your
configuration sits unapplied.

Three more refusals follow from the same rule, and each one names its fix:

- **A present but empty file** — commented out entirely, or `tiers: {}` — is
  refused with *"declares no tiers; delete the file to use the execution
  adapter's built-in tiers"*. An override **replaces** the defaults rather
  than extending them, so an empty one is not "use the defaults": it is a
  configuration with no tiers at all, and every stage would fail later with
  `unknown tier` and no mention of the file that caused it.
- **`default:` naming a tier that is not declared** is refused at load, not at
  the first run that needs the default.
- **`AGENTUM_MODELS_CONFIG` pointing at a file that does not exist** is
  refused rather than searched past. The other locations are a *search*, so an
  absent one is simply a miss; a path you named is a statement of intent, and
  falling through to `<cwd>/models.yaml` or `~/.config` would run the process
  on tiers you did not pick — with the wrong model as the only symptom.

**No `models.yaml` at all is the one non-error**: it means "use the adapter's
built-in defaults", which is the common case.

## What's explicitly not Agentum's job

- API keys, OAuth tokens, refresh tokens.
- Provider base URLs / custom endpoints.
- Generating or placing the agent's own `opencode.json` or auth files.
- Per-task credential isolation (the agent binary owns its own auth).

If your agent binary needs configuration to reach a provider, configure that
binary directly. Agentum will pass the tier's model string and get out of the
way.
