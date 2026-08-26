package agent

import (
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/models"
)

func TestRegistry_ResolvesKnownAndEmptyIDToSameEntry(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(RegistryOptions{})

	named, err := registry.Resolve(AdapterOpencode)
	if err != nil {
		t.Fatalf("Resolve(opencode): %v", err)
	}
	unnamed, err := registry.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(''): %v", err)
	}
	if named.Describe().ID != unnamed.Describe().ID {
		t.Errorf("empty id resolved to %q; want the same entry as %q", unnamed.Describe().ID, named.Describe().ID)
	}
}

func TestRegistry_UnknownIDErrorsAndNamesIt(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(RegistryOptions{})
	if _, err := registry.Resolve("claude-code"); err == nil {
		t.Fatal("unknown id must error")
	} else {
		if !strings.Contains(err.Error(), "claude-code") {
			t.Errorf("error should name the unknown id: %v", err)
		}
		if !strings.Contains(err.Error(), string(AdapterOpencode)) {
			t.Errorf("error should list the known ids: %v", err)
		}
	}
}

func TestRegistry_IDsReturnsExactlyOneEntry(t *testing.T) {
	t.Parallel()
	ids := NewRegistry(RegistryOptions{}).IDs()
	if len(ids) != 1 || ids[0] != AdapterOpencode {
		t.Errorf("IDs() = %v; want exactly [opencode]", ids)
	}
}

func TestRegistry_RuntimeBinaryOverridesDescriptorDefault(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(RegistryOptions{RuntimeBinary: "/opt/wrapped-opencode"})
	adapter, err := registry.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	opencode, ok := adapter.(*OpencodeAdapter)
	if !ok {
		t.Fatalf("Resolve returned %T; want *OpencodeAdapter", adapter)
	}
	if opencode.binary != "/opt/wrapped-opencode" {
		t.Errorf("binary = %q; want the operator override", opencode.binary)
	}

	defaultRegistry := NewRegistry(RegistryOptions{})
	defaultAdapter, err := defaultRegistry.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := defaultAdapter.(*OpencodeAdapter).binary; got != opencodeDescriptor.Binary {
		t.Errorf("binary = %q; want the descriptor default %q", got, opencodeDescriptor.Binary)
	}
}

func TestDescribe_IDMatchesTheConstant(t *testing.T) {
	t.Parallel()
	descriptor := NewOpencodeAdapter("opencode").Describe()
	if descriptor.ID != AdapterOpencode {
		t.Errorf("Describe().ID = %q; want %q", descriptor.ID, AdapterOpencode)
	}
	if descriptor.AdapterVersion == "" || strings.Contains(descriptor.AdapterVersion, "opencode") {
		t.Errorf("AdapterVersion = %q; want a bare version, not a name-shaped string", descriptor.AdapterVersion)
	}
	if descriptor.DefaultTiers.Tiers["strong"] == "" || descriptor.DefaultTiers.Default == "" {
		t.Errorf("DefaultTiers must carry the tier map and a default: %+v", descriptor.DefaultTiers)
	}
}

func TestDescribe_ReturnedTiersAreACopy(t *testing.T) {
	t.Parallel()
	descriptor := NewOpencodeAdapter("opencode").Describe()
	descriptor.DefaultTiers.Tiers["strong"] = "mutated"
	again := NewOpencodeAdapter("opencode").Describe()
	if again.DefaultTiers.Tiers["strong"] == "mutated" {
		t.Error("Describe must return a fresh tiers map; mutating the result leaked into the descriptor")
	}
}

// TestOpencodeDescriptor_DeclaredOptionsMatchArgv is the assertion that keeps
// D3's declaration honest as MVP task 6 lands: the descriptor's declared model
// options must equal the set of option fields buildOpencodeArgs can actually
// emit. A descriptor that declares an option the argv builder ignores would
// silently accept and drop it; an argv builder that emits an undeclared option
// would bypass the SupportedBy refusal. Both are bugs, and this test is where
// they surface.
func TestOpencodeDescriptor_DeclaredOptionsMatchArgv(t *testing.T) {
	t.Parallel()
	descriptor := NewOpencodeAdapter("opencode").Describe()

	// Enumerate what buildOpencodeArgs emits, by selection shape.
	emits := map[models.OptionName]bool{}
	baseline := buildOpencodeArgs("opencode", Invocation{})
	if positionOf(baseline, "--model") >= 0 {
		emits[models.OptionModel] = true
	}
	withModel := buildOpencodeArgs("opencode", Invocation{
		Model: models.Selection{Options: models.Options{Model: "provider/model"}},
	})
	if positionOf(withModel, "--model") < 0 {
		t.Fatal("a populated model option must emit --model")
	}
	emits[models.OptionModel] = true

	declared := map[models.OptionName]bool{}
	for _, name := range descriptor.ModelOptions {
		declared[name] = true
	}
	if len(emits) != len(declared) {
		t.Errorf("declared options %v do not match emitted %v", descriptor.ModelOptions, emits)
	}
	for name := range emits {
		if !declared[name] {
			t.Errorf("buildOpencodeArgs emits option %q the descriptor does not declare", name)
		}
	}
	for name := range declared {
		if !emits[name] {
			t.Errorf("descriptor declares option %q buildOpencodeArgs never emits", name)
		}
	}
}

// positionOf finds a flag's index in argv, or -1.
func positionOf(argv []string, flag string) int {
	for index, arg := range argv {
		if arg == flag {
			return index
		}
	}
	return -1
}
