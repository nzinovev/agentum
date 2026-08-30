package config

import (
	"strings"
	"testing"
)

// TestLoad_RetiredOpencodeBinaryRefused: AGENTUM_OPENCODE_BINARY was replaced
// by the adapter-neutral AGENTUM_RUNTIME_BINARY. An operator who pinned the
// runtime under the old name must be told, not quietly dropped back to a PATH
// lookup — a binary override that stops applying does not look like a
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
// override is empty, which is how the registry knows to use the adapter
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
// than falling back, because the failure mode of a silent fallback is the worst
// one available: an operator who asked for rejection and got redaction believes
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
