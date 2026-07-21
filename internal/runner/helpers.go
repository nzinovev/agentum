package runner

import (
	"os/exec"
	"strings"

	"github.com/sqlc-dev/pqtype"
)

// toNullRaw adapts a marshaled payload to the generated nullable-jsonb type.
// Empty (no result) is stored as NULL; a present result is stored as JSON.
func toNullRaw(raw []byte) pqtype.NullRawMessage {
	return pqtype.NullRawMessage{RawMessage: raw, Valid: len(raw) > 0}
}

// execGit runs git in dir and returns its combined output as a trimmed string.
func execGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
