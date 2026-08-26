package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// Subprocess contract tests for the opencode adapter.
//
// These run in normal CI: instead of a real opencode binary they re-exec the
// test binary itself as a fake agent that speaks just enough of the protocol
// (one NDJSON line, then a result.json). That is enough to pin the properties
// no unit test over rendered structs can reach:
//
//   - the agent keeps running after Invoke returns (the run outlives the call);
//   - the hard timeout terminates it;
//   - the idle cap terminates it;
//   - caller cancellation terminates it;
//   - exactly one Wait reaps it (asserted by running under -race).
//
// The real-binary contract — does opencode actually load our config and honor
// it — is a separate, opt-in job; see docs/capabilities.md.

const (
	fakeModeEnv     = "AGENTUM_FAKE_OPENCODE"
	fakeArtifactEnv = "AGENTUM_FAKE_ARTIFACT_DIR"
	fakeDelayEnv    = "AGENTUM_FAKE_DELAY_MS"
)

// Fake agent behaviours.
const (
	// fakeWorks emits a line, works for the configured delay, then emits a
	// second line and writes result.json.
	fakeWorks = "works"
	// fakeSilent emits one line and then produces nothing for the configured
	// delay — the shape the idle cap exists for.
	fakeSilent = "silent"
)

// TestMain doubles as the fake agent's entry point. It must intercept before
// the testing framework parses flags, because the adapter invokes the binary
// with opencode's argv ("run --format json …", "debug skill", or "--version"),
// not with test flags. The debug-skill mode serves the ContextProber tests
// (ADR 0002 D6); the version mode serves the readiness-probe tests (ADR 0005
// D2).
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		os.Exit(runFakeAgent(mode))
	}
	if os.Getenv(fakeDebugSkillEnv) != "" {
		os.Exit(runFakeDebugSkill(os.Getenv(fakeDebugSkillEnv)))
	}
	if mode := os.Getenv(fakeVersionEnv); mode != "" {
		os.Exit(runFakeVersion(mode))
	}
	os.Exit(m.Run())
}

// fakeDebugSkillEnv switches the test binary into the `opencode debug skill`
// fake. Its value selects the behaviour: well-formed JSON, malformed JSON,
// hang, or non-zero exit.
const fakeDebugSkillEnv = "AGENTUM_FAKE_DEBUG_SKILL"

// runFakeDebugSkill plays the `opencode debug skill` side of the ContextProber
// contract: it writes a JSON array of skills to stdout and exits. The mode
// selects malformed JSON, a hang, or a non-zero exit so the prober's failure
// paths are exercised without a real opencode binary.
func runFakeDebugSkill(mode string) int {
	switch mode {
	case fakeDebugMalformed:
		fmt.Println(`[{not json`)
		return 0
	case fakeDebugExit:
		fmt.Fprintln(os.Stderr, "fake debug error")
		return 2
	case fakeDebugHang:
		// Block long enough that the probe's timeout fires and the process
		// group is killed. The prober test shrinks probeTimeout so this is well
		// within the test's budget.
		time.Sleep(30 * time.Second)
		return 0
	default: // fakeDebugWellformed and any unknown mode
		fmt.Println(fakeDebugSkillOutput)
		return 0
	}
}

// fakeDebugSkill modes.
const (
	fakeDebugWellformed = "wellformed"
	fakeDebugMalformed  = "malformed"
	fakeDebugExit       = "exit"
	fakeDebugHang       = "hang"
)

// fakeDebugSkillOutput is the JSON array the well-formed fake emits. It carries
// a marker body so a test can assert the body is hashed and discarded (never
// returned in the SkillRef).
const fakeDebugSkillOutput = `[
  {"name":"customize-opencode","description":"Customize opencode","location":"<built-in>","content":"MARKER-BODY-SHOULD-NOT-LEAK"},
  {"name":"user-skill","description":"A user skill","location":"/home/user/.claude/skills/user-skill","content":"user body"}
]`

// runFakeAgent plays the agent side of the adapter contract.
func runFakeAgent(mode string) int {
	fmt.Println(`{"type":"text","sessionID":"ses_fake","part":{"type":"text","text":"starting"}}`)
	delay := 100 * time.Millisecond
	if raw := os.Getenv(fakeDelayEnv); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			delay = time.Duration(parsed) * time.Millisecond
		}
	}
	time.Sleep(delay)

	if mode == fakeWorks {
		fmt.Println(`{"type":"text","sessionID":"ses_fake","part":{"type":"text","text":"done"}}`)
	}
	artifactDir := os.Getenv(fakeArtifactEnv)
	if artifactDir == "" {
		return 1
	}
	result := []byte(`{"schema_version":"1","status":"complete","summary":"fake run"}`)
	if err := os.WriteFile(filepath.Join(artifactDir, "result.json"), result, 0o644); err != nil {
		return 1
	}
	return 0
}

// fakeInvocation wires the adapter to the test binary as its agent, and returns
// the adapter plus the invocation to run. profile carries the timeouts under
// test; an empty profile means no caps and no limits.
func fakeInvocation(t *testing.T, mode string, delay time.Duration, profile caps.Profile) (*OpencodeAdapter, Invocation) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	workdir := t.TempDir()
	artifactDir := filepath.Join(workdir, ".agentum", "artifacts", "spec")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}

	t.Setenv(fakeModeEnv, mode)
	t.Setenv(fakeArtifactEnv, artifactDir)
	t.Setenv(fakeDelayEnv, strconv.Itoa(int(delay.Milliseconds())))

	return NewOpencodeAdapter(self), Invocation{
		Workdir:     workdir,
		ArtifactDir: artifactDir,
		Prompt:      "fake stage",
		Profile:     profile,
	}
}

// drain consumes the adapter stream and returns the terminal outcome: the
// parsed result on success, or the terminal error.
func drain(t *testing.T, events <-chan Event) (*Result, error) {
	t.Helper()
	var (
		result *Result
		failed error
	)
	for event := range events {
		switch event.Kind {
		case EventResult:
			result = event.Result
		case EventError:
			failed = event.Err
		case EventStream:
		}
	}
	return result, failed
}

// shrinkKillGrace shortens the terminate-then-force escalation for the duration
// of one test, so a timeout assertion does not wait out the production grace
// period. Safe because every test using it is serial (they all call t.Setenv).
func shrinkKillGrace(t *testing.T) {
	t.Helper()
	original := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = original })
}

// TestInvoke_SubprocessOutlivesInvokeReturn is the regression test for the run
// context being released at the wrong seam. Invoke returns as soon as the
// process starts; if the context that binds the subprocess is cancelled at that
// point, the agent is killed milliseconds into its work and the run always ends
// in "cancelled", whatever the configured timeout says.
func TestInvoke_SubprocessOutlivesInvokeReturn(t *testing.T) {
	adapter, invocation := fakeInvocation(t, fakeWorks, 700*time.Millisecond, caps.Profile{})

	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)
	if failed != nil {
		t.Fatalf("run failed after Invoke returned: %v", failed)
	}
	if result == nil {
		t.Fatal("no result event; the agent did not run to completion")
	}
	if result.SessionID != "ses_fake" {
		t.Errorf("SessionID = %q, want ses_fake", result.SessionID)
	}
	if result.Status != StatusComplete {
		t.Errorf("status = %q, want complete", result.Status)
	}
}

// TestInvoke_HardTimeoutStopsRun: the profile's hard cap terminates a run that
// overruns it, and says so.
func TestInvoke_HardTimeoutStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeWorks, 10*time.Second,
		caps.Profile{HardTimeout: 300 * time.Millisecond})

	started := time.Now()
	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite the hard timeout: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; the hard timeout did not fire")
	}
	if !strings.Contains(failed.Error(), "hard timeout") {
		t.Errorf("error = %q, want it to name the hard timeout", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; the timeout did not terminate the process tree", elapsed)
	}
}

// TestInvoke_IdleTimeoutStopsRun: an agent that goes quiet past the idle cap is
// terminated, and the terminal error names the idle cap rather than a generic
// cancellation.
func TestInvoke_IdleTimeoutStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeSilent, 10*time.Second,
		caps.Profile{IdleTimeout: 300 * time.Millisecond})

	started := time.Now()
	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite the idle cap: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; the idle cap did not fire")
	}
	if !strings.Contains(failed.Error(), "idle cap") {
		t.Errorf("error = %q, want it to name the idle cap", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; the idle cap did not terminate the process tree", elapsed)
	}
}

// TestInvoke_CallerCancellationStopsRun: the §5.1 cancellation seam. The runner
// cancels the context to abort a stage; the agent must go with it.
func TestInvoke_CallerCancellationStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeWorks, 10*time.Second, caps.Profile{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	events, err := adapter.Invoke(ctx, invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite cancellation: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; cancellation did not fire")
	}
	if !strings.Contains(failed.Error(), "cancelled") {
		t.Errorf("error = %q, want it to report cancellation", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; cancellation did not terminate the process tree", elapsed)
	}
}

// TestInvoke_CleansUpConfigAfterRun: the per-invocation config directory is
// released once the run ends — but not before, since it backs the child's
// permissions for as long as the child lives.
func TestInvoke_CleansUpConfigAfterRun(t *testing.T) {
	adapter, invocation := fakeInvocation(t, fakeWorks, 50*time.Millisecond, caps.Profile{})

	plan, err := prepareEnforcement(invocation)
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	configRoot := filepath.Dir(plan.configDir)
	plan.cleanup()
	before := countAdapterConfigDirs(t, configRoot)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if _, failed := drain(t, events); failed != nil {
		t.Fatalf("run failed: %v", failed)
	}

	if after := countAdapterConfigDirs(t, configRoot); after != before {
		t.Errorf("config dirs before=%d after=%d; the run leaked its config directory", before, after)
	}
}

// countAdapterConfigDirs counts the adapter's per-invocation config directories
// under root.
func countAdapterConfigDirs(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "agentum-opencode-") {
			count++
		}
	}
	return count
}
