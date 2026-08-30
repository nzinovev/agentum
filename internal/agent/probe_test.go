package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVersionEnv switches the test binary into the `--version` fake. Its value
// selects the behaviour: a version print, a non-zero exit, empty output, a
// hang, or a non-version banner. The counter file (fakeVersionCounterEnv)
// records how many times the fake executed, which is how the memoization test
// proves the second Probe spawned nothing.
const (
	fakeVersionEnv        = "AGENTUM_FAKE_VERSION"
	fakeVersionCounterEnv = "AGENTUM_FAKE_VERSION_COUNTER"
	fakeVersionDefaultOut = "1.18.11\n"
)

// runFakeVersion plays the runtime's `--version` side of the readiness-probe
// contract.
func runFakeVersion(mode string) int {
	if counterPath := os.Getenv(fakeVersionCounterEnv); counterPath != "" {
		count := 0
		if raw, err := os.ReadFile(counterPath); err == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				count = parsed
			}
		}
		_ = os.WriteFile(counterPath, []byte(strconv.Itoa(count+1)), 0o600)
	}
	switch mode {
	case "exit":
		fmt.Fprintln(os.Stderr, "fake runtime failure")
		return 2
	case "empty":
		return 0
	case "hang":
		// Block long enough that the probe's timeout fires and the process
		// group is killed; the probe test shrinks probeTimeout.
		time.Sleep(30 * time.Second)
		return 0
	case "banner":
		fmt.Println("opencode — the everything agent")
		return 0
	default: // "print" and any unknown mode
		fmt.Print(fakeVersionDefaultOut)
		return 0
	}
}

// fakeVersionAdapter wires the adapter to the test binary as its runtime and
// points the fake's counter at a fresh temp file. Returns the adapter and the
// counter path.
func fakeVersionAdapter(t *testing.T, mode string) (*OpencodeAdapter, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	counterPath := filepath.Join(t.TempDir(), "probe-count")
	t.Setenv(fakeVersionEnv, mode)
	t.Setenv(fakeVersionCounterEnv, counterPath)
	return NewOpencodeAdapter(self), counterPath
}

// probeCount reads the fake's execution counter.
func probeCount(t *testing.T, counterPath string) int {
	t.Helper()
	raw, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read probe counter: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse probe counter %q: %v", raw, err)
	}
	return count
}

// TestProbe_RecordsTheRuntimeVersion: a fake printing 1.18.11 yields
// Ready=true with that version and the adapter's id.
func TestProbe_RecordsTheRuntimeVersion(t *testing.T) {
	adapter, _ := fakeVersionAdapter(t, "print")
	readiness := adapter.Probe(context.Background())
	if !readiness.Ready {
		t.Fatalf("readiness = %+v; want Ready", readiness)
	}
	if readiness.RuntimeVersion != "1.18.11" {
		t.Errorf("RuntimeVersion = %q; want 1.18.11", readiness.RuntimeVersion)
	}
	if readiness.AdapterID != AdapterOpencode {
		t.Errorf("AdapterID = %q; want %q", readiness.AdapterID, AdapterOpencode)
	}
	if readiness.Reason != "" {
		t.Errorf("Reason = %q; want empty when ready", readiness.Reason)
	}
	if readiness.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}
	if label := readiness.Label(); label != "ok" {
		t.Errorf("Label() = %q; want ok", label)
	}
}

// TestProbe_MemoizedPerProcess: a second call (and ten more) spawns no second
// process — the counter in the fake must stay at 1. Run with -race for the
// concurrency half: concurrent Probe calls race only if the memoization is
// wrong.
func TestProbe_MemoizedPerProcess(t *testing.T) {
	adapter, counterPath := fakeVersionAdapter(t, "print")

	first := adapter.Probe(context.Background())
	if !first.Ready {
		t.Fatalf("first probe not ready: %+v", first)
	}

	var wg sync.WaitGroup
	for callIndex := 0; callIndex < 10; callIndex++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if again := adapter.Probe(context.Background()); again.RuntimeVersion != first.RuntimeVersion {
				t.Errorf("memoized probe returned %q; want %q", again.RuntimeVersion, first.RuntimeVersion)
			}
		}()
	}
	wg.Wait()

	if count := probeCount(t, counterPath); count != 1 {
		t.Errorf("fake executed %d times; want exactly 1 (memoized per process)", count)
	}
}

// TestProbe_SurvivesACancelledCaller: the memoized answer is process-scoped,
// so it must not be decided by the lifetime of whichever caller reached it
// first. Without the detached context, cancelling the task that triggered the
// first probe would pin "runtime not ready" for every later run in the
// process, with an empty runtime_version in all of their evidence, until a
// restart.
func TestProbe_SurvivesACancelledCaller(t *testing.T) {
	adapter, counterPath := fakeVersionAdapter(t, "print")

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	readiness := adapter.Probe(cancelledCtx)
	if !readiness.Ready {
		t.Fatalf("readiness = %+v; want Ready despite the caller's cancelled context", readiness)
	}
	if readiness.RuntimeVersion != "1.18.11" {
		t.Errorf("RuntimeVersion = %q; want 1.18.11", readiness.RuntimeVersion)
	}
	// And the poisoned answer is not what later callers see either.
	if again := adapter.Probe(context.Background()); !again.Ready {
		t.Errorf("later probe = %+v; want the memoized ready result", again)
	}
	if count := probeCount(t, counterPath); count != 1 {
		t.Errorf("fake executed %d times; want exactly 1", count)
	}
}

// TestProbe_MissingBinaryIsAProbeResultNotAnError: an absent binary yields
// Ready=false with a reason, and Probe never returns an error by contract.
func TestProbe_MissingBinaryIsAProbeResultNotAnError(t *testing.T) {
	adapter := NewOpencodeAdapter(filepath.Join(t.TempDir(), "no-such-runtime"))
	readiness := adapter.Probe(context.Background())
	if readiness.Ready {
		t.Fatalf("readiness = %+v; want not ready for a missing binary", readiness)
	}
	if readiness.RuntimeVersion != "" {
		t.Errorf("RuntimeVersion = %q; want empty on failure", readiness.RuntimeVersion)
	}
	if !strings.Contains(readiness.Reason, "binary not found") {
		t.Errorf("Reason = %q; want it to name the missing binary", readiness.Reason)
	}
	if label := readiness.Label(); !strings.HasPrefix(label, "failed:") {
		t.Errorf("Label() = %q; want the failed: prefix", label)
	}
}

// TestProbe_NonZeroExitHasADistinctReason.
func TestProbe_NonZeroExitHasADistinctReason(t *testing.T) {
	adapter, _ := fakeVersionAdapter(t, "exit")
	readiness := adapter.Probe(context.Background())
	if readiness.Ready {
		t.Fatalf("readiness = %+v; want not ready", readiness)
	}
	if !strings.Contains(readiness.Reason, "exit") {
		t.Errorf("Reason = %q; want the exit reason", readiness.Reason)
	}
}

// TestProbe_EmptyOutputHasADistinctReason.
func TestProbe_EmptyOutputHasADistinctReason(t *testing.T) {
	adapter, _ := fakeVersionAdapter(t, "empty")
	readiness := adapter.Probe(context.Background())
	if readiness.Ready {
		t.Fatalf("readiness = %+v; want not ready", readiness)
	}
	if !strings.Contains(readiness.Reason, "empty output") {
		t.Errorf("Reason = %q; want the empty-output reason", readiness.Reason)
	}
}

// TestProbe_UnparseableOutputHasADistinctReason: a banner line that is not a
// version must not be recorded as the runtime version.
func TestProbe_UnparseableOutputHasADistinctReason(t *testing.T) {
	adapter, _ := fakeVersionAdapter(t, "banner")
	readiness := adapter.Probe(context.Background())
	if readiness.Ready {
		t.Fatalf("readiness = %+v; want not ready", readiness)
	}
	if !strings.Contains(readiness.Reason, "unparseable") {
		t.Errorf("Reason = %q; want the unparseable reason", readiness.Reason)
	}
}

// TestProbe_HangIsKilledAtTimeout: a runtime that produces nothing and never
// exits is killed at the probe timeout, yields a distinct reason, and leaves
// no child behind (the cancellation watcher retires only after the process is
// reaped — the same machinery the run path uses).
func TestProbe_HangIsKilledAtTimeout(t *testing.T) {
	// Cannot use t.Parallel: it shrinks probeTimeout and killGrace, which are
	// package-global.
	shrinkProbeTimeout(t)
	shrinkKillGrace(t)
	adapter, _ := fakeVersionAdapter(t, "hang")

	started := time.Now()
	readiness := adapter.Probe(context.Background())
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("probe took %s; the timeout did not terminate the process tree", elapsed)
	}
	if readiness.Ready {
		t.Fatalf("readiness = %+v; want not ready", readiness)
	}
	if readiness.Reason != "timeout" {
		t.Errorf("Reason = %q; want timeout", readiness.Reason)
	}
}

// TestParseVersionOutput pins the parsing rules: trim whitespace, take the
// FIRST line, and require a version-shaped token.
func TestParseVersionOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		output string
		want   string
		errMsg string
	}{
		{"1.18.11\n", "1.18.11", ""},
		{"  1.18.11  \n", "1.18.11", ""},
		{"1.18.11\nextra line\n", "1.18.11", ""},
		{"v2.0.0\n", "v2.0.0", ""},
		{"", "", "empty output"},
		{"   \n", "", "empty output"},
		{"opencode — everything agent\n", "", "unparseable"},
	}
	for _, testCase := range cases {
		got, err := parseVersionOutput(testCase.output)
		if testCase.errMsg == "" {
			if err != nil {
				t.Errorf("parseVersionOutput(%q): %v", testCase.output, err)
				continue
			}
			if got != testCase.want {
				t.Errorf("parseVersionOutput(%q) = %q; want %q", testCase.output, got, testCase.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseVersionOutput(%q) = %q; want error %q", testCase.output, got, testCase.errMsg)
		} else if !strings.Contains(err.Error(), testCase.errMsg) {
			t.Errorf("parseVersionOutput(%q) error = %v; want it to contain %q", testCase.output, err, testCase.errMsg)
		}
	}
}
