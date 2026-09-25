package runner

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The per-stage variant contract, driven through the run loop: a pack whose
// stages sit on tiers with different variants executes both in one pipeline,
// the adapter receives each stage's own pair, the evidence records them per
// invocation, and every refusal lands at run start with zero invocations.

// variantRecordingAdapter is a scriptAdapter that records the model options
// each stage's invocation actually carried, and widens the stub's declared
// option set with variant so the catalog's vocabulary check is reachable —
// the plain stub declares {model} only, and run-start validation would stop
// one check earlier.
type variantRecordingAdapter struct {
	scriptAdapter
	mu             sync.Mutex
	optionsByStage map[string]models.Options
}

func (adapter *variantRecordingAdapter) Describe() agent.Descriptor {
	descriptor := adapter.scriptAdapter.Describe()
	descriptor.ModelOptions = []models.OptionName{models.OptionModel, models.OptionVariant}
	return descriptor
}

func (adapter *variantRecordingAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	adapter.mu.Lock()
	if adapter.optionsByStage == nil {
		adapter.optionsByStage = map[string]models.Options{}
	}
	adapter.optionsByStage[stageOf(inv)] = inv.Model.Options
	adapter.mu.Unlock()
	return adapter.scriptAdapter.Invoke(ctx, inv)
}

func (adapter *variantRecordingAdapter) recordedOptions(stage string) (models.Options, bool) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	options, found := adapter.optionsByStage[stage]
	return options, found
}

// variantTierConfig declares two tiers on one model with different variants —
// the shape "plan on high effort, implement on low" takes in models.yaml.
func variantTierConfig() *models.Config {
	return &models.Config{
		Tiers: map[string]models.TierDefinition{
			"plain":    {Model: "stub/fast-model"},
			"reasoned": {Model: "stub/strong-model", Variant: "high"},
			"settled":  {Model: "stub/strong-model", Variant: "low"},
		},
		Default: "plain",
	}
}

// variantCatalog lists every stub model with both variants declared, so a run
// against it is checked-and-passed rather than refused or skipped.
func variantCatalog() models.Catalog {
	return models.Catalog{
		Available: true,
		Models: []models.CatalogModel{
			{ID: "stub/fast-model", VariantsKnown: true},
			{ID: "stub/strong-model", Variants: []string{"high", "low"}, VariantsKnown: true},
			{ID: "stub/reasoning-model", VariantsKnown: true},
		},
	}
}

// newVariantRunner wires a two-stage pack (plain → reasoned) on the variant
// tier config, with the recording adapter and the manifest fake.
func newVariantRunner(t *testing.T, catalog models.Catalog) (*variantRecordingAdapter, *fakeManifestService, *Runner, *fakeStore) {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Tier: "plain", Transitions: []pack.Transition{{To: "impl"}}},
		"impl": {Gate: pack.GateAuto, Prompt: "impl.md", Tier: "reasoned", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	record := sqlc.Run{ID: "Tvar", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &variantRecordingAdapter{scriptAdapter: scriptAdapter{
		stubExecution: stubExecution{enumeratesModels: true, catalog: catalog},
		scripts: map[string]agent.ResultJSON{
			"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "spec done"},
			"impl": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "impl done"},
		},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Models: variantTierConfig()})
	fake := &fakeManifestService{}
	runner.mfst = fake
	return adapter, fake, runner, store
}

// variantStubAdapter widens the stub's declared option set with variant for
// script-level tests that do not record options — same reason as the
// recording adapter's override.
type variantStubAdapter struct {
	scriptAdapter
}

func (adapter *variantStubAdapter) Describe() agent.Descriptor {
	descriptor := adapter.scriptAdapter.Describe()
	descriptor.ModelOptions = []models.OptionName{models.OptionModel, models.OptionVariant}
	return descriptor
}

// recordedVariantOf returns the variant one invocation record carried, or ""
// when the stage recorded none.
func recordedVariantOf(service *fakeManifestService, invocationID string) string {
	service.mu.Lock()
	defer service.mu.Unlock()
	for _, patch := range service.addEvidence {
		for _, record := range patch.Invocations {
			if record.InvocationID == invocationID {
				return record.Model.Options.Variant
			}
		}
	}
	return ""
}

// TestRunner_TwoStagesOnDifferentVariantsInOnePipeline: the pipeline runs both
// stages, each invocation carries its own tier's variant — and the evidence
// records the variants per invocation, so the two attempts are distinguishable
// after the fact.
func TestRunner_TwoStagesOnDifferentVariantsInOnePipeline(t *testing.T) {
	t.Parallel()
	adapter, evidence, runner, store := newVariantRunner(t, variantCatalog())

	if runErr := runner.HandleRun(t.Context(), job("run", "Tvar", "tn", "us")); runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}
	if got := store.taskState(); got != "awaiting_final_review" {
		t.Fatalf("state = %q; want the run to reach the final gate", got)
	}
	if count := len(store.invocations); count != 2 {
		t.Fatalf("stage invocations = %d; want 2", count)
	}
	if options, found := adapter.recordedOptions("spec"); !found || options.Variant != "" {
		t.Errorf("spec stage options = %+v; want the plain tier's bare model", options)
	}
	if options, found := adapter.recordedOptions("impl"); !found || options.Variant != "high" {
		t.Errorf("impl stage options = %+v; want the reasoned tier's variant", options)
	}

	// Each invocation's evidence carries the variant it ran with.
	serviceVariants := map[string]string{}
	store.mu.Lock()
	invocationIDs := make([]string, 0, len(store.invocations))
	for _, invocation := range store.invocations {
		invocationIDs = append(invocationIDs, invocation.ID)
	}
	store.mu.Unlock()
	for _, invocationID := range invocationIDs {
		serviceVariants[invocationID] = recordedVariantOf(evidence, invocationID)
	}
	seenVariants := map[string]bool{}
	for _, variant := range serviceVariants {
		seenVariants[variant] = true
	}
	if !seenVariants[""] || !seenVariants["high"] {
		t.Errorf("recorded invocation variants = %v; want one bare and one high", serviceVariants)
	}
}

// TestRunner_UndeclaredVariantOptionStopsRunStart: a stage on a tier with a
// variant, against an adapter whose descriptor declares {model} only, fails
// at run start — zero stage_invocations — and the refusal names the adapter,
// the option, and the value. The declared set is data, so the refusal path is
// testable before a second adapter exists.
func TestRunner_UndeclaredVariantOptionStopsRunStart(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Tier: "reasoned", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	record := sqlc.Run{ID: "Tund", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{
		// stubExecution's descriptor declares {model} only — the shape the
		// refusal needs.
		scripts: map[string]agent.ResultJSON{
			"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Models: variantTierConfig()})

	runErr := runner.HandleRun(t.Context(), job("run", "Tund", "tn", "us"))
	if runErr == nil {
		t.Fatal("a stage variant the adapter does not declare must fail the run at start")
	}
	if count := len(store.invocations); count != 0 {
		t.Fatalf("stage invocations = %d; want 0 (the refusal is before the first invocation)", count)
	}
	message := runErr.Error()
	for _, wanted := range []string{`"stub"`, `variant="high"`, `"spec"`} {
		if !strings.Contains(message, wanted) {
			t.Errorf("error %q does not contain %q (adapter, option+value, stage)", message, wanted)
		}
	}
}

// TestRunner_VariantOutsideVocabularyFailsRunStart: against a catalog that
// read the model's vocabulary, a tier variant outside it fails the run at
// start — the runtime would run a different effort silently, so the refusal
// here is the only sign the operator gets.
func TestRunner_VariantOutsideVocabularyFailsRunStart(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Tier: "reasoned", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	record := sqlc.Run{ID: "Tvoc", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &variantStubAdapter{scriptAdapter: scriptAdapter{
		stubExecution: stubExecution{
			enumeratesModels: true,
			// The vocabulary is known and does not contain "high".
			catalog: models.Catalog{Available: true, Models: []models.CatalogModel{
				{ID: "stub/strong-model", Variants: []string{"low"}, VariantsKnown: true},
			}},
		},
		scripts: map[string]agent.ResultJSON{
			"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		},
	}}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Models: variantTierConfig()})

	runErr := runner.HandleRun(t.Context(), job("run", "Tvoc", "tn", "us"))
	if runErr == nil {
		t.Fatal("a variant outside the declared vocabulary must fail the run at start")
	}
	if count := len(store.invocations); count != 0 {
		t.Fatalf("stage invocations = %d; want 0", count)
	}
	message := runErr.Error()
	for _, wanted := range []string{`declares no variant "high"`, "declares: low", `"reasoned"`} {
		if !strings.Contains(message, wanted) {
			t.Errorf("error %q does not contain %q", message, wanted)
		}
	}
}

// TestRunner_UnknownVocabularyAndUnavailableCatalogLetTheVariantRun: the two
// states in which the vocabulary question cannot be answered — a catalog whose
// records came without a readable variants field, and no catalog at all —
// leave the run proceeding with the configured variant. "Could not check"
// is never a refusal, for the variant any more than for the model.
func TestRunner_UnknownVocabularyAndUnavailableCatalogLetTheVariantRun(t *testing.T) {
	t.Parallel()
	states := []struct {
		name    string
		catalog models.Catalog
	}{
		{"unknown vocabulary", models.Catalog{Available: true, Models: []models.CatalogModel{
			{ID: "stub/strong-model", VariantsKnown: false},
			{ID: "stub/fast-model", VariantsKnown: false},
		}}},
		{"unavailable catalog", models.Catalog{Reason: "timeout"}},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			adapter, _, runner, store := newVariantRunner(t, state.catalog)
			if runErr := runner.HandleRun(t.Context(), job("run", "Tvar", "tn", "us")); runErr != nil {
				t.Fatalf("run failed: %v", runErr)
			}
			if got := store.taskState(); got != "awaiting_final_review" {
				t.Fatalf("state = %q; want the run to reach the final gate", got)
			}
			if options, found := adapter.recordedOptions("impl"); !found || options.Variant != "high" {
				t.Errorf("impl stage options = %+v; want the configured variant preserved", options)
			}
		})
	}
}

// TestRunner_InconsistentTierConfigAssembledInGoFailsRunStart: a Config with a
// variant but no model — assembled in code, Load never involved — is refused
// at run start by the same consistency check the file path applies.
func TestRunner_InconsistentTierConfigAssembledInGoFailsRunStart(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	runPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Tier: "broken", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	record := sqlc.Run{ID: "Tbrk", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(record, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
	}}
	brokenConfig := &models.Config{
		Tiers:   map[string]models.TierDefinition{"broken": {Variant: "high"}},
		Default: "broken",
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: runPack}, Adapter: adapter, Models: brokenConfig})

	runErr := runner.HandleRun(t.Context(), job("run", "Tbrk", "tn", "us"))
	if runErr == nil {
		t.Fatal("a variant without a model must fail the run at start")
	}
	if count := len(store.invocations); count != 0 {
		t.Fatalf("stage invocations = %d; want 0", count)
	}
	if !strings.Contains(runErr.Error(), `declares variant "high" but no model`) {
		t.Errorf("error %q does not name the variant", runErr.Error())
	}
}
