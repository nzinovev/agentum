package agent

import (
	"bytes"
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
//   - fs.read → opencode `read`/`glob`/`grep`/`list`/`lsp` permission rules,
//     bounded by `external_directory: deny` so reads cannot leave the project.
//   - fs.write / artifact.write → opencode `edit` permission rules, scoped to
//     the granted paths. opencode has no separate `write` permission: the write
//     and patch tools are both governed by `edit`.
//   - exec.bash → opencode `bash` permission rules, with deny patterns that
//     protect delivery refs (git push / reset --hard / branch -D / update-ref /
//     rebase) the agent must never touch.
//   - git.read / git.write → enforced through exec.bash deny patterns; read is
//     always available alongside bash, write is the same path.
//   - net.fetch → opencode `webfetch` / `websearch` permission rules, plus the
//     bash deny patterns for the common network clients when it is not granted.
//   - secret → env scrub: credential-bearing vars are stripped from the child
//     environment unless the profile grants the matching secret.<name>.
//
// Two categories are deliberately absent:
//
//   - git.delivery: the orchestrator owns delivery refs directly (worktree
//     manager), never via the agent subprocess.
//   - mcp: opencode addresses MCP tools by per-tool permission names this
//     adapter cannot enumerate for an arbitrary server, so it cannot honor
//     "this server and nothing else". Rather than emit a config that looks like
//     enforcement and is not, an mcp.* grant makes the profile unenforceable
//     and the invocation refuses to start.
var opencodeSupported = []caps.Category{
	caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite,
	caps.CatExecBash, caps.CatGitRead, caps.CatGitWrite,
	caps.CatNetFetch, "secret",
}

// enforcementPlan is the materialized form of a Profile the adapter is about to
// apply: the rendered opencode config bytes, the child environment, and the
// scope substitutions that went into them. The runner records a snapshot of
// this as audit evidence. cleanup releases the per-invocation config directory
// and must run once the subprocess has been reaped, never before.
type enforcementPlan struct {
	configPath string       // absolute path the rendered config was written to
	configDir  string       // per-invocation directory holding it; removed by cleanup
	config     []byte       // the rendered opencode permission config (indented)
	env        []string     // the child environment (credential-scrubbed + config)
	timeout    timeoutPlan  // time limits applied via ctx
	subst      scopeSubst   // the scope substitutions applied
	audit      caps.Profile // the substituted profile recorded for audit
}

// cleanup removes the per-invocation config directory. Idempotent and
// best-effort: a leftover temp directory is a nuisance, not a correctness
// problem, and the caller has no better recovery than logging.
func (plan enforcementPlan) cleanup() {
	if plan.configDir == "" {
		return
	}
	_ = os.RemoveAll(plan.configDir)
}

type timeoutPlan struct {
	hard caps.Profile // carries HardTimeout/IdleTimeout fields; zero means none
}

type scopeSubst struct {
	worktree string
	artifact string
}

// prepareEnforcement materializes inv.Profile into a concrete enforcement plan:
// it confirms the profile is enforceable, substitutes scope placeholders,
// renders the per-invocation opencode permission config, and builds the child
// environment that carries both the config and the credential scrub. Returns an
// error if the profile grants anything the adapter cannot enforce — in that
// case the invocation MUST NOT start.
func prepareEnforcement(inv Invocation) (enforcementPlan, error) {
	if err := inv.Profile.EnforceableBy(opencodeSupported); err != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: %w", err)
	}
	subst := scopeSubst{worktree: inv.Workdir, artifact: inv.ArtifactDir}
	// The audit profile keeps absolute paths: it is the evidence record, and an
	// absolute path is unambiguous about what was granted. The config that goes
	// to opencode uses relative patterns, for the reason documented on
	// permissionScope.
	substituted := substituteScopes(inv.Profile, subst)

	config, buildErr := buildOpencodeConfig(substituted, subst)
	if buildErr != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: %w", buildErr)
	}
	compact, indented, renderErr := renderOpencodeConfigBytes(config)
	if renderErr != nil {
		return enforcementPlan{}, fmt.Errorf("opencode adapter: render config: %w", renderErr)
	}

	configDir, configPath, writeErr := writeInvocationConfig(indented)
	if writeErr != nil {
		return enforcementPlan{}, writeErr
	}

	return enforcementPlan{
		configPath: configPath,
		configDir:  configDir,
		config:     indented,
		env:        buildChildEnv(substituted, configPath, compact),
		timeout:    timeoutPlan{hard: substituted},
		subst:      subst,
		audit:      substituted,
	}, nil
}

// writeInvocationConfig writes the rendered config to a fresh per-invocation
// directory and returns (dir, path).
//
// The directory lives OUTSIDE the worktree on purpose. A config inside the
// worktree would be (a) writable by the very agent it constrains — fs.write is
// scoped to the worktree — and (b) an untracked file in `git status`, which
// feeds the auto_if_clean gate and can leak into the delivery diff. Only
// `.agentum/` is added to the repo's local excludes; nothing else in the
// worktree is free.
func writeInvocationConfig(rendered []byte) (dir string, path string, err error) {
	dir, err = os.MkdirTemp("", "agentum-opencode-")
	if err != nil {
		return "", "", fmt.Errorf("opencode adapter: create config dir: %w", err)
	}
	path = filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("opencode adapter: write config: %w", err)
	}
	return dir, path, nil
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
		scope = strings.ReplaceAll(scope, caps.ArtifactScope, subst.artifact)
		out.Grants = append(out.Grants, caps.Token(granted.Key())+caps.Token(":"+scope))
	}
	return out
}

// opencodeConfig is the subset of the opencode config schema this adapter
// writes: the permission map, and nothing else. Everything outside permissions
// (providers, models, themes) stays the operator's business.
type opencodeConfig struct {
	Schema     string                  `json:"$schema"`
	Permission opencodePermissionRules `json:"permission"`
}

// Permission actions, as opencode spells them.
const (
	actionAllow = "allow"
	actionDeny  = "deny"
)

// opencodePermissionRules is the per-tool permission map, mirroring opencode's
// documented permission keys (https://opencode.ai/docs/permissions/).
//
// Two rules govern this struct:
//
//  1. EVERY key is set explicitly. opencode merges config sources and only
//     overrides conflicting keys, so a key this adapter omits falls through to
//     the operator's global config or the project's own opencode.json — which
//     would silently widen the profile we just computed.
//  2. Field order is the emitted JSON order, and opencode resolves permission
//     rules with the LAST match winning. The wildcard baseline therefore comes
//     first; every specific key after it is an override.
//
// Values are either a plain action string or an ordered pattern→action map
// (ruleList), which is why the refined keys are typed `any`.
type opencodePermissionRules struct {
	// Wildcard is the deny-by-default baseline. It covers tools this adapter
	// does not model, tools a future opencode adds, and MCP tools whose names
	// it cannot enumerate. Verified to override opencode's own built-in
	// {permission:"*", pattern:"*", action:"allow"} default.
	Wildcard string `json:"*"`

	// Read-shaped tools. All follow fs.read; external_directory is what keeps
	// them inside the project.
	Read string `json:"read"`
	Glob string `json:"glob"`
	Grep string `json:"grep"`
	List string `json:"list"`
	LSP  string `json:"lsp"`

	// Edit governs write, edit, and patch alike — opencode has no separate
	// `write` permission.
	Edit any `json:"edit"`
	// Bash is "allow with deny patterns" when exec.bash is granted.
	Bash any `json:"bash"`

	// Outbound network.
	WebFetch  string `json:"webfetch"`
	WebSearch string `json:"websearch"`

	// Reach the capability model has no token for, so deny is the only honest
	// answer: a subagent runs outside the profile we computed, a skill injects
	// instructions we did not render, and external_directory is the containment
	// boundary for every read-shaped tool above.
	Task              string `json:"task"`
	Skill             string `json:"skill"`
	ExternalDirectory string `json:"external_directory"`

	// Unattended runs have nobody to answer a question, and a doom loop is a
	// stuck agent burning budget.
	Question string `json:"question"`
	DoomLoop string `json:"doom_loop"`

	// Session-local bookkeeping with no reach outside the run.
	TodoWrite string `json:"todowrite"`
}

// buildOpencodeConfig renders the deny-by-default opencode permission config
// for an effective profile. Anything the profile does not grant resolves to
// "deny"; granted categories resolve to "allow", with the profile's scopes
// encoded as per-path / per-command refinement. Returns an error when a granted
// path scope cannot be expressed as a rule opencode will match — the invocation
// must not start on a profile the runtime would silently ignore.
func buildOpencodeConfig(profile caps.Profile, subst scopeSubst) (opencodeConfig, error) {
	edit, editErr := editRules(profile, subst)
	if editErr != nil {
		return opencodeConfig{}, editErr
	}
	readable := profile.Has(caps.CatFsRead)
	network := profile.Has(caps.CatNetFetch)
	return opencodeConfig{
		Schema: "https://opencode.ai/config.json",
		Permission: opencodePermissionRules{
			Wildcard: actionDeny,

			Read: actionFor(readable),
			Glob: actionFor(readable),
			Grep: actionFor(readable),
			List: actionFor(readable),
			LSP:  actionFor(readable),

			Edit: edit,
			Bash: bashRules(profile),

			WebFetch:  actionFor(network),
			WebSearch: actionFor(network),

			Task:              actionDeny,
			Skill:             actionDeny,
			ExternalDirectory: actionDeny,

			Question: actionDeny,
			DoomLoop: actionDeny,

			TodoWrite: actionAllow,
		},
	}, nil
}

// renderOpencodeConfigBytes marshals one config value twice: compact for the
// environment variable that carries the enforcement, indented for the file that
// carries the audit trail. Marshalling the same value twice is what keeps the
// two from drifting.
func renderOpencodeConfigBytes(config opencodeConfig) (compact, indented []byte, err error) {
	compact, err = json.Marshal(config)
	if err != nil {
		return nil, nil, fmt.Errorf("encode opencode config: %w", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, nil, fmt.Errorf("indent opencode config: %w", err)
	}
	return compact, buf.Bytes(), nil
}

// actionFor maps "is this granted" to the opencode action.
func actionFor(granted bool) string {
	if granted {
		return actionAllow
	}
	return actionDeny
}

// ruleList is an ordered pattern→action list rendered as a JSON object.
//
// It exists because opencode evaluates permission rules in file order with the
// last match winning, and Go's encoding/json sorts map keys. A map would put
// the deny baseline wherever the alphabet happened to place it — correct today
// by luck, silently wrong the first time a pattern starts with a different
// character.
type ruleList struct {
	patterns []string
	actions  map[string]string
}

func newRuleList() *ruleList {
	return &ruleList{actions: map[string]string{}}
}

// add appends a rule. A repeated pattern keeps its original position and takes
// the new action — position is what matters for resolution, and re-stating a
// pattern is a caller bug we would rather resolve deterministically than panic
// on.
func (rules *ruleList) add(pattern, action string) *ruleList {
	if _, exists := rules.actions[pattern]; !exists {
		rules.patterns = append(rules.patterns, pattern)
	}
	rules.actions[pattern] = action
	return rules
}

// MarshalJSON emits the rules as a JSON object in insertion order.
func (rules *ruleList) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for index, pattern := range rules.patterns {
		if index > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(pattern)
		if err != nil {
			return nil, fmt.Errorf("encode permission pattern %q: %w", pattern, err)
		}
		buf.Write(key)
		buf.WriteByte(':')
		value, err := json.Marshal(rules.actions[pattern])
		if err != nil {
			return nil, fmt.Errorf("encode permission action for %q: %w", pattern, err)
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// editRules maps fs.write and artifact.write onto opencode's `edit` key. Both
// live here because opencode routes the write and patch tools through `edit`;
// there is no separate `write` permission, so an analyst that could only
// "write" its artifact dir would in fact be unable to produce result.json.
//
// The rule list denies everything, then re-allows each granted scope — deny
// first so the allows win under last-match-wins resolution.
func editRules(profile caps.Profile, subst scopeSubst) (any, error) {
	scopes := append(
		scopesFor(profile, caps.CatArtifactWrite),
		scopesFor(profile, caps.CatFsWrite)...,
	)
	if len(scopes) == 0 {
		return actionDeny, nil
	}
	rules := newRuleList().add(anyPath, actionDeny)
	for _, scope := range scopes {
		pattern, err := permissionScope(scope, subst.worktree)
		if err != nil {
			return nil, err
		}
		rules.add(pattern, actionAllow)
	}
	return rules, nil
}

// anyPath is opencode's match-everything pattern: `*` matches zero or more of
// any character, separators included.
const anyPath = "*"

// permissionScope converts an absolute grant scope into the pattern opencode
// will actually match.
//
// opencode normalises a tool's target path to a form relative to the project
// root before matching, so an ABSOLUTE pattern never matches anything. This is
// not a detail of the glob syntax — it is the difference between a working
// profile and one that silently denies every write while looking correct in the
// audit trail. Verified against opencode 1.18.10: with the agent passing an
// absolute filePath, a relative pattern covering it allowed the write and an
// absolute pattern covering the same file denied it.
//
// A scope that does not resolve under the worktree cannot be expressed at all,
// and is an error rather than a silently dropped grant: the invocation must not
// start on a profile the runtime would ignore.
func permissionScope(scope string, worktreeRoot string) (string, error) {
	if scope == anyPath || scope == "" {
		return anyPath, nil
	}
	base, suffix := splitGlobSuffix(scope)
	if worktreeRoot == "" {
		return "", fmt.Errorf("cannot scope %q: no worktree root to resolve it against", scope)
	}
	rel, err := filepath.Rel(worktreeRoot, base)
	if err != nil {
		return "", fmt.Errorf("cannot scope %q under %q: %w", scope, worktreeRoot, err)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("grant scope %q resolves outside the worktree %q", scope, worktreeRoot)
	}
	if rel == "." {
		// The scope is the worktree root itself (an implementer's fs.write).
		// Relative to the root, that is simply everything.
		return strings.TrimPrefix(suffix, "/"), nil
	}
	return rel + suffix, nil
}

// splitGlobSuffix separates a scope's directory part from its trailing glob
// ("/path/to/dir/**" → "/path/to/dir", "/**"), so the directory can be
// re-spelled without disturbing the pattern. A scope with no glob returns an
// empty suffix.
func splitGlobSuffix(scope string) (base, suffix string) {
	index := strings.IndexByte(scope, '*')
	if index < 0 {
		return scope, ""
	}
	return strings.TrimSuffix(scope[:index], "/"), scope[index-1:]
}

// scopesFor returns the path scopes the profile grants for a category, in grant
// order. An unscoped grant widens to anyPath — the profile said "this category,
// everywhere the runtime can see" — and a category the profile does not grant
// contributes nothing.
func scopesFor(profile caps.Profile, category caps.Category) []string {
	out := make([]string, 0, 2)
	for _, granted := range profile.Grants {
		if caps.CategoryOf(granted) != category {
			continue
		}
		if scope := granted.Scope(); scope != "" {
			out = append(out, scope)
			continue
		}
		out = append(out, anyPath)
	}
	return out
}

// bashRules builds the bash rule list: allow by default (the role granted
// exec.bash), then deny the patterns that would mutate delivery refs, reach a
// credential helper, or open the network the profile did not grant.
func bashRules(profile caps.Profile) any {
	if !profile.Has(caps.CatExecBash) {
		return actionDeny
	}
	rules := newRuleList().add(anyPath, actionAllow)
	for _, pattern := range deniedBashPatterns {
		rules.add(pattern, actionDeny)
	}
	if !profile.Has(caps.CatNetFetch) {
		for _, pattern := range networkBashPatterns {
			rules.add(pattern, actionDeny)
		}
	}
	return rules
}

// deniedBashPatterns are the bash command patterns an agent must never run,
// regardless of role. They protect the delivery boundary (delivery refs and
// checkpoints are orchestrator-owned) and the credential surface.
//
// Every pattern ends in `*`. opencode matches bash rules as wildcard patterns
// against the parsed command, not as substrings: a bare "git push" would match
// only the argument-less command and let `git push origin HEAD` straight
// through. The trailing `*` is what makes each of these a real deny.
var deniedBashPatterns = []string{
	// Delivery-ref mutation: the orchestrator owns agentum/* branches,
	// checkpoint SHAs, and result_commit capture.
	"git push*",
	"git reset --hard*",
	"git branch -D*",
	"git branch -d*",
	"git update-ref*",
	"git rebase*",
	"git worktree*",
	"git checkout agentum/*",
	"git switch agentum/*",
	// Credential helpers: an agent must not install or reconfigure a credential
	// helper that could surface secrets the profile scrubbed from its env.
	"git config credential*",
	"git config --global*",
}

// networkBashPatterns are the common network clients denied when the profile
// does not grant net.fetch. Without them, `bash: allow` would hand back the
// network that the webfetch deny just took away.
//
// Deliberately coarse: these are prefix patterns, so an unrelated command whose
// name starts the same way ("ncdu") is denied too. Over-denying a tool the
// profile never promised is the acceptable side of this trade.
var networkBashPatterns = []string{
	"curl*",
	"wget*",
	"ssh*",
	"scp*",
	"nc*",
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

// configEnvVars are the opencode config-selection variables the adapter owns
// outright. They are dropped from the inherited environment before the
// adapter's own values go in, so an operator's ambient OPENCODE_CONFIG_CONTENT
// cannot quietly replace the profile this invocation computed.
var configEnvVars = []string{
	"OPENCODE_CONFIG",
	"OPENCODE_CONFIG_CONTENT",
	"OPENCODE_CONFIG_DIR",
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
// env, drop every variable whose name matches the credential deny list or names
// an opencode config source, re-add the variables a granted secret.<name>
// un-redacts, then inject this invocation's config.
//
// The config is injected twice on purpose. OPENCODE_CONFIG is a path opencode
// loads BELOW the project's own opencode.json, so a repository that ships its
// own permissions would override it. OPENCODE_CONFIG_CONTENT is loaded above
// every project source, so the inline copy is what actually enforces; the file
// remains as the readable audit artifact the plan records.
func buildChildEnv(profile caps.Profile, configPath string, configContent []byte) []string {
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
		if isAdapterOwnedVar(key) {
			continue // the adapter sets its own value below
		}
		if isCredentialVar(key) && !allowedSecrets[key] {
			continue // scrubbed: not in a granted secret's un-redact set
		}
		filtered = append(filtered, key+"="+value)
	}
	sort.Strings(filtered)

	if configPath != "" {
		filtered = append(filtered, "OPENCODE_CONFIG="+configPath)
	}
	if len(configContent) > 0 {
		filtered = append(filtered, "OPENCODE_CONFIG_CONTENT="+string(configContent))
	}
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

// isAdapterOwnedVar reports whether the adapter sets this variable itself, in
// which case the inherited value is dropped rather than merged.
func isAdapterOwnedVar(key string) bool {
	for _, owned := range configEnvVars {
		if key == owned {
			return true
		}
	}
	return false
}
