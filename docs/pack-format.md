# Pack format

A **pack** is the versioned unit a pipeline run is built from: a set of agent
stages, their gates, the prompts that drive them, the budgets that bound them,
and declarations of the memory scopes and tool capabilities they use. Packs are
the primary extension surface — you run a shipped pack as-is, or override parts
of one without forking the whole thing.

This is the reference for pack format **v1** (`api: agentum/v1`).

## Physical shape

A pack is a **directory** containing a `manifest.yaml` and a tree of prompt
files (conventionally under `prompts/`):

```
packs/my-pack/
  manifest.yaml
  prompts/
    spec.md
    implement.md
```

Prompt files are referenced by path relative to the pack directory. Keeping
prompts as files (not inline YAML) is what makes [overriding a
prompt](#override-document) a plain file swap.

## Manifest

```yaml
api: agentum/v1
pack:
  name: my-pack
  version: 1.0.0
  persona: engineering
  description: Short human-facing summary.
memory:
  reads: [project]
  writes: true
capabilities: [fs.read, fs.write, git]
budgets:
  fix_cycles: 3
  ask_to_edit: 2
tiers:
  default: fast
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: implement
  implement:
    gate: auto_if_clean
    prompt: prompts/implement.md
    tier: strong            # optional; overrides tiers.default
    transitions:
      - to: review
      # - to: security
      #   condition: touches_auth   # opaque until conditional-linear lands
  review:
    gate: human_final
    prompt: prompts/review.md
    transitions:
      - to: fix
        condition: verdict == "changes_requested"
      - to: done
        condition: verdict == "approved"
  fix:
    gate: auto_on_approval
    prompt: prompts/fix.md
    transitions:
      - to: review
  done: {}                  # terminal: engine state, no prompt, no gate
```

### Fields

| Field | Type | Notes |
|---|---|---|
| `api` | string | Must be `agentum/v1`. |
| `pack.name` | string | Required. Matches the directory name when served by a `Source`. |
| `pack.version` | semver | `MAJOR.MINOR.PATCH`, no leading zeros, no pre-release tags. Drives lock-major override. |
| `pack.persona` | string | Free-form metadata tag (e.g. `engineering`). The engine encodes no persona-specific behavior. |
| `pack.description` | string | Optional. |
| `memory.reads` | list | Subset of `{project, user, org}`. Only `project` is wired into retrieval at MVP. |
| `memory.writes` | bool | Whether the pack's agents may emit memory entries. |
| `capabilities` | list | Pack-wide MCP capability declarations. A stage narrows to the pack∩stage subset (enforcement comes later). |
| `budgets.fix_cycles` | int | Per-pipeline fix-loop cap, ≥ 0. Replaces a hardcoded cycle count. |
| `budgets.ask_to_edit` | int | Per-pipeline scoped-edit recursion cap, ≥ 0. |
| `tiers.default` | string | Fallback tier name. Resolves to a concrete model id via the BYO-models config (not yet wired). |
| `entry` | string | The stage the run starts at. Must be defined in `stages`. |
| `stages` | map | Named map of stage id → stage definition. See below. |

### Stages

Stages are a **named map**, not an ordered list. Each stage declares its own
outgoing transitions, and the pack declares one `entry`. This is the substrate
for conditional-linear routing: a transition carries an opaque `condition` and a
stage may fan out to several destinations. (The condition evaluator itself is
not part of format v1.)

```yaml
stages:
  <id>:
    gate: <gate-value>
    prompt: <path>
    tier: <name>           # optional
    transitions:
      - to: <stage-id>
      - to: <stage-id>
        condition: <opaque>
```

| Field | Required | Notes |
|---|---|---|
| `gate` | non-terminal stages | One of the [gate values](#gate-values). Ignored on terminal stages. |
| `prompt` | non-terminal stages | Path relative to the pack dir; must not escape it. Absent on terminal stages. |
| `tier` | optional | Overrides `tiers.default` for this stage. |
| `transitions` | optional | Outgoing edges. Absent ⇒ terminal. |

A **terminal stage** (no `transitions`) is an engine state, not an agent
invocation: it has no prompt and no gate. It is the pipeline's exit.

### Gate values

The six-value control vocabulary:

| Value | Meaning |
|---|---|
| `auto` | advance with no human review |
| `auto_if_clean` | auto-advance if the produced output is clean |
| `auto_on_approval` | auto-advance once a prior approval is on record |
| `human_approval` | a human approves before advancing |
| `human_final` | a human gives final approval (the gate before a terminal) |
| `human_edit` | a human edits the artifact directly; the edit is the approval |

## Validation rules

A pack is rejected at load/resolve time if any of these fail:

- `api` is `agentum/v1`.
- `pack.name` is non-empty; `pack.version` is valid semver.
- `entry` is defined in `stages`.
- Every non-terminal stage has a `gate` from the six-value enum and a `prompt`
  that resolves to a readable, non-empty file inside the pack dir.
- Every `transitions[*].to` references a defined stage; no self-loops.
- Every stage is reachable from `entry` (orphans are errors, not warnings).
- At least one terminal stage is reachable from `entry` (the pipeline has an
  exit).
- Terminal stages declare no `prompt`.
- `memory.reads` ⊆ `{project, user, org}`, no duplicates.
- `budgets.fix_cycles` and `budgets.ask_to_edit` are ≥ 0.

Validation collects **all** problems in one pass and reports them together, so
an author sees every issue rather than fixing one at a time.

## Override document

A consumer customizes a pack by supplying an **override document**
(`overrides.yaml`) without forking the pack itself. Resolution composes a base
pack with modifications through four layers. Layers 1–2 select **which base**;
layers 3–4 mutate it.

```yaml
# overrides.yaml
base: my-pack@^1          # L1 lock major
# fork: true              # L2 detach (mutually exclusive with base tracking)
prompts:                  # L3 swap prompt files
  implement: my-implement.md
stages:                   # L4 patch stage params
  implement:
    gate: human_approval
    tier: strong
budgets:                  # L4 patch budgets
  fix_cycles: 5
```

| Layer | Field | Effect |
|---|---|---|
| 1 — lock major | `base: name@^MAJOR` | Resolve to the available version whose major matches `MAJOR`. `name@X.Y.Z` pins exact; bare `name` accepts any. |
| 2 — fork | `fork: true` | Detach from upstream — the resolved pack is a detached copy, tracked as forked metadata. |
| 3 — prompts | `prompts: {stage: file}` | Replace a stage's prompt text with the named file (path relative to the override dir; must not escape). Unknown or terminal stage is an error. |
| 4 — stages / budgets | `stages: {stage: {gate, tier}}`, `budgets: {fix_cycles, ask_to_edit}` | Patch params. Pointer semantics: a field you omit is left unchanged; a field you set (even to a zero) is applied. |

The **resolved pack is re-validated** after overrides apply — an override that
breaks the contract (invalid gate, empty swapped prompt, removing the only exit)
is rejected.

### Refs (layer 1)

| Ref | Accepts |
|---|---|
| `name` | any version |
| `name@^MAJOR` | available version with major == MAJOR |
| `name@X.Y.Z` | available version == X.Y.Z exactly |

At the dogfooding MVP, a source serves one version per pack, so `^MAJOR`
checks the single available version against the constraint. A multi-version
registry (true "latest within major" across many versions per name) is deferred.

## Programmatic use

```go
src := pack.NewDirSource("packs")

// Layer 1 — pick the base
base, err := src.Resolve(ctx, "my-pack@^1")

// Layers 2–4 — mutate (or pass nil overrides to run the base unchanged)
ov, err := pack.LoadOverrides("path/to/overrides")
resolved, err := pack.Resolve(base, ov)
```

`Resolve` does not mutate its input. The resolved pack carries `BaseRef` and
`Forked` fields for provenance.

## Examples

A minimal two-stage pack (`spec → implement → done`) ships at
[`packs/minimal/`](../packs/minimal/manifest.yaml) and is exercised by the
loader/validator/resolver tests under `internal/pack/`.
