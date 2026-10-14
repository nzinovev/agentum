package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// catalogListing is the runtime's model listing as WE read it: the
// `opencode api GET /api/model` envelope. Every field is optional and unknown
// fields are ignored: this is the runtime's JSON, and a decoder that refuses
// what it does not recognise turns their release into our outage. Data is a
// pointer so an envelope without it reads as "no records", not as an empty
// runtime.
type catalogListing struct {
	Data *[]catalogRecord `json:"data"`
}

// catalogRecord is one model of the listing. Absence is data, not an error —
// see the three states of Variants. Only the identity and the variant names
// are decoded: a record also carries the provider's settings, API keys
// included, and nothing of it may reach a log.
type catalogRecord struct {
	ID         string            `json:"id"`
	ProviderID string            `json:"providerID"`
	Variants   *[]catalogVariant `json:"variants"`
}

type catalogVariant struct {
	ID string `json:"id"`
}

// catalogParse is parseCatalogOutput's result: the models read, or the reason
// the output must be discarded wholesale.
type catalogParse struct {
	models  []models.CatalogModel
	problem string
}

// parseCatalogOutput reads the listing envelope.
//
// A record without id/providerID, or a variant without an id, invalidates the
// WHOLE catalog: a half-read listing would refuse every model it happened to
// miss as non-existent — the exact false refusal an unavailable catalog must
// not produce. Zero records read is the same outcome — an answer with no
// models in it is not an answer.
func parseCatalogOutput(output string) catalogParse {
	if strings.TrimSpace(output) == "" {
		return catalogParse{problem: "empty output"}
	}
	var listing catalogListing
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		// The decoder's message names a position and a type, never the
		// record's content.
		return catalogParse{problem: fmt.Sprintf("unreadable listing: %v", err)}
	}
	if listing.Data == nil || len(*listing.Data) == 0 {
		return catalogParse{problem: "no records"}
	}
	records := *listing.Data
	parsed := make([]models.CatalogModel, 0, len(records))
	unreadable := 0
	for _, record := range records {
		entry, ok := catalogEntry(record)
		if !ok {
			unreadable++
			continue
		}
		parsed = append(parsed, entry)
	}
	if unreadable > 0 {
		return catalogParse{problem: fmt.Sprintf("unreadable records: %d of %d", unreadable, len(records))}
	}
	sort.Slice(parsed, func(left, right int) bool { return parsed[left].ID < parsed[right].ID })
	return catalogParse{models: parsed}
}

// countCatalogRecords is the settle check's view of a listing body: how many
// records it holds, or the decode error that makes it unreadable.
func countCatalogRecords(body []byte) (int, error) {
	var listing catalogListing
	if err := json.Unmarshal(body, &listing); err != nil {
		return 0, fmt.Errorf("unreadable listing: %w", err)
	}
	if listing.Data == nil {
		return 0, errors.New("no records")
	}
	return len(*listing.Data), nil
}

// probeFailureReason renders a probe failure as the short reason the
// evidence records.
func probeFailureReason(err error) string {
	switch {
	case errors.Is(err, errListingEmpty):
		return "no records"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return fmt.Sprintf("cancelled: %v", err)
	default:
		return err.Error()
	}
}

// catalogEntry converts one record, reporting false when it lacks an
// identity to file under.
func catalogEntry(record catalogRecord) (models.CatalogModel, bool) {
	if record.ID == "" || record.ProviderID == "" {
		return models.CatalogModel{}, false
	}
	entry := models.CatalogModel{
		ID:            record.ProviderID + "/" + record.ID,
		VariantsKnown: record.Variants != nil,
	}
	if record.Variants != nil {
		entry.Variants = make([]string, 0, len(*record.Variants))
		for _, variant := range *record.Variants {
			if variant.ID == "" {
				return models.CatalogModel{}, false
			}
			entry.Variants = append(entry.Variants, variant.ID)
		}
		sort.Strings(entry.Variants)
	}
	return entry, true
}

// Catalog runs the runtime's model listing and memoizes the whole result for
// the process lifetime, exactly like the version probe: one subprocess per
// process for every consumer, and the answer — including a failure — is
// sticky, so a recorded fact stays reproducible for the runs that share the
// process. Installing or fixing the runtime is a restart; a probe that
// re-ran would make the memoized evidence unexplainable after the
// fact.
//
// The probe reuses the version probe's machinery verbatim (probeTimeout, the
// scrubbed child environment, the detached cancellation, the process-group
// kill) because the no-output hang is a property of the binary, not of any
// one subcommand. The listing is the runtime's own cached snapshot, so boot
// makes no network round trip for it.
//
// ctx contributes its values but not its cancellation: the memoized answer
// outlives whichever caller happened to ask first.
func (adapter *OpencodeAdapter) Catalog(ctx context.Context) models.Catalog {
	adapter.catalogOnce.Do(func() {
		adapter.catalog = adapter.runCatalogProbe(ctx)
	})
	return adapter.catalog
}

// runCatalogProbe performs one `<binary> api --standalone GET /api/model`
// subprocess and
// classifies the outcome. The start/watcher/wait order mirrors runVersionProbe:
// cmd.Start() runs synchronously (writing cmd.Process) BEFORE
// watchCancellation's goroutine can read it.
func (adapter *OpencodeAdapter) runCatalogProbe(ctx context.Context) models.Catalog {
	catalog := models.Catalog{
		Source:    adapter.catalogSource(),
		CheckedAt: time.Now().UTC(),
	}

	// Process-scoped, not request-scoped: the memoized result must not be
	// decided by the cancellation of whichever caller reached the probe first.
	// Values are kept; only cancellation is dropped, and probeTimeout remains
	// the bound.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	defer cancel()

	// A fresh directory as the server's location: the catalog is
	// process-wide, so no one project's opencode.json may shape it.
	workdir, err := os.MkdirTemp("", "agentum-catalog-")
	if err != nil {
		catalog.Reason = fmt.Sprintf("workdir: %v", err)
		return catalog
	}
	defer os.RemoveAll(workdir)

	// The same credential-scrubbed environment the other probes run under.
	// The listing reads provider auth from disk, not from the environment, so
	// the scrubbed env loses nothing — and a catalog has no reason to see the
	// operator's secrets.
	server, err := adapter.startRuntimeServer(probeCtx, workdir, buildChildEnv(caps.Profile{}, "", nil))
	if err != nil {
		catalog.Reason = probeFailureReason(err)
		return catalog
	}
	defer server.close()
	body, err := server.settledGet(probeCtx, modelListPath, workdir, countCatalogRecords)
	if err != nil {
		catalog.Reason = probeFailureReason(err)
		return catalog
	}

	parsed := parseCatalogOutput(string(body))
	if parsed.problem != "" {
		catalog.Reason = parsed.problem
		slog.Warn("runtime model catalog unreadable; models will not be checked", "reason", parsed.problem)
		return catalog
	}
	logUnknownVariantVocabulary(parsed.models)
	catalog.Models = parsed.models
	catalog.Available = true
	return catalog
}

// logUnknownVariantVocabulary writes ONE warning when any catalog record came
// without a readable variants field: the variant check is skipped for those
// models, and an operator sending a typo'd variant deserves to know it will
// not be caught. One line, not one per record — a runtime that renames the
// field emits it on every model of the listing, and thirty identical warnings
// bury everything else in the log. Records are sorted, so the first unknown
// one is stable across runs. Inside the memoized probe, so the warning fires
// once per process, not once per catalog reader.
func logUnknownVariantVocabulary(entries []models.CatalogModel) {
	unknownCount := 0
	firstUnknown := ""
	for _, entry := range entries {
		if entry.VariantsKnown {
			continue
		}
		if unknownCount == 0 {
			firstUnknown = entry.ID
		}
		unknownCount++
	}
	if unknownCount == 0 {
		return
	}
	slog.Warn("variant vocabulary unavailable; variant validation skipped",
		"adapter", string(opencodeDescriptor.ID),
		"records", unknownCount,
		"first_model", firstUnknown)
}

// catalogSource is the listing command as an operator would type it, for
// refusal texts that name how to list the models. The binary name is the
// adapter's own knowledge (the configured override included), never a literal
// in calling code.
func (adapter *OpencodeAdapter) catalogSource() string {
	return adapter.binary + " models"
}
