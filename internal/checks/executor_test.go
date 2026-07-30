package checks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shellCommand returns the arg vector for a portable inline script. On Windows
// the only ubiquitous interpreter is cmd.exe; elsewhere /bin/sh is reliable.
// Using these keeps the executor tests off a single platform.
func shellCommand(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", script}
	}
	return []string{"sh", "-c", script}
}

func TestExecutorRunStatuses(t *testing.T) {
	// Build the registry manually to keep the command portable per OS.
	defs := []Definition{
		{Name: "passing", Command: shellCommand("exit 0")},
		{Name: "failing", Command: shellCommand("exit 3"), Required: true},
		{Name: "echoing", Command: shellCommand("echo hello-stdout")},
	}
	set := &Set{Items: itemsFromDefs(defs), RegistryRevision: "rev"}

	executor := NewExecutor(ExecutorDeps{})
	report, runErr := executor.Run(context.Background(), set, t.TempDir())
	if runErr != nil {
		t.Fatal(runErr)
	}
	byName := outcomesByName(report)
	if byName["passing"].Status != StatusPass {
		t.Errorf("passing check should be pass, got %s", byName["passing"].Status)
	}
	if byName["failing"].Status != StatusFail {
		t.Errorf("failing check should be fail, got %s", byName["failing"].Status)
	}
	if byName["failing"].ExitCode != 3 {
		t.Errorf("failing check exit code should be 3, got %d", byName["failing"].ExitCode)
	}
	if byName["echoing"].Status != StatusPass {
		t.Errorf("echoing check should be pass, got %s", byName["echoing"].Status)
	}
	if !strings.Contains(byName["echoing"].Stdout, "hello-stdout") {
		t.Errorf("echoing check should capture stdout, got %q", byName["echoing"].Stdout)
	}
	if byName["echoing"].DefinitionRevision == "" {
		t.Error("each outcome must carry a definition revision")
	}
	if report.MandatoryPassed() {
		t.Error("failing required check must make MandatoryPassed false")
	}
	failed := report.FailedMandatory()
	if len(failed) != 1 || failed[0] != "failing" {
		t.Errorf("failed mandatory should be [failing], got %v", failed)
	}
}

func TestExecutorTimeout(t *testing.T) {
	defs := []Definition{
		{Name: "slow", Command: longRunCommand(), TimeoutSeconds: 1, Required: true},
	}
	set := &Set{Items: itemsFromDefs(defs)}
	executor := NewExecutor(ExecutorDeps{DefaultTimeout: 10 * time.Minute})
	start := time.Now()
	report, err := executor.Run(context.Background(), set, t.TempDir())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	outcome := outcomesByName(report)["slow"]
	if outcome.Status != StatusTimeout {
		t.Fatalf("expected timeout status, got %s (reason %q)", outcome.Status, outcome.Reason)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout must kill the process promptly, elapsed %s", elapsed)
	}
	if !strings.Contains(outcome.Reason, "timeout") {
		t.Errorf("reason should explain timeout, got %q", outcome.Reason)
	}
}

func TestExecutorStartError(t *testing.T) {
	defs := []Definition{
		{Name: "missing", Command: []string{"this-binary-does-not-exist-xyz"}, Required: true},
	}
	set := &Set{Items: itemsFromDefs(defs)}
	executor := NewExecutor(ExecutorDeps{})
	report, err := executor.Run(context.Background(), set, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outcome := outcomesByName(report)["missing"]
	if outcome.Status != StatusError {
		t.Fatalf("expected error status for missing binary, got %s", outcome.Status)
	}
	if outcome.Reason == "" {
		t.Error("error outcome must carry a reason")
	}
}

func TestExecutorOutputCap(t *testing.T) {
	defs := []Definition{
		{Name: "noisy", Command: floodCommand(), MaxOutputBytes: 16},
	}
	set := &Set{Items: itemsFromDefs(defs)}
	executor := NewExecutor(ExecutorDeps{DefaultMaxOutput: 16})
	report, err := executor.Run(context.Background(), set, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outcome := outcomesByName(report)["noisy"]
	if !strings.Contains(outcome.Stdout, "truncated") {
		t.Errorf("output beyond cap must be truncated, got %q", outcome.Stdout)
	}
	if len(outcome.Stdout) > 64 {
		t.Errorf("capped output must stay small, got %d bytes", len(outcome.Stdout))
	}
}

func TestExecutorWorkdirScopedAndEnforced(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// A check with a valid workdir reads the sub dir (no marker there).
	defs := []Definition{
		{Name: "insub", Command: shellCommand(dirListingCmd()), Workdir: "sub"},
	}
	set := &Set{Items: itemsFromDefs(defs)}
	executor := NewExecutor(ExecutorDeps{})
	report, err := executor.Run(context.Background(), set, root)
	if err != nil {
		t.Fatal(err)
	}
	outcome := outcomesByName(report)["insub"]
	if outcome.Status != StatusPass {
		t.Fatalf("insub should pass, got %s (%s)", outcome.Status, outcome.Stderr)
	}
	// Escaping workdir rejected before execution.
	escaping := Definition{Name: "escape", Command: shellCommand("echo hi"), Workdir: "../outside"}
	if _, err := resolveWorkdir(root, escaping.Workdir); err == nil {
		t.Fatal("workdir escaping root must be rejected")
	}
}

func TestScrubbedEnvDropsCredentials(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("MY_DB_PASSWORD", "hunter2")
	t.Setenv("RELEASE_TOKEN", "tok")
	t.Setenv("SAFE_VAR", "kept")
	t.Setenv("GO_TESTING", "yes")

	env := scrubbedEnv()
	joined := strings.Join(env, "\n")
	for _, secret := range []string{"sk-secret", "sk-ant", "hunter2", "tok"} {
		if strings.Contains(joined, secret) {
			t.Errorf("scrubbed env must drop credentials, found %q", secret)
		}
	}
	if !strings.Contains(joined, "SAFE_VAR=kept") {
		t.Error("scrubbed env must keep non-credential vars")
	}
}

func TestExecutorEmptySet(t *testing.T) {
	executor := NewExecutor(ExecutorDeps{})
	report, err := executor.Run(context.Background(), &Set{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Outcomes) != 0 {
		t.Fatalf("empty set must produce no outcomes, got %d", len(report.Outcomes))
	}
	if report.Profile != ProfileLabel {
		t.Errorf("report must carry the executor profile label, got %q", report.Profile)
	}
}

// itemsFromDefs wraps definitions as required-false Items for test sets.
func itemsFromDefs(defs []Definition) []Item {
	items := make([]Item, 0, len(defs))
	for _, def := range defs {
		items = append(items, Item{Definition: def, Required: def.Required, Sources: []string{LayerBaseline}})
	}
	return items
}

func outcomesByName(report Report) map[string]Outcome {
	out := make(map[string]Outcome, len(report.Outcomes))
	for _, outcome := range report.Outcomes {
		out[outcome.Item.Definition.Name] = outcome
	}
	return out
}

// longRunCommand returns a portable command that runs for ~30s and is killable
// by the timeout. Windows `sleep` is not built in, so ping (a real binary) is
// used instead; Unix uses sleep.
func longRunCommand() []string {
	if runtime.GOOS == "windows" {
		return []string{"ping", "-n", "60", "127.0.0.1"}
	}
	return []string{"sleep", "30"}
}

// floodCommand returns a command that writes far more than the 16-byte cap so
// the limited buffer must truncate. A single echo of a long plain string works
// on both sh and cmd without quoting pitfalls.
func floodCommand() []string {
	return shellCommand("echo " + strings.Repeat("AB", 200))
}

func dirListingCmd() string {
	if runtime.GOOS == "windows" {
		return `dir /b`
	}
	return `ls`
}
