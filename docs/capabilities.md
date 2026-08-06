# Capability profiles (v1)

Every Agentum invocation runs under a **code-enforced capability profile**. The
profile is computed, persisted, and applied as a real boundary — not as a line
in the prompt. This document is the reference for what v1 enforces, how, and
what it deliberately does **not** protect against.

## Scope of v1

v1 protects against **accidental** agent actions in a single-owner, local
runtime: an analytical stage that accidentally edits source, an implementer that
wanders outside its worktree, an agent that reads a credential it was never
granted. It is **not** a security sandbox. It does not defend against:

- A **malicious** agent process that actively tries to escape (a bash tool can
  still read files and open sockets; see [Escape paths](#escape-paths)).
- **Kernel or container escape.** Agentum runs as the operator's own user; it
  does not drop privileges or sandbox syscalls.
- A **compromised adapter** or **hostile pack**. The adapter enforces the
  profile; a tampered adapter can be made to lie. Packs declare capabilities
  the operator already trusts (same trust as the prompts they carry).
- **Prompt injection.** A profile limits what the runtime lets the agent do, not
  what the agent is tricked into *wanting* to do.
- **Multi-user or public** deployments. The single-owner assumption is what
  makes the env-scrub + permission-config approach sufficient.

The operator accepts these limits by running Agentum locally against repos they
control. The value is that a stage's role becomes a checked boundary, not a
convention an agent can drift from mid-run.

## The model

A **capability** is one of:

| Category | Meaning | Enforcement (opencode) |
|---|---|---|
| `fs.read` | Read files in the worktree | opencode `read` / `glob` / `grep` / `list` / `lsp` rules, bounded by `external_directory: deny` |
| `fs.write` | Write/edit source (scoped to the worktree) | opencode `edit` permission rule, scoped |
| `artifact.write` | Write the per-stage artifact dir (`result.json` + declared artifacts) | opencode `edit` rule, scoped to the artifact dir — opencode has **no** separate `write` permission; write and patch are both governed by `edit` |
| `exec.bash` | Run shell commands | opencode `bash` rule + deny patterns for delivery-ref mutation |
| `git.read` | Inspect git state (log, status, diff) | Routed through `exec.bash` deny-pattern gate |
| `git.write` | Mutate git inside the worktree (commit to the task branch) | Same as `exec.bash`; scoped to the worktree |
| `net.fetch` | Outbound HTTP (web research) | opencode `webfetch` + `websearch` rules; without the grant the common network clients are also denied in `bash` |
| `secret.<name>` | Access a named credential | env scrub: the var is dropped unless the grant un-redacts it |

`mcp.<server>` is **not enforceable in v1.** opencode addresses MCP tools by
per-tool permission names this adapter cannot enumerate for an arbitrary
server, so it cannot express "this server and nothing else". Rather than emit a
config that looks like enforcement and is not, `mcp` is absent from
`adapter.Supported()`: a profile carrying an `mcp.*` grant is unenforceable and
the invocation refuses to start.

Two capabilities are **never granted to an agent**:

- `git.delivery` — operating on `agentum/<task-id>` branch tips and checkpoint
  SHAs. The orchestrator owns these (the worktree manager applies them
  directly); no agent role includes `git.delivery`, and deny-by-default keeps it
  out of every profile regardless of what a pack declares.
- Resource limits (`time.hard`, `time.idle`) are **constraints**, not
  permissions. They ride on the profile as `HardTimeout` / `IdleTimeout` fields.

## How an effective profile is computed

```
effective = host ∩ pack ∩ stage ∩ (role ∪ invocation-grant)
```

| Input | Source | Role in the intersection |
|---|---|---|
| **host** | `adapter.Supported()` | The categories the runtime can technically enforce. A category absent here is dropped — Agentum will not grant a capability it cannot enforce. |
| **pack** | `capabilities:` in `manifest.yaml` | The pack-wide ceiling. Scope-less categories (e.g. `fs.write`). |
| **stage** | `stage.capabilities:` (optional) | Per-stage narrowing. Absent → inherit the pack set. |
| **role** | `stage.role` or derived from the stage id | The scoped baseline template (the only input that carries path/command scopes). |
| **invocation** | per-run grant (resume, explicit add) | Widens the role; still must survive pack ∩ stage ∩ host. |

**Deny by default.** A capability absent from any input is absent from the
result. An empty input set yields an empty profile — the agent may only write
its structured `result.json`.

### Role templates

| Role | Granted baseline | Typical stages |
|---|---|---|
| `analyst` | `fs.read`, `artifact.write`, `git.read` | spec, analyze, design |
| `reviewer` | same as analyst (no source writes, no delivery-ref access) | review |
| `implementer` | `fs.read`, `fs.write:${worktree}/**`, `artifact.write`, `git.read`, `git.write:${worktree}`, `exec.bash` | implement, build |
| `fixer` | same as implementer | fix, patch |
| `system` | `git.delivery`, `git.write`, `fs.write:${artifact-root}/**` | **orchestrator-internal only — never assigned to an agent stage** |

A pack assigns a role with the optional `stage.role` field. When omitted, the
runner derives one from the stage id (`review`→reviewer, `implement`→
implementer, `fix`→fixer, default analyst). `secret.*`, `mcp.*`, and `net.fetch`
are **never** in any role template — they are granted only by an explicit
per-invocation grant that survives the full intersection.

## Enforcement (opencode adapter)

Before the opencode subprocess starts, the adapter materializes the effective
profile into four concrete controls:

1. **A per-invocation opencode permission config.** Five properties make it a
   boundary rather than a suggestion. Each was verified against a real opencode
   binary, because each was wrong at least once when derived from the docs
   alone:

   - **Path scopes are relative to the project root.** opencode normalises a
     tool's target path to a project-root-relative form *before* matching, so an
     absolute pattern never matches anything. The failure mode is silent: the
     deny baseline refuses every write while the config still reads correctly,
     and an analyst simply cannot produce `result.json`. An implementer's
     `fs.write:${worktree}/**` becomes `**`; an analyst's artifact scope becomes
     `.agentum/<task>/.ag-artifacts/<stage>/**`. A scope that cannot be
     expressed relative to the worktree is an error, not a dropped grant — the
     invocation refuses to start. The absolute paths remain in the *audit*
     profile, which is evidence rather than configuration.
   - **Deny by default.** The config opens with the `"*": "deny"` catch-all,
     which overrides opencode's own built-in `{permission:"*", pattern:"*",
     action:"allow"}`, so a tool this adapter does not model — a future opencode
     tool, an MCP tool — is refused rather than inherited. Every documented
     permission key is then set explicitly: opencode *merges* config sources and
     overrides only conflicting keys, so any key left unset would fall through
     to the operator's global config or the repository's own `opencode.json`.
   - **Delivered where the repository cannot override it.** The config is
     rendered to a per-invocation temp directory outside the worktree (passed as
     `OPENCODE_CONFIG` for the audit trail) *and* inlined into
     `OPENCODE_CONFIG_CONTENT`. The inline copy is what enforces: opencode loads
     `OPENCODE_CONFIG` **below** a project's own `opencode.json`, so a repo
     shipping its own permissions would otherwise win. Keeping the file out of
     the worktree also means the agent it constrains cannot edit it, and that it
     never shows up in `git status` — which drives the `auto_if_clean` gate and
     would otherwise leak into the delivery diff.
   - **Rule order is explicit.** opencode resolves permission rules with the
     **last match winning**, so every scoped rule list emits its `deny` baseline
     first and the granted scopes after. The rendering uses an ordered list, not
     a Go map, because `encoding/json` sorts map keys.
   - **Bash denies are patterns, not substrings.** opencode matches bash rules
     as wildcard patterns against the parsed command, so every delivery-ref deny
     ends in `*` (`git push*`, `git reset --hard*`, `git branch -D*`,
     `git update-ref*`, `git rebase*`, `git worktree*`, `git checkout agentum/*`,
     `git config credential*`, `git config --global*`). A bare `git push` would
     match only the argument-less command and let `git push origin HEAD`
     through. Without `net.fetch`, the common network clients (`curl*`, `wget*`,
     `ssh*`, `scp*`, `nc*`) are denied too, so `bash: allow` does not hand back
     the network the `webfetch` deny just took away.
2. **Credential-scrubbed environment.** The child process receives an
   environment built from the parent minus a deny list of credential-bearing
   variables (`AWS_*`, `GITHUB_*`, `ANTHROPIC_*`, `*_TOKEN`, `*_API_KEY`,
   `GIT_ASKPASS`, SSH agent vars, …). A variable is un-redacted **only** when
   the profile grants the matching `secret.<name>` (e.g.
   `secret.github_token` un-redacts `GITHUB_TOKEN`).
3. **Hard timeout** wraps the invocation's context with a deadline
   (`AGENTUM_HARD_TIMEOUT_SECONDS`).
4. **Idle timeout** watches the stream; no chunk for
   `AGENTUM_IDLE_TIMEOUT_SECONDS` cancels the run.

The adapter **confirms enforceability before start**: if the profile grants a
category the adapter cannot technically enforce, `Invoke` returns an error and
the subprocess never spawns. The runner records
`stop_reason=capability_unenforceable` on the invocation row.

### Provider credentials

The credential scrub removes `ANTHROPIC_*`, `OPENAI_*`, and friends from the
agent's environment — including the key the model provider needs. That is
deliberate, and it has an operating consequence:

> **opencode must be authenticated through its own credential store
> (`opencode auth login`), not through provider environment variables.**

An install relying on `ANTHROPIC_API_KEY` in the shell will see every
invocation fail to reach a model — and it fails *silently*: an unauthenticated
`opencode run` blocks indefinitely without writing a byte to stdout or stderr.
The profile's idle cap is what turns that into a named failure instead of a
hung stage.

Widening this is a deliberate decision, not an oversight to patch quietly: the
same process runs the model client and the agent's bash tool, so a key the
runtime can read is a key the agent can `echo`.

### Proving it

Two test layers keep the claims above honest:

- **Subprocess contract tests** (`internal/agent/opencode_lifetime_test.go`) run
  in normal CI. They re-exec the test binary as a fake agent and pin the
  properties no unit test over rendered structs can reach: the run outlives
  `Invoke`, the hard timeout and idle cap terminate the process tree, caller
  cancellation terminates it, and the per-invocation config directory does not
  leak. `TestPermissionScope_*` additionally pins the pattern-generation rule
  against a port of opencode's matcher — deterministic, no model in the loop.
- **The enforcement contract** (`TestOpencodeLive_*`, build tag `integration`)
  runs against a real, pinned opencode: config discovery, an analyst writing its
  own artifact, an analyst refused a source edit, and an implementer permitted
  one — each asserted on the bytes on disk, not on what the agent reports. It is
  the opt-in `enforcement-contract` CI job, and it is the only evidence that the
  config this adapter writes is actually obeyed. **Run it whenever the config
  rendering or the profile model changes, and record the version that passed.**

| Verified against opencode | Date | Result |
|---|---|---|
| 1.18.10 | 2026-08-03 | All four live contracts pass (stream/resume, artifact allow, source deny, implementer allow) |

## What is saved as audit evidence

Every invocation carries its effective profile as evidence in three places:

1. **`stage_invocations.capability_profile`** (migration `0007`) — the
   authoritative per-invocation record. NULL when the profile was empty.
2. **`stage.capability_enforced` event** — carrying the profile (grants +
   role + source inputs) in the event payload. Note it is emitted *before* the
   adapter is invoked; separating requested from enforced is tracked as a
   follow-up.
3. **Manifest `body.capabilities.effective`** — a per-stage snapshot merged
   into the evidence manifest, so a cross-run diff (`manifest/diff`) surfaces a
   changed capability set as an input-level difference.

## Acceptance criteria — what is enforced

| Criterion | How v1 enforces it |
|---|---|
| Invocation without a permitted+supported profile does not start | adapter's `EnforceableBy` check returns an error; runner records `capability_unenforceable` |
| Analytical stage cannot change source | analyst role omits `fs.write`; intersection drops it |
| Reviewer cannot change tracked source or delivery refs | reviewer role = analyst set; `git.delivery` never in an agent role |
| Implementer works only in its worktree | `fs.write` and `git.write` carry the `${worktree}` scope |
| No undeclared credentials / network / commands / MCP / host paths | deny-by-default: the `"*": "deny"` baseline plus explicit `external_directory` / `task` / `websearch` denials; `mcp.*` is refused outright. `skill` is the exception: allowed (knowledge, not reach) and recorded via the `ContextProber` — see the project context channel below |
| Forbidden actions blocked + reflected in audit | opencode permission rules + bash deny patterns; profile + event + manifest columns record what was granted |
| Allowed actions keep working | granted tools resolve to `allow` in the generated config, proven by the live enforcement contract |
| Unattended invocation never starts with unrestricted host perms | the generated config is deny-by-default; an empty profile denies every tool |
| v1 limits + escape paths documented | this document |

## Escape paths (acknowledged, not addressed in v1)

These are the known ways a non-malicious agent *could* still reach beyond its
profile, and why v1 accepts them:

- **Bash filesystem access.** The bash tool can `cat` files outside the
  `fs.read` scope and write outside the `fs.write` scope. The permission rules
  cover opencode's own tools, not arbitrary shell IO. v1 accepts this: the
  worktree is the working boundary, and the credential scrub keeps anything
  valuable out of the env. A follow-up can run the agent under a stricter
  filesystem view.
- **Bash network access.** The deny patterns cover the common clients
  (`curl`, `wget`, `ssh`, `scp`, `nc`) when `net.fetch` is not granted, but they
  are name patterns: a script, an interpreter one-liner, or a differently-named
  client still reaches the network. A network-isolated runtime (namespace,
  egress proxy) is the real answer and is a later concern.
- **Managed / MDM config.** opencode loads system-managed configuration *above*
  `OPENCODE_CONFIG_CONTENT`. On a machine with managed opencode settings the
  operator's policy outranks the profile. That is the correct precedence for a
  managed fleet and it is the operator's own decision — but it is not a boundary
  Agentum controls.
- **Contract adherence is not enforced.** A capability profile controls what the
  agent *may* do, not what it *does*. Observed with a real model: an agent
  refused a source edit then ended the run without writing `result.json` at all,
  and another wrote its artifact under a name of its own choosing. The
  orchestrator sees both as "the agent did not produce the required contract
  file". Making a denial surface as `status: blocked` rather than an
  indistinguishable failure belongs to the pack prompts and the runner's outcome
  classification, not to the permission config.
- **Intermittent silent stalls.** `opencode run` occasionally produces nothing
  on either stream and never exits — reproduced with an unauthenticated install,
  and again on a prompt combining a denied action with an allowed one. The
  profile's idle cap is the only thing that ends such a run.
- **Subprocesses spawned by the agent.** A bash command can spawn a long-lived
  helper that outlives the profile's timeouts. The process-group kill on cancel
  covers the common case; a privileged orphan is the residual gap.
- **Instruction files outside the declared set.** The project-context channel
  (ADR 0002) pins `AGENTS.md` and the `instructions:` list declared in
  `.agentum.yaml`. A nested `sub/AGENTS.md` the project never declared, or the
  operator's global `~/.config/opencode/AGENTS.md`, can still reach the model
  through opencode's own injection — these are unpinned and are named here
  rather than silently ignored. The `edit` deny rule guards `AGENTS.md` and
  `**/AGENTS.md`; a nested file under a different name is out of scope.
- **Bash writes between the pre-stage hash check and the run.** The instruction
  restore (`internal/runner` `restoreInstructions`) runs strictly before each
  stage invocation. A bash write that lands after that check but before the
  invocation starts is not caught in real time — it is caught by the *next*
  stage's restore, recorded as a restoration, and reversed in the next
  checkpoint commit. The tamper and its reversal are both in the git lineage.

When the single-owner assumption no longer holds (multi-user, public
deployment), v1's enforcement is insufficient and a real sandbox (container,
namespace, seccomp) becomes required.

## Project context channel (ADR 0002)

The repository's own instruction files and the runtime's available skills form
a declared input channel, so a pack can be stack-neutral without leaving the
agent without project rules.

- **Instruction set.** `instructions:` in `.agentum.yaml` (repo-relative paths,
  validated) ∪ the runtime-injected `AGENTS.md` baseline. No discovery, no
  globbing — a closed, declared set. Bytes are pinned from `base_commit` (the
  agent-immutability seam), capped at 64 KiB/file and 192 KiB/set with a
  recorded truncation marker, and delivered to the adapter through the same
  config channel as the permission map. The pin's `SourceHash` is over the
  original base_commit bytes; `DeliveredHash` is over the post-truncate bytes,
  so truncation is an attributable fact.
- **Tampering, two layers.** The `edit` deny rule stops an agent rewriting
  `AGENTS.md` (or any declared path) via the edit tool; a pre-stage hash check
  (CRLF-normalised, so a `core.autocrlf=true` checkout is not a false positive)
  stops a `bash`-side rewrite and restores the pinned bytes. Each restoration
  emits `task.instructions_restored` and lands in the manifest context section;
  the rewrite is orchestrator-authored, so the next checkpoint commit shows it
  as a revert.
- **Skills, allowed and recorded.** `skill` resolves to `allow` — a skill grants
  knowledge, not reach; it still meets the same `bash`/`edit`/`net` rules.
  `opencode debug skill` is probed once per run (after the worktree exists),
  with its own 10s timeout and process-group kill, and each skill's name,
  location, and content hash land in the manifest (bodies are never stored).
  A failed probe records an `EvidenceGap("context.skills")`, making
  `evidence_complete` false; an adapter with no prober records `unsupported`
  (a permanent capability gap, not a degraded run). `skill.<name>` joins the
  capability vocabulary as an inert, unenforceable seam — like `mcp.<server>`
  — so narrowing becomes a config change later, not a model redesign.
- **Resolved checks rendered.** The routing block lists the resolved project
  checks (name, command, required) so the agent knows the build/test commands
  and can run them to check its own work. The agent learns *what* the checks
  are; it still cannot change *which* checks gate delivery (`enforceProjectChecks`
  re-resolves independently at the boundary, bound to the verified commit).

| Verified against opencode | What was proven |
|---|---|
| 1.18.11 (2026-08-05 / 2026-08-06) | `AGENTS.md` is auto-injected with zero tool calls; editing it in the worktree changes the next invocation; absolute `instructions` paths resolve (forward slashes); `opencode debug skill` returns `{name, description, location, content}` with no `--dir` and one `<built-in>` entry on a clean machine; `permission.skill` accepts the flat `"allow"` and a per-pattern map. |
| 1.18.11 — instruction pinning + skill enumeration | Step 0 addendum discharged (see ADR 0002 addendum). The live enforcement contract (step 10) — `AGENTS.md` marker reaches the model with zero tool calls; the pinned copy reaches the model; an `edit` of `AGENTS.md` is refused; a `~/.claude/skills/` skill is available and visible; a skill that instructs a denied write does not get the write — is pending manual execution against the pinned binary. |
