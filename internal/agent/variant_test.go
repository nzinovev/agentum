package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// The variant option's contract: the flags reach the process as one adjacent
// pair, every refusal happens before a subprocess exists, and the catalog
// decides the vocabulary question. The fixture catalog ("ok" mode) carries the
// three variants states: muse (declares high, max), glm-5.3 (declares none),
// glm-4.7 (vocabulary unknown).

// TestBuildOpencodeArgs_VariantFollowsModel: --variant is emitted only behind
// a --model, immediately after it, and never on its own — the flag qualifies
// the model, and a bare variant has nothing to qualify.
func TestBuildOpencodeArgs_VariantFollowsModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		options     models.Options
		wantModel   bool
		wantVariant bool
	}{
		{"model and variant", models.Options{Model: "p/m", Variant: "high"}, true, true},
		{"model without variant", models.Options{Model: "p/m"}, true, false},
		{"empty variant is no flag", models.Options{Model: "p/m", Variant: ""}, true, false},
		{"no model no variant", models.Options{}, false, false},
		{"variant without model is no flag pair", models.Options{Variant: "high"}, false, false},
	}
	for _, testCase := range cases {
		argv := buildOpencodeArgs("opencode", Invocation{Model: models.Selection{Options: testCase.options}})
		modelPos := positionOf(argv, "--model")
		variantPos := positionOf(argv, "--variant")
		if testCase.wantModel && modelPos < 0 {
			t.Errorf("%s: --model missing from %v", testCase.name, argv)
			continue
		}
		if !testCase.wantModel && modelPos >= 0 {
			t.Errorf("%s: --model emitted for %+v: %v", testCase.name, testCase.options, argv)
			continue
		}
		if !testCase.wantVariant {
			if variantPos >= 0 {
				t.Errorf("%s: --variant must not be emitted for %+v: %v", testCase.name, testCase.options, argv)
			}
			continue
		}
		if variantPos != modelPos+2 {
			t.Errorf("%s: --variant at %d; want --model position + 2 (%d) in %v",
				testCase.name, variantPos, modelPos+2, argv)
		}
		if argv[variantPos+1] != testCase.options.Variant {
			t.Errorf("%s: --variant value = %q; want %q", testCase.name, argv[variantPos+1], testCase.options.Variant)
		}
	}
}

// variantInvocation is a minimal well-formed invocation on the given options,
// wired to the fake runtime with the argv dump pointed at argvPath.
func variantInvocation(t *testing.T, options models.Options, argvPath string) (*OpencodeAdapter, Invocation) {
	t.Helper()
	self, executableErr := os.Executable()
	if executableErr != nil {
		t.Fatalf("locate test binary: %v", executableErr)
	}
	workdir := t.TempDir()
	artifactDir := filepath.Join(workdir, ".agentum", "artifacts", "spec")
	if mkdirErr := os.MkdirAll(artifactDir, 0o755); mkdirErr != nil {
		t.Fatalf("mkdir artifact dir: %v", mkdirErr)
	}
	t.Setenv(fakeModeEnv, fakeWorks)
	t.Setenv(fakeArtifactEnv, artifactDir)
	t.Setenv(fakeArgvDumpEnv, argvPath)
	return NewOpencodeAdapter(self), Invocation{
		Workdir:     workdir,
		ArtifactDir: artifactDir,
		Prompt:      "fake stage",
		Profile:     caps.Profile{},
		Model:       models.Selection{Tier: "strong", Options: options},
	}
}

// readDumpedArgv reads the argv the fake runtime recorded.
func readDumpedArgv(t *testing.T, argvPath string) []string {
	t.Helper()
	raw, readErr := os.ReadFile(argvPath)
	if readErr != nil {
		t.Fatalf("read dumped argv: %v", readErr)
	}
	var argv []string
	if unmarshalErr := json.Unmarshal(raw, &argv); unmarshalErr != nil {
		t.Fatalf("decode dumped argv %q: %v", raw, unmarshalErr)
	}
	return argv
}

// TestInvoke_VariantReachesTheSubprocessArgv: a tier with a variant runs the
// runtime with `--model M --variant V` exactly — asserted from the child's own
// record of its argv, not from what the caller passed.
func TestInvoke_VariantReachesTheSubprocessArgv(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv(fakeCatalogEnv, "ok")
	counterPath := filepath.Join(t.TempDir(), "run-count")
	t.Setenv(fakeRunCounterEnv, counterPath)
	adapter, invocation := variantInvocation(t,
		models.Options{Model: "opencode/muse-spark-1.3-contributor-free", Variant: "high"}, argvPath)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if _, failed := drain(t, events); failed != nil {
		t.Fatalf("run failed: %v", failed)
	}
	argv := readDumpedArgv(t, argvPath)
	modelPos := positionOf(argv, "--model")
	if modelPos < 0 || argv[modelPos+1] != "opencode/muse-spark-1.3-contributor-free" {
		t.Fatalf("argv carries no --model pair: %v", argv)
	}
	if variantPos := positionOf(argv, "--variant"); variantPos != modelPos+2 || argv[variantPos+1] != "high" {
		t.Errorf("argv = %v; want --variant high immediately after --model", argv)
	}
}

// TestInvoke_TierWithoutVariantKeepsTodayArgv: a selection without a variant
// produces exactly the argv shape that predates the option — no --variant, no
// empty flag.
func TestInvoke_TierWithoutVariantKeepsTodayArgv(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv(fakeCatalogEnv, "ok")
	adapter, invocation := variantInvocation(t,
		models.Options{Model: "opencode/muse-spark-1.3-contributor-free"}, argvPath)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if _, failed := drain(t, events); failed != nil {
		t.Fatalf("run failed: %v", failed)
	}
	argv := readDumpedArgv(t, argvPath)
	if positionOf(argv, "--model") < 0 {
		t.Errorf("argv carries no --model: %v", argv)
	}
	for _, arg := range argv {
		if strings.HasPrefix(arg, "--variant") {
			t.Errorf("argv carries %q for a variant-less selection: %v", arg, argv)
		}
	}
}

// TestInvoke_VariantWithoutModelRefusedBeforeAnything: Invoke refuses a
// selection whose variant has no model before the catalog is even probed and
// before any subprocess starts — the option is not silently dropped, and it
// is not silently kept either.
func TestInvoke_VariantWithoutModelRefusedBeforeAnything(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	counterPath := filepath.Join(t.TempDir(), "run-count")
	t.Setenv(fakeRunCounterEnv, counterPath)
	// A missing binary would be a second refusal reason; point at the real
	// fake so the only possible refusal is the options check.
	t.Setenv(fakeCatalogEnv, "ok")
	adapter, invocation := variantInvocation(t, models.Options{Variant: "high"}, argvPath)

	_, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr == nil {
		t.Fatal("a variant without a model must be refused at Invoke")
	}
	if !strings.Contains(invokeErr.Error(), `declares variant "high" but no model`) {
		t.Errorf("refusal must name the variant: %v", invokeErr)
	}
	if _, statErr := os.Stat(argvPath); !os.IsNotExist(statErr) {
		t.Error("a refused invocation must not start a subprocess (argv was dumped)")
	}
}

// TestInvoke_UndeclaredVariantValueRefusedByVocabulary: against a catalog that
// read the model's vocabulary, a variant outside it is refused at Invoke —
// the runtime would run it at a different effort without a word, so this side
// is the only refusal the typo ever gets.
func TestInvoke_UndeclaredVariantValueRefusedByVocabulary(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	counterPath := filepath.Join(t.TempDir(), "run-count")
	t.Setenv(fakeCatalogEnv, "ok")
	t.Setenv(fakeRunCounterEnv, counterPath)
	adapter, invocation := variantInvocation(t,
		models.Options{Model: "opencode/muse-spark-1.3-contributor-free", Variant: "medium"}, argvPath)

	_, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr == nil {
		t.Fatal("a variant outside the declared vocabulary must be refused at Invoke")
	}
	message := invokeErr.Error()
	for _, wanted := range []string{`declares no variant "medium"`, "declares: high, max", `"opencode"`} {
		if !strings.Contains(message, wanted) {
			t.Errorf("refusal %q does not contain %q", message, wanted)
		}
	}
	if _, statErr := os.Stat(argvPath); !os.IsNotExist(statErr) {
		t.Error("a refused invocation must not start a subprocess (argv was dumped)")
	}
	if count := runCount(t, counterPath); count != 0 {
		t.Errorf("runtime invoked %d times; want 0", count)
	}
}

// TestTestModel_VariantEchoedAndInTheArgv: the diagnostic check runs the same
// pair the tier declared — ModelCheck.Variant carries it, and the child's own
// argv records both flags adjacent.
func TestTestModel_VariantEchoedAndInTheArgv(t *testing.T) {
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	counterPath := filepath.Join(t.TempDir(), "model-check-count")
	t.Setenv(fakeCatalogEnv, "ok")
	t.Setenv(fakeRunCounterEnv, counterPath)
	t.Setenv(fakeModeEnv, fakeWorks)
	t.Setenv(fakeArtifactEnv, filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv(fakeArgvDumpEnv, argvPath)
	self, executableErr := os.Executable()
	if executableErr != nil {
		t.Fatalf("locate test binary: %v", executableErr)
	}
	adapter := NewOpencodeAdapter(self)

	check := adapter.TestModel(context.Background(), models.Selection{
		Tier: "strong",
		Options: models.Options{
			Model:   "opencode/muse-spark-1.3-contributor-free",
			Variant: "high",
		},
	}, 10*time.Second)
	if check.Outcome != ModelCheckOK {
		t.Fatalf("Outcome = %q (%s); want ok", check.Outcome, check.Reason)
	}
	if check.Variant != "high" {
		t.Errorf("ModelCheck.Variant = %q; want high — the pair checked is the pair reported", check.Variant)
	}
	argv := readDumpedArgv(t, argvPath)
	modelPos := positionOf(argv, "--model")
	if modelPos < 0 {
		t.Fatalf("check argv carries no --model: %v", argv)
	}
	if variantPos := positionOf(argv, "--variant"); variantPos != modelPos+2 || argv[variantPos+1] != "high" {
		t.Errorf("check argv = %v; want --variant high immediately after --model", argv)
	}
}

// TestTestModel_VariantOutsideVocabularyIsErrorNotUnknownModel: the wrong
// variant is the error outcome with the vocabulary in the reason — not
// unknown_model (the model exists) and not a run (the configuration can never
// use its answer).
func TestTestModel_VariantOutsideVocabularyIsErrorNotUnknownModel(t *testing.T) {
	adapter, counterPath := modelTestAdapter(t, fakeWorks)

	check := adapter.TestModel(context.Background(), models.Selection{
		Tier: "strong",
		Options: models.Options{
			Model:   "opencode/muse-spark-1.3-contributor-free",
			Variant: "medium",
		},
	}, 10*time.Second)
	if check.Outcome != ModelCheckError {
		t.Fatalf("Outcome = %q (%s); want error", check.Outcome, check.Reason)
	}
	for _, wanted := range []string{`declares no variant "medium"`, "declares: high, max", `"opencode"`} {
		if !strings.Contains(check.Reason, wanted) {
			t.Errorf("Reason %q does not contain %q", check.Reason, wanted)
		}
	}
	if count := runCount(t, counterPath); count != 0 {
		t.Errorf("runtime invoked %d times; the refusal must precede the run", count)
	}
}

// TestTestModel_VariantWithoutModelRefusedBeforeTheRun: the diagnostic path
// refuses an inconsistent selection exactly as Invoke does — before the
// catalog probe spawns anything and before the checked run starts.
func TestTestModel_VariantWithoutModelRefusedBeforeTheRun(t *testing.T) {
	adapter, counterPath := modelTestAdapter(t, fakeWorks)

	check := adapter.TestModel(context.Background(), models.Selection{
		Tier:    "strong",
		Options: models.Options{Variant: "high"},
	}, 10*time.Second)
	if check.Outcome != ModelCheckError {
		t.Fatalf("Outcome = %q (%s); want error", check.Outcome, check.Reason)
	}
	if !strings.Contains(check.Reason, `declares variant "high" but no model`) {
		t.Errorf("Reason = %q; want the consistency refusal naming the variant", check.Reason)
	}
	if count := runCount(t, counterPath); count != 0 {
		t.Errorf("runtime invoked %d times; want 0", count)
	}
}

// TestTestModel_UnknownVocabularySkipsTheVariantCheck: a model whose variants
// field the catalog could not read is still checked for existence and then
// runs with the requested variant — "cannot conclude" is not "declares
// nothing".
func TestTestModel_UnknownVocabularySkipsTheVariantCheck(t *testing.T) {
	adapter, counterPath := modelTestAdapter(t, fakeWorks)

	check := adapter.TestModel(context.Background(), models.Selection{
		Tier:    "reasoning",
		Options: models.Options{Model: "zai/glm-4.7", Variant: "high"},
	}, 10*time.Second)
	if check.Outcome != ModelCheckOK {
		t.Fatalf("Outcome = %q (%s); want ok — an unknown vocabulary skips the check", check.Outcome, check.Reason)
	}
	if check.Variant != "high" {
		t.Errorf("Variant = %q; want the checked pair echoed", check.Variant)
	}
	if count := runCount(t, counterPath); count != 1 {
		t.Errorf("runtime invoked %d times; want 1", count)
	}
}

// TestCatalog_UnknownVariantVocabularyWarnsOnce: a listing whose records came
// without a readable variants field produces ONE warning for the whole catalog
// — adapter, count, first model — and repeated catalog reads do not repeat it,
// because the warning sits inside the memoized probe.
func TestCatalog_UnknownVariantVocabularyWarnsOnce(t *testing.T) {
	// Not parallel: swaps the process-wide default slog logger.
	var records strings.Builder
	previous := slogDefault()
	t.Cleanup(func() { restoreSlogDefault(previous) })
	captureSlog(&records)

	adapter, _ := fakeCatalogAdapter(t, "ok")
	first := adapter.Catalog(context.Background())
	if !first.Available {
		t.Fatalf("catalog not available: %q", first.Reason)
	}
	warningsAfterFirst := strings.Count(records.String(), "variant vocabulary unavailable; variant validation skipped")
	if warningsAfterFirst != 1 {
		t.Fatalf("warnings after the first read = %d; want exactly 1", warningsAfterFirst)
	}
	if !strings.Contains(records.String(), `first_model=zai/glm-4.7`) {
		t.Errorf("warning does not name the first unknown-vocabulary model: %q", records.String())
	}
	if !strings.Contains(records.String(), `adapter=opencode`) {
		t.Errorf("warning does not name the adapter: %q", records.String())
	}

	records.Reset()
	adapter.Catalog(context.Background())
	if strings.Count(records.String(), "variant vocabulary unavailable") != 0 {
		t.Error("a memoized catalog read must not repeat the warning")
	}
}

// The slog capture helpers exist because the catalog's warning goes through
// the package-level default logger — the same channel the unreadable-records
// warning uses — and asserting it requires briefly replacing that default.

func slogDefault() *slog.Logger { return slog.Default() }

func restoreSlogDefault(logger *slog.Logger) { slog.SetDefault(logger) }

// captureSlog routes the default logger's warnings into sink as text records.
func captureSlog(sink *strings.Builder) {
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))
}
