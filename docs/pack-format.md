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
checks:
  required: [build]          # mandatory for this pack; name must exist in .agentum.yaml
  optional: [lint]           # runs but does not block delivery
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
      #   condition: status == "blocked"   # a closed-grammar condition
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
| `pack.name` | string | Required. Matches the directory name when served by a `Source`. The name is one directory segment, cannot start with `.`, and cannot contain `/` or `\`. Invalid names are refused at resolution, so no runnable builtin pack can be omitted from the boot policy floor. |
| `pack.version` | semver | `MAJOR.MINOR.PATCH`, no leading zeros, no pre-release tags. Drives lock-major override. |
| `pack.persona` | string | Free-form metadata tag (e.g. `engineering`). The engine encodes no persona-specific behavior. |
| `pack.description` | string | Optional. |
| `memory.reads` | list | Subset of `{project, user, org}`. Only `project` is wired into retrieval at MVP. |
| `memory.writes` | bool | Whether the pack's agents may emit memory entries. |
| `capabilities` | list | Pack-wide MCP capability declarations. A stage narrows to the pack∩stage subset (enforcement comes later). |
| `budgets.fix_cycles` | int | Per-pipeline fix-loop cap, ≥ 0. Replaces a hardcoded cycle count. |
| `budgets.ask_to_edit` | int | Per-run plan-revision cap, ≥ 0: how many `ask-to-edit` (Request changes) requests a run accepts at its plan gate. `0` disables revision. Each accepted request re-runs the planner with the remarks; the revised plan needs its own approval. |
| `tiers.default` | string | Fallback tier name. Resolves through the BYO-models config to a model id and an optional variant (`docs/models.md`). |
| `checks` | object | Optional. Adds project checks to every run of this pack **by name only** — `required` makes a check mandatory (failure blocks delivery), `optional` runs it without blocking. Names must exist in the project registry (`.agentum.yaml`); `checks.Resolve` rejects unknown names at run time. A pack can never supply a command, remove a baseline check, or weaken an already-mandatory check. |
| `entry` | string | The stage the run starts at. Must be defined in `stages`. |
| `stages` | map | Named map of stage id → stage definition. See below. |
| `approvals` | list | Optional. Pack-level human-gate declarations that gate a capability unlock. See [Approvals](#approvals). |

### Stages

Stages are a **named map**, not an ordered list. Each stage declares its own
outgoing transitions, and the pack declares one `entry`. A transition may carry
a `condition` in the closed grammar below; a stage with several transitions
fans out by condition with first-match-wins ordering.

```yaml
stages:
  <id>:
    gate: <gate-value>
    prompt: <path>
    tier: <name>           # optional
    role: <role-name>      # optional; selects the capability-profile template
    capabilities: [...]    # optional; narrows the pack's capabilities for this stage
    transitions:
      - to: <stage-id>
      - to: <stage-id>
        condition: <condition-term>
```

| Field | Required | Notes |
|---|---|---|
| `gate` | non-terminal stages | One of the [gate values](#gate-values). Ignored on terminal stages. |
| `prompt` | non-terminal stages | Path relative to the pack dir; must not escape it. Absent on terminal stages. |
| `tier` | optional | Overrides `tiers.default` for this stage. |
| `role` | optional | Capability-profile selector: `analyst` \| `reviewer` \| `implementer` \| `fixer`. Absent → derived from the stage id by convention (spec/analyze/design → analyst; review → reviewer; implement → implementer; fix → fixer; default analyst). See [docs/capabilities.md](capabilities.md). |
| `capabilities` | optional | Stage-level subset that narrows the pack's `capabilities` for this stage. Absent → inherit the pack set; present → only the listed categories. Entries are scope-less categories (`fs.read`, `fs.write`, `git.read`, `git.write`, `exec.bash`, `net.fetch`) or named entities (`secret.<name>`, `mcp.<server>`). |
| `transitions` | optional | Outgoing edges. Absent ⇒ terminal. Each may carry a `condition`. |

A **terminal stage** (no `transitions`) is an engine state, not an agent
invocation: it has no prompt and no gate. It is the pipeline's exit.

### Approvals

The optional top-level `approvals:` list declares a human gate whose decision
unlocks a capability for the rest of the run. A pack with no `approvals` block
behaves exactly as before — every stage runs under its role profile from the
first run.

```yaml
approvals:
  - name: plan            # unique, non-empty
    stage: plan           # a defined non-terminal stage
    artifact: plan.md     # bare file name (no path separators); lives in the stage's artifact dir
    unlocks: source_write   # closed set; v1 has one member
```

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Unique within the list, non-empty. Recorded on the `run_approvals` row as the decision's name. |
| `stage` | yes | A defined non-terminal stage. The approval is recorded when the run advances past this stage's gate. |
| `artifact` | yes | A **bare file name** — no path separators. It lives in the stage's artifact dir and is captured as an immutable revision the human may edit via `PUT` before approving. |
| `unlocks` | yes | Closed set of unlock names. v1 has exactly one member: `source_write`. |
| `within_stage` | no | Set `true` for a short plan written by the implementer before source edits. The approval resumes the same stage and its captured session. |

**`source_write` unlock.** While a `source_write` approval is pending (the run
has not yet advanced past the approval stage), the runner withholds
`SourceWriteCategories` from every stage's effective profile, so no stage can
write source until a human approves the named artifact. The approval binds to
the artifact revision the human saw, and a later edit to that artifact is
detected as drift. See [docs/capabilities.md](capabilities.md) and
[docs/execution.md](execution.md) § "Plan-approval lock".

**`auto_if_clean` is the wrong gate for a source-writing stage.** A stage that
writes code under `auto_if_clean` pauses on every successful run, because any
porcelain entry the agent's write produced counts as dirty and the gate holds
for review. Use `auto` for implement/fix stages; the plan-approval lock is what
gates source writes, not the cleanliness signal.

Validation (collected with the rest in one pass):

- `name` values are unique and non-empty.
- `stage` references a defined, **non-terminal** stage.
- `artifact` is a bare file name (no path separators).
- every `unlocks` entry is a known name (`source_write` in v1).
- when a `source_write` approval exists, every source-writing stage (effective
  role `implementer` or `fixer`) is reachable from `entry` only through the
  approval stage. With `within_stage: true`, the approval stage itself may be
  the implementer. Its first invocation runs with source-write capabilities
  withheld, then pauses for approval of its plan artifact. This reachability check is **static and advisory** — it flags
  a pack that could bypass the gate by graph shape. The real guarantee is the
  runtime capability withholding (layer 1), which holds regardless of what the
  static check concludes.

The shipped [`packs/backend-development`](../packs/backend-development/manifest.yaml)
pack is the worked example: `plan → implement → review → (fix → review)* → done`,
with `approvals: [{name: plan, stage: plan, artifact: plan.md, unlocks: source_write}]`.

The shipped [`packs/small-change`](../packs/small-change/manifest.yaml) uses
`within_stage: true` on `implement`. The first invocation writes `plan.md`
without source-write capabilities. Approval requires a current plan revision.
The second invocation resumes the same stage with source-write capabilities.
The shipped [`packs/research-first`](../packs/research-first/manifest.yaml)
adds a read-only `research` stage before the plan gate.

### Transition conditions

A condition is exactly one term in a closed two-token grammar — no boolean
operators, no negation, no commands or scripts. The first matching transition
in declaration order wins; that supplies disjunction and precedence. An empty
`condition` (or no field) is the unconditional fallback edge.

```
condition := enum_term | count_term
enum_term  := ("verdict" | "status") "==" '"' literal '"'
count_term := "fix_cycles" ("<" | "<=" | ">" | ">=" | "==") (integer | "budget")
```

| Subject | Source | Allowed literals |
|---|---|---|
| `verdict` | the reviewer's own `verdict.json`, parsed by the orchestrator (never the agent's prose) | `approved`, `changes_requested` |
| `status` | `result.json.status` of the stage that is transitioning | `complete`, `partial`, `blocked` |
| `fix_cycles` | the durable counter of fixer-stage entries in the current run | a non-negative integer, or the keyword `budget` (resolves to `budgets.fix_cycles`) |

- `verdict` is read by the orchestrator, not the agent: `result.json.summary`
  and stream text cannot move the pipeline past a verdict-conditioned stage.
- A reviewer stage that sources a `verdict` condition writes `verdict.json`
  next to its `result.json` (see [docs/agent-contract.md](agent-contract.md)).
- `fix_cycles` conditions can never establish totality (a stage using them
  requires an unconditional fallback).

### The fix budget

`budgets.fix_cycles: N` ⇒ the run may enter a fixer-role stage at most `N`
times. The `N+1`-th entry is refused with a controlled stop
(`fix_budget_exhausted` → `paused_user_stop`); the branch, checkpoint commits,
artifact revisions, and the unsealed manifest all stay exactly as they are.

The budget binds **entries into fixer-role stages**, not back-edges in general:
bounding the fixer entry stops immediately after a review (so the tree at the
stop point is always a reviewed tree), and with `fix_cycles: 2` the fixer runs
at most twice and the reviewer at most three times. A pack may declare a budget
without declaring a cycle.

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
- Every non-empty `transitions[*].condition` parses under the closed grammar
  with a known subject and a literal from that subject's closed set.
- A stage has at most one unconditional transition, and it must be **last** (a
  fallback declared before a conditional edge would make that edge dead).
- A stage with conditional transitions is **total**: it ends with an
  unconditional fallback, OR its conditions exhaustively cover one closed enum
  subject (every member of `verdict` / `status` appears with `==`). A stage
  using `fix_cycles` conditions requires the fallback.
- Every non-trivial strongly connected component reachable from `entry` contains
  at least one fixer-role stage **and** `budgets.fix_cycles >= 1`. Per-component
  (a pack with one bounded loop and one unbounded loop is rejected). A budget
  without a cycle stays valid.
- Every stage is reachable from `entry` (orphans are errors, not warnings).
- At least one terminal stage is reachable from `entry` (the pipeline has an
  exit).
- Terminal stages declare no `prompt`.
- `approvals[*]` satisfies the [Approvals](#approvals) rules (unique non-empty
  names; defined non-terminal `stage`; bare-file-name `artifact`; known
  `unlocks`; and when a `source_write` approval exists, every source-writing
  stage is reachable from `entry` only through the approval stage).
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

## Project packs

A project supplies its own packs inside its repository, under
`.agentum/packs/<name>/`. Agentum reads them from the run's pinned
`base_commit` — the same agent-immutability seam as `.agentum.yaml` and the
instruction files — so an agent cannot change the pack its own run executes,
and a pack committed later never affects a run started from an earlier commit.

A pack directory carries **exactly one** of:

- `manifest.yaml` — a full pack of the project's own. When the name matches a
  builtin, the project pack replaces that builtin in this project. The
  replacement also covers other packs' `base` lookups: a name the project
  ships is not a base. `pack.name` must equal the directory name, and the run
  ref's version constraint applies to the manifest's version.
- `overrides.yaml` — **inheritance** over a builtin base (layers 3–4 of the
  override document above). The graph — stages, transitions, approvals —
  comes from the base and cannot be patched; a different graph needs a
  `manifest.yaml`. `fork` is refused here: a project pack is authored in the
  project already.

The directory's name is the pack's identity. An heir may carry any name:
`.agentum/packs/my-flow/` with `base: backend-development@^0` is a new pack
`my-flow`. Its `version` and `persona` come from the base, and the run ref's
version constraint applies to the base's version. `base` always resolves
against the **builtin** source. Chains like project-inherits-project are not
supported. A `base` naming a pack the project itself ships — a name under
`.agentum/packs/` other than the heir's own directory — is a configuration
error: a shadowed name would make "which graph is inherited" depend on
resolution order.

### Reading rules

- Every entry in the pack tree at the commit must be a regular file: a symlink
  (git mode `120000`) is a reference outside the pack's control and is
  rejected before any content is read.
- Prompt paths resolve only inside the pack directory (`..` escapes are
  errors, in manifests and overrides alike).
- Size limits, enforced from the tree listing before reading: 64 KiB per
  file, 256 KiB total, 128 files.

### Strict decoding

Both `manifest.yaml` and `overrides.yaml` are decoded with unknown fields
rejected. A `checks:` or `approvals:` block in an overrides document, or a
`transitions` list inside a stage patch, is an error — not a silently ignored
key. For overrides this matters beyond hygiene: a dropped `checks:` block is
indistinguishable from an accepted weakening attempt.

### Distinguishable errors

Every failure to read a project pack wraps one of the sentinel errors in
`internal/pack/project.go`, distinguishable with `errors.Is`:
`ErrBothPackFiles` (both documents at once), `ErrNoPackFile` (neither),
`ErrPromptEscapesDir`, `ErrPackSymlink`, `ErrPackFileTooLarge`,
`ErrPackTreeTooLarge`, `ErrBaseNotBuiltin` (base names a project pack —
including a builtin name this project shadows), `ErrBaseBuiltinNotFound`,
`ErrForkNotAllowed`, `ErrPackNameMismatch`.

### Origin

Resolution reports where the assembled pack's bytes came from — `builtin`,
`project` (a replacement or new-name manifest), or `project+builtin`
(inheritance) — and, for inherited packs, which fields the project layer set
(`stages.<id>.prompt` / `.gate` / `.tier`, `budgets.*`), built from what the
overrides document declares. API responses and run evidence surface both.

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
