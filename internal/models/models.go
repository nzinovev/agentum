// Package models owns what a model selection IS: the tier vocabulary, the
// structured option set handed to an execution adapter, and the resolution of a
// tier name into a concrete selection. Which parts of a selection a given
// runtime UNDERSTANDS is adapter knowledge (internal/agent's descriptor); this
// package depends on the standard library and YAML only, so the adapter seam
// can import it without a cycle.
//
// Agentum is a coordinator, not a credential manager. The operator installs the
// runtime (opencode / …) and configures providers in the runtime itself. Agentum
// only decides the model name; the runtime resolves it to a real provider and
// endpoint using the operator's own config.
//
// Resolution priority:
//  1. An operator override (models.yaml), if present — use its tiers.
//  2. Otherwise the fallback Config the caller supplies (the active execution
//     adapter's Descriptor.DefaultTiers).
//
// Nothing here is ever best-effort: an unknown tier, an empty model string, an
// unknown config key, or an option the adapter does not declare is an error.
// Silent ignoring and default substitution are forbidden.
package models

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNoConfig is returned by Load when no models.yaml is present. Callers fall
// back to the execution adapter's built-in defaults.
var ErrNoConfig = errors.New("models: no " + modelsConfigFile + "; using adapter built-in default")

// modelsConfigFile is the operator-override filename Load looks for in each
// candidate directory. A const so the error message and every search path
// agree on it.
const modelsConfigFile = "models.yaml"

// Config is a tier→model mapping plus the default tier. The file format is
// unchanged from schema day one: tiers stay map[string]string, and the object
// form (strong: {model: …, variant: high}) is later work that will grow the
// value type, not this shape.
type Config struct {
	Tiers   map[string]string `yaml:"tiers"`
	Default string            `yaml:"default"`
}

// OptionName is the name of one model parameter an adapter may or may not
// understand (e.g. "model", later "variant"). The vocabulary of names lives
// here; which subset a runtime accepts lives in the adapter's descriptor.
type OptionName string

// OptionModel selects the model string passed to the runtime's --model flag.
// MVP task 6 adds OptionVariant.
const OptionModel OptionName = "model"

// Options is the structured model configuration handed to an adapter. It is a
// closed struct: a parameter that is not a field here does not exist, and no
// caller appends strings to argv. Reading it field by field (rather than
// rendering it into a string) is what lets an adapter refuse a parameter it
// cannot honor instead of silently dropping it.
type Options struct {
	Model string `json:"model,omitempty"`
}

// Names returns the populated option names, sorted, so the set is stable for
// comparisons and error messages.
func (options Options) Names() []OptionName {
	names := make([]OptionName, 0, 1)
	if options.Model != "" {
		names = append(names, OptionModel)
	}
	sort.Slice(names, func(left, right int) bool { return names[left] < names[right] })
	return names
}

// ErrUnsupportedOption is returned by SupportedBy when an option is populated
// that the declared supported set does not contain.
var ErrUnsupportedOption = errors.New("models: unsupported model option")

// UnsupportedOption wraps ErrUnsupportedOption with the option names the
// declared enforcer cannot take. Mirrors caps.Unsupported deliberately: the
// adapter confirms what it can honor, and the refusal carries the specifics.
type UnsupportedOption struct {
	Options []OptionName
}

func (unsupported *UnsupportedOption) Error() string {
	names := make([]string, 0, len(unsupported.Options))
	for _, name := range unsupported.Options {
		names = append(names, string(name))
	}
	return fmt.Sprintf("models: unsupported model options: %s", strings.Join(names, ", "))
}

func (unsupported *UnsupportedOption) Unwrap() error { return ErrUnsupportedOption }

// SupportedBy reports whether every populated option belongs to the supported
// set the adapter declared in its descriptor. An option outside that set is an
// error — there is no path where an unsupported option becomes a default, an
// empty flag, or an omitted flag. The caller wraps the error with the adapter
// id so the message names both halves.
func (options Options) SupportedBy(supported []OptionName) error {
	supportedSet := make(map[OptionName]struct{}, len(supported))
	for _, name := range supported {
		supportedSet[name] = struct{}{}
	}
	missing := make([]OptionName, 0)
	for _, name := range options.Names() {
		if _, found := supportedSet[name]; !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Slice(missing, func(left, right int) bool { return missing[left] < missing[right] })
		return &UnsupportedOption{Options: missing}
	}
	return nil
}

// Selection is a resolved tier: the tier name, the derived provider, and the
// options the adapter will run with. Provider is the part of the model string
// before the first "/" (opencode documents --model as "provider/model"); a
// runtime whose model names carry no provider simply yields an empty one. The
// split is recorded here rather than re-derived by every reader.
type Selection struct {
	Tier     string  `json:"tier"`
	Provider string  `json:"provider,omitempty"`
	Options  Options `json:"options"`
}

// SplitProvider splits a model string into its provider half and returns the
// whole string as the model. The first slash wins ("a/b/c" → provider "a"), and
// a bare name yields an empty provider. The model string itself is returned
// unchanged — the runtime receives it exactly as configured.
func SplitProvider(model string) (provider string) {
	index := strings.IndexByte(model, '/')
	if index < 0 {
		return ""
	}
	return model[:index]
}

// Load reads the operator override (models.yaml), if present. Resolution order
// of paths: AGENTUM_MODELS_CONFIG env, <cwd>/models.yaml, then
// $XDG_CONFIG_HOME/agentum/models.yaml or ~/.config/agentum/models.yaml.
// Returns ErrNoConfig (wrapped) when absent — callers fall back to the active
// adapter's defaults. Decoding is strict (unknown keys are errors), and a tier
// whose model string is empty is refused: a file that was already broken must
// stop the process rather than silently not apply.
func Load() (*Config, error) {
	for _, path := range candidatePaths() {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("models: read %s: %w", path, err)
		}
		var config Config
		decoder := yaml.NewDecoder(strings.NewReader(string(data)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			return nil, fmt.Errorf("models: parse %s: %w", path, err)
		}
		for tier, model := range config.Tiers {
			if strings.TrimSpace(model) == "" {
				return nil, fmt.Errorf("models: %s: tier %q has an empty model string", path, tier)
			}
		}
		if config.Default != "" {
			if _, ok := config.Tiers[config.Default]; !ok {
				return nil, fmt.Errorf("models: %s: default tier %q is not defined in tiers", path, config.Default)
			}
		}
		return &config, nil
	}
	return nil, ErrNoConfig
}

func candidatePaths() []string {
	out := []string{}
	if env := os.Getenv("AGENTUM_MODELS_CONFIG"); env != "" {
		out = append(out, env)
	}
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(cwd, modelsConfigFile))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, filepath.Join(xdg, "agentum", modelsConfigFile))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".config", "agentum", modelsConfigFile))
	}
	return out
}

// Resolve resolves a tier to a Selection. If override is non-nil, its tiers are
// used (with the override's default); otherwise fallback is used (the active
// adapter's Descriptor.DefaultTiers). An empty tier falls back to the applicable
// default. An unknown tier is an error — Agentum never silently picks a model,
// and never substitutes a default for a name it could not resolve.
func Resolve(override *Config, fallback Config, tier string) (Selection, error) {
	if override != nil {
		return resolveFrom(*override, tier)
	}
	return resolveFrom(fallback, tier)
}

func resolveFrom(config Config, tier string) (Selection, error) {
	if tier == "" {
		tier = config.Default
	}
	if tier == "" {
		return Selection{}, fmt.Errorf("models: no tier given and no default configured")
	}
	model, ok := config.Tiers[tier]
	if !ok {
		return Selection{}, fmt.Errorf("models: unknown tier %q", tier)
	}
	return Selection{
		Tier:     tier,
		Provider: SplitProvider(model),
		Options:  Options{Model: model},
	}, nil
}
