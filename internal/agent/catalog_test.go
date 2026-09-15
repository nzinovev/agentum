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

	"github.com/nzinovev/agentum/internal/models"
)

// fakeCatalogEnv switches the test binary into the `models --verbose` fake.
// Its value selects the behaviour; the counter file (fakeCatalogCounterEnv)
// records how many times the fake executed, which is how the memoization test
// proves the second Catalog spawned nothing.
const (
	fakeCatalogEnv        = "AGENTUM_FAKE_CATALOG"
	fakeCatalogCounterEnv = "AGENTUM_FAKE_CATALOG_COUNTER"
)

// fakeCatalogOutput is the reference listing: three records covering the three
// states of the variants field (declared, declared-empty, absent) plus the
// unknown fields a real listing carries and a decoder must ignore.
const fakeCatalogOutput = `opencode/muse-spark-1.3-contributor-free
{
  "id": "muse-spark-1.3-contributor-free",
  "providerID": "opencode",
  "status": "active",
  "limit": {"context": 160000, "output": 32000},
  "capabilities": {"reasoning": true, "tools": true},
  "variants": {
    "high": {"reasoningEffort": "high"},
    "max": {"reasoningEffort": "max"}
  }
}
zai-coding-plan/glm-5.3
{
  "id": "glm-5.3",
  "providerID": "zai-coding-plan",
  "status": "active",
  "variants": {}
}
zai/glm-4.7
{
  "id": "glm-4.7",
  "providerID": "zai",
  "status": "active"
}
`

// runFakeModels plays the runtime's `models --verbose` side of the catalog
// probe contract. The mode is selected through the environment because the
// probe controls the argv, not the tests.
func runFakeModels(mode string) int {
	if counterPath := os.Getenv(fakeCatalogCounterEnv); counterPath != "" {
		count := 0
		if raw, err := os.ReadFile(counterPath); err == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				count = parsed
			}
		}
		_ = os.WriteFile(counterPath, []byte(strconv.Itoa(count+1)), 0o600)
	}
	switch mode {
	case "unreadable":
		fmt.Print(fakeCatalogOutput)
		fmt.Println("zai-coding-plan/broken")
		fmt.Println("{")
		fmt.Println(`  "id": "broken",`)
		fmt.Println(`  "variants": {`) // JSON ends mid-object: the record must not decode
		return 0
	case "noid":
		fmt.Println("zai-coding-plan/anonymous")
		fmt.Println("{")
		fmt.Println(`  "id": "anonymous",`)
		fmt.Println(`  "status": "active"`) // providerID absent
		fmt.Println("}")
		return 0
	case "empty":
		return 0
	case "headers":
		fmt.Println("zai-coding-plan/glm-5.3")
		fmt.Println("zai/glm-4.7")
		return 0
	case "exit":
		fmt.Fprintln(os.Stderr, "fake runtime failure")
		return 2
	case "hang":
		// Block long enough that the probe's timeout fires and the process
		// group is killed; the catalog test shrinks probeTimeout.
		time.Sleep(30 * time.Second)
		return 0
	default: // "ok" and any unknown mode
		fmt.Print(fakeCatalogOutput)
		return 0
	}
}

// fakeCatalogAdapter wires the adapter to the test binary as its runtime and
// points the fake's counter at a fresh temp file. Returns the adapter and the
// counter path.
func fakeCatalogAdapter(t *testing.T, mode string) (*OpencodeAdapter, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	counterPath := filepath.Join(t.TempDir(), "catalog-count")
	t.Setenv(fakeCatalogEnv, mode)
	t.Setenv(fakeCatalogCounterEnv, counterPath)
	return NewOpencodeAdapter(self), counterPath
}

// catalogCount reads the fake's execution counter.
func catalogCount(t *testing.T, counterPath string) int {
	t.Helper()
	raw, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read catalog counter: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse catalog counter %q: %v", raw, err)
	}
	return count
}

// TestCatalog_ReadsNamesVariantsAndTheUnknownFields: the reference listing
// yields an available catalog whose three records carry the three states of
// the variants field — declared (keys sorted), declared-empty, and absent
// (VariantsKnown=false, the vocabulary may not be concluded either way).
func TestCatalog_ReadsNamesVariantsAndTheUnknownFields(t *testing.T) {
	adapter, _ := fakeCatalogAdapter(t, "ok")
	catalog := adapter.Catalog(context.Background())

	if !catalog.Available {
		t.Fatalf("catalog not available: %q", catalog.Reason)
	}
	if len(catalog.Models) != 3 {
		t.Fatalf("models = %v; want the 3 records of the fixture", catalog.Models)
	}
	byID := make(map[string]models.CatalogModel, len(catalog.Models))
	for _, entry := range catalog.Models {
		byID[entry.ID] = entry
	}
	muse, found := byID["opencode/muse-spark-1.3-contributor-free"]
	if !found {
		t.Fatalf("muse model missing: %v", catalog.Models)
	}
	if !muse.VariantsKnown || strings.Join(muse.Variants, ",") != "high,max" {
		t.Errorf("muse variants = %v (known=%v); want sorted [high max]", muse.Variants, muse.VariantsKnown)
	}
	glm, found := byID["zai-coding-plan/glm-5.3"]
	if !found {
		t.Fatalf("glm model missing: %v", catalog.Models)
	}
	if !glm.VariantsKnown || len(glm.Variants) != 0 {
		t.Errorf("empty-variants model: known=%v variants=%v; want known and empty", glm.VariantsKnown, glm.Variants)
	}
	plain, found := byID["zai/glm-4.7"]
	if !found {
		t.Fatalf("variants-less model missing: %v", catalog.Models)
	}
	if plain.VariantsKnown || len(plain.Variants) != 0 {
		t.Errorf("absent-variants model: known=%v variants=%v; want unknown and empty", plain.VariantsKnown, plain.Variants)
	}
	if catalog.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}
	if !strings.Contains(catalog.Source, "models") {
		t.Errorf("Source = %q; want it to name the listing command", catalog.Source)
	}
	if label := catalog.Label(); label != "ok (3 models)" {
		t.Errorf("Label() = %q; want ok (3 models)", label)
	}
}

// TestCatalog_UnreadableRecordInvalidatesTheWholeCatalog is the rule a
// half-read catalog would break: the test must assert the CATALOG is
// unavailable, not that one model went missing — fifteen read records out of
// thirty would refuse the other fifteen as non-existent, which is the lie an
// unavailable catalog exists to avoid.
func TestCatalog_UnreadableRecordInvalidatesTheWholeCatalog(t *testing.T) {
	adapter, _ := fakeCatalogAdapter(t, "unreadable")
	catalog := adapter.Catalog(context.Background())

	if catalog.Available {
		t.Fatal("a listing with an unreadable record must not be usable")
	}
	if catalog.Reason != "unreadable records: 1 of 4" {
		t.Errorf("Reason = %q; want unreadable records: 1 of 4", catalog.Reason)
	}
	if len(catalog.Models) != 0 {
		t.Errorf("Models = %v; a partially read catalog is never handed out", catalog.Models)
	}
	// And the safe mode validates as nil, like every unavailable catalog.
	if err := catalog.Validate(models.Selection{Options: models.Options{Model: "opencode/muse-spark-1.3-contributor-free"}}); err != nil {
		t.Errorf("Validate on the discarded catalog = %v; want nil", err)
	}
}

// TestCatalog_RecordWithoutIdentityIsUnreadable: a record missing id or
// providerID has no identity to file under — same wholesale invalidation as a
// broken JSON body.
func TestCatalog_RecordWithoutIdentityIsUnreadable(t *testing.T) {
	adapter, _ := fakeCatalogAdapter(t, "noid")
	catalog := adapter.Catalog(context.Background())
	if catalog.Available {
		t.Fatal("a record without providerID must invalidate the catalog")
	}
	if catalog.Reason != "unreadable records: 1 of 1" {
		t.Errorf("Reason = %q; want unreadable records: 1 of 1", catalog.Reason)
	}
}

// TestCatalog_OutputWithoutRecordsIsNotAnEmptyCatalog: an empty answer, or one
// with only header lines, means the listing could not be read — never that
// the runtime runs nothing.
func TestCatalog_OutputWithoutRecordsIsNotAnEmptyCatalog(t *testing.T) {
	t.Run("empty output", func(t *testing.T) {
		adapter, _ := fakeCatalogAdapter(t, "empty")
		catalog := adapter.Catalog(context.Background())
		if catalog.Available || catalog.Reason != "empty output" {
			t.Errorf("catalog = available:%v reason:%q; want unavailable with the empty-output reason", catalog.Available, catalog.Reason)
		}
	})
	t.Run("headers only", func(t *testing.T) {
		adapter, _ := fakeCatalogAdapter(t, "headers")
		catalog := adapter.Catalog(context.Background())
		if catalog.Available || catalog.Reason != "no records" {
			t.Errorf("catalog = available:%v reason:%q; want unavailable with the no-records reason", catalog.Available, catalog.Reason)
		}
	})
}

// TestCatalog_NonZeroExitHasADistinctReason.
func TestCatalog_NonZeroExitHasADistinctReason(t *testing.T) {
	adapter, _ := fakeCatalogAdapter(t, "exit")
	catalog := adapter.Catalog(context.Background())
	if catalog.Available {
		t.Fatal("a failing listing must not be usable")
	}
	if !strings.Contains(catalog.Reason, "exit") {
		t.Errorf("Reason = %q; want the exit reason", catalog.Reason)
	}
	if label := catalog.Label(); !strings.HasPrefix(label, "failed:") {
		t.Errorf("Label() = %q; want the failed: prefix", label)
	}
}

// TestCatalog_HangIsKilledAtTimeout: a listing that produces nothing and
// never exits is killed at the probe timeout, yields the timeout reason, and
// leaves no child behind.
func TestCatalog_HangIsKilledAtTimeout(t *testing.T) {
	// Cannot use t.Parallel: it shrinks probeTimeout and killGrace, which are
	// package-global.
	shrinkProbeTimeout(t)
	shrinkKillGrace(t)
	adapter, _ := fakeCatalogAdapter(t, "hang")

	started := time.Now()
	catalog := adapter.Catalog(context.Background())
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("probe took %s; the timeout did not terminate the process tree", elapsed)
	}
	if catalog.Available {
		t.Fatalf("catalog = %+v; want unavailable", catalog)
	}
	if catalog.Reason != "timeout" {
		t.Errorf("Reason = %q; want timeout", catalog.Reason)
	}
}

// TestCatalog_MemoizedPerProcess: a second call (and ten more, concurrent)
// spawns no second process — the counter in the fake must stay at 1. Run with
// -race for the concurrency half.
func TestCatalog_MemoizedPerProcess(t *testing.T) {
	adapter, counterPath := fakeCatalogAdapter(t, "ok")

	first := adapter.Catalog(context.Background())
	if !first.Available {
		t.Fatalf("first catalog not available: %q", first.Reason)
	}

	var wg sync.WaitGroup
	for callIndex := 0; callIndex < 10; callIndex++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if again := adapter.Catalog(context.Background()); len(again.Models) != len(first.Models) {
				t.Errorf("memoized catalog returned %d models; want %d", len(again.Models), len(first.Models))
			}
		}()
	}
	wg.Wait()

	if count := catalogCount(t, counterPath); count != 1 {
		t.Errorf("fake executed %d times; want exactly 1 (memoized per process)", count)
	}
}

// TestCatalog_SurvivesACancelledCaller: the memoized answer is process-scoped
// and must not be decided by the lifetime of whichever caller reached the
// probe first.
func TestCatalog_SurvivesACancelledCaller(t *testing.T) {
	adapter, counterPath := fakeCatalogAdapter(t, "ok")

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	catalog := adapter.Catalog(cancelledCtx)
	if !catalog.Available {
		t.Fatalf("catalog = %+v; want available despite the caller's cancelled context", catalog)
	}
	if again := adapter.Catalog(context.Background()); !again.Available {
		t.Errorf("later catalog = %+v; want the memoized available result", again)
	}
	if count := catalogCount(t, counterPath); count != 1 {
		t.Errorf("fake executed %d times; want exactly 1", count)
	}
}

// TestCatalog_MissingBinaryIsAProbeResultNotAnError: an absent binary yields
// an unavailable catalog with a reason, and Catalog never returns an error by
// contract.
func TestCatalog_MissingBinaryIsAProbeResultNotAnError(t *testing.T) {
	adapter := NewOpencodeAdapter(filepath.Join(t.TempDir(), "no-such-runtime"))
	catalog := adapter.Catalog(context.Background())
	if catalog.Available {
		t.Fatal("a missing binary must not yield a usable catalog")
	}
	if catalog.Reason != "binary not found" {
		t.Errorf("Reason = %q; want binary not found", catalog.Reason)
	}
}

// TestParseCatalogOutput pins the pure reader: framing lines are skipped, an
// unterminated object at EOF still counts as its (failing) record, and the
// reason carries the unreadable-of-total counts.
func TestParseCatalogOutput(t *testing.T) {
	t.Parallel()
	t.Run("records are read and sorted", func(t *testing.T) {
		parsed := parseCatalogOutput("b/z\n{\n  \"id\": \"z\",\n  \"providerID\": \"b\"\n}\na/y\n{\n  \"id\": \"y\",\n  \"providerID\": \"a\"\n}\n")
		if parsed.problem != "" {
			t.Fatalf("problem = %q; want none", parsed.problem)
		}
		if len(parsed.models) != 2 || parsed.models[0].ID != "a/y" || parsed.models[1].ID != "b/z" {
			t.Errorf("models = %+v; want sorted [a/y b/z]", parsed.models)
		}
	})
	t.Run("unterminated object at EOF is one unreadable record", func(t *testing.T) {
		parsed := parseCatalogOutput("{\n  \"id\": \"z\",\n  \"providerID\": \"b\"\n")
		if parsed.problem != "unreadable records: 1 of 1" {
			t.Errorf("problem = %q; want unreadable records: 1 of 1", parsed.problem)
		}
	})
	t.Run("blank output is empty output", func(t *testing.T) {
		if parsed := parseCatalogOutput("  \n\n"); parsed.problem != "empty output" {
			t.Errorf("problem = %q; want empty output", parsed.problem)
		}
	})
}
