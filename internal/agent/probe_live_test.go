//go:build integration

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProbe_LiveRuntimeVersionMatchesCLI: the probed runtime version equals
// what `opencode --version` prints on this machine.
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

// TestProbe_RuntimeBinaryOverrideSwapsTheVersion: pointing
// AGENTUM_RUNTIME_BINARY's equivalent (the adapter's binary argument) at a
// wrapper reporting a different version changes the recorded version — the
// axis the cross-run diff reads.
func TestProbe_RuntimeBinaryOverrideSwapsTheVersion(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}
	wrapper := writeVersionWrapper(t, "9.9.99")
	readiness := NewOpencodeAdapter(wrapper).Probe(context.Background())
	if !readiness.Ready {
		t.Fatalf("probe not ready through the wrapper: %+v", readiness)
	}
	if readiness.RuntimeVersion != "9.9.99" {
		t.Errorf("probed version = %q, want the wrapper's 9.9.99", readiness.RuntimeVersion)
	}
}

// writeVersionWrapper writes a stand-in runtime that prints one version and
// exits 0 — the shim shape an operator uses with AGENTUM_RUNTIME_BINARY. The
// script is written for the host OS: a .bat on Windows, a shebang script
// elsewhere. A .bat on Linux is mode 0755 with no shebang, so exec returns
// ENOEXEC and the test fails on the one platform (`-tags integration` in CI)
// where it most needs to run.
func writeVersionWrapper(t *testing.T, version string) string {
	t.Helper()
	name, script := "fake-opencode.sh", "#!/bin/sh\necho "+version+"\n"
	if runtime.GOOS == "windows" {
		name, script = "fake-opencode.bat", "@echo "+version+"\r\n"
	}
	wrapper := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return wrapper
}
