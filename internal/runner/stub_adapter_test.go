package runner

import (
	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/models"
)

// stubExecution is the embeddable half of the agent.Adapter contract every
// runner test fake shares: Describe (and, once the runtime probe lands, Probe).
// The fakes exist to exercise loop mechanics, not execution-target facts, so
// one declaration of those facts serves all of them — the same way the real
// adapter is the single source of its own identity.
type stubExecution struct{}

// Describe reports a neutral test execution target. The tiers mirror the shape
// packs in these tests use (fast/strong/reasoning) so run-start model
// resolution succeeds against the stub exactly as it would against a real
// descriptor.
func (stub stubExecution) Describe() agent.Descriptor {
	return agent.Descriptor{
		ID:             "stub",
		AdapterVersion: "0.0.0-test",
		Binary:         "stub-agent",
		ModelOptions:   []models.OptionName{models.OptionModel},
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
