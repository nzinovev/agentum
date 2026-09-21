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
// stub fails the test if that ever changes.
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

// TestValidateModelTiers_DefaultTierDriftStopsTheProcess: the boot check
// covers the EFFECTIVE tiers — the adapter's baked-in defaults when there is
// no models.yaml. Defaults are fixed at build time while the runtime's
// catalog changes (upstream renames and removals have invalidated them
// before), and without this check a clean
// install would boot and then refuse every run at start.
func TestValidateModelTiers_DefaultTierDriftStopsTheProcess(t *testing.T) {
	instance := &Server{
		log: quietLogger(),
		adapter: &catalogStubAdapter{
			descriptor: catalogStubDescriptor(),
			// The catalog lists one unrelated model; every default tier
			// model is unknown to it.
			catalog: models.Catalog{Available: true, Models: []models.CatalogModel{{ID: "stub/somewhere-else"}}},
		},
		models: nil, // no models.yaml: effective tiers are the descriptor's defaults
	}
	err := instance.validateModelTiers(context.Background())
	if err == nil {
		t.Fatal("drifted default tiers must stop the process at boot")
	}
	message := err.Error()
	if !strings.Contains(message, `unknown model "stub/fast-model"`) {
		t.Errorf("error %q does not name the drifted default model", message)
	}
	if !strings.Contains(message, `tier "fast"`) {
		t.Errorf("error %q does not name the tier", message)
	}
}

// TestValidateModelTiers_DefaultsPresentBoots: with the defaults present in
// the catalog and no models.yaml, boot proceeds — the no-configuration case
// holds.
func TestValidateModelTiers_DefaultsPresentBoots(t *testing.T) {
	descriptor := catalogStubDescriptor()
	instance := &Server{
		log: quietLogger(),
		adapter: &catalogStubAdapter{
			descriptor: descriptor,
			catalog: models.Catalog{Available: true, Models: []models.CatalogModel{
				{ID: "stub/fast-model"},
			}},
		},
		models: nil,
	}
	if err := instance.validateModelTiers(context.Background()); err != nil {
		t.Fatalf("valid default tiers must boot: %v", err)
	}
}
