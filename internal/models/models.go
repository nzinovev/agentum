// Package models holds the BYO-models configuration: how operators supply
// provider credentials and how a tier name (e.g. "fast", "strong") maps to a
// concrete model identifier ("provider/model") per provider.
//
// Security rules (AGENTS.md):
//   - Credentials are read from the process environment at invocation time,
//     never stored in committed config. The preferred shape is api_key_env,
//     naming the env var that holds the key.
//   - Inline api_key is supported for local dev convenience only; the loader
//     warns, and Provider masks the key in any log output via slog.LogValuer.
//   - The real config file (models.yaml) is gitignored; ship models.example.yaml.
//
// This package is consumed by the runner (Epic 3 / 5.1). The adapter already
// takes agent.Invocation.Model as a literal "provider/model"; the runner resolves
// a tier to that literal here and injects the provider's credentials into the
// subprocess env via EnvForProvider.
package models

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoConfig is returned by Load when no config file is present. Callers decide
// whether that's fatal: it is when a tier must be resolved, and acceptable when
// the operator drives the adapter with literal model ids + the agent binary's
// own auth.
var ErrNoConfig = errors.New("models: no config file")

// Config is the parsed BYO-models configuration.
type Config struct {
	Providers map[string]Provider `yaml:"providers"`
	Tiers     map[string]string   `yaml:"tiers"`   // tier name -> "provider/model"
	Default   string              `yaml:"default"` // default tier name

	// path is where the config was loaded from (diagnostics).
	path string
	// hadInlineKey tracks whether any provider used api_key inline, so a caller
	// can warn once at startup rather than per-provider.
	hadInlineKey bool
}

// Provider is one model provider's credentials/endpoint.
//
// APIKeyEnv is the env var name the provider's SDK expects (e.g.
// "ANTHROPIC_API_KEY"); it is required so the subprocess gets a correctly
// named variable. APIKey is an optional inline value for that var (dev
// convenience; warned on; masked in logs). At runtime the value is read from
// the process env when APIKey is empty.
type Provider struct {
	APIKeyEnv string `yaml:"api_key_env"` // required: canonical env var name
	APIKey    string `yaml:"api_key"`     // optional inline value for APIKeyEnv
	BaseURL   string `yaml:"base_url"`    // optional endpoint override
}

// LogValue masks the API key so a Provider is safe to log via slog.
func (p Provider) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("api_key_env", p.APIKeyEnv),
		slog.String("api_key", maskKey(p.APIKey)),
		slog.String("base_url", p.BaseURL),
	)
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 4 {
		return "****"
	}
	return k[:2] + "…" + k[len(k)-2:]
}

// Load resolves the config path and parses it. path is examined in order:
//
//	AGENTUM_MODELS_CONFIG env var (if set)
//	<cwd>/models.yaml
//	$XDG_CONFIG_HOME/agentum/models.yaml or ~/.config/agentum/models.yaml
//
// Returns ErrNoConfig (wrapped) when no file is present.
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
		c, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("models: parse %s: %w", p, err)
		}
		c.path = p
		return c, nil
	}
	return nil, ErrNoConfig
}

// LoadFile parses a specific config file. Tests prefer this for hermeticity.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("models: read %s: %w", path, err)
	}
	c, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("models: parse %s: %w", path, err)
	}
	c.path = path
	return c, nil
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

func parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	for name, p := range c.Providers {
		if p.APIKey != "" {
			c.hadInlineKey = true
		}
		// api_key_env is required — it's the canonical name the provider SDK
		// reads on the subprocess. Inline api_key alone is meaningless (we'd
		// have no var name to set).
		if p.APIKeyEnv == "" {
			return nil, fmt.Errorf("models: provider %q is missing api_key_env", name)
		}
	}
	if c.Default != "" {
		if _, ok := c.Tiers[c.Default]; !ok {
			return nil, fmt.Errorf("models: default tier %q is not defined in tiers", c.Default)
		}
	}
	return &c, nil
}

// Path reports where the config was loaded from (empty for in-memory configs).
func (c *Config) Path() string { return c.path }

// HadInlineKey reports whether any provider used an inline api_key. Callers can
// warn once at startup that production should use api_key_env instead.
func (c *Config) HadInlineKey() bool { return c.hadInlineKey }

// Resolve returns the concrete "provider/model" id for a tier. An empty tier
// falls back to Default. Unknown tier (or empty with no Default) is an error —
// the runner never silently picks a model.
func (c *Config) Resolve(tier string) (string, error) {
	if tier == "" {
		tier = c.Default
	}
	if tier == "" {
		return "", fmt.Errorf("models: no tier given and no default configured")
	}
	modelID, ok := c.Tiers[tier]
	if !ok {
		return "", fmt.Errorf("models: unknown tier %q", tier)
	}
	return modelID, nil
}

// EnvForProvider returns the env vars the runner must set on the agent
// subprocess for the named provider. The provider's api_key_env names the
// canonical variable (e.g. ANTHROPIC_API_KEY); its value is the inline api_key
// when set, otherwise read from the live process env. PROVIDER_BASE_URL is
// added when base_url is configured. Returns an error if the provider is
// unknown or has no resolvable credential.
//
// The key value is read here, at call time, from the live process env — it is
// never stored in Config beyond the inline dev fallback.
func (c *Config) EnvForProvider(provider string) (map[string]string, error) {
	p, ok := c.Providers[provider]
	if !ok {
		return nil, fmt.Errorf("models: provider %q not configured", provider)
	}
	out := map[string]string{}

	// Inline value wins (dev convenience); otherwise read the process env.
	if p.APIKey != "" {
		out[p.APIKeyEnv] = p.APIKey
	} else if v := os.Getenv(p.APIKeyEnv); v != "" {
		out[p.APIKeyEnv] = v
	} else {
		return nil, fmt.Errorf("models: env var %s for provider %q is not set", p.APIKeyEnv, provider)
	}

	if p.BaseURL != "" {
		out[strings.ToUpper(provider)+"_BASE_URL"] = p.BaseURL
	}
	return out, nil
}
