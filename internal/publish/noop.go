package publish

import (
	"context"
)

// ProviderNoop is the no-op registry entry: it describes a real provider
// shape and refuses every publication with credentials_missing. Publication
// is wired end to end — row, gate, job, evidence — before any networked
// provider exists, and this entry is what keeps the registry non-empty and
// the coordinator exercised until then.
const ProviderNoop ProviderID = "noop"

// NoopPublisher is the implementation behind ProviderNoop.
type NoopPublisher struct{}

// NewNoopPublisher builds the no-op provider.
func NewNoopPublisher() NoopPublisher {
	return NoopPublisher{}
}

// ID returns ProviderNoop.
func (NoopPublisher) ID() ProviderID { return ProviderNoop }

// Describe returns the no-op provider's self-description. The version is
// bumped when the placeholder's observable behaviour changes.
func (NoopPublisher) Describe() Descriptor {
	return Descriptor{
		ID:              ProviderNoop,
		ProviderVersion: "1.0.0",
		APIBase:         "",
		Capabilities:    nil,
	}
}

// Probe answers ready: the placeholder holds no network to reach.
func (NoopPublisher) Probe(context.Context) (ProbeResult, error) {
	return ProbeResult{Ready: true}, nil
}

// Publish refuses every delivery: no credential can reach this provider.
func (NoopPublisher) Publish(context.Context, Delivery) (Result, error) {
	return Result{}, &Refusal{
		Code:    ReasonCredentialsMissing,
		Message: "no publication provider is configured in this build",
	}
}
