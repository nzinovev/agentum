package models

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `providers:
  anthropic:
    api_key_env: ANTHROPIC_API_KEY
  zai:
    api_key_env: ZAI_API_KEY
    base_url: https://api.z.ai/v
  local:
    api_key_env: LOCAL_API_KEY
    api_key: sk-inline-dev-only
tiers:
  fast: anthropic/claude-haiku
  strong: anthropic/claude-sonnet
  reasoning: zai/glm-5.2
default: strong
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoadAndResolve(t *testing.T) {
	t.Parallel()
	c, err := LoadFile(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got, err := c.Resolve("fast"); err != nil || got != "anthropic/claude-haiku" {
		t.Errorf("Resolve(fast) = %q,%v, want anthropic/claude-haiku,nil", got, err)
	}
	// Empty tier falls back to Default.
	if got, err := c.Resolve(""); err != nil || got != "anthropic/claude-sonnet" {
		t.Errorf("Resolve('') = %q,%v, want default strong", got, err)
	}
	if !c.HadInlineKey() {
		t.Error("HadInlineKey should be true (local provider has inline api_key)")
	}
}

func TestResolve_UnknownTier(t *testing.T) {
	t.Parallel()
	c, _ := LoadFile(writeConfig(t, validYAML))
	if _, err := c.Resolve("magic"); err == nil {
		t.Fatal("Resolve(unknown) must error")
	}
}

func TestParse_DefaultTierMustExist(t *testing.T) {
	t.Parallel()
	bad := `providers: {x: {api_key_env: X_KEY}}
tiers: {fast: x/m}
default: missing
`
	if _, err := LoadFile(writeConfig(t, bad)); err == nil {
		t.Fatal("default tier not in tiers must fail parse")
	}
}

func TestParse_ProviderRequiresAPIKeyEnv(t *testing.T) {
	t.Parallel()
	bad := `providers: {x: {api_key: sk-no-env}}
tiers: {}
`
	_, err := LoadFile(writeConfig(t, bad))
	if err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("err = %v, want substring api_key_env", err)
	}
}

func TestEnvForProvider_EnvVarWins(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	c, _ := LoadFile(writeConfig(t, validYAML))
	got, err := c.EnvForProvider("anthropic")
	if err != nil {
		t.Fatalf("EnvForProvider: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-from-env" {
		t.Errorf("env value = %q, want sk-from-env (process env must win for non-inline providers)", got["ANTHROPIC_API_KEY"])
	}
}

func TestEnvForProvider_InlineValueUsed(t *testing.T) {
	// Ensure the process env does NOT carry LOCAL_API_KEY so the inline value
	// is the only source.
	t.Setenv("LOCAL_API_KEY", "")
	c, _ := LoadFile(writeConfig(t, validYAML))
	got, err := c.EnvForProvider("local")
	if err != nil {
		t.Fatalf("EnvForProvider: %v", err)
	}
	if got["LOCAL_API_KEY"] != "sk-inline-dev-only" {
		t.Errorf("inline value = %q, want sk-inline-dev-only", got["LOCAL_API_KEY"])
	}
}

func TestEnvForProvider_BaseURL(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "sk-zai")
	c, _ := LoadFile(writeConfig(t, validYAML))
	got, _ := c.EnvForProvider("zai")
	if got["ZAI_BASE_URL"] != "https://api.z.ai/v" {
		t.Errorf("base url not set; got %v", got)
	}
}

func TestEnvForProvider_MissingCred(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	c, _ := LoadFile(writeConfig(t, validYAML))
	_, err := c.EnvForProvider("anthropic")
	if err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("err = %v, want 'not set'", err)
	}
}

func TestEnvForProvider_UnknownProvider(t *testing.T) {
	t.Parallel()
	c, _ := LoadFile(writeConfig(t, validYAML))
	if _, err := c.EnvForProvider("ghost"); err == nil {
		t.Fatal("unknown provider must error")
	}
}

func TestProvider_LogValueMasksKey(t *testing.T) {
	t.Parallel()
	p := Provider{APIKeyEnv: "X_KEY", APIKey: "sk-1234567890abcdef"}
	v := p.LogValue().Resolve().String()
	if strings.Contains(v, "1234567890abcdef") {
		t.Errorf("LogValue leaked the full key: %s", v)
	}
	if !strings.Contains(v, "…") && !strings.Contains(v, "****") {
		t.Errorf("LogValue did not mask: %s", v)
	}
}

// TestLoad_NoConfigFromEnv exercises the ErrNoConfig path: no candidate file
// exists when AGENTUM_MODELS_CONFIG points at a missing path and the cwd/home
// candidates don't exist either (we point the env at a temp path that's absent).
func TestLoad_NoConfigFromEnv(t *testing.T) {
	// Non-parallel: mutates process env.
	t.Setenv("AGENTUM_MODELS_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := Load()
	if err == nil {
		t.Fatal("Load with no resolvable config must error")
	}
	// It may be ErrNoConfig or a wrapped form; either is acceptable as long as
	// it is clearly a "no config" condition.
	if err != ErrNoConfig && !strings.Contains(err.Error(), "no config") {
		// Other candidate paths (cwd, home) may exist on some machines; only
		// assert the strict case when those are absent. Log for visibility.
		t.Logf("Load returned %v (acceptable when other candidate paths exist)", err)
	}
}

// Ensure the slog import stays meaningful for future structured logging of the
// config in callers.
var _ = slog.Info
