package runner

import (
	"context"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/models"
)

// stubExecution is the embeddable half of the agent.Adapter contract every
// runner test fake shares: Describe, Probe, and Catalog. The fakes exist to
// exercise loop mechanics, not execution-target facts, so one declaration of
// those facts serves all of them — the same way the real adapter is the single
// source of its own identity.
type stubExecution struct {
	// enumeratesModels / catalog let a test fake the runtime's model listing:
	// the descriptor's declaration and the memoized answer a real adapter
	// would probe. Zero values declare "cannot be asked", which the run-start
	// validation reads as "check skipped", never as a refusal.
	enumeratesModels bool
	catalog          models.Catalog
}

// Describe reports a neutral test execution target. The tiers mirror the shape
// packs in these tests use (fast/strong/reasoning) so run-start model
// resolution succeeds against the stub exactly as it would against a real
// descriptor.
func (stub stubExecution) Describe() agent.Descriptor {
	return agent.Descriptor{
		ID:               "stub",
		AdapterVersion:   "0.0.0-test",
		Binary:           "stub-agent",
		ModelOptions:     []models.OptionName{models.OptionModel},
		EnumeratesModels: stub.enumeratesModels,
		DefaultTiers: models.Config{
			Tiers: map[string]string{
				"fast":      "stub/fast-model",
				"strong":    "stub/strong-model",
				"reasoning": "stub/reasoning-model",
			},
			Default: "fast",
		},
	}
}

// Probe reports a ready stub runtime with a fixed version, so per-invocation
// evidence records a runtime version without any subprocess in loop tests.
func (stub stubExecution) Probe(ctx context.Context) agent.Readiness {
	return agent.Readiness{
		AdapterID:      "stub",
		Ready:          true,
		RuntimeVersion: "1.0.0-stub",
		CheckedAt:      time.Now().UTC(),
	}
}

// Catalog reports the faked listing. The zero value is an unavailable catalog,
// which validates as "not checked" — loop tests keep running exactly as they
// did before the catalog existed.
func (stub stubExecution) Catalog(ctx context.Context) models.Catalog {
	return stub.catalog
}
