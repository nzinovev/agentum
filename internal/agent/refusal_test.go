package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// runCount reads the run-invocation counter. A missing file is zero: the
// counter only exists once the fake agent has run at least once.
func runCount(t *testing.T, counterPath string) int {
	t.Helper()
	raw, err := os.ReadFile(counterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read run counter: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse run counter %q: %v", raw, err)
	}
	return count
}

// TestInvoke_UnknownModelRefusesWithoutSubprocess: the model string reaches
// the runtime verbatim, so Invoke is the last point where a model the catalog
// does not contain can be caught — and the refusal must cost no subprocess.
// The fake binary serves both the catalog probe (argv "models") and run
// invocations; only the latter bump the run counter.
func TestInvoke_UnknownModelRefusesWithoutSubprocess(t *testing.T) {
	adapter, invocation := fakeInvocation(t, fakeWorks, 50*time.Millisecond, caps.Profile{})
	counterPath := filepath.Join(t.TempDir(), "run-count")
	t.Setenv(fakeCatalogEnv, "ok")
	t.Setenv(fakeRunCounterEnv, counterPath)
	// The fixture's catalog knows the three fakeCatalogOutput models; this
	// selection names none of them.
	invocation.Model = models.Selection{
		Tier: "fast", Provider: "zai-coding-plan",
		Options: models.Options{Model: "zai-coding-plan/glm-5.4"},
	}

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr == nil {
		drain(t, events)
		t.Fatal("Invoke accepted a model the runtime does not list")
	}
	if !strings.Contains(invokeErr.Error(), "unknown model") {
		t.Errorf("error = %v; want the unknown-model refusal", invokeErr)
	}
	if !strings.Contains(invokeErr.Error(), "list them with:") {
		t.Errorf("error = %v; want the refusal to name the listing command", invokeErr)
	}
	if count := runCount(t, counterPath); count != 0 {
		t.Errorf("runtime invoked %d times; the refusal must spawn no subprocess", count)
	}
}

// TestInvoke_CatalogListModelArrivesFromTheAdapter: the refusal's listing
// command names the adapter's own binary (the configured override included),
// not a hardcoded executor name.
func TestInvoke_CatalogListModelArrivesFromTheAdapter(t *testing.T) {
	adapter := NewOpencodeAdapter("pinned-runtime")
	catalog := adapter.Catalog(context.Background())
	if !strings.Contains(catalog.Source, "pinned-runtime models") {
		t.Errorf("Source = %q; want the configured binary name plus the subcommand", catalog.Source)
	}
}

// TestInvoke_IdleStopNamesTheModel: the idle-cap terminal error carries the
// model string. A silence has very different fixes depending on whether the
// model is misbehaving or the agent is legitimately working, and the stop
// reason vocabulary deliberately stays generic — the model is the one fact
// this error can add.
func TestInvoke_IdleStopNamesTheModel(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeSilent, 10*time.Second,
		caps.Profile{IdleTimeout: 300 * time.Millisecond})
	// A model the fixture catalog lists, so the run starts.
	invocation.Model = models.Selection{
		Tier: "strong", Provider: "zai-coding-plan",
		Options: models.Options{Model: "zai-coding-plan/glm-5.3"},
	}

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	result, failed := drain(t, events)
	if result != nil || failed == nil {
		t.Fatalf("result=%v failed=%v; want the idle cap to stop the run", result, failed)
	}
	if !strings.Contains(failed.Error(), "idle cap") {
		t.Errorf("error = %q; want it to name the idle cap", failed)
	}
	if !strings.Contains(failed.Error(), `"zai-coding-plan/glm-5.3"`) {
		t.Errorf("error = %q; want it to name the model", failed)
	}
}
