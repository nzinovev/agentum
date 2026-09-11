package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/models"
)

// modelTestAdapter wires the adapter to the test binary as its runtime, with
// the catalog fixture active and the run counter pointed at a fresh temp
// file. Returns the adapter and the run counter path.
func modelTestAdapter(t *testing.T, mode string) (*OpencodeAdapter, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	counterPath := filepath.Join(t.TempDir(), "model-check-count")
	t.Setenv(fakeCatalogEnv, "ok")
	t.Setenv(fakeRunCounterEnv, counterPath)
	t.Setenv(fakeModeEnv, mode)
	// The model check never waits out a fake's full delay; the artifact dir
	// the run fake demands is still named so a run that somehow completes
	// does not fail on it.
	t.Setenv(fakeArtifactEnv, filepath.Join(t.TempDir(), "artifacts"))
	return NewOpencodeAdapter(self), counterPath
}

// modelTestSelection names a model the fixture catalog lists.
func modelTestSelection() models.Selection {
	return models.Selection{
		Tier: "strong", Provider: "zai-coding-plan",
		Options: models.Options{Model: "zai-coding-plan/glm-5.3"},
	}
}

// TestTestModel_RespondingModelIsOkOnFirstLine: a runtime that emits a line
// immediately is ok, the latency to that line is recorded, and the group is
// stopped instead of being left to finish its answer.
func TestTestModel_RespondingModelIsOkOnFirstLine(t *testing.T) {
	adapter, _ := modelTestAdapter(t, fakeWorks)

	started := time.Now()
	check := adapter.TestModel(context.Background(), modelTestSelection(), 10*time.Second)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("check took %s; success should stop the group, not await the answer", elapsed)
	}
	if check.Outcome != ModelCheckOK {
		t.Fatalf("Outcome = %q (%s); want ok", check.Outcome, check.Reason)
	}
	if check.Latency <= 0 || check.Latency > 5*time.Second {
		t.Errorf("Latency = %s; want the time to the first line", check.Latency)
	}
	if check.Tier != "strong" || check.Model != "zai-coding-plan/glm-5.3" {
		t.Errorf("check = %+v; want the selection echoed", check)
	}
	if check.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}
}

// TestTestModel_SilentModelTimesOutAndIsKilled: a runtime producing nothing
// is a timeout at the deadline, and the process group does not outlive the
// check — the fake sleeps far past the deadline, so returning promptly is the
// proof it was terminated and reaped rather than orphaned.
func TestTestModel_SilentModelTimesOutAndIsKilled(t *testing.T) {
	shrinkKillGrace(t)
	adapter, counterPath := modelTestAdapter(t, fakeMute)

	started := time.Now()
	check := adapter.TestModel(context.Background(), modelTestSelection(), 500*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("check took %s; the deadline did not terminate the process tree", elapsed)
	}
	if check.Outcome != ModelCheckTimeout {
		t.Fatalf("Outcome = %q (%s); want timeout", check.Outcome, check.Reason)
	}
	if check.Latency != 0 {
		t.Errorf("Latency = %s; want zero when there was no line", check.Latency)
	}
	if !strings.Contains(check.Reason, "no output within") {
		t.Errorf("Reason = %q; want it to name the silence", check.Reason)
	}
	if count := runCount(t, counterPath); count != 1 {
		t.Errorf("runtime invoked %d times; want 1", count)
	}
}

// TestTestModel_FailingRuntimeIsAnError: a runtime exiting non-zero before
// any output is the error outcome, not a timeout and not ok.
func TestTestModel_FailingRuntimeIsAnError(t *testing.T) {
	adapter, _ := modelTestAdapter(t, fakeDie)

	check := adapter.TestModel(context.Background(), modelTestSelection(), 10*time.Second)
	if check.Outcome != ModelCheckError {
		t.Fatalf("Outcome = %q; want error", check.Outcome)
	}
	if !strings.Contains(check.Reason, "exit") {
		t.Errorf("Reason = %q; want the exit reason", check.Reason)
	}
}

// TestTestModel_UnknownModelSkipsTheInvocation: a model the catalog does not
// list is answered from the catalog — no subprocess, no bill.
func TestTestModel_UnknownModelSkipsTheInvocation(t *testing.T) {
	adapter, counterPath := modelTestAdapter(t, fakeWorks)
	selection := models.Selection{
		Tier: "fast", Provider: "zai-coding-plan",
		Options: models.Options{Model: "zai-coding-plan/glm-5.4"},
	}

	check := adapter.TestModel(context.Background(), selection, 10*time.Second)
	if check.Outcome != ModelCheckUnknownModel {
		t.Fatalf("Outcome = %q; want unknown_model", check.Outcome)
	}
	if !strings.Contains(check.Reason, "unknown model") {
		t.Errorf("Reason = %q; want the catalog refusal", check.Reason)
	}
	if count := runCount(t, counterPath); count != 0 {
		t.Errorf("runtime invoked %d times; the catalog must answer without a call", count)
	}
}

// TestTestModel_NotMemoized: two checks are two runtime invocations — the
// point of the check is "does it answer NOW", and a memoized answer would
// make "checked again after a fix" impossible. The catalog probe itself stays
// memoized at one.
func TestTestModel_NotMemoized(t *testing.T) {
	adapter, counterPath := modelTestAdapter(t, fakeWorks)

	first := adapter.TestModel(context.Background(), modelTestSelection(), 10*time.Second)
	second := adapter.TestModel(context.Background(), modelTestSelection(), 10*time.Second)
	if first.Outcome != ModelCheckOK || second.Outcome != ModelCheckOK {
		t.Fatalf("outcomes = %q, %q; want ok, ok", first.Outcome, second.Outcome)
	}
	if count := runCount(t, counterPath); count != 2 {
		t.Errorf("runtime invoked %d times; want 2 (one honest call per check)", count)
	}
}

// TestTestModel_MissingBinaryIsAnError: no runtime, no catalog, error — never
// a timeout pretending the model hung.
func TestTestModel_MissingBinaryIsAnError(t *testing.T) {
	adapter := NewOpencodeAdapter(filepath.Join(t.TempDir(), "no-such-runtime"))
	check := adapter.TestModel(context.Background(), modelTestSelection(), 10*time.Second)
	if check.Outcome != ModelCheckError {
		t.Fatalf("Outcome = %q; want error", check.Outcome)
	}
	if !strings.Contains(check.Reason, "not found") {
		t.Errorf("Reason = %q; want the missing-binary reason", check.Reason)
	}
}
