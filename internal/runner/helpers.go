package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/routing"
	"github.com/sqlc-dev/pqtype"
)

// priorStageArtifacts scans the worktree's shared artifact root for result.json
// files produced by earlier stages and returns them as routing-block references.
// The current stage is excluded (it has not run yet). This is how a later stage
// reads an earlier stage's structured output by path (filesystem-as-bus): the
// implementer reads the plan, the fixer reads the reviewer findings, etc. Stages
// without a result.json yet are skipped. The result is stage-id-sorted so the
// rendered list is stable across invocations.
func (runner *Runner) priorStageArtifacts(worktreeRoot, taskID, currentStage string) []routing.PriorStage {
	artifactsRoot := filepath.Join(worktreeRoot, ".agentum", taskID, ".ag-artifacts")
	entries, err := os.ReadDir(artifactsRoot)
	if err != nil {
		return nil
	}
	prior := make([]routing.PriorStage, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == currentStage {
			continue
		}
		resultPath := filepath.Join(artifactsRoot, entry.Name(), "result.json")
		if _, statErr := os.Stat(resultPath); statErr != nil {
			continue
		}
		prior = append(prior, routing.PriorStage{Stage: entry.Name(), Path: resultPath})
	}
	sort.Slice(prior, func(left, right int) bool { return prior[left].Stage < prior[right].Stage })
	return prior
}

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
