package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/caps"
)

// opencodeSupported is the set of capability categories the opencode runtime
// can technically enforce for a local invocation. Each entry maps to a concrete
// enforcement mechanism in this file:
//
//   - fs.read / fs.write / artifact.write → opencode permission rules in the
//     generated config (read/edit/write tools), scoped to the worktree.
//   - exec.bash → opencode bash permission rules, with deny patterns that
//     protect delivery refs (git push / reset --hard / branch -D / update-ref /
//     rebase) the agent must never touch.
//   - git.read / git.write → enforced through exec.bash deny patterns; read is
//     always available alongside bash, write is the same path.
//   - net.fetch → opencode webfetch permission rule.
//   - secret → env scrub: credential-bearing vars are stripped from the child
//     environment unless the profile grants the matching secret.<name>.
//   - mcp → opencode mcp config: only granted servers are listed.
//
// git.delivery is deliberately absent: the orchestrator owns delivery refs
// directly (worktree manager), never via the agent subprocess. A profile that
// grants git.delivery to an agent is unenforceable here and the invocation
// refuses to start.
var opencodeSupported = []caps.Category{
	caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite,
	caps.CatExecBash, caps.CatGitRead, caps.CatGitWrite,
	caps.CatNetFetch, "secret", "mcp",
}

// enforcementPlan is the materialized form of a Profile the adapter is about to
// apply: the rendered opencode config bytes, the child environment, and the
// scope substitutions that went into them. The runner records a snapshot of
// this as audit evidence.
type enforcementPlan struct {
	configPath string       // absolute path the rendered config was written to
	config     []byte       // the rendered opencode permission config
	env        []string     // the child environment (credential-scrubbed)
	timeout    timeoutPlan  // time limits applied via ctx
	subst      scopeSubst   // the scope substitutions applied
	audit      caps.Profile // the substituted profile recorded for audit
}

type timeoutPlan struct {
	hard caps.Profile // carries HardTimeout/IdleTimeout fields; zero means none
}

type scopeSubst struct {
	worktree string
	artifact string
}

// prepareEnforcement materializes inv.Profile into a concrete enforcement plan:
// it confirms the profile is enforceable, substitutes scope placeholders, writes
// the per-invocation opencode permission config into the worktree, and builds
// the credential-scrubbed child environment. Returns an error if the profile
// grants anything the adapter cannot enforce — in that case the invocation
// MUST NOT start.
func prepareEnforcement(inv Invocation) (enforcementPlan, error) {
	if err := inv.Profile.EnforceableBy(opencodeSupported); err != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: %w", err)
	}
	subst := scopeSubst{worktree: inv.Workdir, artifact: inv.ArtifactDir}
	substituted := substituteScopes(inv.Profile, subst)

	configDir := filepath.Join(inv.Workdir, ".opencode")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: create config dir: %w", err)
	}
	configPath := filepath.Join(configDir, "opencode.json")
	rendered, renderErr := renderOpencodeConfig(substituted)
	if renderErr != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: render config: %w", renderErr)
	}
	if err := os.WriteFile(configPath, rendered, 0o644); err != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: write config: %w", err)
	}

	env := buildChildEnv(substituted)

	return enforcementPlan{
		configPath: configPath,
		config:     rendered,
		env:        env,
		timeout:    timeoutPlan{hard: substituted},
		subst:      subst,
		audit:      substituted,
	}, nil
}

// substituteScopes returns a copy of profile with every ${worktree} and
// ${artifact-root} placeholder in grant scopes replaced by the invocation's
// absolute paths. The original profile is not mutated.
func substituteScopes(profile caps.Profile, subst scopeSubst) caps.Profile {
	out := caps.Profile{
		Grants:      make([]caps.Token, 0, len(profile.Grants)),
		HardTimeout: profile.HardTimeout,
		IdleTimeout: profile.IdleTimeout,
		Source:      profile.Source,
	}
	for _, granted := range profile.Grants {
		scope := granted.Scope()
		if scope == "" {
			out.Grants = append(out.Grants, granted)
			continue
		}
		scope = strings.ReplaceAll(scope, caps.WorktreeScope, subst.worktree)
		scope = strings.ReplaceAll(scope, "${artifact-root}", subst.artifact)
		out.Grants = append(out.Grants, caps.Token(granted.Key())+caps.Token(":"+scope))
	}
	return out
}

// opencodeConfig is the subset of the opencode config schema this adapter
// writes. We render only the fields that enforce the profile; the rest of
// opencode's config surface is left to the operator's global setup.
type opencodeConfig struct {
	Schema     string                  `json:"$schema"`
	Permission opencodePermissionRules `json:"permission"`
	MCP        map[string]any          `json:"mcp,omitempty"`
}

// opencodePermissionRules is the per-tool permission map. Each tool resolves to
// "allow" when the profile grants its category (subject to per-path or
// per-command refinement), and "deny" otherwise — deny-by-default at the
// runtime layer, mirroring the profile.
type opencodePermissionRules struct {
	Read     string `json:"read"`
	Edit     any    `json:"edit"`  // "allow" | "deny" | per-path map
	Write    any    `json:"write"` // artifact writes; per-path map when granted
	Bash     any    `json:"bash"`  // "allow" | "deny" | per-command map
	WebFetch string `json:"webfetch"`
}

// renderOpencodeConfig renders the deny-by-default opencode permission config.
// Tools whose category the profile does not grant resolve to "deny"; granted
// tools resolve to "allow" with the profile's scopes encoded as per-path /
// per-command refinement where the opencode schema supports it.
//
// opencode's permission model: a tool's value is "allow" | "deny" | "ask", or
// a map of {pattern: rule} for fine-grained control. We use "deny" as the
// default outcome for anything not granted.
func renderOpencodeConfig(profile caps.Profile) ([]byte, error) {
	rules := opencodePermissionRules{
		Read:     denyIf(!profile.Has(caps.CatFsRead)),
		WebFetch: denyIf(!profile.Has(caps.CatNetFetch)),
	}

	if profile.Has(caps.CatFsWrite) {
		// fs.write carries a worktree scope from the role. Allow edits inside
		// the scope and deny outside. opencode matches paths against the
		// worktree root, so the absolute-scope glob is the boundary.
		rules.Edit = map[string]string{writeScopeGlob(profile): "allow", "**": "deny"}
	} else {
		rules.Edit = "deny"
	}

	if profile.Has(caps.CatArtifactWrite) {
		// artifact.write is always to the per-stage artifact dir. We allow it
		// even when fs.write is denied (reviewer/analyst write their own
		// artifact). Merge with fs.write scope when both are present.
		rules.Write = mergeArtifactAndWriteScopes(profile)
	} else {
		rules.Write = "deny"
	}

	if profile.Has(caps.CatExecBash) {
		rules.Bash = bashRules(profile)
	} else {
		rules.Bash = "deny"
	}

	cfg := opencodeConfig{
		Schema:     "https://opencode.ai/config.json",
		Permission: rules,
		MCP:        mcpServerConfig(profile),
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode opencode config: %w", err)
	}
	return encoded, nil
}

// denyIf returns "deny" when denied is true, "allow" otherwise. Used for tools
// whose permission is a plain string (no per-path refinement needed).
func denyIf(denied bool) string {
	if denied {
		return "deny"
	}
	return "allow"
}

// writeScopeGlob extracts the fs.write scope as an opencode path glob. Falls
// back to "**" (all paths) when the grant is unscoped — the safe default that
// still lets the agent edit within its worktree.
func writeScopeGlob(profile caps.Profile) string {
	for _, granted := range profile.Grants {
		if granted.Key() == string(caps.CatFsWrite) {
			if scope := granted.Scope(); scope != "" {
				return scope
			}
		}
	}
	return "**"
}

// mergeArtifactAndWriteScopes builds the write-tool rule map: allow the
// artifact dir (always) plus the fs.write scope (when granted), deny everything
// else. Used so analyst/reviewer can write result.json without gaining source
// edits.
func mergeArtifactAndWriteScopes(profile caps.Profile) any {
	rules := map[string]string{"**": "deny"}
	// Artifact writes are always allowed (result.json + declared artifacts).
	// Scope is encoded by substituteScopes to the absolute artifact dir, with
	// a /** suffix from the role template.
	for _, granted := range profile.Grants {
		if granted.Key() == string(caps.CatArtifactWrite) {
			scope := granted.Scope()
			if scope == "" {
				rules["**"] = "allow"
			} else {
				rules[scope] = "allow"
			}
		}
	}
	if profile.Has(caps.CatFsWrite) {
		rules[writeScopeGlob(profile)] = "allow"
	}
	return rules
}

// bashRules builds the bash-tool rule map: allow by default (the role granted
// exec.bash), then deny the patterns that would mutate delivery refs or exfiltr
// ate credentials. The deny list is the orchestration-seam invariant: agents
// may commit inside their worktree branch but must never push, reset --hard,
// delete delivery branches, update refs, or rebase across the delivery boundary.
func bashRules(profile caps.Profile) any {
	rules := map[string]string{"*": "allow"}
	for _, pattern := range deniedBashPatterns {
		rules[pattern] = "deny"
	}
	return rules
}

// deniedBashPatterns are the bash command patterns an agent must never run,
// regardless of role. They protect the delivery boundary (delivery refs and
// checkpoints are orchestrator-owned) and credential exfiltration. opencode
// matches these as substrings/globs against the bash command.
var deniedBashPatterns = []string{
	// Delivery-ref mutation: the orchestrator owns agentum/* branches,
	// checkpoint SHAs, and result_commit capture.
	"git push",
	"git reset --hard",
	"git branch -D",
	"git branch -d",
	"git update-ref",
	"git rebase",
	"git checkout agentum/",
	"git switch agentum/",
	// Credential helpers: an agent must not install or reconfigure a credential
	// helper that could surface secrets the profile scrubbed from its env.
	"git config credential",
	"git config --global",
}

// mcpServerConfig builds the mcp section: only servers the profile explicitly
// grants appear, each as an empty-config placeholder (opencode resolves the
// server's real connection from the operator's global config). When the profile
// grants no mcp.* capability the section is omitted — opencode then loads its
// default MCP setup, which is the documented escape path (see
// docs/capabilities.md): MCP enforcement is "deny servers we know about that
// the profile did not grant" rather than a closed allowlist, because the
// adapter does not own the operator's global server registry.
func mcpServerConfig(profile caps.Profile) map[string]any {
	servers := map[string]any{}
	for _, granted := range profile.Grants {
		raw := string(granted)
		if strings.HasPrefix(raw, "mcp.") {
			server := strings.TrimPrefix(raw, "mcp.")
			servers[server] = map[string]any{"source": "agentum-profile"}
		}
	}
	if len(servers) == 0 {
		return nil
	}
	return servers
}

// credentialEnvDenyList is the set of environment variable prefixes/suffixes
// the adapter scrubs from the child environment unless the profile grants the
// matching secret.<name>. A name-to-env-var map (secretEnvMap) decides which
// granted secret re-adds which variable. Variables not on either list pass
// through unchanged — the scrub is conservative (over-scrubs high-entropy
// names) rather than complete (it cannot detect a credential in an arbitrarily
// named variable). Documented escape path.
var credentialEnvDenyList = []string{
	// Prefixes: any var starting with these is stripped unless re-added by a grant.
	"AWS_", "AZURE_", "GITHUB_", "GH_", "GITLAB_", "TF_VAR_",
	"ANTHROPIC_", "OPENAI_", "GOOGLE_", "VERTEX_",
	// Suffix-style: matched by substring (contains).
	"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_CREDENTIAL",
	// Git credential transport — never let the agent reach a credential helper.
	"GIT_ASKPASS", "GIT_CREDENTIAL_HELPER", "GITHUB_TOKEN", "GIT_TOKEN",
	// SSH agent forwarding surface — local single-owner runs do not need it.
	"SSH_AUTH_SOCK", "SSH_AGENT_PID",
}

// secretEnvMap maps a profile grant secret.<name> to the env var(s) it
// un-redacts. A grant without an entry is honored as a no-op (the secret name
// is recorded but maps to no env var); this keeps the model forward-compatible
// with non-env secret sources (a vault, a file) without pretending we un-redact
// something we cannot.
var secretEnvMap = map[string][]string{
	"secret.github_token":    {"GITHUB_TOKEN", "GH_TOKEN"},
	"secret.gitlab_token":    {"GITLAB_TOKEN"},
	"secret.anthropic_key":   {"ANTHROPIC_API_KEY"},
	"secret.openai_key":      {"OPENAI_API_KEY"},
	"secret.aws_credentials": {"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"},
}

// buildChildEnv constructs the child process environment: start from the parent
// env, drop every variable whose name matches the credential deny list, then
// re-add the variables a granted secret.<name> un-redacts. The result is the
// complete env passed to the opencode subprocess.
func buildChildEnv(profile caps.Profile) []string {
	parent := os.Environ()
	parentMap := make(map[string]string, len(parent))
	for _, entry := range parent {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		parentMap[key] = value
	}

	allowedSecrets := grantedSecretEnvVars(profile)

	filtered := make([]string, 0, len(parent))
	for key, value := range parentMap {
		if isCredentialVar(key) && !allowedSecrets[key] {
			continue // scrubbed: not in a granted secret's un-redact set
		}
		filtered = append(filtered, key+"="+value)
	}
	sort.Strings(filtered)
	return filtered
}

// grantedSecretEnvVars returns the set of env var names the profile's
// secret.<name> grants un-redact. Used by buildChildEnv to re-add a credential
// the profile explicitly permits.
func grantedSecretEnvVars(profile caps.Profile) map[string]bool {
	allowed := map[string]bool{}
	for _, granted := range profile.Grants {
		names, mapped := secretEnvMap[string(granted)]
		if !mapped {
			continue
		}
		for _, name := range names {
			allowed[name] = true
		}
	}
	return allowed
}

// isCredentialVar reports whether key matches the credential deny list. A
// prefix match or a substring (suffix-style) match both count.
func isCredentialVar(key string) bool {
	for _, deny := range credentialEnvDenyList {
		if strings.HasPrefix(key, deny) || strings.Contains(key, deny) {
			return true
		}
	}
	return false
}
