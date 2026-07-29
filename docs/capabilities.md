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
| `fs.read` | Read files in the worktree | opencode `read` permission rule |
| `fs.write` | Write/edit source (scoped to the worktree) | opencode `edit` permission rule, scoped |
| `artifact.write` | Write the per-stage artifact dir (`result.json` + declared artifacts) | opencode `write` rule, scoped to the artifact dir |
| `exec.bash` | Run shell commands | opencode `bash` rule + deny patterns for delivery-ref mutation |
| `git.read` | Inspect git state (log, status, diff) | Routed through `exec.bash` deny-pattern gate |
| `git.write` | Mutate git inside the worktree (commit to the task branch) | Same as `exec.bash`; scoped to the worktree |
| `net.fetch` | Outbound HTTP (web research) | opencode `webfetch` permission rule + credential scrub |
| `secret.<name>` | Access a named credential | env scrub: the var is dropped unless the grant un-redacts it |
| `mcp.<server>` | Use a named MCP server | opencode `mcp` config lists only granted servers |

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

1. **A per-invocation opencode permission config** is written to
   `<worktree>/.opencode/opencode.json`. Every tool resolves to `allow` or
   `deny` based on the profile, with the role's scopes encoded as per-path /
   per-command rules. The bash rule map includes **deny patterns** for every
   command that would mutate a delivery ref (`git push`, `git reset --hard`,
   `git branch -D`, `git update-ref`, `git rebase`, `git checkout agentum/`,
   `git config credential`, `git config --global`). Because the agent runs with
   `--dir <worktree>`, this config is the authoritative one for the run.
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

## What is saved as audit evidence

Every invocation carries its effective profile as evidence in three places:

1. **`stage_invocations.capability_profile`** (migration `0007`) — the
   authoritative per-invocation record. NULL when the profile was empty.
2. **`stage.capability_enforced` event** — emitted before the adapter is
   invoked, carrying the profile (grants + role + source inputs) in the event
   payload.
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
| No undeclared credentials / network / commands / MCP / host paths | deny-by-default: secret/mcp/net absent from the role and ungranted by the invocation |
| Forbidden actions blocked + reflected in audit | opencode permission rules + bash deny patterns; profile + event + manifest columns record what was granted |
| Allowed actions keep working | granted tools resolve to `allow` in the generated config |
| Unattended invocation never starts with unrestricted host perms | the generated config is deny-by-default; an empty profile denies every tool |
| v1 limits + escape paths documented | this document |

## Escape paths (acknowledged, not addressed in v1)

These are the known ways a non-malicious agent *could* still reach beyond its
profile, and why v1 accepts them:

- **Bash filesystem access.** The bash tool can `cat` files outside the
  `fs.read` scope and write outside the `fs.write` scope. The permission rules
  cover opencode's `read`/`edit`/`write` tools, not arbitrary shell IO. v1
  accepts this: the worktree is the working boundary, and the credential scrub
  keeps anything valuable out of the env. A follow-up can run the agent under a
  stricter filesystem view.
- **Bash network access.** `curl` / `wget` are not blocked by `net.fetch`
  denial. v1 makes bash network useless by stripping credentials; a
  network-isolated runtime (network namespace, egress proxy) is a later
  concern.
- **Global MCP config.** opencode loads the operator's global MCP config in
  addition to the per-invocation one we write. v1 cannot disable a global
  server it does not know about; the per-invocation config is an allowlist of
  what the *pack* declared, and an undeclared global server is the documented
  gap. Operators running unattended agents should keep global MCP servers
  scoped to what every invocation may use.
- **Subprocesses spawned by the agent.** A bash command can spawn a long-lived
  helper that outlives the profile's timeouts. The process-group kill on cancel
  covers the common case; a privileged orphan is the residual gap.

When the single-owner assumption no longer holds (multi-user, public
deployment), v1's enforcement is insufficient and a real sandbox (container,
namespace, seccomp) becomes required.
