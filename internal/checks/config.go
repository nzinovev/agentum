// Package checks owns the orchestrator-enforced project check mechanism.
//
// A project ships a versioned registry of named checks (a `.agentum.yaml` file
// tracked in the repo). Each named check carries its execution contract: an
// argument-vector command (no shell), a worktree-relative working directory, a
// timeout, and an output cap. Packs and per-task input may add checks to the
// effective set *by name only*; they can never supply a command, never remove a
// mandatory (baseline) check, and never weaken the mandatory set — `Resolve`
// enforces all three. The Executor then runs the resolved set under a fixed
// minimal boundary (scrubbed environment, no provider credentials) and the
// runner blocks delivery on any mandatory failure.
//
// This is the dogfooding seam: Agentum reads its own build/test result from its
// own executor, never from an agent's claim that "tests passed."
package checks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the api value the project config declares. An empty api is
// treated as v1 to ease adoption; a non-empty api must match exactly so a
// future incompatible format is detected rather than silently misread.
const APIVersion = "agentum/v1"

// ConfigFile is the versioned project configuration file the checks registry
// lives in. It is read from the worktree root so the registry reflects the
// task's commit. It is intentionally distinct from the worktree-local `.agentum/`
// directory (gitignored runtime state): this file is tracked source the project
// owns, the registry is versioned with the code, and pack/agent input can never
// override what it declares.
const ConfigFile = ".agentum.yaml"

// Definition is one named check in the project registry. The command is an
// argument vector (first element is the binary, the rest its args) so neither
// pack nor agent input can inject shell operators — there is no shell. Only the
// project's versioned config supplies commands; pack and task inputs reference
// checks by name only.
//
// Required marks a project-baseline mandatory check: it is always included in
// the effective set and a failure blocks delivery. Mandatory is monotonic across
// layers — see Resolve.
type Definition struct {
	Name           string   `yaml:"name" json:"name"`
	Command        []string `yaml:"command" json:"command"`
	Workdir        string   `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	TimeoutSeconds int      `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	MaxOutputBytes int      `yaml:"max_output_bytes,omitempty" json:"max_output_bytes,omitempty"`
	Required       bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Description    string   `yaml:"description,omitempty" json:"description,omitempty"`
}

// ProjectConfig is the parsed `.agentum.yaml`. Only the checks registry is
// defined today; the shape is a top-level struct so future project-owned config
// lands here without reshaping the file.
type ProjectConfig struct {
	API    string       `yaml:"api"`
	Checks []Definition `yaml:"checks"`
}

// Registry is the validated, name-indexed set of check definitions the project
// owns. Revision is a content hash of the canonical definition set so two runs
// are distinguishable when the registry changed between them (the manifest
// records it as audit evidence).
type Registry struct {
	api      string
	items    []Definition
	byName   map[string]Definition
	Revision string
}

// Definitions returns the registry's definitions in declaration order. Callers
// must not mutate the slice; it shares storage with the registry.
func (registry *Registry) Definitions() []Definition {
	if registry == nil {
		return nil
	}
	return registry.items
}

// Get returns the definition for name and whether it exists.
func (registry *Registry) Get(name string) (Definition, bool) {
	if registry == nil {
		return Definition{}, false
	}
	def, ok := registry.byName[name]
	return def, ok
}

// Names returns the registry's check names sorted for stable output.
func (registry *Registry) Names() []string {
	if registry == nil {
		return nil
	}
	names := make([]string, 0, len(registry.byName))
	for name := range registry.byName {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// LoadFromRepo reads the config from repoRoot/ConfigFile. A missing file is a
// valid empty registry: it returns (nil, nil) — the project simply defines no
// checks, which passes delivery. Any other read or parse error is returned.
func LoadFromRepo(repoRoot string) (*Registry, error) {
	file := filepath.Join(repoRoot, ConfigFile)
	raw, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("checks: read %s: %w", ConfigFile, err)
	}
	return Parse(raw)
}

// Parse validates and indexes a config blob. Used by LoadFromRepo and tests.
func Parse(raw []byte) (*Registry, error) {
	var cfg ProjectConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("checks: parse %s: %w", ConfigFile, err)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	byName := make(map[string]Definition, len(cfg.Checks))
	for _, definition := range cfg.Checks {
		byName[definition.Name] = definition
	}
	return &Registry{
		api:      cfg.API,
		items:    cfg.Checks,
		byName:   byName,
		Revision: registryRevision(cfg.Checks),
	}, nil
}

// validateConfig checks the project config contract and returns every problem
// at once so an author sees the full list. Rules:
//   - api is empty (defaults to v1) or exactly APIVersion.
//   - check names are non-empty and unique.
//   - each command is a non-empty arg vector with a non-empty binary.
//   - workdir, when set, does not escape the repo root.
//   - timeout / output caps are non-negative.
func validateConfig(cfg ProjectConfig) error {
	var problems []string
	if cfg.API != "" && cfg.API != APIVersion {
		problems = append(problems, fmt.Sprintf("api must be %q when set, got %q", APIVersion, cfg.API))
	}
	seen := make(map[string]bool, len(cfg.Checks))
	for index, definition := range cfg.Checks {
		problems = append(problems, validateDefinition(definition, index, seen)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("checks: invalid %s: %s", ConfigFile, strings.Join(problems, "; "))
	}
	return nil
}

// validateDefinition checks one check definition and records its name in seen so
// duplicates surface. An empty name short-circuits the rest (there is nothing to
// cross-reference a nameless entry against).
func validateDefinition(definition Definition, index int, seen map[string]bool) []string {
	if strings.TrimSpace(definition.Name) == "" {
		return []string{fmt.Sprintf("checks[%d].name is empty", index)}
	}
	var problems []string
	if seen[definition.Name] {
		problems = append(problems, fmt.Sprintf("checks[%d].name %q is duplicated", index, definition.Name))
	}
	seen[definition.Name] = true
	if len(definition.Command) == 0 || strings.TrimSpace(definition.Command[0]) == "" {
		problems = append(problems, fmt.Sprintf("check %q command must be a non-empty arg vector", definition.Name))
	}
	if definition.Workdir != "" && pathEscapes(definition.Workdir) {
		problems = append(problems, fmt.Sprintf("check %q workdir %q escapes the repo root", definition.Name, definition.Workdir))
	}
	if definition.TimeoutSeconds < 0 {
		problems = append(problems, fmt.Sprintf("check %q timeout_seconds must be non-negative", definition.Name))
	}
	if definition.MaxOutputBytes < 0 {
		problems = append(problems, fmt.Sprintf("check %q max_output_bytes must be non-negative", definition.Name))
	}
	return problems
}

// pathEscapes reports whether rel, when joined under the repo root, would leave
// it. Mirrors the pack loader's safeJoin discipline: an absolute path or a
// ".."-leading path escapes.
func pathEscapes(rel string) bool {
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) {
		return true
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return true
	}
	return false
}

// DefinitionRevision returns the sha256 hex of the canonical JSON encoding of
// definition. Two definitions with identical contracts hash equal regardless of
// YAML formatting, so a run is distinguishable from another only when the
// execution contract actually changed.
func DefinitionRevision(definition Definition) string {
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// registryRevision hashes the canonical, name-sorted definition set so the
// registry revision is stable across equivalent edits and changes only when a
// definition's contract changes.
func registryRevision(definitions []Definition) string {
	defs := make([]Definition, len(definitions))
	copy(defs, definitions)
	sortByNamed(defs)
	encoded, err := json.Marshal(struct {
		API    string       `json:"api"`
		Checks []Definition `json:"checks"`
	}{Checks: defs})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// sortStrings sorts in place; kept local so the package has no sort import
// scattered across files (the resolve file also needs ordering).
func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for back := index; back > 0 && values[back-1] > values[back]; back-- {
			values[back-1], values[back] = values[back], values[back-1]
		}
	}
}

// sortByNamed sorts definitions by Name in place.
func sortByNamed(definitions []Definition) {
	for index := 1; index < len(definitions); index++ {
		for back := index; back > 0 && definitions[back-1].Name > definitions[back].Name; back-- {
			definitions[back-1], definitions[back] = definitions[back], definitions[back-1]
		}
	}
}
