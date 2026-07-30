package checks

import (
	"strings"
	"testing"
)

func sampleRegistry() *Registry {
	registry, err := Parse([]byte(`
api: agentum/v1
checks:
  - name: build
    command: ["go", "build", "./..."]
    required: true
  - name: test
    command: ["go", "test", "./..."]
    required: true
  - name: lint
    command: ["golangci-lint", "run"]
  - name: security
    command: ["gosec", "./..."]
`))
	if err != nil {
		panic(err)
	}
	return registry
}

func TestResolveBaselineAlwaysIncluded(t *testing.T) {
	registry := sampleRegistry()
	// No pack or task requests: only the baseline (required) checks run.
	set, err := Resolve(registry, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := setItemNames(set)
	if len(names) != 2 || !contains(names, "build") || !contains(names, "test") {
		t.Fatalf("baseline must include build+test, got %v", names)
	}
	for _, item := range set.Items {
		if !item.Required {
			t.Errorf("baseline check %q must be required", item.Definition.Name)
		}
		if item.Sources[0] != LayerBaseline {
			t.Errorf("baseline check %q source must be baseline", item.Definition.Name)
		}
	}
}

func TestResolvePackAddsByName(t *testing.T) {
	registry := sampleRegistry()
	set, err := Resolve(registry, []Request{{Name: "lint", Required: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := setItemNames(set)
	// baseline (build, test) + pack lint.
	if len(names) != 3 || !contains(names, "lint") {
		t.Fatalf("expected build+test+lint, got %v", names)
	}
	lint := setItem(set, "lint")
	if !lint.Required {
		t.Error("pack required request must make lint required")
	}
	if !contains(lint.Sources, LayerPack) {
		t.Error("lint source must include pack")
	}
}

func TestResolveOptionalDoesNotBlock(t *testing.T) {
	registry := sampleRegistry()
	set, err := Resolve(registry, []Request{{Name: "lint", Required: false}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lint := setItem(set, "lint")
	if lint.Required {
		t.Error("pack optional request must not make lint required")
	}
}

func TestResolveCannotWeakenBaseline(t *testing.T) {
	registry := sampleRegistry()
	// build is baseline-required. A pack optional request must NOT downgrade it.
	set, err := Resolve(registry, []Request{{Name: "build", Required: false}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	build := setItem(set, "build")
	if !build.Required {
		t.Fatal("a pack optional request must not weaken a baseline-required check")
	}
	if !contains(build.Sources, LayerBaseline) {
		t.Error("build source must still include baseline")
	}
}

func TestResolveRequiredIsMonotonicAcrossLayers(t *testing.T) {
	registry := sampleRegistry()
	// lint is optional in the registry. Pack optional + task required → required.
	set, err := Resolve(registry,
		[]Request{{Name: "lint", Required: false}},
		[]Request{{Name: "lint", Required: true}},
	)
	if err != nil {
		t.Fatal(err)
	}
	lint := setItem(set, "lint")
	if !lint.Required {
		t.Fatal("task required must make lint mandatory even if pack listed it optional")
	}
	if !contains(lint.Sources, LayerPack) || !contains(lint.Sources, LayerTask) {
		t.Errorf("lint sources must include pack and task, got %v", lint.Sources)
	}
}

func TestResolveUnknownNamesRejectedBeforeExecution(t *testing.T) {
	registry := sampleRegistry()
	_, err := Resolve(registry,
		[]Request{{Name: "nope"}},
		[]Request{{Name: "also-missing"}},
	)
	if err == nil {
		t.Fatal("unknown names must be rejected")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "also-missing") {
		t.Fatalf("error must list both unknown names, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unregistered") {
		t.Fatalf("error must explain the cause, got %q", err.Error())
	}
}

func TestResolveEmptyRegistryPasses(t *testing.T) {
	set, err := Resolve(nil, []Request{{Name: "anything"}}, nil)
	if err != nil {
		t.Fatalf("nil registry must not error: %v", err)
	}
	if !set.Empty() {
		t.Fatal("nil registry must yield an empty set")
	}
}

func TestResolveSetVersionStableAndSensitive(t *testing.T) {
	registry := sampleRegistry()
	base, err := Resolve(registry, []Request{{Name: "lint"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Same inputs → same set version.
	again, err := Resolve(registry, []Request{{Name: "lint"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.SetVersion != again.SetVersion {
		t.Fatal("set version must be stable for identical inputs")
	}
	// Different required flag → different set version (weaker/stronger set).
	stronger, err := Resolve(registry, []Request{{Name: "lint", Required: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base.SetVersion == stronger.SetVersion {
		t.Fatal("set version must change when a check's required flag changes")
	}
	// Registry revision carries through.
	if base.RegistryRevision != registry.Revision {
		t.Fatal("set must carry the registry revision")
	}
}

func setItemNames(set *Set) []string {
	out := make([]string, 0, len(set.Items))
	for _, item := range set.Items {
		out = append(out, item.Definition.Name)
	}
	return out
}

func setItem(set *Set, name string) Item {
	for _, item := range set.Items {
		if item.Definition.Name == name {
			return item
		}
	}
	return Item{}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
