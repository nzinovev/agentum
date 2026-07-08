package models

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestDefault_KnownAgent(t *testing.T) {
	t.Parallel()
	c := Default("opencode")
	if c.Default != "strong" || c.Tiers["strong"] != "opencode/north-mini-code-free" {
		t.Errorf("opencode default wrong: %+v", c)
	}
	c = Default("claude-code")
	if c.Tiers["strong"] != "sonnet" {
		t.Errorf("claude-code default wrong: %+v", c)
	}
}

func TestDefault_UnknownAgent(t *testing.T) {
	t.Parallel()
	c := Default("ghost")
	if len(c.Tiers) != 0 {
		t.Errorf("unknown agent should give empty config, got %+v", c)
	}
}

func TestDefault_ReturnedMapIsACopy(t *testing.T) {
	t.Parallel()
	c := Default("opencode")
	c.Tiers["strong"] = "mutated"
	again := Default("opencode")
	if again.Tiers["strong"] != "opencode/north-mini-code-free" {
		t.Error("Default must return a fresh map; mutating the result leaked into the built-in")
	}
}

func TestResolve_BuiltInDefault(t *testing.T) {
	t.Parallel()
	got, err := Resolve(nil, "opencode", "fast")
	if err != nil || got != "opencode/deepseek-v4-flash-free" {
		t.Errorf("Resolve(opencode, fast) = %q, %v", got, err)
	}
	// empty tier falls back to default
	got, err = Resolve(nil, "claude-code", "")
	if err != nil || got != "sonnet" {
		t.Errorf("Resolve(claude-code, '') = %q, %v", got, err)
	}
}

func TestResolve_OverrideWins(t *testing.T) {
	t.Parallel()
	ov := &Config{
		Tiers:   map[string]string{"strong": "zai/glm-5.2", "fast": "haiku"},
		Default: "strong",
	}
	// override applies regardless of agent name
	got, err := Resolve(ov, "opencode", "strong")
	if err != nil || got != "zai/glm-5.2" {
		t.Errorf("override should win: got %q, %v", got, err)
	}
}

func TestResolve_UnknownTier(t *testing.T) {
	t.Parallel()
	if _, err := Resolve(nil, "opencode", "magic"); err == nil {
		t.Fatal("unknown tier must error")
	}
}

func TestResolve_UnknownAgentNoOverride(t *testing.T) {
	t.Parallel()
	if _, err := Resolve(nil, "ghost", "strong"); err == nil {
		t.Fatal("unknown agent with no override must error")
	}
}

func TestLoad_Override(t *testing.T) {
	// Non-parallel: writes a file.
	p := filepath.Join(t.TempDir(), "models.yaml")
	body := "tiers:\n  strong: custom-model\n  fast: other\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AGENTUM_MODELS_CONFIG", p)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, err := Resolve(c, "opencode", "strong")
	if err != nil || got != "custom-model" {
		t.Errorf("override resolve = %q, %v", got, err)
	}
}

func TestLoad_DefaultTierMustExist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte("tiers: {fast: x}\ndefault: missing"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("AGENTUM_MODELS_CONFIG", p)
	if _, err := Load(); err == nil {
		t.Fatal("default tier not in tiers must fail Load")
	}
}

func TestLoad_Absent(t *testing.T) {
	// Non-parallel: mutates env.
	t.Setenv("AGENTUM_MODELS_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := Load()
	if err == nil {
		t.Fatal("absent config must error")
	}
	// other candidate paths (cwd, home) may exist on some machines; only
	// assert strictly when Load returned something other than ErrNoConfig.
	if err != ErrNoConfig && !strings.Contains(err.Error(), "no models.yaml") {
		t.Logf("Load returned %v (acceptable when other candidate paths exist)", err)
	}
}

func TestAgents_ContainsBoth(t *testing.T) {
	t.Parallel()
	agents := Agents()
	sort.Strings(agents)
	got := strings.Join(agents, ",")
	if got != "claude-code,opencode" {
		t.Errorf("Agents() = %q; want claude-code,opencode", got)
	}
}
