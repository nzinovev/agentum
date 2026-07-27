package artifacts

import (
	"regexp"
	"strings"
)

// DefaultRedactor is the best-effort secret scrubber. It targets patterns that
// are unambiguously credentials in real configs (Bearer / Authorization
// headers, AWS AKIA keys, GitHub PATs, generic token/secret/password labels)
// and leaves everything else alone. It is intentionally conservative: false
// negatives (missed secrets) are the operator's problem; false positives
// (mangled artifacts) are worse, because they silently corrupt evidence.
type DefaultRedactor struct{}

// NewDefaultRedactor returns a DefaultRedactor.
func NewDefaultRedactor() *DefaultRedactor { return &DefaultRedactor{} }

// secretRule is one pattern plus the template that replaces each match. The
// template may reference captured groups ($1, $2 …) so non-secret context
// (labels, surrounding quotes) is preserved.
type secretRule struct {
	pattern   *regexp.Regexp
	replaceBy string
}

// secretRules are applied in order. The list is deliberately short and
// pattern-driven (no dictionary lookups) — predictable for operators and cheap
// to run on every Put.
var secretRules = []secretRule{
	// Generic token / secret / password / api_key labeled values. Captured
	// group 2 is the secret itself; the label and surrounding quote are kept.
	// The pattern tolerates JSON-style "key":"value" and bare key=value.
	{
		pattern:   regexp.MustCompile(`(?i)(token|secret|password|api[_-]?key|access[_-]?key)["']?\s*[:=]\s*["']?([A-Za-z0-9+/=_-]{20,})["']?`),
		replaceBy: `${1}=[REDACTED]`,
	},
	// Bearer tokens.
	{
		pattern:   regexp.MustCompile(`(?i)(bearer\s+)([A-Za-z0-9._~+/=-]{8,})`),
		replaceBy: `${1}[REDACTED]`,
	},
	// Authorization header (also covered above when labeled, but raw auth
	// headers may not be).
	{
		pattern:   regexp.MustCompile(`(?i)(authorization\s*[:=]\s*"?)([A-Za-z]+[ ]?[A-Za-z0-9._~+/=-]{8,})(?:"?)(\s|$)`),
		replaceBy: `${1}[REDACTED]${3}`,
	},
	// AWS access key id (AKIA...) — no label needed, the prefix is the signal.
	{pattern: regexp.MustCompile(`AKIA[0-9A-Z]{16}`), replaceBy: `[REDACTED:aws-akid]`},
	// AWS secret access key (40-char base64) under the canonical label.
	{
		pattern:   regexp.MustCompile(`(?i)(aws_secret_access_key\s*[:=]\s*"?)([A-Za-z0-9/+=]{40})"?`),
		replaceBy: `${1}[REDACTED]`,
	},
	// GitHub PATs (classic and fine-grained).
	{pattern: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`), replaceBy: `[REDACTED:github-pat]`},
	// Generic private key PEM blocks.
	{
		pattern:   regexp.MustCompile(`-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+PRIVATE KEY-----`),
		replaceBy: `[REDACTED:pem-private-key]`,
	},
}

// redactedPlaceholder is the canonical marker the redactor writes in place of
// a secret. Exported so tests can grep for it.
const redactedPlaceholder = "[REDACTED]"

// Redact implements Redactor. It returns a copy of bytes with secret-shaped
// substrings replaced. It never returns an error and never mutates the input
// slice — the caller may keep using its copy.
func (*DefaultRedactor) Redact(name string, kind string, bytes []byte) ([]byte, error) {
	// Only scan text-shaped content. Binary kinds are passed through.
	if !looksTextual(name, kind, bytes) {
		return bytes, nil
	}
	text := string(bytes)
	redacted := text
	for _, rule := range secretRules {
		if rule.pattern.MatchString(redacted) {
			redacted = rule.pattern.ReplaceAllString(redacted, rule.replaceBy)
		}
	}
	if redacted == text {
		return bytes, nil
	}
	return []byte(redacted), nil
}

// looksTextual reports whether the bytes are worth scanning for secrets. We
// avoid scanning binary blobs (images, compiled artifacts) — running regexes
// on them is expensive and produces false positives.
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
