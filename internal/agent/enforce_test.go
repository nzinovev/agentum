package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// allHostProfile is a profile an implementer would receive from a host that
// supports every category. Used by the rendering tests below.
func implementerProfile(t *testing.T) caps.Profile {
	t.Helper()
	profile := caps.Effective(caps.Input{
		Host:  opencodeSupported,
		Pack:  []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatFsWrite), caps.Token(caps.CatGitWrite), caps.Token(caps.CatExecBash), caps.Token(caps.CatArtifactWrite)},
		Stage: []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatFsWrite), caps.Token(caps.CatGitWrite), caps.Token(caps.CatExecBash), caps.Token(caps.CatArtifactWrite)},
		Role:  caps.RoleImplementer,
	})
	if !profile.Has(caps.CatFsWrite) || !profile.Has(caps.CatExecBash) {
		t.Fatalf("test fixture: implementer profile missing expected grants: %v", profile.Grants)
	}
	return profile
}

// TestRenderConfig_DenyByDefault: an empty profile renders every tool as
// "deny". This is the load-bearing rendering invariant.
func TestRenderConfig_DenyByDefault(t *testing.T) {
	t.Parallel()
	rendered, err := renderOpencodeConfig(caps.Profile{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(rendered, &cfg); err != nil {
		t.Fatalf("parse rendered config: %v", err)
	}
	if cfg.Permission.Read != "deny" {
		t.Errorf("read = %q, want deny", cfg.Permission.Read)
	}
	if cfg.Permission.WebFetch != "deny" {
		t.Errorf("webfetch = %q, want deny", cfg.Permission.WebFetch)
	}
	if cfg.Permission.Edit != "deny" {
		t.Errorf("edit = %q, want deny", cfg.Permission.Edit)
	}
	if cfg.Permission.Bash != "deny" {
		t.Errorf("bash = %q, want deny", cfg.Permission.Bash)
	}
	if cfg.MCP != nil {
		t.Errorf("mcp section must be absent for empty profile, got %v", cfg.MCP)
	}
}

// decodeBashRules extracts the bash rule map from a rendered config, failing
// the test if the rule is not a map. JSON unmarshaling into the `any`-typed
// field produces map[string]any.
func decodeBashRules(t *testing.T, rendered []byte) map[string]string {
	t.Helper()
	var cfg opencodeConfig
	if err := json.Unmarshal(rendered, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	raw, ok := cfg.Permission.Bash.(map[string]any)
	if !ok {
		t.Fatalf("bash rule = %T, want map", cfg.Permission.Bash)
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		out[key] = value.(string)
	}
	return out
}

// TestRenderConfig_AnalystCanReadCannotWrite: analyst (read + own artifact)
// renders read allowed, edit denied, artifact writes allowed.
func TestRenderConfig_AnalystCanReadCannotWrite(t *testing.T) {
	t.Parallel()
	profile := caps.Effective(caps.Input{
		Host:  opencodeSupported,
		Pack:  []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatArtifactWrite)},
		Stage: []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatArtifactWrite)},
		Role:  caps.RoleAnalyst,
	})
	rendered, renderErr := renderOpencodeConfig(profile)
	if renderErr != nil {
		t.Fatalf("render: %v", renderErr)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(rendered, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Permission.Read != "allow" {
		t.Errorf("read = %q, want allow", cfg.Permission.Read)
	}
	if cfg.Permission.Edit != "deny" {
		t.Errorf("analyst edit = %q, want deny (no source writes)", cfg.Permission.Edit)
	}
	if cfg.Permission.Bash != "deny" {
		t.Errorf("analyst bash = %q, want deny", cfg.Permission.Bash)
	}
	writeRules, ok := cfg.Permission.Write.(map[string]any)
	if !ok {
		t.Fatalf("write rule = %T, want map for artifact writes", cfg.Permission.Write)
	}
	if writeRules["**"] != "deny" {
		t.Errorf("write default = %q, want deny (only artifact dir allowed)", writeRules["**"])
	}
}

// TestRenderConfig_ImplementerBashDeniesDeliveryRefMutation: the bash rule map
// includes deny patterns for every command that would mutate a delivery ref.
// This is the "reviewer/implementer cannot change delivery refs" enforcement.
func TestRenderConfig_ImplementerBashDeniesDeliveryRefMutation(t *testing.T) {
	t.Parallel()
	profile := implementerProfile(t)
	bashRules := decodeBashRules(t, mustRender(t, profile))
	if bashRules["*"] != "allow" {
		t.Errorf("bash default = %q, want allow", bashRules["*"])
	}
	requiredDenials := []string{
		"git push", "git reset --hard", "git branch -D",
		"git update-ref", "git rebase",
	}
	for _, denied := range requiredDenials {
		if bashRules[denied] != "deny" {
			t.Errorf("bash rule %q = %q, want deny", denied, bashRules[denied])
		}
	}
}

// mustRender renders the profile, failing the test on any error.
func mustRender(t *testing.T, profile caps.Profile) []byte {
	t.Helper()
	rendered, err := renderOpencodeConfig(profile)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return rendered
}

// TestRenderConfig_NetFetchAllowedOnlyWhenGranted: net.fetch maps to the
// webfetch tool; an implementer without net.fetch in its pack/stage gets it
// denied, an invocation grant that survives intersection turns it to allow.
func TestRenderConfig_NetFetchAllowedOnlyWhenGranted(t *testing.T) {
	t.Parallel()
	denied := caps.Effective(caps.Input{
		Host: opencodeSupported,
		Pack: []caps.Token{caps.Token(caps.CatFsRead)}, Stage: []caps.Token{caps.Token(caps.CatFsRead)},
		Role: caps.RoleAnalyst,
	})
	rendered, _ := renderOpencodeConfig(denied)
	var cfg opencodeConfig
	_ = json.Unmarshal(rendered, &cfg)
	if cfg.Permission.WebFetch != "deny" {
		t.Errorf("webfetch without grant = %q, want deny", cfg.Permission.WebFetch)
	}

	allowed := caps.Effective(caps.Input{
		Host:       opencodeSupported,
		Pack:       []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatNetFetch)},
		Stage:      []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatNetFetch)},
		Role:       caps.RoleAnalyst,
		Invocation: []caps.Token{caps.Token(caps.CatNetFetch)},
	})
	if !allowed.Has(caps.CatNetFetch) {
		t.Fatalf("fixture: net.fetch did not survive intersection: %v", allowed.Grants)
	}
	rendered, _ = renderOpencodeConfig(allowed)
	_ = json.Unmarshal(rendered, &cfg)
	if cfg.Permission.WebFetch != "allow" {
		t.Errorf("webfetch with grant = %q, want allow", cfg.Permission.WebFetch)
	}
}

// TestRenderConfig_MCPOnlyGrantedServers: the mcp section lists only servers
// the profile explicitly grants.
func TestRenderConfig_MCPOnlyGrantedServers(t *testing.T) {
	t.Parallel()
	profile := caps.Profile{Grants: []caps.Token{"mcp.github", "mcp.filesystem"}}
	rendered, renderErr := renderOpencodeConfig(profile)
	if renderErr != nil {
		t.Fatalf("render: %v", renderErr)
	}
	var cfg opencodeConfig
	if err := json.Unmarshal(rendered, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := cfg.MCP["github"]; !ok {
		t.Errorf("mcp.github missing from %v", cfg.MCP)
	}
	if _, ok := cfg.MCP["filesystem"]; !ok {
		t.Errorf("mcp.filesystem missing from %v", cfg.MCP)
	}
	if len(cfg.MCP) != 2 {
		t.Errorf("mcp section = %v, want exactly 2 servers", cfg.MCP)
	}
}

// TestBuildChildEnv_ScrubsCredentialsUnlessGranted: GITHUB_TOKEN is stripped by
// default and re-added only when secret.github_token is granted.
func TestBuildChildEnv_ScrubsCredentialsUnlessGranted(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret_value")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin") // non-credential, must survive

	scrubbed := buildChildEnv(caps.Profile{})
	if envLookup(scrubbed, "GITHUB_TOKEN") != "" {
		t.Errorf("GITHUB_TOKEN survived scrub: %v", scrubbed)
	}
	if envLookup(scrubbed, "PATH") == "" {
		t.Errorf("PATH was scrubbed; non-credential vars must pass through")
	}

	granted := buildChildEnv(caps.Profile{Grants: []caps.Token{"secret.github_token"}})
	if got := envLookup(granted, "GITHUB_TOKEN"); got != "ghp_secret_value" {
		t.Errorf("GITHUB_TOKEN with grant = %q, want ghp_secret_value", got)
	}
}

// TestBuildChildEnv_ScrubsAllKnownCredentialPrefixes: every deny-list prefix
// is stripped unless re-added.
func TestBuildChildEnv_ScrubsAllKnownCredentialPrefixes(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("MY_APP_TOKEN", "tok")
	t.Setenv("SOME_PASSWORD", "hunter2")

	scrubbed := buildChildEnv(caps.Profile{})
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MY_APP_TOKEN", "SOME_PASSWORD"} {
		if envLookup(scrubbed, key) != "" {
			t.Errorf("%s survived scrub", key)
		}
	}
}

// TestPrepareEnforcement_RefusesUnenforceableProfile: a profile granting
// git.delivery (orchestrator-only) to an agent is rejected before any
// subprocess starts.
func TestPrepareEnforcement_RefusesUnenforceableProfile(t *testing.T) {
	t.Parallel()
	inv := Invocation{
		Workdir:     t.TempDir(),
		ArtifactDir: filepath.Join(t.TempDir(), "artifacts"),
		Profile:     caps.Profile{Grants: []caps.Token{caps.Token(caps.CatGitDelivery)}},
	}
	_, err := prepareEnforcement(inv)
	if err == nil {
		t.Fatal("expected unenforceable error for git.delivery, got nil")
	}
	if !strings.Contains(err.Error(), "cannot enforce") && !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error message = %q, want an enforceability failure", err.Error())
	}
}

// TestPrepareEnforcement_WritesScopedConfig: a real enforceable profile writes
// a .opencode/opencode.json into the worktree with the worktree scope
// substituted into the fs.write rule.
func TestPrepareEnforcement_WritesScopedConfig(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	artifact := filepath.Join(workdir, ".agentum", "artifacts", "spec")
	profile := implementerProfile(t)

	plan, err := prepareEnforcement(Invocation{
		Workdir: workdir, ArtifactDir: artifact, Profile: profile,
	})
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	if plan.configPath != filepath.Join(workdir, ".opencode", "opencode.json") {
		t.Errorf("configPath = %q, want .opencode/opencode.json in worktree", plan.configPath)
	}
	if _, statErr := os.Stat(plan.configPath); statErr != nil {
		t.Errorf("config file not written: %v", statErr)
	}
	// The substituted audit profile must carry the absolute worktree scope, not
	// the placeholder.
	for _, granted := range plan.audit.Grants {
		if strings.Contains(string(granted), caps.WorktreeScope) {
			t.Errorf("audit grant still has placeholder: %q", granted)
		}
	}
	// The rendered config references the worktree path for fs.write. Compared
	// against the JSON-encoded form: on Windows the path separators are escaped
	// inside the rendered config, so a raw-string comparison fails there for a
	// config that is in fact correct.
	if !strings.Contains(string(plan.config), jsonPath(t, workdir)) {
		t.Errorf("rendered config does not reference the worktree path: %s", plan.config)
	}
}

// jsonPath renders a filesystem path the way it appears inside JSON, so path
// assertions hold on every host the adapter builds for.
func jsonPath(t *testing.T, path string) string {
	t.Helper()
	encoded, err := json.Marshal(path)
	if err != nil {
		t.Fatalf("encode path %q: %v", path, err)
	}
	return strings.Trim(string(encoded), `"`)
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
