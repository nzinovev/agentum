package publish

import (
	"fmt"
)

// RegistryOptions configures registry construction. The credential reaches
// providers at construction — the one place it is handed out — and never
// travels with a Delivery.
type RegistryOptions struct {
	// DefaultProvider is the entry an empty id resolves to. Empty means the
	// first registered entry.
	DefaultProvider ProviderID
	// Credential is the provider access secret, resolved by the provider
	// only.
	Credential Credential
}

// Registry is the set of publication providers this build can run. Data, not
// a switch in the caller: the table holds the minimum that must exist before
// construction — an id and a constructor — and everything else is asked of
// the constructed provider via Describe.
type Registry struct {
	publishers map[ProviderID]Publisher
	orderedIDs []ProviderID
	defaultID  ProviderID
}

// NewRegistry builds the registry with every provider this compile ships. The
// table currently holds one entry that refuses every publication with
// credentials_missing; the networked provider is a row here when it lands,
// not a branch in a caller.
func NewRegistry(options RegistryOptions) *Registry {
	noopPublisher := NewNoopPublisher()
	publishers := map[ProviderID]Publisher{
		noopPublisher.ID(): noopPublisher,
	}
	orderedIDs := []ProviderID{noopPublisher.ID()}
	defaultID := options.DefaultProvider
	if defaultID == "" {
		defaultID = orderedIDs[0]
	}
	return &Registry{
		publishers: publishers,
		orderedIDs: orderedIDs,
		defaultID:  defaultID,
	}
}

// Resolve returns the provider registered under id. An empty id resolves to
// the registry's default entry, so "no configuration" and "the default
// configuration" are one code path. An unknown id is an error naming it and
// the known ids — the provider is never substituted.
func (registry *Registry) Resolve(id ProviderID) (Publisher, error) {
	resolvedID := id
	if resolvedID == "" {
		resolvedID = registry.defaultID
	}
	publisher, found := registry.publishers[resolvedID]
	if !found {
		// Name the id that failed to resolve — for an empty id that is the
		// configured default, which is the id the operator must fix.
		return nil, fmt.Errorf("publish: unknown publication provider %q (known: %s)", resolvedID, joinProviderIDs(registry.IDs()))
	}
	return publisher, nil
}

// IDs returns the registered provider ids in registration order. A readiness
// surface enumerates providers through this plus Resolve, without a special
// case per provider.
func (registry *Registry) IDs() []ProviderID {
	return append([]ProviderID(nil), registry.orderedIDs...)
}

// joinProviderIDs renders ids as a comma-separated list for error messages.
func joinProviderIDs(ids []ProviderID) string {
	rendered := ""
	for index, id := range ids {
		if index > 0 {
			rendered += ", "
		}
		rendered += string(id)
	}
	return rendered
}
