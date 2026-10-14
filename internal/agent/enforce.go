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
//   - fs.read → opencode `read`/`glob`/`grep`/`lsp` permission rules,
//     bounded by an `external_directory` deny so reads cannot leave the project.
//   - fs.write / artifact.write → opencode `edit` permission rules, scoped to
//     the granted paths. opencode has no separate `write` permission: the write
//     and patch tools are both governed by `edit`.
//   - exec.bash → opencode `shell` permission rules, with deny patterns that
//     protect delivery refs (git push / reset --hard / branch -D / update-ref /
//     rebase) the agent must never touch.
//   - git.read / git.write → enforced through exec.bash deny patterns; read is
//     always available alongside bash, write is the same path.
//   - net.fetch → opencode `webfetch` / `websearch` permission rules, plus the
//     shell deny patterns for the common network clients when it is not granted.
//   - secret → env scrub: credential-bearing vars are stripped from the child
//     environment unless the profile grants the matching secret.<name>.
//
// Three categories are deliberately absent:
//
//   - git.delivery: the orchestrator owns delivery refs directly (worktree
//     manager), never via the agent subprocess.
//   - mcp: opencode addresses MCP tools by per-tool permission names this
//     adapter cannot enumerate for an arbitrary server, so it cannot honor
//     "this server and nothing else". Rather than emit a config that looks like
//     enforcement and is not, an mcp.* grant makes the profile unenforceable
//     and the invocation refuses to start.
//   - skill (ADR 0002 D7): skills are ALLOWED at runtime (opencode loads the
//     user's own skills), not narrowed. An adapter cannot honor "this skill and
//     nothing else" without enumerating per-skill permission names, so until
//     narrowing becomes a policy this adapter treats skill.* the same way it
//     treats mcp.*: a grant makes the profile unenforceable. The seam is real
//     because permission.skill accepts the same ordered pattern→action map this
//     adapter already renders; adding CatSkill here is the change that turns
//     narrowing on.
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
	configPath       string       // absolute path the rendered config was written to
	configDir        string       // per-invocation directory holding it; removed by cleanup
	config           []byte       // the rendered opencode permission config (indented)
	env              []string     // the child environment (credential-scrubbed + config)
	timeout          timeoutPlan  // time limits applied via ctx
	subst            scopeSubst   // the scope substitutions applied
	audit            caps.Profile // the substituted profile recorded for audit
	instructionPaths []string     // repo-relative paths denied in edit rules (D4)
	attachments      []string     // staged pinned instruction files, passed with --file
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
func (adapter *OpencodeAdapter) prepareEnforcement(inv Invocation) (enforcementPlan, error) {
	errPrefix := adapter.errPrefix()
	if err := inv.Profile.EnforceableBy(opencodeSupported); err != nil {
		return enforcementPlan{}, fmt.Errorf("%s%w", errPrefix, err)
	}
	subst := scopeSubst{worktree: inv.Workdir, artifact: inv.ArtifactDir}
	// The audit profile keeps absolute paths: it is the evidence record, and an
	// absolute path is unambiguous about what was granted. The config that goes
	// to opencode uses relative patterns, for the reason documented on
	// permissionScope.
	substituted := substituteScopes(inv.Profile, subst)

	// The repo-relative paths of the pinned instruction files feed the edit-deny
	// rules (D4 layer 1). They are derived from inv.Instructions, which carries
	// the pinned bytes the runner read from base_commit; the adapter stages the
	// bytes into the per-invocation config directory below.
	instructionRelPaths := make([]string, 0, len(inv.Instructions))
	for _, file := range inv.Instructions {
		instructionRelPaths = append(instructionRelPaths, file.RepoPath)
	}

	config, buildErr := buildOpencodeConfig(substituted, subst, instructionRelPaths)
	if buildErr != nil {
		return enforcementPlan{}, fmt.Errorf("%s%w", errPrefix, buildErr)
	}

	configDir, configPath, dirErr := createConfigDir(errPrefix)
	if dirErr != nil {
		return enforcementPlan{}, dirErr
	}

	// Stage each pinned instruction file into the config directory and collect
	// its absolute, forward-slash path for a --file attachment. Outside the
	// worktree the agent cannot reach the pinned copies, and opencode reads an
	// attachment itself, so external_directory stays denied. Delivery only adds
	// — the runtime's own AGENTS.md injection still happens — but after the
	// edit deny and the runner's pre-stage hash check the two copies are
	// byte-identical.
	attachments, stageErr := stageInstructionFiles(configDir, inv.Instructions, errPrefix)
	if stageErr != nil {
		_ = os.RemoveAll(configDir)
		return enforcementPlan{}, stageErr
	}

	compact, indented, renderErr := renderOpencodeConfigBytes(config)
	if renderErr != nil {
		_ = os.RemoveAll(configDir)
		return enforcementPlan{}, fmt.Errorf("%srender config: %w", errPrefix, renderErr)
	}
	if writeErr := os.WriteFile(configPath, indented, 0o600); writeErr != nil {
		_ = os.RemoveAll(configDir)
		return enforcementPlan{}, fmt.Errorf("%swrite config: %w", errPrefix, writeErr)
	}

	return enforcementPlan{
		configPath:       configPath,
		configDir:        configDir,
		config:           indented,
		env:              buildChildEnv(substituted, configPath, compact),
		timeout:          timeoutPlan{hard: substituted},
		subst:            subst,
		audit:            substituted,
		instructionPaths: instructionRelPaths,
		attachments:      attachments,
	}, nil
}

// stageInstructionFiles writes each pinned instruction file into the per-
// invocation config directory and returns the absolute, forward-slash paths to
// attach to the message. Files are named by index and basename
// so two declared paths that happen to share a basename do not clobber each
// other. Mode 0600 matches the config file itself; cleanup removes the whole
// directory.
func stageInstructionFiles(configDir string, files []InstructionFile, errPrefix string) ([]string, error) {
	paths := make([]string, 0, len(files))
	for index, file := range files {
		if len(file.Content) == 0 {
			// A file truncated to zero by the total budget, or missing at the
			// commit, delivers nothing. Do not stage it: an empty instructions
			// entry is noise, and opencode would still try to read it.
			continue
		}
		basename := forwardSlashBase(file.RepoPath)
		name := fmt.Sprintf("pinned-%d-%s", index, basename)
		absolute := filepath.Join(configDir, name)
		if err := os.WriteFile(absolute, file.Content, 0o600); err != nil {
			return nil, fmt.Errorf("%sstage instruction %q: %w", errPrefix, file.RepoPath, err)
		}
		paths = append(paths, filepath.ToSlash(absolute))
	}
	return paths, nil
}

// createConfigDir makes a fresh per-invocation directory for the rendered
// config and the pinned instruction files. Returns the directory and the path
// opencode.json will occupy.
//
// The directory lives OUTSIDE the worktree on purpose. A config inside the
// worktree would be (a) writable by the very agent it constrains — fs.write is
// scoped to the worktree — and (b) an untracked file in `git status`, which
// feeds the auto_if_clean gate and can leak into the delivery diff. Only
// `.agentum/` is added to the repo's local excludes; nothing else in the
// worktree is free.
func createConfigDir(errPrefix string) (dir string, path string, err error) {
	dir, err = os.MkdirTemp("", "agentum-opencode-")
	if err != nil {
		return "", "", fmt.Errorf("%screate config dir: %w", errPrefix, err)
	}
	return dir, filepath.Join(dir, "opencode.json"), nil
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
// writes: the permission rules and nothing else. Providers, models, and
// themes stay the operator's business.
//
// The pinned instruction files are deliberately not listed under
// `instructions`: opencode 2 parses that key and never loads it, so a config
// carrying it would claim a delivery that does not happen. They travel as
// message attachments instead (see buildOpencodeArgs).
type opencodeConfig struct {
	Schema      string           `json:"$schema"`
	Permissions []permissionRule `json:"permissions"`
}

// permissionRule is one entry of opencode's ordered permission list. opencode
// evaluates the list in order with the LAST matching rule winning, so the
// wildcard baseline comes first and every later rule is an override.
type permissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// Permission effects, as opencode spells them.
const (
	actionAllow = "allow"
	actionDeny  = "deny"
)

// Permission actions, as opencode 2 names them. Every one of them is emitted
// explicitly, even when the wildcard would already deny it: opencode merges
// config sources, and a rule this adapter leaves out falls through to the
// operator's global config or the project's own opencode.json.
const (
	// permissionAny is the deny-by-default baseline. It covers tools this
	// adapter does not model, tools a future opencode adds (CodeMode's
	// `execute` is one), and MCP tools whose names it cannot enumerate.
	permissionAny = "*"

	// Read-shaped tools. All follow fs.read; external_directory is what keeps
	// them inside the project.
	permissionRead = "read"
	permissionGlob = "glob"
	permissionGrep = "grep"
	permissionLSP  = "lsp"

	// permissionEdit governs the write, edit, and patch tools alike — opencode
	// has no separate write permission.
	permissionEdit = "edit"
	// permissionShell is "allow with deny patterns" when exec.bash is granted.
	permissionShell = "shell"

	// Outbound network.
	permissionWebFetch  = "webfetch"
	permissionWebSearch = "websearch"

	// Reach the capability model has no token for, so deny is the only
	// answer: a subagent runs outside the profile we computed, and
	// external_directory is the containment boundary for every read-shaped
	// tool above.
	//
	// Skill is the exception. A skill grants knowledge, not reach: an agent
	// that reads a skill telling it to run a command or write a file still
	// meets the same shell and edit rules, so nothing escalates. In the
	// single-owner threat model this codebase claims (accidental agent
	// actions, not a hostile process), the skills on the machine are the
	// operator's own. Skills are therefore allowed unconditionally; the
	// protection that replaces a blanket deny is visibility — the runner
	// probes the runtime's skill list once per run and records each skill's
	// name, location, and content hash in the manifest (ContextProber).
	permissionSubagent          = "subagent"
	permissionSkill             = "skill"
	permissionExternalDirectory = "external_directory"

	// Unattended runs have nobody to answer a question, and a doom loop is a
	// stuck agent burning budget.
	permissionQuestion = "question"
	permissionDoomLoop = "doom_loop"
)

// buildOpencodeConfig renders the deny-by-default opencode permission config
// for an effective profile. Anything the profile does not grant resolves to
// "deny"; granted categories resolve to "allow", with the profile's scopes
// encoded as per-path / per-command refinement. instructionPaths are the
// repo-relative paths of the pinned instruction files, used only for the
// edit-deny rules. Returns an error when a granted path scope cannot be
// expressed as a rule opencode will match — the invocation must not start on
// a profile the runtime would ignore.
func buildOpencodeConfig(profile caps.Profile, subst scopeSubst, instructionPaths []string) (opencodeConfig, error) {
	edit, editErr := editRules(profile, subst, instructionPaths)
	if editErr != nil {
		return opencodeConfig{}, editErr
	}
	readable := profile.Has(caps.CatFsRead)
	network := profile.Has(caps.CatNetFetch)

	rules := []permissionRule{wholeAction(permissionAny, actionDeny)}
	for _, action := range []string{permissionRead, permissionGlob, permissionGrep, permissionLSP} {
		rules = append(rules, wholeAction(action, actionFor(readable)))
	}
	rules = append(rules, edit...)
	rules = append(rules, shellRules(profile)...)
	rules = append(rules,
		wholeAction(permissionWebFetch, actionFor(network)),
		wholeAction(permissionWebSearch, actionFor(network)),
		wholeAction(permissionSubagent, actionDeny),
		wholeAction(permissionSkill, actionAllow),
		wholeAction(permissionExternalDirectory, actionDeny),
		wholeAction(permissionQuestion, actionDeny),
		wholeAction(permissionDoomLoop, actionDeny),
	)
	return opencodeConfig{
		Schema:      "https://opencode.ai/config.json",
		Permissions: rules,
	}, nil
}

// wholeAction is the rule that decides an action for every resource.
func wholeAction(action, effect string) permissionRule {
	return permissionRule{Action: action, Resource: anyPath, Effect: effect}
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

// editRules maps fs.write and artifact.write onto opencode's `edit` action.
// Both live here because opencode routes the write and patch tools through `edit`;
// there is no separate `write` permission, so an analyst that could only
// "write" its artifact dir would in fact be unable to produce result.json.
//
// The rule list denies everything, then re-allows each granted scope — deny
// first so the allows win under last-match-wins resolution.
//
// instructionPaths (ADR 0002 D4, layer 1) are the repo-relative paths of the
// pinned instruction files (e.g. "AGENTS.md", "docs/conventions.md"). After the
// allows, each path is appended as an EXACT deny rule. A path whose basename
// matches a runtime-injected filename (the autoInstructionBaseline set, e.g.
// AGENTS.md) ALSO gets a `**/<basename>` name guard, because the runtime injects
// that filename from anywhere in the tree — a nested copy the project never
// declared can still reach the model. The root case (AGENTS.md at the repo
// root) is exactly the one the guard exists for: the exact rule covers the root
// and **/AGENTS.md covers every nested copy. Other declared paths get the exact
// rule only (the runtime does not inject them from elsewhere). Last-match-wins
// means these denies land after the granted scopes and override them: an
// implementer with fs.write to the whole worktree cannot edit AGENTS.md to
// rewrite the rules its reviewer will judge it by in the same run. The bash
// escape path (a write outside the edit tool) is caught by the runner's
// pre-stage hash check, not here — see internal/instructions and the runner's
// restore step.
//
// When the profile grants no write at all, `edit` is one whole-action deny and
// the instruction denies are skipped: there is no allow for them to override.
func editRules(profile caps.Profile, subst scopeSubst, instructionPaths []string) ([]permissionRule, error) {
	scopes := append(
		scopesFor(profile, caps.CatArtifactWrite),
		scopesFor(profile, caps.CatFsWrite)...,
	)
	if len(scopes) == 0 {
		return []permissionRule{wholeAction(permissionEdit, actionDeny)}, nil
	}
	rules := []permissionRule{wholeAction(permissionEdit, actionDeny)}
	for _, scope := range scopes {
		pattern, err := permissionScope(scope, subst.worktree)
		if err != nil {
			return nil, err
		}
		rules = append(rules, permissionRule{Action: permissionEdit, Resource: pattern, Effect: actionAllow})
	}
	// Instruction denies come last so last-match-wins makes them win over the
	// granted scopes. De-duplicated to keep the rule list readable.
	//
	// Each path gets an EXACT deny. A path whose basename matches a
	// runtime-injected filename (the autoInstructionBaseline set, e.g.
	// AGENTS.md) ALSO gets a **/<basename> name guard — because the runtime
	// injects that filename from anywhere in the tree, a nested copy the project
	// never declared can still reach the model. The root case (AGENTS.md at the
	// repo root) is exactly the one the guard exists for: the exact rule covers
	// the root, and **/AGENTS.md covers every nested copy. Other declared paths
	// (e.g. docs/conventions.md) get the exact rule only — the runtime does not
	// inject them from elsewhere, so a same-named file in another directory is
	// not an instruction-channel concern.
	seen := make(map[string]bool, len(instructionPaths)*2)
	for _, path := range instructionPaths {
		basename := forwardSlashBase(path)
		exact := forwardSlashClean(path)
		if !seen[exact] {
			rules = append(rules, permissionRule{Action: permissionEdit, Resource: exact, Effect: actionDeny})
			seen[exact] = true
		}
		if isRuntimeInjectedBasename(basename) {
			guard := "**/" + basename
			if !seen[guard] {
				rules = append(rules, permissionRule{Action: permissionEdit, Resource: guard, Effect: actionDeny})
				seen[guard] = true
			}
		}
	}
	return rules, nil
}

// isRuntimeInjectedBasename reports whether opencode auto-injects a file of
// this name from the project tree (so a nested copy the project never declared
// can still reach the model and must be denied anywhere). Today that is
// AGENTS.md at any depth. Kept as a function so the set has one owner.
func isRuntimeInjectedBasename(basename string) bool {
	for _, name := range autoInstructionBaseline {
		if forwardSlashBase(name) == basename {
			return true
		}
	}
	return false
}

// forwardSlashClean normalises a repo-relative instruction path to forward
// slashes and a clean form, the shape opencode's permission matcher expects.
func forwardSlashClean(path string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
}

// forwardSlashBase returns the final path element of a forward-slash path.
func forwardSlashBase(path string) string {
	cleaned := forwardSlashClean(path)
	if idx := strings.LastIndex(cleaned, "/"); idx >= 0 {
		return cleaned[idx+1:]
	}
	return cleaned
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
// profile and one that denies every write while looking correct in the
// audit trail. Verified against opencode 1.18.10: with the agent passing an
// absolute filePath, a relative pattern covering it allowed the write and an
// absolute pattern covering the same file denied it. Re-checked on 2.0.29: a
// relative `artifacts/**` allow still admits a write there and nowhere else.
//
// A scope that does not resolve under the worktree cannot be expressed at all,
// and is an error rather than a dropped grant: the invocation must not
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

// shellRules builds the shell rule list: allow by default (the role granted
// exec.bash), then deny the patterns that would mutate delivery refs, reach a
// credential helper, or open the network the profile did not grant.
func shellRules(profile caps.Profile) []permissionRule {
	if !profile.Has(caps.CatExecBash) {
		return []permissionRule{wholeAction(permissionShell, actionDeny)}
	}
	rules := []permissionRule{wholeAction(permissionShell, actionAllow)}
	for _, pattern := range deniedBashPatterns {
		rules = append(rules, permissionRule{Action: permissionShell, Resource: pattern, Effect: actionDeny})
	}
	if !profile.Has(caps.CatNetFetch) {
		for _, pattern := range networkBashPatterns {
			rules = append(rules, permissionRule{Action: permissionShell, Resource: pattern, Effect: actionDeny})
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
// profile never granted is the acceptable side of this trade.
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
// cannot replace the profile this invocation computed.
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
//
// A Git provider's token has no entry and never gets one: the publisher that
// delivers a run is not an agent, and no capability grant may hand the
// delivery credential to a stage. The model and infrastructure secrets below
// are a different surface — they name the accounts the project itself runs
// on, not its repository.
var secretEnvMap = map[string][]string{
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

// withWorkingDirectory returns env with PWD naming dir. opencode takes its
// working directory from PWD, not from the process's actual one: a child that
// inherited the parent's PWD runs its session in the directory Agentum was
// started from — a different project, with that project's AGENTS.md and the
// worktree outside external_directory. Setting cmd.Dir alone is therefore not
// enough; every opencode subprocess with a working directory goes through
// here.
func withWorkingDirectory(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "PWD=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "PWD="+dir)
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
