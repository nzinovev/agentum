// Package taskinput is the typed shape of a task request (ADR 0004): the
// requested behaviour (title + description) and the run overrides, which have
// different audiences. The request reaches the model through the routing
// block's Task section; the overrides reach the orchestrator only and are
// never rendered to the agent (D2).
//
// The package is standard-library-only by design, like internal/instructions:
// the mapping from Overrides onto checks.Request stays with the runner (its
// single caller), so this package never imports internal/checks.
package taskinput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Byte budgets for the request fields (D4). Over-budget input is rejected at
// the boundary, not truncated: a truncated request is a *different* request,
// and the agent has no way to know which half it lost. 32 KiB sits below the
// per-instruction-file budget because the description is prepended to every
// stage's context for the life of the run, unlike an instruction file with no
// degradation path.
const (
	MaxTitleBytes       = 200
	MaxDescriptionBytes = 32 << 10
)

// Overrides is the orchestrator-facing half of a task request: how this run
// differs from the project defaults. It is never rendered into a routing block
// (ADR 0004 D2), so every member must be safe to withhold from the model. A
// named container with one member today keeps a future `overrides.model`
// unambiguous rather than indistinguishable from part of the request.
type Overrides struct {
	Checks ChecksOverride `json:"checks"`
}

// ChecksOverride adds registered checks to this run BY NAME. A command is
// never accepted here; the project registry is the only source of commands
// (ADR 0002 D8), and typing this shape must not become an opportunity to let
// one in. Names are validated against the registry at resolve time.
type ChecksOverride struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// Request is the validated task request: what the run exists to satisfy.
type Request struct {
	Title       string
	Description string
	Overrides   Overrides
}

// Validate enforces D3 + D4: title and description must be present and
// non-empty after trimming, and each within its byte budget. Errors name the
// offending field so a 400 message tells the author what to fix.
func (request Request) Validate() error {
	if strings.TrimSpace(request.Title) == "" {
		return errors.New("title is required")
	}
	if len(request.Title) > MaxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes (got %d)", MaxTitleBytes, len(request.Title))
	}
	if strings.TrimSpace(request.Description) == "" {
		return errors.New("description is required")
	}
	if len(request.Description) > MaxDescriptionBytes {
		return fmt.Errorf("description exceeds %d bytes (got %d)", MaxDescriptionBytes, len(request.Description))
	}
	return nil
}

// Revision is the canonical hash of the whole request (D9): sha256 over a
// serialization with fixed field order, no incidental whitespace, and trimmed
// strings, so the same request always hashes the same regardless of how the
// source JSON was formatted. This is what the manifest's input-revision diff
// axis has always claimed to mean.
func (request Request) Revision() string {
	canonicalOverrides, marshalErr := request.Overrides.Marshal()
	if marshalErr != nil {
		// Unreachable for this shape (two string slices); if it ever fires,
		// hashing a degraded form would silently break revision stability, so
		// panic loudly instead — same stance as routing.Render.
		panic(fmt.Sprintf("taskinput: canonical marshal of overrides: %v", marshalErr))
	}
	// Struct fields marshal in declaration order, so the field order is fixed
	// by this declaration, not by the source bytes.
	canonical := struct {
		Description string          `json:"description"`
		Overrides   json.RawMessage `json:"overrides"`
		Title       string          `json:"title"`
	}{
		Description: strings.TrimSpace(request.Description),
		Overrides:   canonicalOverrides,
		Title:       strings.TrimSpace(request.Title),
	}
	encoded, marshalErr := json.Marshal(canonical)
	if marshalErr != nil {
		panic(fmt.Sprintf("taskinput: canonical marshal of request: %v", marshalErr))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Marshal returns the canonical JSON of the overrides: fixed field order, no
// incidental whitespace, and the check-name lists sorted and de-duplicated (a
// list of names is a set; neither the order the author typed them in nor a
// repeat carries meaning, and checks.Resolve collapses both anyway). The
// boundary stores exactly these bytes, so two identically-valued overrides
// produce identical rows and identical revisions.
//
// An empty value marshals to `{}`, not to a skeleton of empty lists: `{}` is
// what the column defaults to, what the migration backfills, and what the API
// documents a task with no overrides to carry. A canonical form that could not
// express "nothing" would make the common case the odd one out.
func (overrides Overrides) Marshal() ([]byte, error) {
	type canonicalChecks struct {
		Required []string `json:"required,omitempty"`
		Optional []string `json:"optional,omitempty"`
	}
	canonical := struct {
		Checks *canonicalChecks `json:"checks,omitempty"`
	}{}
	required := canonicalNames(overrides.Checks.Required)
	optional := canonicalNames(overrides.Checks.Optional)
	if len(required) > 0 || len(optional) > 0 {
		canonical.Checks = &canonicalChecks{Required: required, Optional: optional}
	}
	return json.Marshal(canonical)
}

// canonicalNames returns a sorted, de-duplicated copy of names, or nil when
// there are none. Operates on a fresh slice, so the receiver is never mutated.
func canonicalNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	sort.Strings(unique)
	return unique
}

// ParseOverrides strictly decodes stored or submitted override bytes (D3/D7):
// an unknown field is an error, never a silently dropped key — the typo that
// decodes into a zero value is the failure mode this package exists to close.
// Absent or empty bytes are the zero Overrides: a task created with no
// overrides at all is the common case, not an error.
func ParseOverrides(raw []byte) (Overrides, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Overrides{}, nil
	}
	// Two-level decode so the error names where the unknown field was: the
	// top level catches e.g. a future `model`, the nested decode catches
	// `checks.requred` with a message the author can act on.
	var topLevel struct {
		Checks json.RawMessage `json:"checks"`
	}
	topDecoder := json.NewDecoder(bytes.NewReader(trimmed))
	topDecoder.DisallowUnknownFields()
	if err := topDecoder.Decode(&topLevel); err != nil {
		return Overrides{}, fmt.Errorf("overrides: %w", err)
	}
	if len(bytes.TrimSpace(topLevel.Checks)) == 0 {
		return Overrides{}, nil
	}
	var checks ChecksOverride
	checksDecoder := json.NewDecoder(bytes.NewReader(topLevel.Checks))
	checksDecoder.DisallowUnknownFields()
	if err := checksDecoder.Decode(&checks); err != nil {
		return Overrides{}, fmt.Errorf("overrides.checks: %w", err)
	}
	// A `null` body decodes into the zero value with no error; that is a
	// legitimate "no overrides" and reads identically to an absent field.
	return Overrides{Checks: checks}, nil
}
