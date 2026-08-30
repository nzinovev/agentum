package artifacts

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// DefaultScanner is the best-effort credential guard. It targets patterns that
// are unambiguously credentials in real configs (Bearer / Authorization
// headers, AWS AKIA keys, GitHub PATs, generic token/secret/password labels)
// and leaves everything else alone. It is intentionally conservative: false
// negatives (missed secrets) are the operator's problem; false positives
// (mangled artifacts) are worse, because they silently corrupt evidence.
type DefaultScanner struct {
	// Policy decides what a finding does. Zero value is PolicyRedact, so a
	// DefaultScanner{} behaves like the pre-policy redactor.
	Policy ScanPolicy

	// proseOnly restricts the rule set to the credential-shape rules, for
	// human-authored text rather than machine-written config. Unexported so the
	// zero value keeps the full rule set: a bare DefaultScanner{} scans artifacts
	// exactly as it always did.
	proseOnly bool
}

// NewDefaultScanner returns a DefaultScanner with the given policy. An empty
// policy means PolicyRedact.
func NewDefaultScanner(policy ScanPolicy) *DefaultScanner {
	if policy == "" {
		policy = PolicyRedact
	}
	return &DefaultScanner{Policy: policy}
}

// NewProseScanner returns a scanner for human-authored text — a task title or
// description, not a config file an agent produced. It runs only the
// credentialShape rules.
//
// The label-context rules must not run here. They exist to catch `token: <20
// chars>` in a machine-written config, and in prose they fire on a sentence
// that merely *discusses* credentials: "Add Bearer authentication to
// /settings" matches bearer-token, and "secret: AGENTUM_WEBHOOK_SECRET_ENV
// should be read from env" matches labeled-secret. Under PolicyReject that
// makes an ordinary auth-related backend task impossible to create, with no
// override path for the author — a false positive that costs more than the
// false negative it prevents, which is the trade this package already declares
// it makes.
func NewProseScanner(policy ScanPolicy) *DefaultScanner {
	if policy == "" {
		policy = PolicyRedact
	}
	return &DefaultScanner{Policy: policy, proseOnly: true}
}

// secretRule is one pattern plus the template that replaces each match. The
// template may reference captured groups ($1, $2 …) so non-secret context
// (labels, surrounding quotes) is preserved. name identifies the rule in
// ScanResult.Findings — operators see which rule fired, not just that one did.
type secretRule struct {
	name      string
	pattern   *regexp.Regexp
	replaceBy string
	// literal marks rules whose pattern is a fixed ASCII shape with no
	// surrounding context. Only these are run against binary content: a
	// context-sensitive rule (a "token:" label) produces noise in a binary
	// stream, while an AKIA prefix or a PEM header does not.
	literal bool
	// credentialShape marks rules whose pattern matches the credential
	// material itself — a key prefix, a PEM block, a canonical key name
	// together with its fixed-width value — rather than a label followed by
	// arbitrary text. Only these run against prose (NewProseScanner): a rule
	// that keys off the word "secret" or "bearer" cannot tell a pasted
	// credential from a sentence about credentials, and human-authored text is
	// full of the latter.
	//
	// Distinct from literal, which is about binary streams: every literal rule
	// is a credential shape, but aws-secret-access-key is a credential shape
	// that is not literal (it needs its label to bound the 40-char value).
	credentialShape bool
}

// secretRules are applied in order. The list is deliberately short and
// pattern-driven (no dictionary lookups) — predictable for operators and cheap
// to run on every Put.
var secretRules = []secretRule{
	// Generic token / secret / password / api_key labeled values. Captured
	// group 2 is the secret itself; the label and surrounding quote are kept.
	// The pattern tolerates JSON-style "key":"value" and bare key=value.
	{
		name:      "labeled-secret",
		pattern:   regexp.MustCompile(`(?i)(token|secret|password|api[_-]?key|access[_-]?key)["']?\s*[:=]\s*["']?([A-Za-z0-9+/=_-]{20,})["']?`),
		replaceBy: `${1}=[REDACTED]`,
	},
	// Bearer tokens.
	{
		name:      "bearer-token",
		pattern:   regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]{8,})`),
		replaceBy: `${1}[REDACTED]`,
	},
	// Authorization header (also covered above when labeled, but raw auth
	// headers may not be).
	{
		name:      "authorization-header",
		pattern:   regexp.MustCompile(`(?i)(authorization\s*[:=]\s*"?)([A-Za-z]+[ ]?[A-Za-z0-9._~+/=-]{8,})(?:"?)(\s|$)`),
		replaceBy: `${1}[REDACTED]${3}`,
	},
	// AWS access key id (AKIA...) — no label needed, the prefix is the signal.
	{
		name:            "aws-access-key-id",
		pattern:         regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		replaceBy:       `[REDACTED:aws-akid]`,
		literal:         true,
		credentialShape: true,
	},
	// AWS secret access key (40-char base64) under the canonical label.
	{
		name:            "aws-secret-access-key",
		pattern:         regexp.MustCompile(`(?i)(aws_secret_access_key\s*[:=]\s*"?)([A-Za-z0-9/+=]{40})"?`),
		replaceBy:       `${1}[REDACTED]`,
		credentialShape: true,
	},
	// GitHub PATs (classic and fine-grained).
	{
		name:            "github-pat",
		pattern:         regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),
		replaceBy:       `[REDACTED:github-pat]`,
		literal:         true,
		credentialShape: true,
	},
	// Generic private key PEM blocks.
	{
		name:            "pem-private-key",
		pattern:         regexp.MustCompile(`-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+PRIVATE KEY-----`),
		replaceBy:       `[REDACTED:pem-private-key]`,
		literal:         true,
		credentialShape: true,
	},
}

// redactedPlaceholder is the canonical marker the scanner writes in place of
// a secret. Exported so tests can grep for it.
const redactedPlaceholder = "[REDACTED]"

// secretPaths are artifact names that are credentials by location rather than
// by content. An agent has no legitimate reason to declare one as stage output,
// and unlike bytes a path cannot be redacted — so these are refused under every
// policy. Matched against the slash-separated name and each of its segments.
var secretPaths = []struct {
	name    string
	matches func(name, base string) bool
}{
	{
		name: "ssh-private-key",
		matches: func(name, base string) bool {
			return hasSegment(name, ".ssh") ||
				base == "id_rsa" || base == "id_dsa" || base == "id_ecdsa" || base == "id_ed25519"
		},
	},
	{
		name: "cloud-credentials",
		matches: func(name, base string) bool {
			return (hasSegment(name, ".aws") && (base == "credentials" || base == "config")) ||
				hasSegment(name, ".gnupg") ||
				(hasSegment(name, "gcloud") && strings.HasSuffix(base, ".json"))
		},
	},
	{
		name: "credential-file",
		matches: func(_, base string) bool {
			switch base {
			case ".netrc", "_netrc", ".npmrc", ".pypirc", ".pgpass", ".htpasswd":
				return true
			}
			return false
		},
	},
	{
		name: "key-material",
		matches: func(_, base string) bool {
			switch path.Ext(base) {
			case ".pem", ".p12", ".pfx", ".jks", ".keystore":
				return true
			}
			return false
		},
	},
	{
		name:    "dotenv",
		matches: func(_, base string) bool { return isSecretDotenv(base) },
	},
}

// isSecretDotenv reports whether a base name is a real .env file rather than a
// checked-in template. ".env.example" is documentation and routinely a genuine
// stage output; ".env.production" is a credential.
func isSecretDotenv(base string) bool {
	if base != ".env" && !strings.HasPrefix(base, ".env.") {
		return false
	}
	switch strings.TrimPrefix(base, ".env.") {
	case "example", "sample", "template", "dist", "defaults":
		return false
	}
	return true
}

// hasSegment reports whether a slash-separated path contains the given path
// segment. Segment-wise so "docs/ssh-notes.md" does not match ".ssh".
func hasSegment(name, segment string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, segment) {
			return true
		}
	}
	return false
}

// ScanName implements Scanner. Credential-shaped names are refused under every
// policy — there is nothing to redact in a path, and storing the bytes under a
// different name would only hide where they came from.
func (*DefaultScanner) ScanName(name string) error {
	normalized := strings.ToLower(strings.ReplaceAll(name, `\`, "/"))
	base := path.Base(normalized)
	for _, rule := range secretPaths {
		if rule.matches(normalized, base) {
			return fmt.Errorf("%w: name %q matches %s", ErrSecretDetected, name, rule.name)
		}
	}
	return nil
}

// Scan implements Scanner. It never mutates the input slice — the caller may
// keep using its copy.
func (scanner *DefaultScanner) Scan(name string, kind string, bytes []byte) (ScanResult, error) {
	textual := looksTextual(name, kind, bytes)
	result := ScanResult{Bytes: bytes}
	text := string(bytes)
	scanned := text
	for _, rule := range secretRules {
		// Binary content is matched only against the context-free rules, and
		// never rewritten: substituting a placeholder into a binary stream
		// changes its length and corrupts the artifact. Detection still matters,
		// because PolicyReject can then stop the write.
		if !textual && !rule.literal {
			continue
		}
		// Prose runs only against rules that match credential material itself. A
		// label-context rule cannot distinguish a pasted secret from a sentence
		// about secrets.
		if scanner.proseOnly && !rule.credentialShape {
			continue
		}
		// Detection runs against the original text and rewriting against the
		// accumulated one. A rule whose match an earlier rule already redacted
		// still has to be reported: "token: ghp_…" is both a labeled secret and
		// a GitHub PAT, and an operator reading the findings needs to see both.
		if rule.pattern.MatchString(text) {
			result.Findings = append(result.Findings, rule.name)
		}
		if textual && rule.pattern.MatchString(scanned) {
			scanned = rule.pattern.ReplaceAllString(scanned, rule.replaceBy)
		}
	}
	if result.Clean() {
		return result, nil
	}
	if scanner.policy() == PolicyReject {
		return ScanResult{}, fmt.Errorf("%w: %q matches %s", ErrSecretDetected, name, strings.Join(result.Findings, ", "))
	}
	if textual && scanned != text {
		result.Bytes = []byte(scanned)
		result.Rewritten = true
	}
	return result, nil
}

// policy returns the configured policy, defaulting the zero value to redact so
// a bare DefaultScanner{} is usable.
func (scanner *DefaultScanner) policy() ScanPolicy {
	if scanner.Policy == "" {
		return PolicyRedact
	}
	return scanner.Policy
}

// looksTextual reports whether the bytes can be safely rewritten. Binary blobs
// (images, compiled artifacts) are still scanned for the context-free rules,
// but never modified.
func looksTextual(name string, kind string, bytes []byte) bool {
	if nulIndex := indexOfByte(bytes, 0); nulIndex >= 0 && nulIndex < 1024 {
		return false // binary content has a NUL byte near the start
	}
	switch strings.ToLower(kind) {
	case "binary", "image", "blob":
		return false
	}
	switch extensionOf(strings.ToLower(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".tar", ".gz", ".tgz":
		return false
	}
	return true
}

func indexOfByte(bytes []byte, target byte) int {
	for index, value := range bytes {
		if value == target {
			return index
		}
	}
	return -1
}

func extensionOf(name string) string {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return ""
	}
	return name[dot:]
}
