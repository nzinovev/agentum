package artifacts

import (
	"strings"
	"testing"
)

func TestDefaultRedactor_BearerToken(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	input := []byte("Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789")
	out, err := redactor.Redact("config.yaml", "config", input)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if strings.Contains(string(out), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("bearer token not redacted: %q", out)
	}
	if !strings.Contains(string(out), redactedPlaceholder) {
		t.Errorf("placeholder missing in: %q", out)
	}
}

func TestDefaultRedactor_AWSAccessKey(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	// AKIA followed by exactly 16 upper-alphanumerics.
	input := []byte("AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	out, _ := redactor.Redact("env.txt", "config", input)
	if strings.Contains(string(out), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS AKID not redacted: %q", out)
	}
	if strings.Contains(string(out), "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") {
		t.Errorf("AWS secret not redacted: %q", out)
	}
}

func TestDefaultRedactor_GitHubPAT(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	input := []byte("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")
	out, _ := redactor.Redact("config.yml", "yaml", input)
	if strings.Contains(string(out), "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD") {
		t.Errorf("GitHub PAT not redacted: %q", out)
	}
}

func TestDefaultRedactor_LabeledSecret(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	input := []byte(`{"api_key":"extremelylongsecrettokenvalue1234567890"}`)
	out, _ := redactor.Redact("env.json", "json", input)
	if strings.Contains(string(out), "extremelylongsecrettokenvalue1234567890") {
		t.Errorf("labeled api_key not redacted: %q", out)
	}
}

func TestDefaultRedactor_PreservesNonSecretContent(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	input := []byte("title: My Spec\nbody: ordinary content without secrets")
	out, _ := redactor.Redact("spec.md", "spec", input)
	if string(out) != string(input) {
		t.Errorf("non-secret content changed: %q", out)
	}
}

func TestDefaultRedactor_BinaryContentPassThrough(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	// NUL byte near the start flags binary content.
	input := []byte{0x00, 0xFF, 0xFE, 't', 'e', 's', 't'}
	out, _ := redactor.Redact("blob.bin", "binary", input)
	if !bytesEqual(out, input) {
		t.Errorf("binary content modified by redactor")
	}
}

func TestDefaultRedactor_PemPrivateKey(t *testing.T) {
	t.Parallel()
	redactor := NewDefaultRedactor()
	input := []byte(
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\n",
	)
	out, _ := redactor.Redact("key.pem", "text", input)
	if strings.Contains(string(out), "MIIEpAIBAA") {
		t.Errorf("PEM private key not redacted: %q", out)
	}
}

func TestLooksTextual_KnownBinaryExtensions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		kind string
		want bool
	}{
		{name: "spec.md", kind: "spec", want: true},
		{name: "result.json", kind: "result_json", want: true},
		{name: "screenshot.png", kind: "image", want: false},
		{name: "archive.zip", kind: "", want: false},
		{name: "binary", kind: "binary", want: false},
	}
	for _, table := range cases {
		table := table
		t.Run(table.name, func(t *testing.T) {
			t.Parallel()
			got := looksTextual(table.name, table.kind, []byte("ok"))
			if got != table.want {
				t.Errorf("looksTextual(%q,%q) = %v, want %v", table.name, table.kind, got, table.want)
			}
		})
	}
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
