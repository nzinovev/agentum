// Package models resolves a tier name (e.g. "fast", "strong") to the model
// string Agentum passes to the agent binary's --model flag.
//
// Agentum is a coordinator, not a credential manager. The operator installs
// opencode / claude-code / etc. and configures providers in the agent binary
// itself (opencode auth, ~/.claude/settings.json, env vars — however the agent
// reads them). Agentum only decides the model name; the agent resolves it to a
// real provider+endpoint using the operator's own config.
//
// Resolution priority:
//  1. An operator override (models.yaml), if present — use its tiers.
//  2. Otherwise the baked-in default for the active agent.
//
// Baked defaults exist so the common case needs no configuration: clone,
// `make run`, and Agentum works because the operator's agent binary is already
// set up. See docs/models.md.
package models

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ErrNoConfig is returned by Load when no models.yaml is present. Callers fall
// back to the baked-in default for the active agent.
var ErrNoConfig = errors.New("models: no models.yaml; using built-in default")

// Config is a tier→model mapping plus the default tier.
type Config struct {
	Tiers   map[string]string `yaml:"tiers"`
	Default string            `yaml:"default"`
}

// builtIn holds the per-agent defaults. opencode's defaults use the free models
// on opencode Zen (the `-free` suffix is explicit) so a fresh install works
// without a paid provider once Zen is connected. claude-code uses its short
// aliases, which the operator's settings can remap to any compatible provider.
// Operators using other providers override via models.yaml.
var builtIn = map[string]Config{
	"opencode": {
		Tiers: map[string]string{
			"fast":      "opencode/deepseek-v4-flash-free",
			"strong":    "opencode/north-mini-code-free",
			"reasoning": "opencode/nemotron-3-ultra-free",
		},
		Default: "strong",
	},
	// claude-code accepts short model aliases that the operator's settings can
	// remap to any compatible provider.
	"claude-code": {
		Tiers: map[string]string{
			"fast":      "haiku",
			"strong":    "sonnet",
			"reasoning": "opus",
		},
		Default: "strong",
	},
}

// Default returns the baked-in default Config for an agent (e.g. "opencode").
// The returned Tiers map is a copy; callers may mutate it safely. Returns a
// zero Config when the agent is unknown.
func Default(agent string) Config {
	src, ok := builtIn[agent]
	if !ok {
		return Config{}
	}
	out := Config{Default: src.Default, Tiers: make(map[string]string, len(src.Tiers))}
	for k, v := range src.Tiers {
		out.Tiers[k] = v
	}
	return out
}

// Agents returns the agent names with baked-in defaults (e.g. ["claude-code",
// "opencode"]). Useful for docs/diagnostics.
func Agents() []string {
	out := make([]string, 0, len(builtIn))
	for k := range builtIn {
		out = append(out, k)
	}
	return out
}

// Load reads the operator override (models.yaml), if present. Resolution order
// of paths: AGENTUM_MODELS_CONFIG env, <cwd>/models.yaml, then
// $XDG_CONFIG_HOME/agentum/models.yaml or ~/.config/agentum/models.yaml.
// Returns ErrNoConfig (wrapped) when absent — callers fall back to Default(agent).
func Load() (*Config, error) {
	for _, p := range candidatePaths() {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("models: read %s: %w", p, err)
		}
		var c Config
		if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("models: parse %s: %w", p, err)
		}
		if c.Default != "" {
			if _, ok := c.Tiers[c.Default]; !ok {
				return nil, fmt.Errorf("models: default tier %q is not defined in tiers", c.Default)
			}
		}
		return &c, nil
	}
	return nil, ErrNoConfig
}

func candidatePaths() []string {
	out := []string{}
	if env := os.Getenv("AGENTUM_MODELS_CONFIG"); env != "" {
		out = append(out, env)
	}
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(cwd, "models.yaml"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, filepath.Join(xdg, "agentum", "models.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".config", "agentum", "models.yaml"))
	}
	return out
}

// Resolve resolves a tier to a model string. If override is non-nil, its tiers
// are used (with the override's default); otherwise the baked-in default for
// agent is used. An empty tier falls back to the applicable default. Unknown
// tier (or unknown agent with no override) is an error — Agentum never silently
// picks a model.
func Resolve(override *Config, agent, tier string) (string, error) {
	if override != nil {
		return resolveFrom(*override, tier)
	}
	cfg := Default(agent)
	if len(cfg.Tiers) == 0 {
		return "", fmt.Errorf("models: no override and no built-in default for agent %q", agent)
	}
	return resolveFrom(cfg, tier)
}

func resolveFrom(c Config, tier string) (string, error) {
	if tier == "" {
		tier = c.Default
	}
	if tier == "" {
		return "", fmt.Errorf("models: no tier given and no default configured")
	}
	model, ok := c.Tiers[tier]
	if !ok {
		return "", fmt.Errorf("models: unknown tier %q", tier)
	}
	return model, nil
}
