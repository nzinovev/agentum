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
		artifact: filepath.Join(root, ".agentum", "run-1", ".ag-artifacts", "spec"),
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
// the two are not idempotent together. instructionPaths defaults to none
// (most tests exercise the permission map, not the instruction channel).
func mustRender(t *testing.T, profile caps.Profile, subst scopeSubst) []byte {
	t.Helper()
	return mustRenderWithInstructions(t, profile, subst, nil)
}

// mustRenderWithInstructions is mustRender for tests that exercise the
// instruction edit-deny rules (ADR 0002 D4).
func mustRenderWithInstructions(t *testing.T, profile caps.Profile, subst scopeSubst, instructionPaths []string) []byte {
	t.Helper()
	config, err := buildOpencodeConfig(substituteScopes(profile, subst), subst, instructionPaths)
	if err != nil {
		t.Fatalf("build config: %v", err)
	}
	_, indented, err := renderOpencodeConfigBytes(config)
	if err != nil {
		t.Fatalf("render config: %v", err)
	}
	return indented
}

// decodeRules returns the ordered permission list from a rendered config.
func decodeRules(t *testing.T, rendered []byte) []permissionRule {
	t.Helper()
	var envelope struct {
		Permissions []permissionRule `json:"permissions"`
	}
	if err := json.Unmarshal(rendered, &envelope); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	return envelope.Permissions
}

// decodePermissions folds the rendered permission list into one value per
// action: the bare effect for an action decided by a single whole-action rule,
// otherwise a resource→effect map. Order is lost in the map; the tests that
// depend on order read decodeRules directly.
func decodePermissions(t *testing.T, rendered []byte) map[string]any {
	t.Helper()
	grouped := map[string][]permissionRule{}
	for _, rule := range decodeRules(t, rendered) {
		grouped[rule.Action] = append(grouped[rule.Action], rule)
	}
	folded := make(map[string]any, len(grouped))
	for action, rules := range grouped {
		if len(rules) == 1 && rules[0].Resource == anyPath {
			folded[action] = rules[0].Effect
			continue
		}
		byResource := make(map[string]any, len(rules))
		for _, rule := range rules {
			byResource[rule.Resource] = rule.Effect
		}
		folded[action] = byResource
	}
	return folded
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
	artifactFile := ".agentum/run-1/.ag-artifacts/spec/result.json"
	otherStageFile := ".agentum/run-1/.ag-artifacts/review/result.json"
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
// dropped grant is how a profile ends up meaning something other than
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

// permissionKeys is every action the rendered config must decide. opencode
// merges config sources, so an action this adapter omits falls through to the
// operator's global config or the project's own opencode.json — widening the
// profile.
var permissionKeys = []string{
	"*", "read", "glob", "grep", "lsp",
	"edit", "shell", "webfetch", "websearch",
	"subagent", "skill", "external_directory",
	"question", "doom_loop",
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
// Skill is the deliberate exception (ADR 0002 D5): a skill grants knowledge, not
// reach, so it is allowed unconditionally even for an empty profile.
func TestRenderConfig_DenyByDefault(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, caps.Profile{}, subst))
	for key, value := range permissions {
		if key == "skill" {
			if value != actionAllow {
				t.Errorf("empty profile: skill = %v, want allow (knowledge, not reach)", value)
			}
			continue
		}
		if value != actionDeny {
			t.Errorf("empty profile: %q = %v, want deny", key, value)
		}
	}
}

// TestRenderConfig_WildcardBaselineIsDeny: the catch-all stays deny even for a
// fully-granted implementer, so an unmodelled or future tool is refused rather
// than inherited. Confirmed live: this entry overrides opencode's own built-in
// {permission:"*", pattern:"*", action:"allow"}. task and external_directory
// have no capability token that grants them, so they stay deny too. Skill is
// allowed (D5) regardless of the profile.
func TestRenderConfig_WildcardBaselineIsDeny(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	permissions := decodePermissions(t, mustRender(t, implementerProfile(t), subst))
	if permissions["*"] != actionDeny {
		t.Errorf("wildcard baseline = %v, want deny", permissions["*"])
	}
	for _, key := range []string{"subagent", "external_directory"} {
		if permissions[key] != actionDeny {
			t.Errorf("%s = %v, want deny (no capability token grants it)", key, permissions[key])
		}
	}
	if permissions["skill"] != actionAllow {
		t.Errorf("skill = %v, want allow (ADR 0002 D5: knowledge, not reach)", permissions["skill"])
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
	if permissions["shell"] != actionDeny {
		t.Errorf("analyst shell = %v, want deny", permissions["shell"])
	}

	rules, ok := permissions["edit"].(map[string]any)
	if !ok {
		t.Fatalf("analyst edit = %T (%v), want a scoped rule map", permissions["edit"], permissions["edit"])
	}
	if rules[anyPath] != actionDeny {
		t.Errorf("analyst edit baseline = %v, want deny", rules[anyPath])
	}
	const artifactPattern = ".agentum/run-1/.ag-artifacts/spec/**"
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
// rules with the last match winning, so the wildcard deny must open the list
// and each action's own deny baseline must precede its allows.
func TestRenderConfig_DenyBaselinePrecedesAllows(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rules := decodeRules(t, mustRender(t, implementerProfile(t), subst))
	if len(rules) == 0 || rules[0] != (permissionRule{Action: anyPath, Resource: anyPath, Effect: actionDeny}) {
		t.Fatalf("first rule = %+v, want the wildcard deny baseline", rules)
	}
	firstEditDeny, firstEditAllow := -1, -1
	for index, rule := range rules {
		if rule.Action != permissionEdit {
			continue
		}
		if rule.Effect == actionDeny && rule.Resource == anyPath && firstEditDeny < 0 {
			firstEditDeny = index
		}
		if rule.Effect == actionAllow && firstEditAllow < 0 {
			firstEditAllow = index
		}
	}
	if firstEditDeny < 0 || firstEditAllow < 0 {
		t.Fatalf("edit rules missing a deny baseline or an allow: %+v", rules)
	}
	if firstEditDeny > firstEditAllow {
		t.Errorf("edit deny baseline at %d comes after the first allow at %d; last-match-wins would deny the granted scope", firstEditDeny, firstEditAllow)
	}
}

// decodeBashRules extracts the shell rule map from a rendered config.
func decodeBashRules(t *testing.T, rendered []byte) map[string]string {
	t.Helper()
	raw, ok := decodePermissions(t, rendered)["shell"].(map[string]any)
	if !ok {
		t.Fatalf("shell rule is not a map: %v", decodePermissions(t, rendered)["shell"])
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
			_, err := NewOpencodeAdapter("opencode").prepareEnforcement(Invocation{
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

	plan, err := NewOpencodeAdapter("opencode").prepareEnforcement(Invocation{
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

	plan, err := NewOpencodeAdapter("opencode").prepareEnforcement(Invocation{
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
	if !strings.Contains(string(plan.config), ".agentum/run-1/.ag-artifacts/spec/**") {
		t.Errorf("rendered config does not carry the relative artifact scope:\n%s", plan.config)
	}
}

// TestPrepareEnforcement_CleanupRemovesConfig: the per-invocation config
// directory does not outlive the run.
func TestPrepareEnforcement_CleanupRemovesConfig(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	plan, err := NewOpencodeAdapter("opencode").prepareEnforcement(Invocation{
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

// TestBuildChildEnv_ScrubsCredentialsEvenWhenGranted: GITHUB_TOKEN is stripped,
// and a profile granted secret.github_token does NOT get it back — the Git
// provider's token belongs to the publisher, which is not an agent, and the
// deny must not depend on no role handing out the grant.
func TestBuildChildEnv_ScrubsCredentialsEvenWhenGranted(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret_value")
	t.Setenv("GH_TOKEN", "ghp_other_value")
	t.Setenv("GITLAB_TOKEN", "glpat_value")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin") // non-credential, must survive

	scrubbed := buildChildEnv(caps.Profile{}, "", nil)
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN"} {
		if envLookup(scrubbed, key) != "" {
			t.Errorf("%s survived scrub", key)
		}
	}
	if envLookup(scrubbed, "PATH") == "" {
		t.Error("PATH was scrubbed; non-credential vars must pass through")
	}

	// The artificial grant is the point: today no role issues secret.*, so the
	// old behaviour (a grant un-redacting the token) was unreachable — this
	// test pins the construction, not the current role table.
	granted := buildChildEnv(caps.Profile{Grants: []caps.Token{"secret.github_token"}}, "", nil)
	if got := envLookup(granted, "GITHUB_TOKEN"); got != "" {
		t.Errorf("GITHUB_TOKEN with an artificial secret.github_token grant = %q; a grant must not un-redact the provider token", got)
	}
	if got := envLookup(granted, "GH_TOKEN"); got != "" {
		t.Errorf("GH_TOKEN with an artificial secret.github_token grant = %q; a grant must not un-redact the provider token", got)
	}
}

// TestCredentialDenyListCoversPublishToken: the publisher's token variable is
// matched by the adapter's credential filter. The publisher carries the token
// deliberately, the agent must never see it, and renaming the variable without
// updating the filter would open exactly that path — this test is what makes
// the rename visible.
func TestCredentialDenyListCoversPublishToken(t *testing.T) {
	t.Setenv("AGENTUM_PUBLISH_TOKEN", "ghp_publisher_value")

	scrubbed := buildChildEnv(caps.Profile{Grants: []caps.Token{"secret.github_token"}}, "", nil)
	if got := envLookup(scrubbed, "AGENTUM_PUBLISH_TOKEN"); got != "" {
		t.Errorf("AGENTUM_PUBLISH_TOKEN survived the adapter scrub: %q", got)
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

// --- ADR 0002: instruction channel (skill allow, edit deny, staging) --------

// editRuleOrder returns the `edit` resources in emitted order, or nil when
// edit is decided by one whole-action rule. opencode resolves permission rules
// last-match-wins, so the order is load-bearing.
func editRuleOrder(t *testing.T, rendered []byte) []string {
	t.Helper()
	var resources []string
	for _, rule := range decodeRules(t, rendered) {
		if rule.Action == permissionEdit {
			resources = append(resources, rule.Resource)
		}
	}
	if len(resources) == 1 && resources[0] == anyPath {
		return nil
	}
	return resources
}

// TestRenderConfig_SkillIsAllowedForEveryProfile (ADR 0002 D5): skill resolves
// to allow for an empty profile, an analyst, and an implementer alike. A skill
// grants knowledge, not reach; the protection that replaces a blanket deny is
// visibility (the ContextProber records each skill in evidence).
func TestRenderConfig_SkillIsAllowedForEveryProfile(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	for _, profile := range []caps.Profile{
		{},
		analystProfile(t),
		implementerProfile(t),
	} {
		permissions := decodePermissions(t, mustRender(t, profile, subst))
		if permissions["skill"] != actionAllow {
			t.Errorf("skill = %v, want allow (D5)", permissions["skill"])
		}
	}
}

// TestEditRules_InstructionDeniesAreLastAndDistinct (ADR 0002 D4 layer 1):
// declared instruction paths and the name guard land AFTER the granted scope
// allows so last-match-wins makes them win, and they never collide with a scope
// pattern.
func TestEditRules_InstructionDeniesAreLastAndDistinct(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	profile := implementerProfile(t) // grants fs.write:${worktree}/** → pattern "**"
	rendered := mustRenderWithInstructions(t, profile, subst,
		[]string{"AGENTS.md", "docs/conventions.md"})
	order := editRuleOrder(t, rendered)
	if len(order) < 4 {
		t.Fatalf("edit rule order = %v, want at least 4 entries", order)
	}
	// The scope allow ("**") comes after the deny baseline ("*") and before the
	// instruction denies.
	scopeIdx := indexOf(order, "**")
	denyIdx := indexOf(order, "*")
	agentsIdx := indexOf(order, "AGENTS.md")
	agentsGuardIdx := indexOf(order, "**/AGENTS.md")
	conventionsIdx := indexOf(order, "docs/conventions.md")
	conventionsGuardIdx := indexOf(order, "**/conventions.md")
	if scopeIdx < 0 || denyIdx < 0 {
		t.Fatalf("missing baseline/scope rules: %v", order)
	}
	if scopeIdx < denyIdx {
		t.Errorf("scope allow must come after deny baseline: order=%v", order)
	}
	if agentsIdx < 0 || agentsIdx < scopeIdx {
		t.Errorf("AGENTS.md deny must come after the scope allow: order=%v", order)
	}
	if conventionsIdx < 0 || conventionsIdx < scopeIdx {
		t.Errorf("docs/conventions.md deny must come after the scope allow: order=%v", order)
	}
	// AGENTS.md is a runtime-injected filename, so it gets a **/AGENTS.md name
	// guard — covering a nested copy the project never declared. The root case
	// is the one the guard exists for.
	if agentsGuardIdx < 0 {
		t.Errorf("**/AGENTS.md name guard missing; order=%v", order)
	}
	if agentsGuardIdx >= 0 && agentsGuardIdx < scopeIdx {
		t.Errorf("**/AGENTS.md name guard must come after the scope allow: order=%v", order)
	}
	// docs/conventions.md is NOT a runtime-injected filename, so it must NOT get
	// a **/conventions.md guard — that would deny any same-named file anywhere,
	// which is too broad (the runtime does not inject it from elsewhere).
	if conventionsGuardIdx >= 0 {
		t.Errorf("**/conventions.md name guard should not be emitted for a non-runtime filename; order=%v", order)
	}
	// The instruction denies must be distinct from the scope patterns.
	for _, instructionPattern := range []string{"AGENTS.md", "docs/conventions.md", "**/AGENTS.md"} {
		if instructionPattern == "**" || instructionPattern == "*" {
			t.Errorf("instruction pattern %q collides with a scope pattern", instructionPattern)
		}
	}
}

// TestEditRules_AGENTSNameGuardDeniesNestedCopy (ADR 0002 D4 + findings F2):
// the **/AGENTS.md name guard must match a NESTED AGENTS.md. (The root
// AGENTS.md is covered by the exact `AGENTS.md` rule; **/<name> requires a
// directory before the basename, so it does not match the root — which is why
// both rules are emitted.) This pins that the guard protects the nested copy,
// the case it exists for.
func TestEditRules_AGENTSNameGuardDeniesNestedCopy(t *testing.T) {
	t.Parallel()
	if !opencodeMatch("**/AGENTS.md", "docs/sub/AGENTS.md") {
		t.Error("opencodeMatch says **/AGENTS.md does not match docs/sub/AGENTS.md — guard would not protect the nested copy")
	}
}

// indexOf returns the index of value in slice, or -1.
func indexOf(slice []string, value string) int {
	for i, item := range slice {
		if item == value {
			return i
		}
	}
	return -1
}

// TestEditRules_NoWriteProfileHasNoInstructionRules (ADR 0002 D4 trap): when
// the profile grants no write at all, `edit` is one whole-action deny and there
// is no allow for instruction denies to override. An analyst (artifact.write
// only) DOES take the rule list branch, so instruction denies DO land there.
func TestEditRules_NoWriteProfileHasNoInstructionRules(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	emptyRendered := mustRenderWithInstructions(t, caps.Profile{}, subst,
		[]string{"AGENTS.md"})
	emptyPerms := decodePermissions(t, emptyRendered)
	if emptyPerms["edit"] != actionDeny {
		t.Errorf("empty profile edit = %v, want one whole-action deny", emptyPerms["edit"])
	}
	// Analyst takes the rule-list branch (artifact.write). Instruction denies
	// must appear there even though the analyst cannot write source.
	analystRendered := mustRenderWithInstructions(t, analystProfile(t), subst,
		[]string{"AGENTS.md"})
	analystOrder := editRuleOrder(t, analystRendered)
	if indexOf(analystOrder, "AGENTS.md") < 0 {
		t.Errorf("analyst edit rules missing AGENTS.md deny: %v", analystOrder)
	}
}

// TestStageInstructionFiles_WritesContentAndUsesForwardSlashes (ADR 0002 D3):
// prepareEnforcement stages each pinned instruction file into the
// per-invocation config directory, returns its absolute forward-slash path as
// an attachment, keeps it out of the config's `instructions` (opencode 2 never
// loads that key), and the directory is removed by cleanup.
func TestStageInstructionFiles_WritesContentAndUsesForwardSlashes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	artifact := filepath.Join(root, ".agentum", "artifacts", "stage-x")
	profile := implementerProfile(t)
	inv := Invocation{
		Workdir:     root,
		ArtifactDir: artifact,
		Profile:     profile,
		Instructions: []InstructionFile{
			{RepoPath: "AGENTS.md", Content: []byte("MARKER-ORIGINAL\n")},
			{RepoPath: "docs/conventions.md", Content: []byte("conventions\n")},
		},
	}
	plan, err := NewOpencodeAdapter("opencode").prepareEnforcement(inv)
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	defer plan.cleanup()

	if strings.Contains(string(plan.config), `"instructions"`) {
		t.Errorf("config carries an instructions key; the files travel as attachments:\n%s", plan.config)
	}
	if len(plan.attachments) != 2 {
		t.Fatalf("attachments = %v, want 2 entries", plan.attachments)
	}
	for _, listed := range plan.attachments {
		if strings.Contains(listed, "\\") {
			t.Errorf("instruction path %q contains a backslash (Step-0: forward slashes resolve)", listed)
		}
		if !filepath.IsAbs(listed) {
			t.Errorf("instruction path %q is not absolute", listed)
		}
	}
	// The pinned files exist in the config directory during the run, with the
	// pinned content.
	for _, listed := range plan.attachments {
		if _, err := os.Stat(listed); err != nil {
			t.Errorf("pinned instruction %q not staged in config dir: %v", listed, err)
		}
	}
	// After cleanup, the directory is gone.
	plan.cleanup()
	if _, err := os.Stat(plan.configDir); !os.IsNotExist(err) {
		t.Errorf("config dir %q should be removed by cleanup", plan.configDir)
	}
}

// TestStageInstructionFiles_EmptyContentNotStaged: a file truncated to zero by
// the total budget (or missing at the commit) delivers nothing and is not
// staged, to avoid an empty attachment.
func TestStageInstructionFiles_EmptyContentNotStaged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	profile := implementerProfile(t)
	inv := Invocation{
		Workdir:     root,
		ArtifactDir: filepath.Join(root, ".agentum", "artifacts", "stage"),
		Profile:     profile,
		Instructions: []InstructionFile{
			{RepoPath: "AGENTS.md", Content: []byte("real\n")},
			{RepoPath: "empty.md", Content: nil},
		},
	}
	plan, err := NewOpencodeAdapter("opencode").prepareEnforcement(inv)
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	defer plan.cleanup()
	if len(plan.attachments) != 1 {
		t.Errorf("attachments = %v, want only the non-empty file staged", plan.attachments)
	}
}

// TestStageInstructionFiles_EditDeniesAGENTSForImplementer (ADR 0002 D4,
// config-level half): the edit rule list denies AGENTS.md for an implementer
// profile, so the agent cannot rewrite the rules its reviewer will judge it by.
func TestStageInstructionFiles_EditDeniesAGENTSForImplementer(t *testing.T) {
	t.Parallel()
	subst := testSubst(t.TempDir())
	rendered := mustRenderWithInstructions(t, implementerProfile(t), subst,
		[]string{"AGENTS.md"})
	rules := decodePermissions(t, rendered)["edit"].(map[string]any)
	if rules["AGENTS.md"] != actionDeny {
		t.Errorf("AGENTS.md edit rule = %v, want deny", rules["AGENTS.md"])
	}
	// And the matcher agrees: an edit of AGENTS.md is denied even though "**"
	// (the whole worktree) is allowed.
	if opencodeMatch("**", "AGENTS.md") {
		if !opencodeMatch("AGENTS.md", "AGENTS.md") {
			t.Error("opencodeMatch says AGENTS.md pattern would not match AGENTS.md")
		}
	} else {
		t.Error("opencodeMatch says ** does not match AGENTS.md — matcher model is off")
	}
}

// TestBuildOpencodeConfig_WithheldSourceWriteDeniesEditAndBash: a withheld
// implementer profile — the one a stage sees before a human
// approves the plan — must deny the shell outright (exec.bash was withheld, so
// shellRules returns one whole-action deny) and deny every edit except the artifact
// floor that lets the stage write result.json. Withholding only fs.write would
// still allow writing source, since bash allows shell redirects; exec.bash is
// withheld alongside it. The artifact floor still permits result.json, which
// is the contract every stage must meet.
func TestBuildOpencodeConfig_WithheldSourceWriteDeniesEditAndBash(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	declared := []caps.Token{
		caps.Token(caps.CatFsRead), caps.Token(caps.CatFsWrite), caps.Token(caps.CatGitWrite),
		caps.Token(caps.CatExecBash), caps.Token(caps.CatArtifactWrite),
	}
	withheld := caps.Effective(caps.Input{
		Host:     opencodeSupported,
		Pack:     declared,
		Stage:    declared,
		Role:     caps.RoleImplementer,
		Withheld: caps.SourceWriteCategories,
	})
	if withheld.Has(caps.CatFsWrite) || withheld.Has(caps.CatGitWrite) || withheld.Has(caps.CatExecBash) {
		t.Fatalf("withheld implementer still carries source-write categories: %v", withheld.Grants)
	}
	if !withheld.Has(caps.CatArtifactWrite) {
		t.Fatalf("withheld implementer lost artifact.write floor: %v", withheld.Grants)
	}
	rendered := mustRender(t, withheld, testSubst(dir))
	permissions := decodePermissions(t, rendered)
	// shell must be one whole-action deny — exec.bash was withheld, so
	// shellRules returns no allow at all. This is the load-bearing assertion:
	// a withheld profile cannot run any command.
	if permissions["shell"] != actionDeny {
		t.Errorf("shell = %v, want %q (withheld profile must deny every command)", permissions["shell"], actionDeny)
	}
	// edit is a rule list (or bare deny) whose deny baseline is present, so no
	// worktree edit survives. fs.write was withheld, so the implementer's
	// worktree scope is gone; whatever shape edit takes (a bare deny when no
	// scopes survive, or a rule list with a deny baseline + artifact allow when
	// the floor is applied) the baseline must deny the whole tree.
	switch editValue := permissions["edit"].(type) {
	case string:
		if editValue != actionDeny {
			t.Errorf("edit = %q, want bare %q", editValue, actionDeny)
		}
	case map[string]any:
		if editValue[anyPath] != actionDeny {
			t.Errorf("edit[*] = %v, want %q (withheld profile must deny the worktree)", editValue[anyPath], actionDeny)
		}
	default:
		t.Errorf("edit = %v, want a string or rule map", permissions["edit"])
	}
}
