package checks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Request asks to include a named registry check in the effective set. Required
// makes it mandatory (a failure blocks delivery). Required is monotonic: once
// any layer (project baseline, pack, task) marks a check required, no other
// layer can undo it — the effective set ORs the flags. A pack that lists a
// baseline check as optional does not downgrade it; the baseline's required flag
// wins and is preserved.
type Request struct {
	Name     string
	Required bool
}

// Layer labels recorded on an Item's Sources so the manifest evidence shows
// which layer contributed each check.
const (
	LayerBaseline = "baseline"
	LayerPack     = "pack"
	LayerTask     = "task"
)

// Item is one resolved check: the project-owned definition (the only source of
// a command), the effective mandatory flag (OR of every layer that requested
// it), and the layers that asked for it.
type Item struct {
	Definition Definition
	Required   bool
	Sources    []string
}

// Set is the effective check set: the union of the project baseline and the
// pack/task requests, with required flags OR'd. SetVersion is a content hash of
// the resolved set so two runs with different effective sets (different pack,
// different task requests, or a changed registry) are distinguishable in the
// manifest. RegistryRevision is carried through for the same reason.
type Set struct {
	Items            []Item
	SetVersion       string
	RegistryRevision string
}

// Empty reports whether the set runs no checks.
func (set *Set) Empty() bool { return set == nil || len(set.Items) == 0 }

// Resolve builds the effective set from the project registry plus pack and task
// requests. The rules enforced here are the security boundary of the mechanism:
//
//   - Project baseline (Definition.Required) is always included; it cannot be
//     removed. This is the minimum the project mandates.
//   - Pack and task requests add checks by name only. An unknown name (not in
//     the registry) is rejected before any execution — a pack or task cannot
//     reference a check the project did not register, and can never supply a
//     command.
//   - Required is monotonic (OR). A layer cannot weaken a mandatory check:
//     baseline required, or a pack/task required request, makes the check
//     mandatory for the whole run.
//
// A nil registry yields an empty set (the project defines no checks); delivery
// passes with no checks run.
func Resolve(registry *Registry, packRequests, taskRequests []Request) (*Set, error) {
	if registry == nil {
		return &Set{}, nil
	}

	items := make(map[string]*Item, len(registry.items))

	// Project baseline: every Required definition is in the set and mandatory.
	for _, definition := range registry.items {
		if !definition.Required {
			continue
		}
		items[definition.Name] = &Item{
			Definition: definition, Required: true, Sources: []string{LayerBaseline},
		}
	}

	// Pack and task requests add by name only; unknown names are reported
	// together so a misconfigured pack surfaces every bad reference at once.
	var unknown []string
	for _, layer := range []struct {
		label string
		reqs  []Request
	}{
		{LayerPack, packRequests},
		{LayerTask, taskRequests},
	} {
		unknown = applyRequests(items, registry.byName, layer.label, layer.reqs, unknown)
	}

	if len(unknown) > 0 {
		sortStrings(unknown)
		return nil, fmt.Errorf("checks: unregistered check names referenced by pack/task: %s", strings.Join(unknown, ", "))
	}

	set := &Set{
		Items:            materialize(items),
		RegistryRevision: registry.Revision,
	}
	set.SetVersion = setVersion(set.Items, registry.Revision)
	return set, nil
}

// applyRequests folds one layer's requests into items. A request for a name not
// in byName is appended to unknown; otherwise the named item is created (or its
// sources extended) and the required flag is OR'd in (monotonic — a request can
// never clear an existing required flag). Returns the updated unknown list.
func applyRequests(items map[string]*Item, byName map[string]Definition, layer string, reqs []Request, unknown []string) []string {
	for _, req := range reqs {
		definition, ok := byName[req.Name]
		if !ok {
			unknown = appendUnique(unknown, req.Name)
			continue
		}
		item, exists := items[req.Name]
		if !exists {
			item = &Item{Definition: definition, Sources: []string{layer}}
			items[req.Name] = item
		} else {
			item.Sources = appendUnique(item.Sources, layer)
		}
		// Monotonic OR: a required request makes the check mandatory; a
		// request cannot undo an existing required flag.
		if req.Required {
			item.Required = true
		}
	}
	return unknown
}

// materialize turns the name→item map into a name-sorted slice so the effective
// set has a stable, reproducible order (the manifest version hash depends on it).
func materialize(items map[string]*Item) []Item {
	out := make([]Item, 0, len(items))
	for _, item := range items {
		out = append(out, *item)
	}
	for index := 1; index < len(out); index++ {
		for back := index; back > 0 && out[back-1].Definition.Name > out[back].Definition.Name; back-- {
			out[back-1], out[back] = out[back], out[back-1]
		}
	}
	return out
}

// setVersion hashes the resolved set (name | required | definition revision per
// item, plus the registry revision) so two runs hash equal only when their
// effective check contracts are identical.
func setVersion(items []Item, registryRevision string) string {
	type entry struct {
		Name     string `json:"name"`
		Required bool   `json:"required"`
		Revision string `json:"revision"`
	}
	entries := make([]entry, 0, len(items))
	for _, item := range items {
		entries = append(entries, entry{
			Name: item.Definition.Name, Required: item.Required,
			Revision: DefinitionRevision(item.Definition),
		})
	}
	encoded, err := json.Marshal(struct {
		Registry string  `json:"registry"`
		Items    []entry `json:"items"`
	}{Registry: registryRevision, Items: entries})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// appendUnique appends value when not already present. Returns a new slice; the
// input is not mutated.
func appendUnique(values []string, value string) []string {
	for _, present := range values {
		if present == value {
			return values
		}
	}
	return append(values, value)
}
