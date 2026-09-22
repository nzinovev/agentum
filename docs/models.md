# Models

Agentum is a **coordinator, not a credential manager.** It does not handle API
keys, provider endpoints, or base URLs. You install and configure the coding
agent (`opencode`) yourself, exactly as you would if you were running it
standalone. Agentum's only model-handling job is to decide which **model
string** — and, on tiers that declare one, which **variant** — reaches the
agent binary's `--model` / `--variant` flags.

The intended UX: clone, `make run`, and Agentum works — because your opencode is
already configured on your machine.

## How it works

1. A pack's stage names a **tier** (`fast`, `strong`, `reasoning`) — a portable
   label, not a concrete model. (See `docs/pack-format.md`.)
2. At run time Agentum resolves the tier to a model string and an optional
   variant, and passes `--model <string>` (plus `--variant <value>` when set)
   to the agent subprocess.
3. The **agent binary** resolves that string to a real provider + endpoint +
   credentials using **your** configuration (`opencode auth`, env vars, …).

Agentum never touches credentials. If your agent is configured so that the model
string `"zai-coding-plan/glm-5.1"` routes to your z.ai coding plan, that's where
it routes — Agentum only passed the string.

## Defaults (no configuration needed)

Agentum ships per-agent defaults so the common case needs no `models.yaml`:

| Agent | `fast` | `strong` | `reasoning` | default |
|---|---|---|---|---|
| `opencode` | `opencode/nemotron-3.5-lightning-free` | `opencode/muse-spark-1.3-contributor-free` | `opencode/nemotron-3-ultra-free` | `strong` |

The `opencode` defaults use the **free models on opencode Zen** (the `-free`
suffix is explicit), so a fresh install works without a paid provider once you
connect Zen (`/connect opencode` in the TUI, or `opencode auth login`).
Default names are fixed at build time while the runtime's catalog changes —
upstream renames and removals have invalidated defaults before — which is why
the boot-time catalog check covers the effective tiers (your `models.yaml`,
or these defaults when you have none), not only the operator's file.

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

A tier value is either a bare model string (above) or a `{model, variant}`
mapping. `variant` is the runtime's reasoning-effort setting, passed to the
agent's `--variant` flag; the two forms mix freely in one file:

```yaml
tiers:
  fast: zai-coding-plan/glm-5.2-highspeed
  strong:
    model: zai-coding-plan/glm-5.3
    variant: low
  reasoning:
    model: zai-coding-plan/glm-5.3
    variant: high
default: strong
```

**A variant is declared on a tier, nowhere else.** There is no per-run or
per-project variant override. Different stages running at different efforts is
the pack's job: a stage names a tier (`stage.tier`), the tier carries the
variant, and two tiers on one model with different variants are the way
"plan on high, implement on low" is written.

When `models.yaml` is present it **replaces** the built-in defaults. Per-adapter
overrides are a future addition; today the file applies globally, so pick strings
your active agent understands.

## Resolution rules

- Operator override (`models.yaml`) takes precedence when present.
- Otherwise the active execution adapter's built-in defaults are used — the
  tier table travels with the adapter's descriptor, because "these model
  strings work with this runtime" is runtime knowledge.
- An empty tier falls back to the configured default.
- An unknown tier is an **error**: Agentum does not pick a model or substitute
  a default on its own. A run whose
  pack names an unresolvable tier fails at run start, before the first
  invocation.
- The resolved model selection is a typed struct (`tier`, derived `provider`,
  options). A model option the active adapter does not declare is an **error**
  naming the adapter and the option — at boot for configured tiers, at run
  start for the pack, and again at Invoke as defence in depth. An undeclared
  option is refused, not dropped.

## Strict loading

`models.yaml` is decoded strictly: an unknown key (a `teirs:` typo, a
misspelled `defualt:`), a tier whose model string is empty, or a value that is
neither a model string nor a `{model, variant}` mapping are load **errors**
that stop the process at boot with the file named — the process does not fall
back to the defaults and leave your configuration unapplied.

The mapping form carries its own strictness, and each refusal names the line:

| Input | Refusal |
|---|---|
| `strong: {model: m, varaint: high}` | `line N: unknown field "varaint" in a tier definition (known: model, variant)` |
| `strong: 42`, `strong: true` | `line N: a tier is either a model string or a {model, variant} mapping, got !!int` / `!!bool` |
| `strong: [a, b]` | `line N: a tier is either a model string or a {model, variant} mapping` |
| `model` or `variant` carrying a number, boolean, `null`, a list, or an object | `line N: tier field "model"` / `"variant" must be a string, got TAG` |
| `strong: {model: a, model: b}` | `line N: mapping key "model" already defined at line M` |
| `strong: {variant: high}` | `tier "strong": declares variant "high" but no model` |
| `strong:` (null), `strong: ""`, `strong: {model: "  "}` | `tier "strong": has an empty model string` |
| `strong: {model: m, variant: ""}` or a whitespace-only variant | `line N: a tier declares an empty variant; remove the key to run without one` |

The empty-variant row is the boundary between "no option" and "broken option":
an absent `variant` key runs the model at the runtime's own default effort, and
an explicitly empty one is refused so the file cannot look like it configured
something it did not.

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
  on tiers you did not pick — the only sign would be the wrong model.

**No `models.yaml` at all is the one non-error**: it means "use the adapter's
built-in defaults", which is the common case.

## Checking the model against the runtime's catalog

The model string is the one input that reaches the runtime verbatim, so it is
checked against the runtime's own model listing: the adapter probes
the listing once per process (memoized, sticky including a
failure — installing or fixing the runtime requires a restart) and every resolved
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
known providers instead — the likely cause is a misspelled prefix, and thirty
model names would hide it.

```
unknown model "zai-coding-plan/glm-5.3-hispeed" (did you mean "zai-coding-plan/glm-5.3-highspeed"?); list them with: opencode models zai-coding-plan
```

### The variant vocabulary

A configured variant is checked against the same listing, because the runtime
does not check it itself: `--variant hgih` runs as if it read `--variant high`,
with no warning and no error, and the only sign of the typo would be every run
answering at the wrong effort. The listing declares each model's variant
vocabulary (`opencode models <provider> --verbose`, the `variants` field), so
the check asks the runtime rather than maintaining a list here — a new provider
arrives with its own vocabulary. The refusal enumerates it:

```
validate models config, tier "strong": execution adapter "opencode": model "zai-coding-plan/glm-5.3" declares no variant "medium" (declares: high, low, max)
```

The check runs at the same three points as the model check (boot, run start,
`Invoke`) and refuses at the first one it reaches — before any invocation. What
each catalog state means for it:

| Catalog state | A configured variant |
|---|---|
| obtained, model found, vocabulary read | accepted on exact match; anything else is the refusal above |
| obtained, model found, vocabulary read and empty | refused — the model declares no variants at all |
| obtained, but the record carried no readable `variants` field | accepted; the boot log warns once with `variant vocabulary unavailable; variant validation skipped` |
| not obtained | accepted; the run proceeds with the variant unchecked, like the model |

A model that declares no variants is a fact the checker acts on; a record whose
`variants` field was absent or reshaped is the runtime's format moving, and
nothing about variants may be concluded from it. The two states stay distinct.

**"Could not check" is never "does not exist."** The listing is the runtime's
own output, read as a schema with optional fields: unknown fields are ignored,
and one unreadable record invalidates the whole catalog rather than dropping
models it would then refuse as non-existent. A catalog that
could not be obtained — binary missing, timeout, non-zero exit, empty answer,
unreadable records — validates as *nil*: the process boots and runs proceed
with models unchecked, and the run's evidence records the fact (the `adapter`
section's `model_catalog` label: `ok (N models)`, `failed: <reason>`, or
`unsupported` for an adapter that cannot list its runtime). A transient probe
failure reported as "no such model" for every configured tier at once would
refuse models that exist, with no operator workaround.

## Checking a model by calling it (on demand)

Presence in the catalog does not prove a model responds. A model can be
listed, declared active, and still hang every run
that uses it, with no error, no exit code, and no diagnostic. That class has
two answers, and neither is automatic:

- **`AGENTUM_FIRST_EVENT_TIMEOUT_SECONDS`** (default 120, zero disables)
  bounds how long an invocation may produce no output *at all*: a working
  model emits its first event in fractions of a second; a hung one emits
  nothing forever. The watchdog is disabled permanently once the first line
  arrives, so
  legitimate long work — which begins with an event — never falls under it.
  Silence *mid-work* is a different question, handled by the idle cap
  (`AGENTUM_IDLE_TIMEOUT_SECONDS`, default and semantics untouched).
- **The explicit check** — `POST /api/v1/models/test` (see `docs/api.md`)
  invokes the runtime once with a trivial prompt and reports whether the model
  produced output. It runs only when an operator asks for it, because it is a
  paid call; success is the first output line, so the check costs a few tokens
  rather than a whole answer, and the process group is stopped the moment the
  line arrives.

The check covers one thing: the model produced a first output line. A model
that emits one line and then goes quiet passes it; mid-run silence is the
first-event watchdog's and the idle cap's job. It does not check provider quotas or
billing — that state changes independently of Agentum and any snapshot would
be stale before it was useful.

## What's explicitly not Agentum's job

- API keys, OAuth tokens, refresh tokens.
- Provider base URLs / custom endpoints.
- Generating or placing the agent's own `opencode.json` or auth files.
- Per-run credential isolation (the agent binary owns its own auth).

If your agent binary needs configuration to reach a provider, configure that
binary directly. Agentum passes the tier's model string and does nothing else.
