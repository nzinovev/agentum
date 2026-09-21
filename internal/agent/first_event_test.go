package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// firstEventInvocation wires a fake invocation whose model the fixture
// catalog lists (so the catalog check passes) and arms the adapter's
// first-event watchdog.
func firstEventInvocation(t *testing.T, mode string, delay time.Duration, firstEventTimeout time.Duration) (*OpencodeAdapter, Invocation) {
	t.Helper()
	adapter, invocation := fakeInvocation(t, mode, delay, caps.Profile{})
	t.Setenv(fakeCatalogEnv, "ok")
	adapter.firstEventTimeout = firstEventTimeout
	invocation.Model = models.Selection{
		Tier: "strong", Provider: "zai-coding-plan",
		Options: models.Options{Model: "zai-coding-plan/glm-5.3"},
	}
	return adapter, invocation
}

// TestInvoke_FirstEventTimeoutStopsASilentStart: an invocation that emits
// nothing at all is stopped at the first-event bound, and the terminal error
// names the model — "the runtime never produced anything" is a fact about that
// model on this configuration, and it is the fact the operator needs.
func TestInvoke_FirstEventTimeoutStopsASilentStart(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := firstEventInvocation(t, fakeMute, 10*time.Second, 300*time.Millisecond)

	started := time.Now()
	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	result, failed := drain(t, events)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("run took %s; the bound did not terminate the process tree", elapsed)
	}
	if result != nil || failed == nil {
		t.Fatalf("result=%v failed=%v; want the watchdog to stop the run", result, failed)
	}
	message := failed.Error()
	if !strings.Contains(message, "before the first stream event") {
		t.Errorf("error = %q; want it to name the first-event bound", message)
	}
	if !strings.Contains(message, `"zai-coding-plan/glm-5.3"`) {
		t.Errorf("error = %q; want it to name the model", message)
	}
	if strings.Contains(message, "idle cap") {
		t.Errorf("error = %q; the first-event stop must not read as an idle-cap stop", message)
	}
}

// TestInvoke_FirstEventWatchdogRetiresOnTheFirstLine: a run that emits one
// line and then stays silent LONGER than the first-event bound must NOT be
// stopped by it — mid-work silence is the idle cap's question, and this run
// has no idle cap configured. The two bounds stay separate concepts or the
// watchdog degenerates into a second, harsher idle cap that terminates
// legitimate work.
func TestInvoke_FirstEventWatchdogRetiresOnTheFirstLine(t *testing.T) {
	adapter, invocation := firstEventInvocation(t, fakeSilent, 700*time.Millisecond, 300*time.Millisecond)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	result, failed := drain(t, events)
	if failed != nil {
		t.Fatalf("run failed: %v; the watchdog must not fire after the first line", failed)
	}
	if result == nil {
		t.Fatal("no result; the run should have completed")
	}
}

// TestInvoke_ZeroFirstEventTimeoutDisablesTheWatchdog: with the bound unset,
// a run whose first line arrives late (past what an armed watchdog would have
// allowed) completes on its own — a zero bound is an off switch, not a
// default.
func TestInvoke_ZeroFirstEventTimeoutDisablesTheWatchdog(t *testing.T) {
	adapter, invocation := firstEventInvocation(t, fakeQuiet, 700*time.Millisecond, 0)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	result, failed := drain(t, events)
	if failed != nil {
		t.Fatalf("run failed: %v; a zero bound must disable the watchdog", failed)
	}
	if result == nil {
		t.Fatal("no result; the run should have completed")
	}
}
