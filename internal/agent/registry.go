package agent

import (
	"fmt"

	"github.com/nzinovev/agentum/internal/models"
)

// AdapterID is the stable identity of an execution adapter. It is declared
// exactly once, next to the implementation it names, and after ADR 0005 the
// executor is named ONLY inside this package: callers select an adapter by id
// through the registry and never construct a concrete type by name.
type AdapterID string

// AdapterOpencode is the opencode CLI adapter — the single registry entry this
// build ships. Adding the second adapter is a row here, not a branch in a
// caller.
const AdapterOpencode AdapterID = "opencode"

// Descriptor is an execution adapter's self-description: everything knowable
// about it without running it. It comes from the adapter itself (Describe)
// rather than sitting beside it in the registry table, so there is one record
// of the adapter's identity, not two that can drift.
type Descriptor struct {
	// ID is the adapter's stable identity (AdapterOpencode for this one).
	ID AdapterID
	// AdapterVersion versions THIS implementation of the adapter — bumped when
	// its observable behaviour changes (argv shape, permission config
	// rendering, environment scrubbing). It is not the external runtime's
	// version; that is probed at runtime (Probe).
	AdapterVersion string
	// Binary is the default executable name; the operator may override it via
	// AGENTUM_RUNTIME_BINARY, and the registry applies the override (or this
	// default) when it constructs the adapter.
	Binary string
	// ModelOptions is the set of model parameters this adapter understands.
	// A selection carrying an option outside this set is refused at Invoke —
	// never silently dropped (models.Options.SupportedBy).
	ModelOptions []models.OptionName
	// DefaultTiers is the adapter's baked-in tier→model map, used when the
	// operator has no models.yaml. "These model names work with this runtime"
	// is runtime knowledge, so it lives here (ADR 0005 D5).
	DefaultTiers models.Config
}

// clone returns a deep copy so a caller cannot mutate the adapter's declared
// defaults through the value Describe handed it. The tiers map is the only
// mutable part.
func (descriptor Descriptor) clone() Descriptor {
	out := descriptor
	out.ModelOptions = append([]models.OptionName(nil), descriptor.ModelOptions...)
	out.DefaultTiers.Tiers = make(map[string]string, len(descriptor.DefaultTiers.Tiers))
	for tier, model := range descriptor.DefaultTiers.Tiers {
		out.DefaultTiers.Tiers[tier] = model
	}
	return out
}

// opencodeDescriptor is the opencode adapter's self-description. The default
// tiers use the free models on opencode Zen (the `-free` suffix is explicit)
// so a fresh install works without a paid provider once Zen is connected.
var opencodeDescriptor = Descriptor{
	ID:             AdapterOpencode,
	AdapterVersion: "1.0.0",
	Binary:         "opencode",
	ModelOptions:   []models.OptionName{models.OptionModel},
	DefaultTiers: models.Config{
		Tiers: map[string]string{
			"fast":      "opencode/deepseek-v4-flash-free",
			"strong":    "opencode/north-mini-code-free",
			"reasoning": "opencode/nemotron-3-ultra-free",
		},
		Default: "strong",
	},
}

// RegistryOptions configures registry construction. Both fields come from
// adapter-neutral configuration: an id (empty selects the default entry) and a
// runtime binary override (empty selects each descriptor's Binary).
type RegistryOptions struct {
	// DefaultAdapter is the entry an empty id resolves to. Empty means the
	// first registered entry.
	DefaultAdapter AdapterID
	// RuntimeBinary overrides every descriptor's default binary. Empty keeps
	// the descriptor's Binary.
	RuntimeBinary string
}

// Registry is the set of execution adapters this build can run. Data, not a
// switch in the caller: the table holds the minimum that must exist before
// construction — an id and a constructor — and everything else is asked of the
// constructed adapter via Describe.
type Registry struct {
	constructors  map[AdapterID]func(binary string) Adapter
	orderedIDs    []AdapterID
	defaultID     AdapterID
	runtimeBinary string
}

// NewRegistry builds the registry with every adapter this compile ships. In the
// MVP it holds exactly one entry — that is the point of it being a table.
func NewRegistry(options RegistryOptions) *Registry {
	constructors := map[AdapterID]func(binary string) Adapter{
		AdapterOpencode: func(binary string) Adapter { return NewOpencodeAdapter(binary) },
	}
	orderedIDs := []AdapterID{AdapterOpencode}
	defaultID := options.DefaultAdapter
	if defaultID == "" {
		defaultID = orderedIDs[0]
	}
	return &Registry{
		constructors:  constructors,
		orderedIDs:    orderedIDs,
		defaultID:     defaultID,
		runtimeBinary: options.RuntimeBinary,
	}
}

// Resolve returns the adapter registered under id. An empty id resolves to the
// registry's default entry, so "no configuration" and "the default
// configuration" are one code path. An unknown id is an error naming it — the
// executor is never silently substituted.
func (registry *Registry) Resolve(id AdapterID) (Adapter, error) {
	resolvedID := id
	if resolvedID == "" {
		resolvedID = registry.defaultID
	}
	constructor, found := registry.constructors[resolvedID]
	if !found {
		return nil, fmt.Errorf("agent: unknown execution adapter %q (known: %s)", id, joinAdapterIDs(registry.IDs()))
	}
	// The operator's binary override beats the descriptor's default. The
	// default itself is asked of a throwaway construction — construction is a
	// struct literal, and Describe is the one record of the adapter's identity.
	binary := registry.runtimeBinary
	if binary == "" {
		binary = constructor("").Describe().Binary
	}
	return constructor(binary), nil
}

// IDs returns the registered adapter ids in registration order. A future
// readiness surface enumerates adapters through this plus Resolve, without
// special cases per adapter.
func (registry *Registry) IDs() []AdapterID {
	return append([]AdapterID(nil), registry.orderedIDs...)
}

// joinAdapterIDs renders ids as a comma-separated list for error messages.
func joinAdapterIDs(ids []AdapterID) string {
	rendered := ""
	for index, id := range ids {
		if index > 0 {
			rendered += ", "
		}
		rendered += string(id)
	}
	return rendered
}

// Describe returns the opencode adapter's self-description. The capability
// categories come from Supported() — that method is referenced by name
// throughout docs/capabilities.md and by the runner's profile computation, and
// folding it into the descriptor would churn the capability model for no gain.
func (adapter *OpencodeAdapter) Describe() Descriptor {
	return opencodeDescriptor.clone()
}
