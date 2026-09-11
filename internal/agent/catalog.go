package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// catalogRecord is one record of the runtime's verbose model listing as WE
// read it. Every field is optional and unknown fields are ignored: this is
// the runtime's JSON, and a decoder that refuses what it does not recognise
// turns their release into our outage. Absence is data, not an error — see
// the three states of Variants.
type catalogRecord struct {
	ID         string                      `json:"id"`
	ProviderID string                      `json:"providerID"`
	Variants   *map[string]json.RawMessage `json:"variants"`
}

// catalogParse is parseCatalogOutput's result: the models read, and when the
// output must be discarded wholesale, the reason and the first offending
// record (capped) for the log line that makes the breakage fixable.
type catalogParse struct {
	models  []models.CatalogModel
	problem string
	badCap  string
}

// parseCatalogOutput reads the verbose listing: a "provider/model" header
// line, then a pretty-printed object per model, repeated. The header is
// deliberately not read — the object carries the same identity in its own
// fields (providerID + id), and reading both would create two sources of one
// fact with no good answer to "which one wins when they disagree". Framing
// lines outside objects are skipped; a "{" at column zero opens a record and
// its matching "}" at column zero closes it (everything nested is indented by
// the printer).
//
// A record that does not decode, or decodes without id/providerID, invalidates
// the WHOLE catalog: a half-read listing would refuse every model it happened
// to miss as non-existent, which is exactly the lie an unavailable catalog is
// forbidden to tell. Zero objects read is the same outcome — an answer with no
// models in it is not an answer.
func parseCatalogOutput(output string) catalogParse {
	var (
		blocks     [][]string
		current    []string
		inBlock    bool
		unreadable int
		badSample  string
	)
	appendBlock := func() {
		if !inBlock {
			return
		}
		blocks = append(blocks, current)
		current = nil
		inBlock = false
	}
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		switch {
		case line == "{":
			appendBlock() // an unterminated previous block, treated as its own record below
			inBlock = true
			current = append(current, line)
		case line == "}":
			if inBlock {
				current = append(current, line)
			}
			appendBlock()
		case inBlock:
			current = append(current, line)
		default:
			// Header or framing text at column zero outside an object: skipped.
		}
	}
	appendBlock() // an object the output ended inside still gets its chance to fail

	if len(blocks) == 0 {
		if strings.TrimSpace(output) == "" {
			return catalogParse{problem: "empty output"}
		}
		return catalogParse{problem: "no records"}
	}

	parsed := make([]models.CatalogModel, 0, len(blocks))
	for _, block := range blocks {
		record, decodeErr := decodeCatalogRecord(block)
		if decodeErr != nil || record.ID == "" || record.ProviderID == "" {
			unreadable++
			if badSample == "" {
				badSample = capForLog(strings.Join(block, "\n"))
			}
			continue
		}
		entry := models.CatalogModel{
			ID:            record.ProviderID + "/" + record.ID,
			VariantsKnown: record.Variants != nil,
		}
		if record.Variants != nil {
			entry.Variants = make([]string, 0, len(*record.Variants))
			for variant := range *record.Variants {
				entry.Variants = append(entry.Variants, variant)
			}
			sort.Strings(entry.Variants)
		}
		parsed = append(parsed, entry)
	}
	if unreadable > 0 {
		return catalogParse{
			problem: fmt.Sprintf("unreadable records: %d of %d", unreadable, len(blocks)),
			badCap:  badSample,
		}
	}
	sort.Slice(parsed, func(left, right int) bool { return parsed[left].ID < parsed[right].ID })
	return catalogParse{models: parsed}
}

// decodeCatalogRecord decodes one accumulated object block.
func decodeCatalogRecord(block []string) (catalogRecord, error) {
	var record catalogRecord
	decodeErr := json.Unmarshal([]byte(strings.Join(block, "\n")), &record)
	return record, decodeErr
}

// badRecordLogCap bounds how much of an unreadable record reaches the log:
// enough of the head to recognise the shape change, not enough to dump a
// third party's output into the journal.
const badRecordLogCap = 200

func capForLog(text string) string {
	if len(text) <= badRecordLogCap {
		return text
	}
	return text[:badRecordLogCap] + "…"
}

// Catalog runs the runtime's model listing and memoizes the whole result for
// the process lifetime, exactly like the version probe: one subprocess per
// process for every consumer, and the answer — including a failure — is
// sticky, so a recorded fact stays reproducible for the runs that share the
// process. Installing or fixing the runtime is a restart; a probe that
// silently re-ran would make the memoized evidence unexplainable after the
// fact.
//
// The probe reuses the version probe's machinery verbatim (probeTimeout, the
// scrubbed child environment, the detached cancellation, the process-group
// kill) because the no-output hang is a property of the binary, not of any
// one subcommand. The runtime keeps its own catalog cache; --refresh is never
// passed, so boot never pays a network round trip it does not need.
//
// ctx contributes its values but not its cancellation: the memoized answer
// outlives whichever caller happened to ask first.
func (adapter *OpencodeAdapter) Catalog(ctx context.Context) models.Catalog {
	adapter.catalogOnce.Do(func() {
		adapter.catalog = adapter.runCatalogProbe(ctx)
	})
	return adapter.catalog
}

// runCatalogProbe performs one `<binary> models --verbose` subprocess and
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

	bin, err := exec.LookPath(adapter.binary)
	if err != nil {
		catalog.Reason = "binary not found"
		return catalog
	}

	cmd := exec.CommandContext(probeCtx, bin, "models", "--verbose")
	setProcessGroup(cmd)
	// The same credential-scrubbed environment the other probes run under.
	// The listing reads provider auth from disk, not from the environment, so
	// the scrubbed env loses nothing — and a catalog has no reason to see the
	// operator's secrets.
	cmd.Env = buildChildEnv(caps.Profile{}, "", nil)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if startErr := cmd.Start(); startErr != nil {
		catalog.Reason = fmt.Sprintf("start: %v", startErr)
		return catalog
	}
	reaped := make(chan struct{})
	watcherDone := watchCancellation(probeCtx, cmd, reaped)
	runErr := cmd.Wait()
	close(reaped)
	<-watcherDone

	if ctxErr := probeCtx.Err(); ctxErr != nil {
		catalog.Reason = "timeout"
		if !errors.Is(ctxErr, context.DeadlineExceeded) {
			catalog.Reason = fmt.Sprintf("cancelled: %v", ctxErr)
		}
		return catalog
	}
	if runErr != nil {
		reason := fmt.Sprintf("exit: %v", runErr)
		if errText := strings.TrimSpace(stderr.String()); errText != "" {
			reason += ": " + errText
		}
		catalog.Reason = reason
		return catalog
	}

	parsed := parseCatalogOutput(stdout.String())
	if parsed.problem != "" {
		catalog.Reason = parsed.problem
		if parsed.badCap != "" {
			slog.Warn("runtime model catalog unreadable; models will not be checked",
				"reason", parsed.problem, "record", parsed.badCap)
		}
		return catalog
	}
	catalog.Models = parsed.models
	catalog.Available = true
	return catalog
}

// catalogSource is the listing command as an operator would type it, for
// refusal texts that name how to list the models. The binary name is the
// adapter's own knowledge (the configured override included), never a literal
// in calling code.
func (adapter *OpencodeAdapter) catalogSource() string {
	return adapter.binary + " models"
}
