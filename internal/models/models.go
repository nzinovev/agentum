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
// Ignoring an input or substituting a default is forbidden.
package models

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
// agree on it. modelsConfigEnv names the explicit override path.
const (
	modelsConfigFile = "models.yaml"
	modelsConfigEnv  = "AGENTUM_MODELS_CONFIG"
)

// errEmptyConfig is the reason a present-but-empty override is refused: it
// carries the fix, because "delete the file" is the difference between a boot
// failure and a working default. Wrapped with the path by Load.
var errEmptyConfig = errors.New("declares no tiers; delete the file to use the execution adapter's built-in tiers")

// TierDefinition is one tier as an operator writes it in models.yaml. The
// value is either a bare string naming the model, or a mapping of model and an
// optional variant. Variant is the runtime's reasoning-effort setting.
//
// This is the FILE format, deliberately not the adapter contract: a key added
// here does not reach an argv until the adapter's descriptor declares it, and
// an adapter option does not become a file key here. Merging the two would
// make every future adapter option YAML surface and every future file key an
// argv candidate by construction.
type TierDefinition struct {
	Model   string `yaml:"model"`
	Variant string `yaml:"variant,omitempty"`
}

// Config is a tier→definition mapping plus the default tier.
type Config struct {
	Tiers   map[string]TierDefinition `yaml:"tiers"`
	Default string                    `yaml:"default"`
}

// OptionName is the name of one model parameter an adapter may or may not
// understand (e.g. "model", "variant"). The vocabulary of names lives here;
// which subset a runtime accepts lives in the adapter's descriptor.
type OptionName string

// OptionModel selects the model string passed to the runtime's --model flag.
// OptionVariant selects the reasoning-effort setting passed to --variant.
const (
	OptionModel   OptionName = "model"
	OptionVariant OptionName = "variant"
)

// Options is the structured model configuration handed to an adapter. It is a
// closed struct: a parameter that is not a field here does not exist, and no
// caller appends strings to argv. Reading it field by field (rather than
// rendering it into a string) is what lets an adapter refuse a parameter it
// cannot honor instead of dropping it.
type Options struct {
	Model   string `json:"model,omitempty"`
	Variant string `json:"variant,omitempty"`
}

// Option is one populated model parameter: its name and the value it carries.
// The value is part of the record because a refusal naming only "variant" does
// not say which of the three tiers declared it.
type Option struct {
	Name  OptionName
	Value string
}

// Populated returns the populated options as name+value pairs, sorted by name,
// so the set is stable for comparisons and error messages.
func (options Options) Populated() []Option {
	populated := make([]Option, 0, 2)
	if options.Model != "" {
		populated = append(populated, Option{Name: OptionModel, Value: options.Model})
	}
	if options.Variant != "" {
		populated = append(populated, Option{Name: OptionVariant, Value: options.Variant})
	}
	sort.Slice(populated, func(left, right int) bool { return populated[left].Name < populated[right].Name })
	return populated
}

// Names returns the populated option names, sorted, so the set is stable for
// comparisons and error messages.
func (options Options) Names() []OptionName {
	populated := options.Populated()
	names := make([]OptionName, 0, len(populated))
	for _, option := range populated {
		names = append(names, option.Name)
	}
	return names
}

// Validate returns an error for an empty or whitespace-only model and for a
// variant that is only whitespace. An empty Variant is the absence of the
// option and passes; a variant without a model names the variant, because
// "which effort" is not a question until "which model" is answered. The
// strictly-empty-variant-key refusal belongs to the file format and lives in
// TierDefinition.UnmarshalYAML.
func (options Options) Validate() error {
	if strings.TrimSpace(options.Model) == "" {
		if options.Variant != "" {
			return fmt.Errorf("declares variant %q but no model", options.Variant)
		}
		return errors.New("has an empty model string")
	}
	if options.Variant != "" && strings.TrimSpace(options.Variant) == "" {
		return fmt.Errorf("variant %q is only whitespace", options.Variant)
	}
	return nil
}

// ErrUnsupportedOption is returned by SupportedBy when an option is populated
// that the declared supported set does not contain.
var ErrUnsupportedOption = errors.New("models: unsupported model option")

// UnsupportedOption wraps ErrUnsupportedOption with the populated options the
// declared enforcer cannot take. Mirrors caps.Unsupported deliberately: the
// adapter confirms what it can honor, and the refusal carries the specifics —
// the values included, because variant may sit on any of several tiers and a
// name alone leaves the operator guessing which one.
type UnsupportedOption struct {
	Options []Option
}

func (unsupported *UnsupportedOption) Error() string {
	rendered := make([]string, 0, len(unsupported.Options))
	for _, option := range unsupported.Options {
		rendered = append(rendered, fmt.Sprintf("%s=%q", option.Name, option.Value))
	}
	return fmt.Sprintf("models: unsupported model options: %s", strings.Join(rendered, ", "))
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
	populated := options.Populated()
	missing := make([]Option, 0)
	for _, option := range populated {
		if _, found := supportedSet[option.Name]; !found {
			missing = append(missing, option)
		}
	}
	if len(missing) > 0 {
		sort.Slice(missing, func(left, right int) bool { return missing[left].Name < missing[right].Name })
		return &UnsupportedOption{Options: missing}
	}
	return nil
}

// UnmarshalYAML accepts both tier forms — a bare model string, or a
// {model, variant} mapping — and enforces the mapping's own strictness.
// Node.Decode does not inherit KnownFields, so without the key check here an
// unknown key inside the mapping would reach the struct unrefused. Decode also
// renders !!int and !!bool scalars as strings, so a tier written as 42 would
// otherwise decode as the model "42"; the tag checks refuse that. Decoding is
// kept after the checks because it owns the duplicate-key refusal, which a
// hand-rolled walk cannot replace without losing it.
func (definition *TierDefinition) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag != "!!str" {
			return fmt.Errorf("line %d: a tier is either a model string or a {model, variant} mapping, got %s",
				value.Line, value.Tag)
		}
		*definition = TierDefinition{Model: value.Value}
		return nil
	case yaml.MappingNode:
		for index := 0; index+1 < len(value.Content); index += 2 {
			key := value.Content[index]
			fieldValue := value.Content[index+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("line %d: a tier field name must be a string", key.Line)
			}
			switch key.Value {
			case "model", "variant":
			default:
				return fmt.Errorf("line %d: unknown field %q in a tier definition (known: model, variant)",
					key.Line, key.Value)
			}
			if fieldValue.Kind != yaml.ScalarNode || fieldValue.Tag != "!!str" {
				return fmt.Errorf("line %d: tier field %q must be a string, got %s",
					fieldValue.Line, key.Value, fieldValue.Tag)
			}
			if key.Value == "variant" && strings.TrimSpace(fieldValue.Value) == "" {
				return fmt.Errorf("line %d: a tier declares an empty variant; remove the key to run without one",
					fieldValue.Line)
			}
		}
		// A local type sheds this method, so Decode lands on the plain struct
		// instead of recursing.
		type tierFields TierDefinition
		var fields tierFields
		if err := value.Decode(&fields); err != nil {
			return err
		}
		*definition = TierDefinition(fields)
		return nil
	default:
		return fmt.Errorf("line %d: a tier is either a model string or a {model, variant} mapping", value.Line)
	}
}

// Selection is a resolved tier: the tier name, the derived provider, and the
// options the adapter will run with. Provider is the part of the model string
// before the first "/" (opencode documents --model as "provider/model"); a
// runtime whose model names carry no provider yields an empty one. The
// split is recorded here rather than re-derived by every reader.
type Selection struct {
	Tier     string  `json:"tier"`
	Provider string  `json:"provider,omitempty"`
	Options  Options `json:"options"`
}

// SplitProvider returns the provider half of a model string: the part before
// the first slash ("a/b/c" → "a"), or "" for a bare name. The model string
// itself is never rewritten — the runtime receives it exactly as configured,
// so this derives the provider for evidence and comparison, nothing more.
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
// stop the process instead of going unapplied.
//
// AGENTUM_MODELS_CONFIG is the one path whose absence is an error rather than
// a miss: naming a file that is not there is a broken configuration, and
// searching past it would run the process on tiers the operator did not pick.
func Load() (*Config, error) {
	for _, candidate := range candidatePaths() {
		path := candidate.path
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			// A path the operator named explicitly is a statement of intent,
			// not a place to look: falling through to <cwd>/models.yaml or
			// ~/.config would run on tiers they did not choose, and the only
			// visible sign would be the wrong model. The implicit
			// candidates are searched, so an absent one is just absent.
			if candidate.explicit {
				return nil, fmt.Errorf(
					"models: %s=%s does not exist; create it, or unset %s to search the default locations",
					modelsConfigEnv, path, modelsConfigEnv)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("models: read %s: %w", path, err)
		}
		var config Config
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&config); err != nil {
			// An empty or comment-only file decodes as io.EOF. That is not a
			// syntax error and must not surface as the bare word "EOF" on a
			// refused boot: the operator commented their tiers out, and the
			// actionable answer is that an empty override file is not one.
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("models: %s: %w", path, errEmptyConfig)
			}
			return nil, fmt.Errorf("models: parse %s: %w", path, err)
		}
		// A file that declares no tiers reaches the same refusal through
		// `tiers: {}`, and refusing it here matters: a non-nil
		// override REPLACES the adapter's defaults, so every tier resolution
		// would fail at run start with "unknown tier" instead of here, where
		// the file is named.
		if len(config.Tiers) == 0 {
			return nil, fmt.Errorf("models: %s: %w", path, errEmptyConfig)
		}
		for tier, definition := range config.Tiers {
			// The same consistency check resolveFrom applies at resolution
			// time, paid here so the file is named once at boot instead of
			// the failing tier surfacing per run.
			if err := (Options{Model: definition.Model, Variant: definition.Variant}).Validate(); err != nil {
				return nil, fmt.Errorf("models: %s: tier %q: %w", path, tier, err)
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

// candidate is one place Load looks. explicit marks the path the operator
// named through the environment: it is searched first and, unlike the
// conventional locations, its absence is an error rather than a miss.
type candidate struct {
	path     string
	explicit bool
}

func candidatePaths() []candidate {
	out := []candidate{}
	if env := os.Getenv(modelsConfigEnv); env != "" {
		out = append(out, candidate{path: env, explicit: true})
	}
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, candidate{path: filepath.Join(cwd, modelsConfigFile)})
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, candidate{path: filepath.Join(xdg, "agentum", modelsConfigFile)})
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, candidate{path: filepath.Join(home, ".config", "agentum", modelsConfigFile)})
	}
	return out
}

// Resolve resolves a tier to a Selection. If override is non-nil, its tiers are
// used (with the override's default); otherwise fallback is used (the active
// adapter's Descriptor.DefaultTiers). An empty tier falls back to the applicable
// default. An unknown tier is an error: Agentum does not pick a model or
// substitute a default on its own.
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
	definition, ok := config.Tiers[tier]
	if !ok {
		return Selection{}, fmt.Errorf("models: unknown tier %q", tier)
	}
	// Options are checked before the selection exists: a caller assembling a
	// Config in Go — without Load ever running — gets the same consistency
	// refusal the file path produces.
	options := Options{Model: definition.Model, Variant: definition.Variant}
	if err := options.Validate(); err != nil {
		return Selection{}, fmt.Errorf("models: tier %q: %w", tier, err)
	}
	return Selection{
		Tier:     tier,
		Provider: SplitProvider(definition.Model),
		Options:  options,
	}, nil
}
