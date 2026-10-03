package config

import (
	"strings"
	"testing"
)

// TestLoad_RetiredOpencodeBinaryRefused: AGENTUM_OPENCODE_BINARY was replaced
// by the adapter-neutral AGENTUM_RUNTIME_BINARY. An operator who pinned the
// runtime under the old name must be told, not dropped back to a PATH
// lookup unannounced — a binary override that stops applying does not look like a
// configuration change, it looks like the runtime failing.
func TestLoad_RetiredOpencodeBinaryRefused(t *testing.T) {
	t.Setenv("AGENTUM_OPENCODE_BINARY", "/opt/opencode/bin/opencode.exe")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted the retired AGENTUM_OPENCODE_BINARY")
	}
	if !strings.Contains(err.Error(), "AGENTUM_OPENCODE_BINARY") {
		t.Errorf("error %q does not name the retired variable", err)
	}
	if !strings.Contains(err.Error(), "AGENTUM_RUNTIME_BINARY") {
		t.Errorf("error %q does not name the replacement", err)
	}
	if !strings.Contains(err.Error(), "/opt/opencode/bin/opencode.exe") {
		t.Errorf("error %q does not carry the value to move over", err)
	}
}

// TestLoad_RuntimeBinaryDefaultsToTheDescriptor: with neither variable set the
// override is empty, which is how the registry selects the adapter
// descriptor's own binary name.
func TestLoad_RuntimeBinaryDefaultsToTheDescriptor(t *testing.T) {
	t.Setenv("AGENTUM_HTTP_ADDR", ":0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.RuntimeBinary != "" {
		t.Errorf("RuntimeBinary = %q, want empty (the descriptor's default)", cfg.RuntimeBinary)
	}
	if cfg.ExecutionAdapter != "" {
		t.Errorf("ExecutionAdapter = %q, want empty (the registry's default entry)", cfg.ExecutionAdapter)
	}
}

// TestLoad_ArtifactScanPolicy pins the one config value that decides whether a
// credential-shaped artifact is rewritten or refused. It fails at load rather
// than falling back, because an unnoticed fallback is the worst failure mode
// available: an operator who asked for rejection and got redaction believes
// secrets are being blocked when they are being stored.
func TestLoad_ArtifactScanPolicy(t *testing.T) {
	cases := []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "", want: "redact"}, // unset → the documented default
		{value: "redact", want: "redact"},
		{value: "reject", want: "reject"},
		{value: "fail", wantErr: true},   // plausible synonym, not a real value
		{value: "REJECT", wantErr: true}, // policies are matched exactly
		{value: "off", wantErr: true},
	}
	for _, table := range cases {
		name := table.value
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			if table.value == "" {
				// Cannot t.Setenv("") — that sets an empty value, which is a
				// different case from an absent variable.
				t.Setenv("AGENTUM_HTTP_ADDR", ":0")
			} else {
				t.Setenv("AGENTUM_ARTIFACT_SCAN_POLICY", table.value)
			}
			cfg, err := Load()
			if table.wantErr {
				if err == nil {
					t.Fatalf("Load() accepted policy %q", table.value)
				}
				if !strings.Contains(err.Error(), "AGENTUM_ARTIFACT_SCAN_POLICY") {
					t.Errorf("error %q does not name the offending variable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.ArtifactScanPolicy != table.want {
				t.Errorf("ArtifactScanPolicy = %q, want %q", cfg.ArtifactScanPolicy, table.want)
			}
		})
	}
}

func TestPublicationConfiguration(test *testing.T) {
	for _, scenario := range []struct {
		name, enabled, token, provider, apiBase string
		wantError                               bool
	}{
		{name: "disabled", enabled: "false", provider: "github", apiBase: "https://api.github.com"},
		{name: "enabled", enabled: "true", token: "test-token", provider: "github", apiBase: "https://api.github.com"},
		{name: "missing-token", enabled: "true", provider: "github", apiBase: "https://api.github.com", wantError: true},
		{name: "blank-token", enabled: "true", token: " ", provider: "github", apiBase: "https://api.github.com", wantError: true},
		{name: "unknown-provider", enabled: "true", token: "test-token", provider: "absent", apiBase: "https://api.github.com", wantError: true},
		{name: "invalid-boolean", enabled: "perhaps", provider: "github", apiBase: "https://api.github.com", wantError: true},
		{name: "credential-url", enabled: "true", token: "test-token", provider: "github", apiBase: "https://secret@api.github.com", wantError: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("AGENTUM_PUBLISH_ENABLED", scenario.enabled)
			test.Setenv("AGENTUM_PUBLISH_TOKEN", scenario.token)
			test.Setenv("AGENTUM_PUBLISH_PROVIDER", scenario.provider)
			test.Setenv("AGENTUM_PUBLISH_API_BASE", scenario.apiBase)
			cfg, err := Load()
			if (err != nil) != scenario.wantError {
				test.Fatalf("Load error=%v wantError=%t", err, scenario.wantError)
			}
			if err != nil && (strings.Contains(err.Error(), "test-token") || strings.Contains(err.Error(), "secret@")) {
				test.Fatal("error contains credential")
			}
			if scenario.name == "missing-token" && (!strings.Contains(err.Error(), "AGENTUM_PUBLISH_ENABLED") || !strings.Contains(err.Error(), "AGENTUM_PUBLISH_TOKEN")) {
				test.Fatalf("missing context: %v", err)
			}
			if err == nil && cfg.PublishEnabled != (scenario.enabled == "true") {
				test.Fatal("enabled setting lost")
			}
		})
	}
}

func TestPublicationTargetConfigurationFailsAtBoot(test *testing.T) {
	for _, scenario := range []struct{ name, remote, base, invalid string }{
		{"defaults", "origin", "", ""},
		{"slash-branch", "delivery", "release/1.2", ""},
		{"empty-remote", "", "main", "AGENTUM_PUBLISH_REMOTE"},
		{"remote-option", "-origin", "main", "AGENTUM_PUBLISH_REMOTE"},
		{"remote-space", "my remote", "main", "AGENTUM_PUBLISH_REMOTE"},
		{"remote-newline", "origin\n", "main", "AGENTUM_PUBLISH_REMOTE"},
		{"base-space", "origin", "my branch", "AGENTUM_PUBLISH_BASE_BRANCH"},
		{"base-parent", "origin", "release/../main", "AGENTUM_PUBLISH_BASE_BRANCH"},
		{"base-newline", "origin", "main\n", "AGENTUM_PUBLISH_BASE_BRANCH"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("AGENTUM_PUBLISH_REMOTE", scenario.remote)
			test.Setenv("AGENTUM_PUBLISH_BASE_BRANCH", scenario.base)
			_, err := Load()
			if scenario.invalid == "" {
				if err != nil {
					test.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), scenario.invalid) {
				test.Fatalf("err=%v want %s", err, scenario.invalid)
			}
		})
	}
}

// AGENTUM_HOST_CAPS parses comma-separated category names; the secret/mcp
// meta-categories belong to the vocabulary (CategoryOf returns them), and an
// unknown name fails the load naming the env var.
func TestLoad_HostCaps(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		value   string
		want    int
		invalid string
	}{
		{name: "unset", value: "", want: 0},
		{name: "single", value: "fs.read", want: 1},
		{name: "list with spaces", value: "fs.read, git.write, secret", want: 3},
		{name: "meta categories", value: "mcp,skill", want: 2},
		{name: "unknown", value: "fs.read, telepathy", want: 0, invalid: "AGENTUM_HOST_CAPS"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("AGENTUM_HOST_CAPS", scenario.value)
			cfg, err := Load()
			if scenario.invalid != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.invalid) {
					test.Fatalf("err=%v want %s", err, scenario.invalid)
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if len(cfg.HostCaps) != scenario.want {
				test.Fatalf("HostCaps = %v, want %d entries", cfg.HostCaps, scenario.want)
			}
		})
	}
}
