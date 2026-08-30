package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
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
// the descriptor's declaration honest as MVP task 6 lands: the declared model
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

// TestRegistry_ResolveReturnsOneInstancePerID: the readiness probe memoizes on
// the adapter value, so "one subprocess per process" holds only while one
// instance exists per id. Constructing per Resolve made the invariant depend
// on nobody resolving twice.
func TestRegistry_ResolveReturnsOneInstancePerID(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(RegistryOptions{})
	first, err := registry.Resolve(AdapterOpencode)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := registry.Resolve("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if first != second {
		t.Error("Resolve must hand out one instance per id, or the probe memoizes per caller")
	}
}

// TestNoExecutorNameOutsideThisPackage is the acceptance criterion "the
// adapter's name and version do not appear as literals in calling code" made
// enforceable. It parses each non-test Go file OUTSIDE internal/agent with
// comments discarded and fails on any string literal naming the executor:
// prose describing the seam is allowed and expected, a literal in code is the
// coupling the adapter seam removed.
//
// The adapter VERSION is not scanned: a bare semver is too generic to look for
// without false positives. It has one declaration by construction —
// Descriptor.AdapterVersion is set only in opencodeDescriptor — and this scan
// is what stops a copy of it from reaching calling code under the name.
func TestNoExecutorNameOutsideThisPackage(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join("..", "..")
	thisPackage := filepath.Join(repoRoot, "internal", "agent")
	executor := strings.ToLower(string(AdapterOpencode))

	walkErr := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Vendored, generated and non-Go trees carry no calling code.
			switch entry.Name() {
			case ".git", "tmp", "packs", "docs", "node_modules":
				return fs.SkipDir
			}
			if path == thisPackage {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		// Mode 0: comments are not attached, so prose about the seam is out of
		// scope and only real string literals are examined.
		parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, isLiteral := node.(*ast.BasicLit)
			if !isLiteral || literal.Kind != token.STRING {
				return true
			}
			if strings.Contains(strings.ToLower(literal.Value), executor) && !allowedExecutorLiteral(literal.Value) {
				t.Errorf("%s:%d: string literal %s names the executor; select it through the registry instead",
					path, fileSet.Position(literal.Pos()).Line, literal.Value)
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}

// allowedExecutorLiteral lists the string literals outside internal/agent that
// may name the executor. There is exactly one, and it names a RETIRED setting
// rather than selecting anything: config.Load recognises the retired
// AGENTUM_OPENCODE_BINARY only to refuse it by name, so the operator learns
// where the override moved. Removing the literal would remove the refusal and
// bring back the silent-ignore the refusal exists to prevent. Any addition is
// a decision, not a formality.
func allowedExecutorLiteral(literal string) bool {
	return literal == `"AGENTUM_OPENCODE_BINARY"`
}
