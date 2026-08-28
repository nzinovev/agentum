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
		Tiers: map[string]string{
			"fast":      "opencode/deepseek-v4-flash-free",
			"strong":    "opencode/north-mini-code-free",
			"reasoning": "opencode/nemotron-3-ultra-free",
		},
		Default: "strong",
	}
}

func TestLoad_UnknownTopLevelKeyRejected(t *testing.T) {
	// A typo like "teirs:" must be a load error, not a silently-ignored key
	// that quietly falls back to the adapter defaults.
	writeConfig(t, "teirs:\n  fast: some-model\ndefault: fast\n")
	if _, err := Load(); err == nil {
		t.Fatal("unknown top-level key must fail Load")
	}
}

func TestLoad_UnknownKeyUnderTiersRejected(t *testing.T) {
	// The object form per tier is later work; today a nested map under a tier
	// is a decode error, not a silently dropped entry.
	writeConfig(t, "tiers:\n  fast:\n    model: some-model\n")
	if _, err := Load(); err == nil {
		t.Fatal("nested key under tiers must fail Load")
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

func TestLoad_AbsentReturnsErrNoConfig(t *testing.T) {
	// Non-parallel: mutates env.
	t.Setenv("AGENTUM_MODELS_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	_, err := Load()
	if err == nil {
		t.Fatal("absent config must error")
	}
	// Other candidate paths (cwd, home) may exist on some machines; only
	// assert strictly when Load returned something other than ErrNoConfig.
	if err != ErrNoConfig && !strings.Contains(err.Error(), "no models.yaml") {
		t.Logf("Load returned %v (acceptable when other candidate paths exist)", err)
	}
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
	if selection.Options.Model != "opencode/deepseek-v4-flash-free" {
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
	if selection.Tier != "strong" || selection.Options.Model != "opencode/north-mini-code-free" {
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
	// Simulate an option this build cannot carry (task 6's variant) against a
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
