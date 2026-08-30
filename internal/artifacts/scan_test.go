package artifacts

import (
	"errors"
	"strings"
	"testing"
)

// mustScan runs the redacting scanner and fails the test on an unexpected
// error. Most cases here assert on what was stored, not on the error path.
func mustScan(t *testing.T, name, kind string, input []byte) ScanResult {
	t.Helper()
	result, err := NewDefaultScanner(PolicyRedact).Scan(name, kind, input)
	if err != nil {
		t.Fatalf("Scan(%q): %v", name, err)
	}
	return result
}

func TestDefaultScanner_BearerToken(t *testing.T) {
	t.Parallel()
	input := []byte("Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789")
	out := mustScan(t, "config.yaml", "config", input)
	if strings.Contains(string(out.Bytes), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("bearer token not redacted: %q", out.Bytes)
	}
	if !strings.Contains(string(out.Bytes), redactedPlaceholder) {
		t.Errorf("placeholder missing in: %q", out.Bytes)
	}
	if out.Clean() {
		t.Error("scan reported clean despite redacting a token")
	}
	if !out.Rewritten {
		t.Error("Rewritten = false after a substitution")
	}
}

func TestDefaultScanner_AWSAccessKey(t *testing.T) {
	t.Parallel()
	// AKIA followed by exactly 16 upper-alphanumerics.
	input := []byte("AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	out := mustScan(t, "env.txt", "config", input)
	if strings.Contains(string(out.Bytes), "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("AWS AKID not redacted: %q", out.Bytes)
	}
	if strings.Contains(string(out.Bytes), "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY") {
		t.Errorf("AWS secret not redacted: %q", out.Bytes)
	}
}

func TestDefaultScanner_GitHubPAT(t *testing.T) {
	t.Parallel()
	input := []byte("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")
	out := mustScan(t, "config.yml", "yaml", input)
	if strings.Contains(string(out.Bytes), "ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD") {
		t.Errorf("GitHub PAT not redacted: %q", out.Bytes)
	}
}

func TestDefaultScanner_LabeledSecret(t *testing.T) {
	t.Parallel()
	input := []byte(`{"api_key":"extremelylongsecrettokenvalue1234567890"}`)
	out := mustScan(t, "env.json", "json", input)
	if strings.Contains(string(out.Bytes), "extremelylongsecrettokenvalue1234567890") {
		t.Errorf("labeled api_key not redacted: %q", out.Bytes)
	}
}

func TestDefaultScanner_PreservesNonSecretContent(t *testing.T) {
	t.Parallel()
	input := []byte("title: My Spec\nbody: ordinary content without secrets")
	out := mustScan(t, "spec.md", "spec", input)
	if string(out.Bytes) != string(input) {
		t.Errorf("non-secret content changed: %q", out.Bytes)
	}
	if !out.Clean() {
		t.Errorf("clean content reported findings: %v", out.Findings)
	}
	if out.Rewritten {
		t.Error("Rewritten = true for untouched content")
	}
}

func TestDefaultScanner_PemPrivateKey(t *testing.T) {
	t.Parallel()
	input := []byte(
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\n",
	)
	out := mustScan(t, "key.txt", "text", input)
	if strings.Contains(string(out.Bytes), "MIIEpAIBAA") {
		t.Errorf("PEM private key not redacted: %q", out.Bytes)
	}
}

// TestDefaultScanner_BinaryIsScannedButNeverRewritten pins the compromise the
// review asked for. Binary artifacts used to be skipped outright, so a
// credential inside one entered the store unnoticed. They are now matched
// against the context-free rules — but rewriting them would change their length
// and corrupt the blob, so under the redact policy the bytes are stored intact
// and only the finding is reported. Rejecting the write is the only way to
// actually stop it, which is what the next test covers.
func TestDefaultScanner_BinaryIsScannedButNeverRewritten(t *testing.T) {
	t.Parallel()
	// NUL byte near the start flags binary content; an AWS key id rides along.
	input := append([]byte{0x00, 0xFF, 0xFE}, []byte("AKIAIOSFODNN7EXAMPLE")...)
	out := mustScan(t, "blob.bin", "binary", input)

	if !bytesEqual(out.Bytes, input) {
		t.Errorf("binary content modified: %q", out.Bytes)
	}
	if out.Rewritten {
		t.Error("Rewritten = true for binary content")
	}
	if out.Clean() {
		t.Error("credential inside binary content went unreported")
	}
	if len(out.Findings) != 1 || out.Findings[0] != "aws-access-key-id" {
		t.Errorf("findings = %v, want [aws-access-key-id]", out.Findings)
	}
}

// TestDefaultScanner_BinaryContextRulesDoNotFire: the context-sensitive rules
// (a "token:" label, an Authorization header) are not run against binary
// content. They exist to catch config files; on a binary stream they are noise,
// and under PolicyReject noise means refusing legitimate artifacts.
func TestDefaultScanner_BinaryContextRulesDoNotFire(t *testing.T) {
	t.Parallel()
	input := append([]byte{0x00, 0xFF}, []byte(`token: aaaaaaaaaaaaaaaaaaaaaaaaaaaa`)...)
	out := mustScan(t, "blob.bin", "binary", input)
	if !out.Clean() {
		t.Errorf("context rule fired on binary content: %v", out.Findings)
	}
}

func TestDefaultScanner_RejectPolicyRefusesTheWrite(t *testing.T) {
	t.Parallel()
	scanner := NewDefaultScanner(PolicyReject)
	input := []byte("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")

	result, err := scanner.Scan("config.yml", "yaml", input)
	if !errors.Is(err, ErrSecretDetected) {
		t.Fatalf("Scan error = %v, want ErrSecretDetected", err)
	}
	if result.Bytes != nil {
		t.Error("rejected scan still returned bytes to store")
	}
	if !strings.Contains(err.Error(), "github-pat") {
		t.Errorf("error %q does not name the rule that fired", err)
	}
}

func TestDefaultScanner_RejectPolicyAllowsCleanContent(t *testing.T) {
	t.Parallel()
	input := []byte("# Spec\n\nNothing sensitive here.\n")
	result, err := NewDefaultScanner(PolicyReject).Scan("spec.md", "spec", input)
	if err != nil {
		t.Fatalf("clean content rejected: %v", err)
	}
	if string(result.Bytes) != string(input) {
		t.Errorf("clean content altered: %q", result.Bytes)
	}
}

// TestDefaultScanner_ZeroValueRedacts: a DefaultScanner{} with no policy set
// behaves like the pre-policy redactor, so an embedding that forgets the field
// fails safe-ish rather than silently disabling the scanner.
func TestDefaultScanner_ZeroValueRedacts(t *testing.T) {
	t.Parallel()
	scanner := &DefaultScanner{}
	result, err := scanner.Scan("config.yml", "yaml", []byte("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !result.Rewritten {
		t.Error("zero-value scanner did not redact")
	}
}

// TestDefaultScanner_ScanNameRejectsCredentialPaths covers the third escape the
// review named: an artifact whose *name* is a credential location. There is
// nothing to redact in a path, so these are refused under every policy.
func TestDefaultScanner_ScanNameRejectsCredentialPaths(t *testing.T) {
	t.Parallel()
	rejected := []string{
		".ssh/id_rsa",
		"home/user/.ssh/known_hosts",
		"id_ed25519",
		".aws/credentials",
		".aws/config",
		".gnupg/secring.gpg",
		".netrc",
		".npmrc",
		".pgpass",
		"certs/server.pem",
		"keystore.jks",
		".env",
		".env.production",
		"config/.env.local",
	}
	allowed := []string{
		"spec.md",
		"result.json",
		"src/main.go",
		".env.example",
		".env.sample",
		"docs/ssh-setup.md",
		"internal/keys/registry.go",
		"testdata/aws-region-list.json",
	}
	scanner := NewDefaultScanner(PolicyRedact)

	for _, name := range rejected {
		if err := scanner.ScanName(name); !errors.Is(err, ErrSecretDetected) {
			t.Errorf("ScanName(%q) = %v, want ErrSecretDetected", name, err)
		}
	}
	for _, name := range allowed {
		if err := scanner.ScanName(name); err != nil {
			t.Errorf("ScanName(%q) rejected a legitimate artifact: %v", name, err)
		}
	}
}

// TestDefaultScanner_ScanNameNormalizesSeparators: a name that reached the
// store with Windows separators must be checked the same way as a slash path,
// or the deny-list is trivially bypassed on one platform.
func TestDefaultScanner_ScanNameNormalizesSeparators(t *testing.T) {
	t.Parallel()
	if err := NewDefaultScanner(PolicyRedact).ScanName(`.SSH\id_rsa`); !errors.Is(err, ErrSecretDetected) {
		t.Errorf(`ScanName(".SSH\\id_rsa") = %v, want ErrSecretDetected`, err)
	}
}

func TestNoScan_PassesEverythingThrough(t *testing.T) {
	t.Parallel()
	input := []byte("token: ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD")
	result, err := NoScan{}.Scan("config.yml", "yaml", input)
	if err != nil {
		t.Fatalf("NoScan.Scan: %v", err)
	}
	if !bytesEqual(result.Bytes, input) {
		t.Error("NoScan altered the content")
	}
	if err := (NoScan{}).ScanName(".ssh/id_rsa"); err != nil {
		t.Errorf("NoScan.ScanName: %v", err)
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

// TestProseScanner_OnlyCredentialShapes pins the rule split. NewProseScanner
// must ignore the label-context rules — they cannot tell a pasted credential
// from a sentence discussing one, and human-authored task text is full of the
// latter — while still catching material that identifies itself by its own
// shape.
//
// fullRejects records what the unchanged artifact scanner does with the same
// text, so the table doubles as the documented difference between the two.
// It is not uniformly true: "Authorization: Bearer header upstream" trips
// neither scanner, because both label rules need 8+ credential-ish characters
// after the label and "header" is six.
func TestProseScanner_OnlyCredentialShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		text         string
		proseRejects bool
		fullRejects  bool
	}{
		{
			name:        "bearer in prose",
			text:        "Add Bearer authentication to the /settings endpoint.",
			fullRejects: true,
		},
		{
			name:        "labeled env var",
			text:        "secret: AGENTUM_WEBHOOK_SECRET_ENV should be read from env.",
			fullRejects: true,
		},
		{
			name:        "api_key assignment to an env name",
			text:        "Rename the config key api_key = AGENTUM_WEBHOOK_SECRET_ENV",
			fullRejects: true,
		},
		{
			name: "authorization header in prose",
			text: "Send the Authorization: Bearer header upstream.",
		},
		{
			name:         "github pat",
			text:         "Paste ghp_0123456789abcdefghijklmnopqrstuvwxyz0123456789 into the config.",
			proseRejects: true,
			fullRejects:  true,
		},
		{
			name:         "aws access key id",
			text:         "The key AKIAIOSFODNN7EXAMPLE leaked.",
			proseRejects: true,
			fullRejects:  true,
		},
		{
			name:         "pem block",
			text:         "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----",
			proseRejects: true,
			fullRejects:  true,
		},
	}
	prose := NewProseScanner(PolicyReject)
	full := NewDefaultScanner(PolicyReject)
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, proseErr := prose.Scan("description", "task_request", []byte(testCase.text))
			if rejected := proseErr != nil; rejected != testCase.proseRejects {
				t.Errorf("prose scanner rejected = %v, want %v (err=%v)", rejected, testCase.proseRejects, proseErr)
			}
			// The artifact path is unchanged by the narrowing.
			_, fullErr := full.Scan("artifact.json", "config", []byte(testCase.text))
			if rejected := fullErr != nil; rejected != testCase.fullRejects {
				t.Errorf("full scanner rejected = %v, want %v (err=%v)", rejected, testCase.fullRejects, fullErr)
			}
		})
	}
}
