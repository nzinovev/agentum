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
| `opencode` | `opencode/nemotron-3.5-lightning-free` | `opencode/muse-spark-1.3-contributor-free` | `opencode/nemotron-3-ultra-free` | `strong` |

The `opencode` defaults use the **free models on opencode Zen** (the `-free`
suffix is explicit), so a fresh install works without a paid provider once you
connect Zen (`/connect opencode` in the TUI, or `opencode auth login`).
Default names are a build-time claim about the runtime's catalog — upstream
renames and removals have retired defaults before — which is why the boot-time
catalog check covers the effective tiers (your `models.yaml`, or these
defaults when you have none), not only the operator's file.

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
  never substitutes a default for a name it could not resolve. A run whose
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

## Checking the model against the runtime's catalog

The model string is the one input that reaches the runtime verbatim, so it is
checked against what the runtime itself says it can run: the adapter probes
the runtime's model listing once per process (memoized, sticky including a
failure — installing or fixing the runtime is a restart) and every resolved
selection is validated against it. Three points refuse, in order of how early
they catch the mistake:

| Point | What is checked | How it fails |
|---|---|---|
| Process start | every effective tier — `models.yaml` when present, otherwise the adapter's defaults | `Run` returns an error; HTTP never comes up, no job is claimed |
| Run start | the model of every pack stage | the run fails before the first invocation — no `stage_invocations` rows |
| `Invoke` | the same selection, assembled by any path | the adapter refuses; no subprocess is spawned |

The refusal names the tier, the model, and the adapter, and carries its fix:
a near spelling (edit distance ≤ 3, at most three candidates, nearest first)
and the listing command for the provider. An unknown provider enumerates the
known providers instead — the operator misspelled the prefix, and thirty
model names would bury that.

```
unknown model "zai-coding-plan/glm-5.3-hispeed" (did you mean "zai-coding-plan/glm-5.3-highspeed"?); list them with: opencode models zai-coding-plan
```

**"Could not check" is never "does not exist."** The listing is the runtime's
own output, read as a schema with optional fields: unknown fields are ignored,
and one unreadable record invalidates the whole catalog rather than silently
dropping models the checker would then refuse as non-existent. A catalog that
could not be obtained — binary missing, timeout, non-zero exit, empty answer,
unreadable records — validates as *nil*: the process boots and runs proceed
with models unchecked, and the run's evidence records the fact (the `adapter`
section's `model_catalog` label: `ok (N models)`, `failed: <reason>`, or
`unsupported` for an adapter that cannot list its runtime). A transient probe
failure turning into "no such model" for every configured tier at once would
be a refusal that lies, with no operator workaround.

## Checking a model by calling it (on demand)

Sitting in the catalog proves the runtime *knows* a model — not that the model
*answers*. A model can be listed, declared active, and still hang every run
that uses it, with no error, no exit code, and no diagnostic. That class has
two answers, and neither is automatic:

- **`AGENTUM_FIRST_EVENT_TIMEOUT_SECONDS`** (default 120, zero disables)
  bounds how long an invocation may produce no output *at all*: a working
  model emits its first event in fractions of a second; a hung one emits
  nothing forever. The watchdog retires permanently on the first line, so
  legitimate long work — which begins with an event — never falls under it.
  Silence *mid-work* is a different question, owned by the idle cap
  (`AGENTUM_IDLE_TIMEOUT_SECONDS`, default and semantics untouched).
- **The explicit check** — `POST /api/v1/models/test` (see `docs/api.md`)
  invokes the runtime once with a trivial prompt and reports whether the model
  produced output. It runs only when an operator asks for it, because it is a
  paid call; success is the first output line, so the check costs a few tokens
  rather than a whole answer, and the process group is stopped the moment the
  line arrives.

The explicit check is honest about its boundary: a model that emits one line
and goes quiet passes it. Catching that mid-run silence is the first-event
watchdog's and the idle cap's job. It does not check provider quotas or
billing — that state changes independently of Agentum and any snapshot would
be stale before it was useful.

## What's explicitly not Agentum's job

- API keys, OAuth tokens, refresh tokens.
- Provider base URLs / custom endpoints.
- Generating or placing the agent's own `opencode.json` or auth files.
- Per-run credential isolation (the agent binary owns its own auth).

If your agent binary needs configuration to reach a provider, configure that
binary directly. Agentum will pass the tier's model string and get out of the
way.
