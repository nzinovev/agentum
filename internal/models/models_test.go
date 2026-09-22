package models

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a models.yaml into a temp dir and points
// AGENTUM_MODELS_CONFIG at it, so Load reads exactly this file.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTUM_MODELS_CONFIG", path)
}

// adapterDefaults stands in for the fallback Config an execution adapter's
// descriptor would carry (the real one lives in internal/agent).
func adapterDefaults() Config {
	return Config{
		Tiers: map[string]TierDefinition{
			"fast":      {Model: "opencode/nemotron-3.5-lightning-free"},
			"strong":    {Model: "opencode/muse-spark-1.3-contributor-free"},
			"reasoning": {Model: "opencode/nemotron-3-ultra-free"},
		},
		Default: "strong",
	}
}

func TestLoad_UnknownTopLevelKeyRejected(t *testing.T) {
	// A typo like "teirs:" must be a load error, not an ignored key
	// that falls back to the adapter defaults.
	writeConfig(t, "teirs:\n  fast: some-model\ndefault: fast\n")
	if _, err := Load(); err == nil {
		t.Fatal("unknown top-level key must fail Load")
	}
}

// TestLoad_UnknownKeyInsideTierMappingRejected: the object tier form accepts
// exactly {model, variant}, and a typo'd key inside it is a decode error —
// Node.Decode does not inherit the decoder's KnownFields, so this strictness
// belongs to the tier type itself. Without it a "varaint:" typo would decode
// as a tier silently running without one.
func TestLoad_UnknownKeyInsideTierMappingRejected(t *testing.T) {
	writeConfig(t, "tiers:\n  fast:\n    model: some-model\n    varaint: high\n")
	_, err := Load()
	if err == nil {
		t.Fatal("an unknown key inside a tier mapping must fail Load")
	}
	for _, wanted := range []string{"unknown field", `"varaint"`, "model, variant"} {
		if !strings.Contains(err.Error(), wanted) {
			t.Errorf("error %q does not contain %q", err.Error(), wanted)
		}
	}
}

func TestLoad_EmptyModelRejected(t *testing.T) {
	writeConfig(t, "tiers:\n  fast: \"\"\ndefault: fast\n")
	if _, err := Load(); err == nil {
		t.Fatal("empty model string for a declared tier must fail Load")
	} else if !strings.Contains(err.Error(), "empty model") {
		t.Errorf("error should name the empty model: %v", err)
	}
}

// TestLoad_EmptyFileRejectedWithTheFix: a present-but-empty override — the
// shape an operator produces by commenting their tiers out — is refused, and
// the message says what to do about it. The strict decoder reports io.EOF for
// this input, and a boot failure whose entire explanation is "EOF" is a
// support ticket, not a configuration error.
func TestLoad_EmptyFileRejectedWithTheFix(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":      "",
		"comments only":   "# tiers:\n#   fast: some-model\n",
		"empty tiers map": "tiers: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeConfig(t, content)
			_, err := Load()
			if err == nil {
				t.Fatal("an override declaring no tiers must fail Load")
			}
			if strings.Contains(err.Error(), "EOF") {
				t.Errorf("the reason must be the empty file, not the decoder's EOF: %v", err)
			}
			if !strings.Contains(err.Error(), "declares no tiers") || !strings.Contains(err.Error(), "delete the file") {
				t.Errorf("error must name the cause and the fix: %v", err)
			}
			if !strings.Contains(err.Error(), modelsConfigFile) {
				t.Errorf("error must name the file: %v", err)
			}
		})
	}
}

// TestLoad_ExplicitPathThatDoesNotExistIsAnError: the one path the operator
// named is a statement of intent. Searching past it to <cwd>/models.yaml or
// ~/.config would run the process on tiers nobody chose, and the only visible
// sign would be the wrong model — the unnoticed-fallback class model
// resolution removes everywhere else.
func TestLoad_ExplicitPathThatDoesNotExistIsAnError(t *testing.T) {
	// Non-parallel: mutates env.
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	t.Setenv("AGENTUM_MODELS_CONFIG", absent)
	_, err := Load()
	if err == nil {
		t.Fatal("a named config path that does not exist must error")
	}
	if errors.Is(err, ErrNoConfig) {
		t.Fatalf("Load fell through to the search paths: %v", err)
	}
	// The message must name both the variable and the path, so the operator
	// can see which of the two is wrong.
	if !strings.Contains(err.Error(), "AGENTUM_MODELS_CONFIG") || !strings.Contains(err.Error(), absent) {
		t.Errorf("error %q must name the variable and the path", err)
	}
}

// TestLoad_NothingConfiguredReturnsErrNoConfig: no override anywhere is the
// common case and the one non-error — callers fall back to the adapter's
// built-in tiers.
func TestLoad_NothingConfiguredReturnsErrNoConfig(t *testing.T) {
	// Non-parallel: mutates env.
	t.Setenv("AGENTUM_MODELS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := Load()
	if errors.Is(err, ErrNoConfig) {
		return
	}
	// cwd and the home directory are outside this test's control; a real
	// models.yaml in either is a legitimate reason not to see ErrNoConfig.
	t.Logf("Load returned %v (acceptable when a models.yaml exists in cwd or home)", err)
}

func TestLoad_ValidOverride(t *testing.T) {
	writeConfig(t, "tiers:\n  strong: zai-coding-plan/glm-5.3\n  fast: zai-coding-plan/glm-5-turbo\ndefault: strong\n")
	config, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	selection, err := Resolve(config, adapterDefaults(), "strong")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selection.Options.Model != "zai-coding-plan/glm-5.3" {
		t.Errorf("override should win: got %+v", selection)
	}
}

func TestLoad_DefaultTierMustExist(t *testing.T) {
	writeConfig(t, "tiers: {fast: x}\ndefault: missing")
	if _, err := Load(); err == nil {
		t.Fatal("default tier not in tiers must fail Load")
	}
}

func TestResolve_FallbackTiers(t *testing.T) {
	selection, err := Resolve(nil, adapterDefaults(), "fast")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selection.Options.Model != "opencode/nemotron-3.5-lightning-free" {
		t.Errorf("Resolve(fast) = %+v; want the fallback tier's model", selection)
	}
	if selection.Tier != "fast" {
		t.Errorf("Tier = %q; want fast", selection.Tier)
	}
	if selection.Provider != "opencode" {
		t.Errorf("Provider = %q; want opencode", selection.Provider)
	}
}

func TestResolve_EmptyTierFallsBackToDefault(t *testing.T) {
	selection, err := Resolve(nil, adapterDefaults(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selection.Tier != "strong" || selection.Options.Model != "opencode/muse-spark-1.3-contributor-free" {
		t.Errorf("Resolve('') = %+v; want the fallback default tier", selection)
	}
}

func TestResolve_UnknownTierIsAnError(t *testing.T) {
	// The refusal, never an empty selection: a caller that ignored the error
	// would run on the runtime's default model — the exact substitution this
	// package exists to forbid.
	selection, err := Resolve(nil, adapterDefaults(), "magic")
	if err == nil {
		t.Fatal("unknown tier must error")
	}
	if !strings.Contains(err.Error(), "magic") {
		t.Errorf("error should name the tier: %v", err)
	}
	if selection.Options.Model != "" || selection.Tier != "" {
		t.Errorf("unknown tier must not yield a usable selection: %+v", selection)
	}
}

func TestSplitProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		model    string
		provider string
	}{
		{"zai-coding-plan/glm-5.3", "zai-coding-plan"},
		{"sonnet", ""}, // no provider in the name
		{"openrouter/vendor/model", "openrouter"}, // first slash wins
		{"", ""},
	}
	for _, testCase := range cases {
		if got := SplitProvider(testCase.model); got != testCase.provider {
			t.Errorf("SplitProvider(%q) = %q; want %q", testCase.model, got, testCase.provider)
		}
	}
}

func TestOptions_Names(t *testing.T) {
	t.Parallel()
	names := Options{Model: "x"}.Names()
	if len(names) != 1 || names[0] != OptionModel {
		t.Errorf("Names() = %v; want [model]", names)
	}
	if len(Options{}.Names()) != 0 {
		t.Errorf("empty Options must yield no names, got %v", Options{}.Names())
	}
}

func TestOptions_SupportedBy(t *testing.T) {
	t.Parallel()
	full := []OptionName{OptionModel}
	if err := (Options{Model: "x"}).SupportedBy(full); err != nil {
		t.Errorf("exact supported set must pass: %v", err)
	}
	if err := (Options{}).SupportedBy(nil); err != nil {
		t.Errorf("no populated options must pass even against an empty set: %v", err)
	}
}

func TestOptions_SupportedBy_SupersetRejectedAndNamed(t *testing.T) {
	t.Parallel()
	// Simulate an option this build cannot carry (a future variant option) against a
	// descriptor that declares only {model}.
	err := (Options{Model: "x"}).SupportedBy([]OptionName{OptionModel, "variant"})
	if err != nil {
		t.Fatalf("options within the declared set must pass: %v", err)
	}
	err = (Options{Model: "x"}).SupportedBy(nil)
	if err == nil {
		t.Fatal("option outside the declared set must be refused")
	}
	if !errors.Is(err, ErrUnsupportedOption) {
		t.Errorf("refusal must wrap ErrUnsupportedOption: %v", err)
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("refusal must name the missing option: %v", err)
	}
}

// TestLoad_ObjectAndStringFormsMixFreely: both tier forms are valid and can
// sit in one file — the string form keeps working unchanged, and the mapping
// form yields both the model and the variant.
func TestLoad_ObjectAndStringFormsMixFreely(t *testing.T) {
	writeConfig(t, "tiers:\n"+
		"  fast: zai-coding-plan/glm-5.2-highspeed\n"+
		"  strong:\n"+
		"    model: zai-coding-plan/glm-5.3\n"+
		"    variant: high\n"+
		"  reasoning: zai-coding-plan/glm-5.3\n"+
		"default: strong\n")
	config, err := Load()
	if err != nil {
		t.Fatalf("mixed-form file must load: %v", err)
	}
	if got := config.Tiers["fast"]; got != (TierDefinition{Model: "zai-coding-plan/glm-5.2-highspeed"}) {
		t.Errorf("string form = %+v; want the model only", got)
	}
	if got := config.Tiers["strong"]; got != (TierDefinition{Model: "zai-coding-plan/glm-5.3", Variant: "high"}) {
		t.Errorf("mapping form = %+v; want model and variant", got)
	}
	if got := config.Tiers["reasoning"]; got != (TierDefinition{Model: "zai-coding-plan/glm-5.3"}) {
		t.Errorf("mapping form without variant = %+v; want model only", got)
	}
	selection, err := Resolve(config, adapterDefaults(), "strong")
	if err != nil {
		t.Fatalf("Resolve on the mapping form: %v", err)
	}
	if selection.Options.Variant != "high" {
		t.Errorf("Resolve variant = %q; want high", selection.Options.Variant)
	}
}

// TestLoad_MalformedTierValuesRejected is the strictness table of the tier
// forms: every row is an input that would otherwise decode into something the
// operator never wrote — a number as a model, a boolean as a variant, a list
// where a scalar belongs. Each refusal names the line.
func TestLoad_MalformedTierValuesRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"int tier", "tiers:\n  fast: 42\n", "!!int"},
		{"bool tier", "tiers:\n  fast: true\n", "!!bool"},
		{"list tier", "tiers:\n  fast: [a, b]\n", "either a model string or a {model, variant} mapping"},
		{"int model", "tiers:\n  fast:\n    model: 42\n", `tier field "model" must be a string, got !!int`},
		{"bool variant", "tiers:\n  fast:\n    model: m\n    variant: true\n", `tier field "variant" must be a string, got !!bool`},
		{"null model", "tiers:\n  fast:\n    model: null\n", `tier field "model" must be a string, got !!null`},
		{"list variant", "tiers:\n  fast:\n    model: m\n    variant: [high]\n", `tier field "variant" must be a string`},
		{"object variant", "tiers:\n  fast:\n    model: m\n    variant: {effort: high}\n", `tier field "variant" must be a string, got !!map`},
		{"non-string key", "tiers:\n  fast:\n    42: m\n", "tier field name must be a string"},
		{"empty variant", "tiers:\n  fast:\n    model: m\n    variant: \"\"\n", "declares an empty variant; remove the key"},
		{"whitespace variant", "tiers:\n  fast:\n    model: m\n    variant: \"  \"\n", "declares an empty variant; remove the key"},
		{"null variant", "tiers:\n  fast:\n    model: m\n    variant: null\n", `tier field "variant" must be a string, got !!null`},
		{"duplicate model", "tiers:\n  fast:\n    model: a\n    model: b\n", `mapping key "model" already defined`},
		{"duplicate variant", "tiers:\n  fast:\n    model: m\n    variant: high\n    variant: low\n", `mapping key "variant" already defined`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			writeConfig(t, testCase.body)
			_, err := Load()
			if err == nil {
				t.Fatalf("input must be refused: %s", testCase.body)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error %q does not contain %q", err.Error(), testCase.want)
			}
		})
	}
}

// TestLoad_EmptyTierAndVariantWithoutModelRejected: a null or blank tier and a
// variant without a model are consistency refusals at load time — the file is
// named, and the variant case says which variant was declared.
func TestLoad_EmptyTierAndVariantWithoutModelRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"null tier", "tiers:\n  fast:\ndefault: fast\n", `tier "fast": has an empty model string`},
		{"empty string tier", "tiers:\n  fast: \"\"\ndefault: fast\n", `tier "fast": has an empty model string`},
		{"whitespace model", "tiers:\n  fast:\n    model: \"  \"\ndefault: fast\n", `tier "fast": has an empty model string`},
		{"variant without model", "tiers:\n  fast:\n    variant: high\ndefault: fast\n", `tier "fast": declares variant "high" but no model`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			writeConfig(t, testCase.body)
			_, err := Load()
			if err == nil {
				t.Fatalf("input must be refused: %s", testCase.body)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("error %q does not contain %q", err.Error(), testCase.want)
			}
			if !strings.Contains(err.Error(), modelsConfigFile) {
				t.Errorf("error %q must name the file", err.Error())
			}
		})
	}
}

// TestResolve_RejectsVariantWithoutModelAssembledInGo: Load's strictness is
// not the only door. A Config assembled in code — the adapter defaults path,
// a test fixture — meets the same consistency check at resolution time.
func TestResolve_RejectsVariantWithoutModelAssembledInGo(t *testing.T) {
	config := Config{
		Tiers:   map[string]TierDefinition{"fast": {Variant: "high"}},
		Default: "fast",
	}
	_, err := Resolve(&config, adapterDefaults(), "fast")
	if err == nil {
		t.Fatal("a variant without a model must be refused at resolution")
	}
	if !strings.Contains(err.Error(), `declares variant "high" but no model`) {
		t.Errorf("error %q does not name the variant", err.Error())
	}
}

// TestOptions_Validate: the boundary between "no option" and "broken option"
// — an empty variant is the absence of the option and passes; whitespace is a
// value someone typed and is refused.
func TestOptions_Validate(t *testing.T) {
	t.Parallel()
	if err := (Options{Model: "m", Variant: ""}).Validate(); err != nil {
		t.Errorf("empty variant must pass as the option's absence: %v", err)
	}
	if err := (Options{Model: "m", Variant: "high"}).Validate(); err != nil {
		t.Errorf("populated variant must pass: %v", err)
	}
	if err := (Options{Model: "m", Variant: "  "}).Validate(); err == nil {
		t.Error("whitespace-only variant must be refused")
	}
	if err := (Options{Model: "  ", Variant: "high"}).Validate(); err == nil {
		t.Error("whitespace model with a variant must be refused")
	}
}

// TestOptions_PopulatedAndNames: the populated set is sorted by name and
// carries values; Names derives from it and cannot drift.
func TestOptions_PopulatedAndNames(t *testing.T) {
	t.Parallel()
	populated := Options{Variant: "high", Model: "m"}.Populated()
	if len(populated) != 2 {
		t.Fatalf("Populated() = %v; want both options", populated)
	}
	// "model" sorts before "variant".
	if populated[0] != (Option{Name: OptionModel, Value: "m"}) || populated[1] != (Option{Name: OptionVariant, Value: "high"}) {
		t.Errorf("Populated() = %v; want name-sorted pairs", populated)
	}
	names := Options{Variant: "high", Model: "m"}.Names()
	if len(names) != 2 || names[0] != OptionModel || names[1] != OptionVariant {
		t.Errorf("Names() = %v; want [model variant]", names)
	}
	if names := (Options{}).Names(); len(names) != 0 {
		t.Errorf("empty Options must yield no names, got %v", names)
	}
}

// TestOptions_SupportedBy_RefusalCarriesTheValue: the refusal names the
// option AND its value — variant may sit on any of several tiers, and without
// the value the operator hunts the culprit by trial.
func TestOptions_SupportedBy_RefusalCarriesTheValue(t *testing.T) {
	t.Parallel()
	err := (Options{Model: "m", Variant: "high"}).SupportedBy([]OptionName{OptionModel})
	if err == nil {
		t.Fatal("variant outside the declared set must be refused")
	}
	if !strings.Contains(err.Error(), `variant="high"`) {
		t.Errorf("refusal must name the option and its value: %v", err)
	}
}
