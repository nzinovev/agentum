package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// testSubst is the scope substitution for a worktree laid out the way the
// runner lays one out: the per-stage artifact directory under two dot
// directories inside the worktree root.
func testSubst(root string) scopeSubst {
	return scopeSubst{
		worktree: root,
		artifact: filepath.Join(root, ".agentum", "task-1", ".ag-artifacts", "spec"),
	}
}

// implementerProfile is the profile an implementer receives from a host that
// supports every category.
func implementerProfile(t *testing.T) caps.Profile {
	t.Helper()
	declared := []caps.Token{
		caps.Token(caps.CatFsRead), caps.Token(caps.CatFsWrite), caps.Token(caps.CatGitWrite),
		caps.Token(caps.CatExecBash), caps.Token(caps.CatArtifactWrite),
	}
	profile := caps.Effective(caps.Input{
		Host: opencodeSupported, Pack: declared, Stage: declared, Role: caps.RoleImplementer,
	})
	if !profile.Has(caps.CatFsWrite) || !profile.Has(caps.CatExecBash) {
		t.Fatalf("fixture: implementer profile missing expected grants: %v", profile.Grants)
	}
	return profile
}

// analystProfile is the read-plus-own-artifact profile every analytical stage
// gets.
func analystProfile(t *testing.T) caps.Profile {
	t.Helper()
	declared := []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatArtifactWrite)}
	profile := caps.Effective(caps.Input{
		Host: opencodeSupported, Pack: declared, Stage: declared, Role: caps.RoleAnalyst,
	})
	if !profile.Has(caps.CatArtifactWrite) {
		t.Fatalf("fixture: analyst profile missing artifact.write: %v", profile.Grants)
	}
	return profile
}

// mustRender substitutes the profile's scope placeholders and renders the
// indented config, failing on any error. Substitution lives here so a test
// cannot accidentally hand prepareEnforcement an already-substituted profile —
// the two are not idempotent together.
func mustRender(t *testing.T, profile caps.Profile, subst scopeSubst) []byte {
	t.Helper()
	config, err := buildOpencodeConfig(substituteScopes(profile, subst), subst)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	_, indented, err := renderOpencodeConfigBytes(config)
	if err != nil {
		t.Fatalf("render config: %v", err)
	}
	return indented
}

// decodePermissions returns the raw permission object from a rendered config.
func decodePermissions(t *testing.T, rendered []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Permission map[string]any `json:"permission"`
	}
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	return envelope.Permission
}

// --- the matching contract -------------------------------------------------

// opencodeMatch mirrors opencode's permission matcher (util/wildcard): the
// pattern is anchored end to end, `*` stands for any run of characters, `?` for
// exactly one, backslashes are normalised to forward slashes on both sides, and
// matching is case-insensitive on Windows.
//
// It is a model of the runtime, not the runtime itself — a live contract test
// covers the real binary. Its job here is to make the pattern-generation rule
// deterministic and regression-proof without a model in the loop, which is
// exactly what manual probing could not offer.
func opencodeMatch(pattern, input string) bool {
	normalise := func(value string) string { return strings.ReplaceAll(value, `\`, "/") }
	var expr strings.Builder
	expr.WriteString("^")
	for _, char := range normalise(pattern) {
		switch char {
		case '*':
			expr.WriteString(".*")
		case '?':
			expr.WriteString(".")
		default:
			expr.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	expr.WriteString("$")
	flags := "s"
	if runtime.GOOS == "windows" {
		flags = "si"
	}
	matcher := regexp.MustCompile("(?" + flags + ")" + expr.String())
	return matcher.MatchString(normalise(input))
}

// TestPermissionScope_IsRelativeToTheWorktree pins the single fact the whole
// permission config rests on: opencode normalises a tool's target path to a
// form relative to the project root before matching, so grant scopes must be
// emitted relative too. An absolute pattern matches nothing, which shows up not
// as an error but as an agent that cannot write its own result.json while the
// config still looks correct.
//
// Verified against opencode 1.18.10 by giving the agent an absolute filePath
// and covering it first with a relative pattern (allowed) and then with an
// absolute one (denied).
func TestPermissionScope_IsRelativeToTheWorktree(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator)+"tmp", "wt")
	subst := testSubst(root)

	artifactPattern, err := permissionScope(subst.artifact+"/**", root)
	if err != nil {
		t.Fatalf("scope artifact dir: %v", err)
	}
	if filepath.IsAbs(artifactPattern) || strings.Contains(artifactPattern, root) {
		t.Fatalf("artifact pattern %q is absolute; opencode would never match it", artifactPattern)
	}

	worktreePattern, err := permissionScope(root+"/**", root)
	if err != nil {
		t.Fatalf("scope worktree: %v", err)
	}
	if worktreePattern != "**" {
		t.Errorf("worktree scope = %q, want ** (everything under the project root)", worktreePattern)
	}

	// The paths opencode would present, given the layout above.
	artifactFile := ".agentum/task-1/.ag-artifacts/spec/result.json"
	otherStageFile := ".agentum/task-1/.ag-artifacts/review/result.json"
	sourceFile := "internal/agent/opencode.go"

	for _, testCase := range []struct {
		name    string
		pattern string
		input   string
		want    bool
	}{
		{"artifact scope covers its own dir", artifactPattern, artifactFile, true},
		{"artifact scope excludes another stage", artifactPattern, otherStageFile, false},
		{"artifact scope excludes source", artifactPattern, sourceFile, false},
		{"worktree scope covers source", worktreePattern, sourceFile, true},
		{"worktree scope covers the artifact dir", worktreePattern, artifactFile, true},
	} {
		if got := opencodeMatch(testCase.pattern, testCase.input); got != testCase.want {
			t.Errorf("%s: match(%q, %q) = %v, want %v", testCase.name, testCase.pattern, testCase.input, got, testCase.want)
		}
	}
}

// TestPermissionScope_RefusesScopeOutsideWorktree: a scope that cannot be
// expressed relative to the project root is an error, not a dropped grant. A
// silently dropped grant is how a profile ends up meaning something other than
// what it says.
func TestPermissionScope_RefusesScopeOutsideWorktree(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator)+"tmp", "wt")
	if _, err := permissionScope(filepath.Join(string(filepath.Separator)+"etc", "ssh")+"/**", root); err == nil {
		t.Error("expected an error for a scope outside the worktree, got nil")
	}
	if _, err := permissionScope("/tmp/wt/x/**", ""); err == nil {
		t.Error("expected an error when there is no worktree root to resolve against, got nil")
	}
}

// --- rendered config -------------------------------------------------------

// permissionKeys is every key the rendered config must carry. opencode merges
// config sources and overrides only conflicting keys, so a key this adapter
// omits falls through to the operator's global config or the project's own
// opencode.json — silently widening the profile.
var permissionKeys = []string{
	"*", "read", "glob", "grep", "list", "lsp",
	"edit", "bash", "webfetch", "websearch",
	"task", "skill", "external_directory",
	"question", "doom_loop", "todowrite",
}

func TestRenderConfig_SetsEveryPermissionKey(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, implementerProfile(t), subst))
	for _, key := range permissionKeys {
		if _, present := permissions[key]; !present {
			t.Errorf("permission key %q missing; it would inherit from another config source", key)
		}
	}
	if len(permissions) != len(permissionKeys) {
		t.Errorf("permission keys = %d, want %d — update permissionKeys when adding one", len(permissions), len(permissionKeys))
	}
}

// TestRenderConfig_DenyByDefault: an empty profile renders every tool as deny,
// including the wildcard baseline that covers tools this adapter does not model.
func TestRenderConfig_DenyByDefault(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, caps.Profile{}, subst))
	for key, value := range permissions {
		if key == "todowrite" {
			continue // session-local bookkeeping with no reach
		}
		if value != actionDeny {
			t.Errorf("empty profile: %q = %v, want deny", key, value)
		}
	}
}

// TestRenderConfig_WildcardBaselineIsDeny: the catch-all stays deny even for a
// fully-granted implementer, so an unmodelled or future tool is refused rather
// than inherited. Confirmed live: this entry overrides opencode's own built-in
// {permission:"*", pattern:"*", action:"allow"}.
func TestRenderConfig_WildcardBaselineIsDeny(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, implementerProfile(t), subst))
	if permissions["*"] != actionDeny {
		t.Errorf("wildcard baseline = %v, want deny", permissions["*"])
	}
	for _, key := range []string{"task", "skill", "external_directory"} {
		if permissions[key] != actionDeny {
			t.Errorf("%s = %v, want deny (no capability token grants it)", key, permissions[key])
		}
	}
}

// TestRenderConfig_AnalystWritesArtifactsButNotSource: opencode routes the
// write tool through `edit`, so the analyst's artifact scope must appear there.
// A blanket `edit: deny` leaves the analyst unable to produce result.json.
func TestRenderConfig_AnalystWritesArtifactsButNotSource(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, analystProfile(t), subst))

	if permissions["read"] != actionAllow {
		t.Errorf("analyst read = %v, want allow", permissions["read"])
	}
	if permissions["bash"] != actionDeny {
		t.Errorf("analyst bash = %v, want deny", permissions["bash"])
	}

	rules, ok := permissions["edit"].(map[string]any)
	if !ok {
		t.Fatalf("analyst edit = %T (%v), want a scoped rule map", permissions["edit"], permissions["edit"])
	}
	if rules[anyPath] != actionDeny {
		t.Errorf("analyst edit baseline = %v, want deny", rules[anyPath])
	}
	const artifactPattern = ".agentum/task-1/.ag-artifacts/spec/**"
	if rules[artifactPattern] != actionAllow {
		t.Errorf("analyst edit %q = %v, want allow (result.json must be writable)", artifactPattern, rules[artifactPattern])
	}
	for pattern, action := range rules {
		if pattern != artifactPattern && action == actionAllow {
			t.Errorf("analyst edit allows %q; only the artifact dir may be writable", pattern)
		}
	}
}

// TestRenderConfig_ImplementerWritesTheWorktree: the implementer's fs.write is
// the worktree root, which relative to that root is everything.
func TestRenderConfig_ImplementerWritesTheWorktree(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rules, ok := decodePermissions(t, mustRender(t, implementerProfile(t), subst))["edit"].(map[string]any)
	if !ok {
		t.Fatal("implementer edit is not a scoped rule map")
	}
	if rules["**"] != actionAllow {
		t.Errorf("implementer edit ** = %v, want allow", rules["**"])
	}
	if rules[anyPath] != actionDeny {
		t.Errorf("implementer edit baseline = %v, want deny", rules[anyPath])
	}
}

// TestRenderConfig_DenyBaselinePrecedesAllows: opencode resolves permission
// rules with the last match winning, so the deny baseline must be emitted
// before the allows. Asserted on the raw bytes because that ordering is exactly
// what a Go map would destroy.
func TestRenderConfig_DenyBaselinePrecedesAllows(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rendered := string(mustRender(t, implementerProfile(t), subst))
	baseline := strings.Index(rendered, `"*": "deny"`)
	if baseline < 0 {
		t.Fatalf("edit deny baseline not found in rendered config:\n%s", rendered)
	}
	allow := strings.Index(rendered, `": "allow"`)
	if allow < 0 {
		t.Fatalf("no allow rule found in rendered config:\n%s", rendered)
	}
	if baseline > allow {
		t.Errorf("deny baseline at %d comes after the first allow at %d; last-match-wins would deny the granted scope", baseline, allow)
	}
}

// decodeBashRules extracts the bash rule map from a rendered config.
func decodeBashRules(t *testing.T, rendered []byte) map[string]string {
	t.Helper()
	raw, ok := decodePermissions(t, rendered)["bash"].(map[string]any)
	if !ok {
		t.Fatalf("bash rule is not a map: %v", decodePermissions(t, rendered)["bash"])
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		out[key] = value.(string)
	}
	return out
}

// TestRenderConfig_BashDeniesDeliveryRefMutation: every delivery-ref deny
// pattern is present AND ends in a wildcard. opencode matches bash rules as
// patterns against the parsed command, so a bare "git push" would let
// `git push origin HEAD` through — the trailing `*` is the whole enforcement.
func TestRenderConfig_BashDeniesDeliveryRefMutation(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rules := decodeBashRules(t, mustRender(t, implementerProfile(t), subst))
	if rules[anyPath] != actionAllow {
		t.Errorf("bash default = %q, want allow", rules[anyPath])
	}
	for _, command := range []string{
		"git push", "git reset --hard", "git branch -D",
		"git update-ref", "git rebase", "git worktree",
	} {
		pattern := command + "*"
		if rules[pattern] != actionDeny {
			t.Errorf("bash rule %q = %q, want deny", pattern, rules[pattern])
		}
		if rules[command] == actionDeny {
			t.Errorf("bash rule %q is an exact-match deny; it would miss the same command with arguments", command)
		}
		// The pattern must actually cover the command with arguments.
		if !opencodeMatch(pattern, command+" origin main") {
			t.Errorf("bash pattern %q does not match %q", pattern, command+" origin main")
		}
	}
}

// TestRenderConfig_BashDeniesNetworkWithoutNetFetch: `bash: allow` must not
// hand back the network that the webfetch deny took away.
func TestRenderConfig_BashDeniesNetworkWithoutNetFetch(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rules := decodeBashRules(t, mustRender(t, implementerProfile(t), subst))
	for _, pattern := range []string{"curl*", "wget*", "ssh*"} {
		if rules[pattern] != actionDeny {
			t.Errorf("bash rule %q = %q, want deny (profile has no net.fetch)", pattern, rules[pattern])
		}
	}

	networked := implementerProfile(t)
	networked.Grants = append(networked.Grants, caps.Token(caps.CatNetFetch))
	if _, denied := decodeBashRules(t, mustRender(t, networked, subst))["curl*"]; denied {
		t.Error("net.fetch granted but curl is still denied; the network deny must lift with the grant")
	}
}

// TestRenderConfig_NetFetchAllowedOnlyWhenGranted: net.fetch maps to the
// webfetch and websearch tools.
func TestRenderConfig_NetFetchAllowedOnlyWhenGranted(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	denied := decodePermissions(t, mustRender(t, analystProfile(t), subst))
	if denied["webfetch"] != actionDeny || denied["websearch"] != actionDeny {
		t.Errorf("without net.fetch: webfetch=%v websearch=%v, want both deny", denied["webfetch"], denied["websearch"])
	}

	granted := analystProfile(t)
	granted.Grants = append(granted.Grants, caps.Token(caps.CatNetFetch))
	allowed := decodePermissions(t, mustRender(t, granted, subst))
	if allowed["webfetch"] != actionAllow || allowed["websearch"] != actionAllow {
		t.Errorf("with net.fetch: webfetch=%v websearch=%v, want both allow", allowed["webfetch"], allowed["websearch"])
	}
}

// --- the enforcement plan --------------------------------------------------

// TestPrepareEnforcement_RefusesUnenforceableProfile: a profile granting a
// capability the adapter cannot express — git.delivery (orchestrator-only) or
// an MCP server whose tool names it cannot enumerate — is rejected before any
// subprocess starts.
func TestPrepareEnforcement_RefusesUnenforceableProfile(t *testing.T) {
	t.Parallel()
	for name, grant := range map[string]caps.Token{
		"git.delivery": caps.Token(caps.CatGitDelivery),
		"mcp server":   "mcp.github",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := prepareEnforcement(Invocation{
				Workdir:     t.TempDir(),
				ArtifactDir: filepath.Join(t.TempDir(), "artifacts"),
				Profile:     caps.Profile{Grants: []caps.Token{grant}},
			})
			if err == nil {
				t.Fatalf("expected an unenforceable error for %s, got nil", name)
			}
			if !strings.Contains(err.Error(), "cannot enforce") && !strings.Contains(err.Error(), "unsupported") {
				t.Errorf("error = %q, want an enforceability failure", err.Error())
			}
		})
	}
}

// TestPrepareEnforcement_ConfigLivesOutsideWorktree: the rendered config must
// not land in the worktree. Inside it, the agent it constrains could edit it
// (fs.write is worktree-scoped) and `git status` would report it — which drives
// the auto_if_clean gate and can leak into the delivery diff.
func TestPrepareEnforcement_ConfigLivesOutsideWorktree(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	subst := testSubst(workdir)

	plan, err := prepareEnforcement(Invocation{
		Workdir: workdir, ArtifactDir: subst.artifact, Profile: implementerProfile(t),
	})
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	defer plan.cleanup()

	if strings.HasPrefix(plan.configPath, workdir) {
		t.Errorf("configPath %q is inside the worktree %q", plan.configPath, workdir)
	}
	if _, statErr := os.Stat(plan.configPath); statErr != nil {
		t.Errorf("config file not written: %v", statErr)
	}
	entries, readErr := os.ReadDir(workdir)
	if readErr != nil {
		t.Fatalf("read worktree: %v", readErr)
	}
	for _, entry := range entries {
		t.Errorf("prepareEnforcement created %q in the worktree; it must leave no trace there", entry.Name())
	}
}

// TestPrepareEnforcement_AuditKeepsAbsoluteScopes: the config opencode reads is
// relative, but the evidence record is absolute — a reviewer reading the audit
// trail should not have to guess which root a path was relative to.
func TestPrepareEnforcement_AuditKeepsAbsoluteScopes(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	subst := testSubst(workdir)

	plan, err := prepareEnforcement(Invocation{
		Workdir: workdir, ArtifactDir: subst.artifact, Profile: implementerProfile(t),
	})
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	defer plan.cleanup()

	for _, granted := range plan.audit.Grants {
		if strings.Contains(string(granted), caps.WorktreeScope) || strings.Contains(string(granted), caps.ArtifactScope) {
			t.Errorf("audit grant still carries a placeholder: %q", granted)
		}
	}
	if !strings.Contains(string(plan.config), ".agentum/task-1/.ag-artifacts/spec/**") {
		t.Errorf("rendered config does not carry the relative artifact scope:\n%s", plan.config)
	}
}

// TestPrepareEnforcement_CleanupRemovesConfig: the per-invocation config
// directory does not outlive the run.
func TestPrepareEnforcement_CleanupRemovesConfig(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	plan, err := prepareEnforcement(Invocation{
		Workdir: workdir, ArtifactDir: testSubst(workdir).artifact, Profile: analystProfile(t),
	})
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	plan.cleanup()
	if _, statErr := os.Stat(plan.configDir); !os.IsNotExist(statErr) {
		t.Errorf("config dir still present after cleanup: %v", statErr)
	}
	plan.cleanup() // idempotent
}

// --- child environment -----------------------------------------------------

// TestBuildChildEnv_CarriesInlineConfig: the enforcement rides on
// OPENCODE_CONFIG_CONTENT, which opencode loads above a project's own
// opencode.json. A repository shipping its own permissions must not be able to
// override the profile.
func TestBuildChildEnv_CarriesInlineConfig(t *testing.T) {
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"permission":{"*":"allow"}}`)
	t.Setenv("OPENCODE_CONFIG", "/operator/opencode.json")

	env := buildChildEnv(caps.Profile{}, "/tmp/agentum/opencode.json", []byte(`{"permission":{"*":"deny"}}`))
	if got := envLookup(env, "OPENCODE_CONFIG_CONTENT"); got != `{"permission":{"*":"deny"}}` {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %q, want the adapter's config", got)
	}
	if got := envLookup(env, "OPENCODE_CONFIG"); got != "/tmp/agentum/opencode.json" {
		t.Errorf("OPENCODE_CONFIG = %q, want the adapter's path", got)
	}
	if countEnv(env, "OPENCODE_CONFIG_CONTENT") != 1 {
		t.Error("OPENCODE_CONFIG_CONTENT appears more than once; the ambient value was not dropped")
	}
}

// TestBuildChildEnv_ScrubsCredentialsUnlessGranted: GITHUB_TOKEN is stripped by
// default and re-added only when secret.github_token is granted.
func TestBuildChildEnv_ScrubsCredentialsUnlessGranted(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret_value")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin") // non-credential, must survive

	scrubbed := buildChildEnv(caps.Profile{}, "", nil)
	if envLookup(scrubbed, "GITHUB_TOKEN") != "" {
		t.Errorf("GITHUB_TOKEN survived scrub: %v", scrubbed)
	}
	if envLookup(scrubbed, "PATH") == "" {
		t.Error("PATH was scrubbed; non-credential vars must pass through")
	}

	granted := buildChildEnv(caps.Profile{Grants: []caps.Token{"secret.github_token"}}, "", nil)
	if got := envLookup(granted, "GITHUB_TOKEN"); got != "ghp_secret_value" {
		t.Errorf("GITHUB_TOKEN with grant = %q, want ghp_secret_value", got)
	}
}

// TestBuildChildEnv_ScrubsAllKnownCredentialPrefixes: every deny-list prefix is
// stripped unless re-added.
func TestBuildChildEnv_ScrubsAllKnownCredentialPrefixes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("MY_APP_TOKEN", "tok")
	t.Setenv("SOME_PASSWORD", "hunter2")

	scrubbed := buildChildEnv(caps.Profile{}, "", nil)
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MY_APP_TOKEN", "SOME_PASSWORD"} {
		if envLookup(scrubbed, key) != "" {
			t.Errorf("%s survived scrub", key)
		}
	}
}

// TestApplyTimeouts_HardTimeoutWrapsCtx: a non-zero HardTimeout wraps ctx with
// a deadline; zero leaves ctx un-deadlined.
func TestApplyTimeouts_HardTimeoutWrapsCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := applyTimeouts(t.Context(), caps.Profile{HardTimeout: 50 * time.Millisecond})
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("HardTimeout set but ctx has no deadline")
	}

	ctxNoTimeout, cancelNoTimeout := applyTimeouts(t.Context(), caps.Profile{})
	defer cancelNoTimeout()
	if _, ok := ctxNoTimeout.Deadline(); ok {
		t.Error("zero HardTimeout produced a deadline")
	}
}

// envLookup extracts the value for key from a "key=value" slice.
func envLookup(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

// countEnv counts how many entries define key.
func countEnv(env []string, key string) int {
	prefix := key + "="
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}
	return count
}
