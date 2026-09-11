package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// catalogStubAdapter fakes the adapter surface validateModelTiers reads:
// identity from the descriptor, the model listing from a fixed value. Invoke
// is never reached — the validation runs before any worker starts — so its
// stub refuses loudly if that ever changes.
type catalogStubAdapter struct {
	descriptor agent.Descriptor
	catalog    models.Catalog
}

func (stub *catalogStubAdapter) Invoke(context.Context, agent.Invocation) (<-chan agent.Event, error) {
	return nil, errors.New("catalog validation must not invoke the adapter")
}
func (stub *catalogStubAdapter) Supported() []caps.Category { return nil }
func (stub *catalogStubAdapter) Describe() agent.Descriptor { return stub.descriptor }
func (stub *catalogStubAdapter) Probe(context.Context) agent.Readiness {
	return agent.Readiness{Ready: true}
}
func (stub *catalogStubAdapter) Catalog(context.Context) models.Catalog { return stub.catalog }

func catalogStubDescriptor() agent.Descriptor {
	return agent.Descriptor{
		ID:               "stub",
		AdapterVersion:   "0.0.0-test",
		ModelOptions:     []models.OptionName{models.OptionModel},
		EnumeratesModels: true,
		DefaultTiers: models.Config{
			Tiers:   map[string]string{"fast": "stub/fast-model"},
			Default: "fast",
		},
	}
}

// TestValidateModelTiers_BrokenTierStopsTheProcess: a tier naming a model the
// runtime does not list is a boot refusal that names the tier, the model, and
// the adapter — the fix is a one-line file edit, and this is where it is
// named, not four stages into the first run.
func TestValidateModelTiers_BrokenTierStopsTheProcess(t *testing.T) {
	instance := &Server{
		log: quietLogger(),
		adapter: &catalogStubAdapter{
			descriptor: catalogStubDescriptor(),
			// The catalog lists the descriptor's real model; the configured
			// tier names a typo of it.
			catalog: models.Catalog{Available: true, Models: []models.CatalogModel{{ID: "stub/fast-model"}}},
		},
		models: &models.Config{
			Tiers:   map[string]string{"fast": "stub/typo-model"},
			Default: "fast",
		},
	}
	err := instance.validateModelTiers(context.Background())
	if err == nil {
		t.Fatal("a tier on an unknown model must stop the process")
	}
	message := err.Error()
	for _, wanted := range []string{`tier "fast"`, `unknown model "stub/typo-model"`, `"stub"`} {
		if !strings.Contains(message, wanted) {
			t.Errorf("error %q does not name %q", message, wanted)
		}
	}
}

// TestValidateModelTiers_UnavailableCatalogBoots: a catalog that could not be
// obtained is a recorded fact, not a boot failure — the process starts and
// runs proceed with models unchecked.
func TestValidateModelTiers_UnavailableCatalogBoots(t *testing.T) {
	instance := &Server{
		log: quietLogger(),
		adapter: &catalogStubAdapter{
			descriptor: catalogStubDescriptor(),
			catalog:    models.Catalog{Reason: "timeout"},
		},
		models: &models.Config{
			Tiers:   map[string]string{"fast": "stub/any-model-at-all"},
			Default: "fast",
		},
	}
	if err := instance.validateModelTiers(context.Background()); err != nil {
		t.Fatalf("an unavailable catalog must not stop the process: %v", err)
	}
}

// TestValidateModelTiers_NonEnumeratingAdapterIsNeverAsked: an adapter that
// cannot list its runtime's models is skipped entirely, whatever the
// configuration says.
func TestValidateModelTiers_NonEnumeratingAdapterIsNeverAsked(t *testing.T) {
	descriptor := catalogStubDescriptor()
	descriptor.EnumeratesModels = false
	instance := &Server{
		log:     quietLogger(),
		adapter: &catalogStubAdapter{descriptor: descriptor},
		models: &models.Config{
			Tiers:   map[string]string{"fast": "stub/fast-model"},
			Default: "fast",
		},
	}
	if err := instance.validateModelTiers(context.Background()); err != nil {
		t.Fatalf("a non-enumerating adapter must not be checked: %v", err)
	}
}
