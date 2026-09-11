package models

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ErrUnknownModel is returned by Catalog.Validate for a selection whose model
// the catalog — obtained and read in full — does not contain. Wrapped by
// *UnknownModel, which carries the fix (near spellings, the provider list, the
// list command); mirrors ErrUnsupportedOption deliberately.
var ErrUnknownModel = errors.New("models: unknown model")

// CatalogUnsupported is the evidence label for an adapter that cannot be asked
// to enumerate its runtime's models. A run records that models were never
// checked rather than checked-and-passed; the two facts must stay
// distinguishable after the fact, or a degraded check would read as a clean
// one.
const CatalogUnsupported = "unsupported"

// suggestMaxDistance is the Levenshtein bound below which a catalog model
// counts as a near spelling of the requested one. Far enough to catch a real
// typo, near enough that thirty names do not produce three plausible-looking
// strangers.
const suggestMaxDistance = 3

// suggestLimit caps how many near spellings the refusal carries. The full list
// is what the runtime's own listing command prints; the error names the
// nearest, not everything.
const suggestLimit = 3

// CatalogModel is one model the runtime says it can run. Variants is the
// declared reasoning-variant vocabulary of exactly this model (empty = none
// declared); VariantsKnown distinguishes "declares no variants" from "the
// variants field was absent or reshaped" — the first is a model fact the
// checker can act on, the second means the runtime's format moved and nothing
// about variants may be concluded.
type CatalogModel struct {
	ID            string   // "providerID/id", assembled from the runtime's record
	Variants      []string // keys of the declared variants map; empty = none
	VariantsKnown bool     // false = the record carried no readable variants field
}

// Catalog is what a runtime says it can run: a value obtained by asking the
// runtime, never a table maintained here. A catalog that could not be obtained
// or read in full is Available=false with a Reason — a fact to record, never a
// refusal ground: "could not check" must not become "does not exist".
type Catalog struct {
	Available bool
	Reason    string
	// Source is the command the answer came from, for refusal texts that name
	// how to list the models. Filled by the adapter: the executor's name is
	// adapter knowledge and may not appear in this package.
	Source    string
	Models    []CatalogModel
	CheckedAt time.Time
}

// UnknownModel is the typed refusal for a model a fully-read catalog does not
// contain. The fields are the ingredients of the message: the model asked for,
// the nearest spellings found, the known providers (when the provider half
// itself is unknown), and the listing command.
type UnknownModel struct {
	Model       string
	Suggestions []string
	Providers   []string
	Source      string
}

// Error renders the refusal with its fix. Three shapes:
//
//   - the provider is unknown: enumerate the providers, not the models —
//     "no provider x (known: …)";
//   - a near spelling exists: "did you mean …" plus the listing command;
//   - nothing near: the listing command alone.
func (unknown *UnknownModel) Error() string {
	provider := SplitProvider(unknown.Model)
	if provider != "" && len(unknown.Providers) > 0 && !containsString(unknown.Providers, provider) {
		return fmt.Sprintf("unknown model %q: no provider %q (known: %s)",
			unknown.Model, provider, strings.Join(unknown.Providers, ", "))
	}
	message := fmt.Sprintf("unknown model %q", unknown.Model)
	if len(unknown.Suggestions) > 0 {
		quoted := make([]string, 0, len(unknown.Suggestions))
		for _, suggestion := range unknown.Suggestions {
			quoted = append(quoted, fmt.Sprintf("%q", suggestion))
		}
		message += " (did you mean " + strings.Join(quoted, ", ") + "?)"
	}
	if unknown.Source != "" {
		listCommand := unknown.Source
		if provider != "" {
			listCommand += " " + provider
		}
		message += "; list them with: " + listCommand
	}
	return message
}

func (unknown *UnknownModel) Unwrap() error { return ErrUnknownModel }

// usable reports whether this catalog may ground a refusal: it was obtained,
// and it is not empty. An empty-yet-Available catalog is treated as not
// obtained rather than as "the runtime runs nothing" — the second reading
// would refuse every model with a text that lies about the reason.
func (catalog Catalog) usable() bool {
	return catalog.Available && len(catalog.Models) > 0
}

// Lookup returns the catalog entry for a full "provider/model" id.
func (catalog Catalog) Lookup(model string) (CatalogModel, bool) {
	for index := range catalog.Models {
		if catalog.Models[index].ID == model {
			return catalog.Models[index], true
		}
	}
	return CatalogModel{}, false
}

// Validate checks a selection against the catalog. It returns nil when the
// catalog cannot ground a refusal — not obtained, unreadable, or empty — so a
// transient probe failure never turns into "no such model" for models that
// exist. Against a full catalog an unknown model is a typed *UnknownModel
// carrying the fix. An empty model string is not this check's question
// (configuration refuses it elsewhere) and passes.
func (catalog Catalog) Validate(selection Selection) error {
	if !catalog.usable() {
		return nil
	}
	model := selection.Options.Model
	if model == "" {
		return nil
	}
	if _, found := catalog.Lookup(model); found {
		return nil
	}
	provider := SplitProvider(model)
	if provider != "" && !containsString(catalog.Providers(), provider) {
		return &UnknownModel{Model: model, Providers: catalog.Providers()}
	}
	return &UnknownModel{Model: model, Suggestions: catalog.Suggest(model), Source: catalog.Source}
}

// Suggest returns the catalog models within suggestMaxDistance edits of the
// requested id — at most suggestLimit, sorted by (distance, name) so the
// nearest spelling comes first and equal distances are alphabetical.
func (catalog Catalog) Suggest(model string) []string {
	if model == "" || len(catalog.Models) == 0 {
		return nil
	}
	type nearSpell struct {
		name     string
		distance int
	}
	candidates := make([]nearSpell, 0, len(catalog.Models))
	for _, entry := range catalog.Models {
		if entry.ID == model {
			continue
		}
		distance := levenshtein(model, entry.ID)
		if distance <= suggestMaxDistance {
			candidates = append(candidates, nearSpell{name: entry.ID, distance: distance})
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].distance != candidates[right].distance {
			return candidates[left].distance < candidates[right].distance
		}
		return candidates[left].name < candidates[right].name
	})
	if len(candidates) > suggestLimit {
		candidates = candidates[:suggestLimit]
	}
	suggestions := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		suggestions = append(suggestions, candidate.name)
	}
	return suggestions
}

// Providers returns the distinct provider halves of the catalog, sorted. The
// refusal for an unknown provider enumerates these, not the models.
func (catalog Catalog) Providers() []string {
	seen := make(map[string]struct{}, len(catalog.Models))
	providers := make([]string, 0, len(catalog.Models))
	for _, entry := range catalog.Models {
		provider := SplitProvider(entry.ID)
		if provider == "" {
			continue
		}
		if _, duplicate := seen[provider]; duplicate {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

// Label renders the catalog probe outcome for evidence: "ok (N models)" or
// "failed: <reason>" — the same vocabulary the runtime readiness probe uses,
// so evidence readers see one shape for probed runtime facts. The third label
// of the vocabulary, CatalogUnsupported, is decided by the caller from the
// adapter descriptor: a value this type cannot know is "was never asked".
func (catalog Catalog) Label() string {
	if catalog.Available {
		return fmt.Sprintf("ok (%d models)", len(catalog.Models))
	}
	return "failed: " + catalog.Reason
}

// levenshtein computes the edit distance between two strings over runes. The
// two-row DP keeps it allocation-light for the sizes a catalog suggests over.
func levenshtein(left, right string) int {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	if len(leftRunes) == 0 {
		return len(rightRunes)
	}
	if len(rightRunes) == 0 {
		return len(leftRunes)
	}
	previous := make([]int, len(rightRunes)+1)
	current := make([]int, len(rightRunes)+1)
	for column := 0; column <= len(rightRunes); column++ {
		previous[column] = column
	}
	for row := 1; row <= len(leftRunes); row++ {
		current[0] = row
		for column := 1; column <= len(rightRunes); column++ {
			substitution := previous[column-1]
			if leftRunes[row-1] != rightRunes[column-1] {
				substitution++
			}
			current[column] = min(current[column-1]+1, previous[column]+1, substitution)
		}
		previous, current = current, previous
	}
	return previous[len(rightRunes)]
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
