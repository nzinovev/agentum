package models

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// testCatalog mirrors the shape a runtime listing produces on a real machine:
// three providers, a model with variants, a model without, and two
// speed-suffixed siblings that differ from each other by two edits.
func testCatalog() Catalog {
	return Catalog{
		Available: true,
		Source:    "runtime models",
		CheckedAt: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
		Models: []CatalogModel{
			{ID: "opencode/muse-spark-1.3-contributor-free", Variants: []string{"high", "low", "medium", "minimal", "xhigh"}, VariantsKnown: true},
			{ID: "zai/glm-4.7", VariantsKnown: true},
			{ID: "zai-coding-plan/glm-5.2", Variants: []string{"high", "max"}, VariantsKnown: true},
			{ID: "zai-coding-plan/glm-5.3", Variants: []string{"high", "low", "max"}, VariantsKnown: true},
			{ID: "zai-coding-plan/glm-5.3-flash", Variants: []string{"high", "low", "max"}, VariantsKnown: true},
			{ID: "zai-coding-plan/glm-5.3-highspeed", Variants: []string{"high", "low", "max"}, VariantsKnown: true},
		},
	}
}

func selectionFor(tier, model string) Selection {
	return Selection{Tier: tier, Provider: SplitProvider(model), Options: Options{Model: model}}
}

// TestCatalog_ValidateReturnsNilWhenCatalogUnavailable is the rule the whole
// design hangs on, so it is its own test rather than a table row: a catalog
// that could not be obtained must never ground a refusal. Collapsing this
// into "not found" would turn a transient probe failure or a format change
// into "no such model" for every configured tier at once — a refusal that
// lies, with no operator workaround.
func TestCatalog_ValidateReturnsNilWhenCatalogUnavailable(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Available: false, Reason: "timeout", Source: "runtime models"}
	if err := catalog.Validate(selectionFor("fast", "zai-coding-plan/glm-5.3")); err != nil {
		t.Fatalf("Validate against an unavailable catalog = %v; want nil (the run proceeds unverified)", err)
	}
}

// TestCatalog_Validate covers the answering paths: a known model passes, an
// unknown model is refused with the typed error, and the error wraps
// ErrUnknownModel so callers can match on it.
func TestCatalog_Validate(t *testing.T) {
	t.Parallel()
	catalog := testCatalog()

	if err := catalog.Validate(selectionFor("strong", "zai-coding-plan/glm-5.3")); err != nil {
		t.Fatalf("known model refused: %v", err)
	}

	err := catalog.Validate(selectionFor("fast", "zai-coding-plan/glm-5.4"))
	if err == nil {
		t.Fatal("unknown model accepted")
	}
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("error does not wrap ErrUnknownModel: %v", err)
	}
	var unknown *UnknownModel
	if !errors.As(err, &unknown) {
		t.Errorf("error is not *UnknownModel: %T", err)
	}
}

// TestCatalog_ValidateUnknownProviderListsProviders: when the provider half
// itself is unknown, the message enumerates the providers — the operator
// misspelled the prefix, and thirty model names would bury the fix.
func TestCatalog_ValidateUnknownProviderListsProviders(t *testing.T) {
	t.Parallel()
	err := testCatalog().Validate(selectionFor("strong", "zia-coding-plan/glm-5.3"))
	if err == nil {
		t.Fatal("model with an unknown provider accepted")
	}
	message := err.Error()
	if !strings.Contains(message, `no provider "zia-coding-plan"`) {
		t.Errorf("message does not name the unknown provider: %q", message)
	}
	if !strings.Contains(message, "(known: opencode, zai, zai-coding-plan)") {
		t.Errorf("message does not enumerate the known providers, sorted: %q", message)
	}
	if strings.Contains(message, "muse-spark") {
		t.Errorf("message enumerates models instead of providers: %q", message)
	}
}

// TestCatalog_ValidateTypoSuggestsNearestSpelling: a typo within the
// suggestion distance names the nearest catalog spelling and the listing
// command (Source plus the provider). A model name nothing is near to gets no
// suggestion but still names the command — how to list them is the fix even
// when nothing was close.
func TestCatalog_ValidateTypoSuggestsNearestSpelling(t *testing.T) {
	t.Parallel()
	catalog := testCatalog()

	typo := catalog.Validate(selectionFor("fast", "zai-coding-plan/glm-5.3-hispeed"))
	if typo == nil {
		t.Fatal("typo accepted")
	}
	if !strings.Contains(typo.Error(), `did you mean "zai-coding-plan/glm-5.3-highspeed"`) {
		t.Errorf("typo message does not suggest the near spelling: %q", typo.Error())
	}
	if strings.Contains(typo.Error(), "glm-5.3-flash") {
		t.Errorf("typo message suggests a name outside the distance bound: %q", typo.Error())
	}
	if !strings.Contains(typo.Error(), "list them with: runtime models zai-coding-plan") {
		t.Errorf("typo message does not name the listing command: %q", typo.Error())
	}

	alien := catalog.Validate(selectionFor("fast", "zai-coding-plan/qwen-9"))
	if alien == nil {
		t.Fatal("alien model accepted")
	}
	if strings.Contains(alien.Error(), "did you mean") {
		t.Errorf("alien model got a suggestion anyway: %q", alien.Error())
	}
	if !strings.Contains(alien.Error(), "list them with: runtime models zai-coding-plan") {
		t.Errorf("alien model message does not name the listing command: %q", alien.Error())
	}
}

// TestCatalog_ValidateSourceComesFromTheCatalog: the listing command in the
// text is whatever Source the adapter recorded — this package renders it and
// does not decide which executor it names.
func TestCatalog_ValidateSourceComesFromTheCatalog(t *testing.T) {
	t.Parallel()
	catalog := testCatalog()
	catalog.Source = "some-other-runtime models"
	err := catalog.Validate(selectionFor("fast", "zai-coding-plan/qwen-9"))
	if err == nil {
		t.Fatal("unknown model accepted")
	}
	if !strings.Contains(err.Error(), "list them with: some-other-runtime models zai-coding-plan") {
		t.Errorf("message does not use the catalog's Source: %q", err.Error())
	}
}

// TestCatalog_EmptyAvailableCatalogValidatesAsUnavailable: an Available
// catalog with zero models is a contradiction (a listing that read in full
// produced nothing readable), and reading it as "the runtime runs nothing"
// would refuse every model. It must behave exactly like a catalog that was
// never obtained.
func TestCatalog_EmptyAvailableCatalogValidatesAsUnavailable(t *testing.T) {
	t.Parallel()
	catalog := Catalog{Available: true, Source: "runtime models"}
	if err := catalog.Validate(selectionFor("fast", "any/model")); err != nil {
		t.Fatalf("empty-but-available catalog refused a model: %v; want the not-obtained behaviour", err)
	}
	if catalog.Label() != "ok (0 models)" {
		t.Errorf("Label = %q; the label reports what was obtained, the refusal rule is Validate's", catalog.Label())
	}
}

// TestCatalog_ValidateEmptyModelStringPasses: the empty model string is
// refused by configuration loading (a tier with an empty model is a broken
// file), not by the catalog — here it means "nothing was claimed to exist".
func TestCatalog_ValidateEmptyModelStringPasses(t *testing.T) {
	t.Parallel()
	if err := testCatalog().Validate(Selection{Tier: "fast"}); err != nil {
		t.Fatalf("empty model string refused by the catalog: %v", err)
	}
}

// TestCatalog_ProvidersSortedAndUnique pins the enumeration contract the
// unknown-provider message prints.
func TestCatalog_ProvidersSortedAndUnique(t *testing.T) {
	t.Parallel()
	providers := testCatalog().Providers()
	want := []string{"opencode", "zai", "zai-coding-plan"}
	if len(providers) != len(want) {
		t.Fatalf("Providers() = %v; want %v", providers, want)
	}
	for index := range want {
		if providers[index] != want[index] {
			t.Fatalf("Providers() = %v; want %v", providers, want)
		}
	}
}

// TestCatalog_SuggestCapsAndSorts: at most three candidates, nearest first,
// ties alphabetical.
func TestCatalog_SuggestCapsAndSorts(t *testing.T) {
	t.Parallel()
	catalog := Catalog{
		Available: true,
		Models: []CatalogModel{
			{ID: "p/beta-1"}, {ID: "p/beta-2"}, {ID: "p/beta-3"}, {ID: "p/beta-4"},
		},
	}
	suggestions := catalog.Suggest("p/beta-9")
	if len(suggestions) != 3 {
		t.Fatalf("Suggest returned %d candidates; want the cap of 3: %v", len(suggestions), suggestions)
	}
	// All four are one edit away; alphabetical order breaks the tie.
	if suggestions[0] != "p/beta-1" || suggestions[1] != "p/beta-2" || suggestions[2] != "p/beta-3" {
		t.Errorf("Suggest = %v; want the first three alphabetically", suggestions)
	}
}

// TestCatalog_Label covers the two labels a Catalog value itself can render.
func TestCatalog_Label(t *testing.T) {
	t.Parallel()
	if label := testCatalog().Label(); label != "ok (6 models)" {
		t.Errorf("Label() = %q; want ok (6 models)", label)
	}
	failed := Catalog{Available: false, Reason: "timeout"}
	if label := failed.Label(); label != "failed: timeout" {
		t.Errorf("Label() = %q; want failed: timeout", label)
	}
}

// TestCatalog_LookupAndVariantsSemantics pins the three states a record's
// variants field can leave behind: known-with-keys, known-empty, unknown.
func TestCatalog_LookupAndVariantsSemantics(t *testing.T) {
	t.Parallel()
	catalog := testCatalog()
	entry, found := catalog.Lookup("zai/glm-4.7")
	if !found {
		t.Fatal("known model not found by Lookup")
	}
	if !entry.VariantsKnown || len(entry.Variants) != 0 {
		t.Errorf("model without variants: VariantsKnown=%v Variants=%v; want known and empty", entry.VariantsKnown, entry.Variants)
	}
	if _, found := catalog.Lookup("zai-coding-plan/nope"); found {
		t.Error("unknown model found by Lookup")
	}
}

// TestLevenshtein pins the distance primitive on the cases the suggestion
// rule is reasoned about with, including the two-edit typo and a
// beyond-the-bound pair.
func TestLevenshtein(t *testing.T) {
	t.Parallel()
	cases := []struct {
		left  string
		right string
		want  int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"hispeed", "highspeed", 2},
		{"hispeed", "flash", 7},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
		{"abc", "", 3},
	}
	for _, testCase := range cases {
		if got := levenshtein(testCase.left, testCase.right); got != testCase.want {
			t.Errorf("levenshtein(%q, %q) = %d; want %d", testCase.left, testCase.right, got, testCase.want)
		}
	}
}
