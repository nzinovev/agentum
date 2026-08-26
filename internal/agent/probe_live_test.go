//go:build integration

package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProbe_LiveRuntimeVersionMatchesCLI (ADR 0005 step 10.1, adapter half):
// the probed runtime version equals what `opencode --version` prints on this
// machine.
func TestProbe_LiveRuntimeVersionMatchesCLI(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}
	readiness := NewOpencodeAdapter("opencode").Probe(context.Background())
	if !readiness.Ready {
		t.Fatalf("probe not ready: %+v", readiness)
	}
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		t.Fatalf("run opencode --version: %v", err)
	}
	want := strings.TrimSpace(string(out))
	if readiness.RuntimeVersion != want {
		t.Errorf("probed version = %q, CLI prints %q", readiness.RuntimeVersion, want)
	}
	t.Logf("probed runtime version: %s", readiness.RuntimeVersion)
}

// TestProbe_RuntimeBinaryOverrideSwapsTheVersion (ADR 0005 step 10.3, adapter
// half): pointing AGENTUM_RUNTIME_BINARY's equivalent (the adapter's binary
// argument) at a wrapper reporting a different version changes the recorded
// version — the axis the cross-run diff reads.
func TestProbe_RuntimeBinaryOverrideSwapsTheVersion(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}
	wrapperDir := t.TempDir()
	wrapper := wrapperDir + "/fake-opencode.bat"
	script := "@echo 9.9.99\r\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	readiness := NewOpencodeAdapter(wrapper).Probe(context.Background())
	if !readiness.Ready {
		t.Fatalf("probe not ready through the wrapper: %+v", readiness)
	}
	if readiness.RuntimeVersion != "9.9.99" {
		t.Errorf("probed version = %q, want the wrapper's 9.9.99", readiness.RuntimeVersion)
	}
}
