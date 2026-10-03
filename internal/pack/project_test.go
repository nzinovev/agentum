package pack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeTreeFile is one in-memory commit-tree entry: mode plus content.
type fakeTreeFile struct {
	mode    string
	content []byte
}

// fakeCommitTree serves an in-memory commit tree. It satisfies CommitTree so
// the project source is tested without a git repository; mode and size come
// from the fake exactly as the real listing would report them.
type fakeCommitTree struct {
	files map[string]fakeTreeFile
}

func (tree fakeCommitTree) FileAtCommit(_ context.Context, _, _, path string) ([]byte, error) {
	file, ok := tree.files[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, path)
	}
	return file.content, nil
}

func (tree fakeCommitTree) ListTreeAtCommit(_ context.Context, _, _, dir string) ([]TreeEntry, error) {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	entries := make([]TreeEntry, 0, len(tree.files))
	for path, file := range tree.files {
		if strings.HasPrefix(path, prefix) {
			entries = append(entries, TreeEntry{Path: path, Mode: file.mode, Size: int64(len(file.content))})
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Path < entries[right].Path })
	return entries, nil
}

// basePackManifest is a known-good builtin manifest with the given name: a
// plan gate with a source_write approval, implement and fix source-writing
// stages, and a verdict-conditional review loop — the backend-development
// shape the inheritance tests layer onto.
func basePackManifest(name string) string {
	return fmt.Sprintf(`api: agentum/v1
pack:
  name: %s
  version: 1.2.0
  persona: engineering
memory: {reads: [project], writes: false}
capabilities: [fs.read, fs.write, git.read, exec.bash]
budgets: {fix_cycles: 2, ask_to_edit: 3}
tiers: {default: fast}
checks: {}
entry: plan
approvals:
  - {name: plan, stage: plan, artifact: plan.md, unlocks: source_write}
stages:
  plan:
    gate: human_approval
    role: analyst
    prompt: prompts/plan.md
    transitions:
      - to: implement
  implement:
    gate: auto
    role: implementer
    prompt: prompts/implement.md
    transitions:
      - to: review
  review:
    gate: auto
    role: reviewer
    prompt: prompts/review.md
    transitions:
      - {to: fix, condition: 'verdict == "changes_requested"'}
      - {to: done, condition: 'verdict == "approved"'}
  fix:
    gate: auto
    role: fixer
    prompt: prompts/fix.md
    transitions:
      - to: review
  done: {}
`, name)
}

func basePackPrompts() map[string]string {
	return map[string]string{
		"prompts/plan.md":      "base plan prompt",
		"prompts/implement.md": "base implement prompt",
		"prompts/review.md":    "base review prompt",
		"prompts/fix.md":       "base fix prompt",
	}
}

// builtinSourceWithBasePack builds a DirSource whose root carries one builtin
// pack named "base-pack" at version 1.2.0.
func builtinSourceWithBasePack(t *testing.T) *DirSource {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "base-pack")
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(basePackManifest("base-pack")), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, body := range basePackPrompts() {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return NewDirSource(root)
}

// projectFiles renders a project pack's files into the fake tree under
// .agentum/packs/<name>/, with regular-blob modes.
func projectFiles(name string, files map[string]string) map[string]string {
	out := make(map[string]string, len(files))
	for rel, body := range files {
		out[ProjectPacksDir+"/"+name+"/"+rel] = body
	}
	return out
}

// fakeTreeFromProject renders project pack files (path -> body) into a fake
// commit tree.
func fakeTreeFromProject(files map[string]string) fakeCommitTree {
	tree := fakeCommitTree{files: map[string]fakeTreeFile{}}
	for path, body := range files {
		tree.files[path] = fakeTreeFile{mode: "100644", content: []byte(body)}
	}
	return tree
}

const repoPath = "/project/checkout"
const baseCommit = "commit-a"

func TestProjectSource_BuiltinFallback(t *testing.T) {
	t.Parallel()
	source := NewProjectSource(builtinSourceWithBasePack(t), fakeTreeFromProject(nil))
	resolved, err := source.ResolveForCommit(t.Context(), "base-pack@^1", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Origin != OriginBuiltin {
		t.Errorf("origin = %q, want builtin", resolved.Origin)
	}
	if resolved.Pack.BaseRef != "base-pack@^1" {
		t.Errorf("BaseRef = %q, want the requested ref", resolved.Pack.BaseRef)
	}
	if resolved.Pack.ContentHash == "" {
		t.Error("builtin resolution must carry a ContentHash")
	}
}

func TestProjectSource_NilReaderResolvesBuiltin(t *testing.T) {
	t.Parallel()
	source := NewProjectSource(builtinSourceWithBasePack(t), nil)
	resolved, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Origin != OriginBuiltin {
		t.Errorf("origin = %q, want builtin", resolved.Origin)
	}
}

// replacementManifest is a full project manifest replacing the builtin of the
// same name: a smaller graph the tests can tell apart from the base shape.
const replacementManifest = `api: agentum/v1
pack:
  name: base-pack
  version: 2.0.0
  persona: engineering
memory: {reads: [project], writes: false}
capabilities: [fs.read]
budgets: {fix_cycles: 1, ask_to_edit: 1}
tiers: {default: fast}
entry: spec
stages:
  spec:
    gate: human_approval
    prompt: prompts/spec.md
    transitions:
      - to: done
  done: {}
`

func TestProjectSource_ManifestReplacesBuiltin(t *testing.T) {
	t.Parallel()
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(projectFiles("base-pack", map[string]string{
			"manifest.yaml":   replacementManifest,
			"prompts/spec.md": "project spec prompt",
		})),
	)
	resolved, err := source.ResolveForCommit(t.Context(), "base-pack@^2", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Origin != OriginProject {
		t.Errorf("origin = %q, want project", resolved.Origin)
	}
	if resolved.Pack.Pack.Version != "2.0.0" {
		t.Errorf("version = %q, want the project manifest's 2.0.0, not the builtin's", resolved.Pack.Pack.Version)
	}
	if got := resolved.Pack.Stages["spec"].PromptText(); got != "project spec prompt" {
		t.Errorf("spec prompt = %q, want the project layer's text", got)
	}
	if _, hasImplement := resolved.Pack.Stages["implement"]; hasImplement {
		t.Error("a replacement must not inherit the builtin's stages")
	}
	if resolved.Pack.ContentHash == "" {
		t.Error("manifest resolution must carry a ContentHash")
	}

	// The ref's version constraint applies to the project manifest's version.
	if _, err := source.ResolveForCommit(t.Context(), "base-pack@^1", repoPath, baseCommit); err == nil {
		t.Error("constraint ^1 must reject the 2.0.0 replacement")
	}
}

// A project pack under a name no builtin carries is simply a new pack.
func TestProjectSource_NewNameResolves(t *testing.T) {
	t.Parallel()
	newPackManifest := strings.Replace(replacementManifest, "name: base-pack", "name: my-flow", 1)
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(projectFiles("my-flow", map[string]string{
			"manifest.yaml":   newPackManifest,
			"prompts/spec.md": "my-flow spec",
		})),
	)
	resolved, err := source.ResolveForCommit(t.Context(), "my-flow", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Origin != OriginProject || resolved.Pack.Pack.Name != "my-flow" {
		t.Errorf("origin/name = %q/%q, want project/my-flow", resolved.Origin, resolved.Pack.Pack.Name)
	}
}

// The inheritance case the acceptance criteria name: a project overrides
// document swapping the implement and fix prompts (and patching a budget),
// everything else taken from the builtin base.
func TestProjectSource_OverridesInheritAndSwapPrompts(t *testing.T) {
	t.Parallel()
	overrides := `base: base-pack@^1
prompts:
  implement: prompts/implement.md
  fix: prompts/fix.md
stages:
  plan:
    gate: auto_on_approval
budgets:
  fix_cycles: 5
`
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(projectFiles("base-pack", map[string]string{
			"overrides.yaml":       overrides,
			"prompts/implement.md": "project implement prompt",
			"prompts/fix.md":       "project fix prompt",
		})),
	)
	resolved, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Origin != OriginProjectOverBuiltin {
		t.Fatalf("origin = %q, want project+builtin", resolved.Origin)
	}
	if got := resolved.Pack.Stages["implement"].PromptText(); got != "project implement prompt" {
		t.Errorf("implement prompt = %q, want the project layer's", got)
	}
	if got := resolved.Pack.Stages["fix"].PromptText(); got != "project fix prompt" {
		t.Errorf("fix prompt = %q, want the project layer's", got)
	}
	if got := resolved.Pack.Stages["plan"].PromptText(); got != "base plan prompt" {
		t.Errorf("plan prompt = %q, want the builtin base's (not overridden)", got)
	}
	if resolved.Pack.Budgets.FixCycles != 5 {
		t.Errorf("fix_cycles = %d, want the patched 5", resolved.Pack.Budgets.FixCycles)
	}
	if resolved.Pack.Stages["plan"].Gate != GateAutoOnApproval {
		t.Errorf("plan gate = %q, want the patched auto_on_approval", resolved.Pack.Stages["plan"].Gate)
	}
	// The graph shape comes from the base: stages, transitions, approvals.
	approval, hasApproval := resolved.Pack.SourceWriteApproval()
	if !hasApproval || approval.Stage != "plan" {
		t.Errorf("source_write approval = %+v, want the base's plan approval", approval)
	}
	if resolved.Pack.BaseRef != "base-pack@^1" {
		t.Errorf("BaseRef = %q, want the base ref", resolved.Pack.BaseRef)
	}
	// Provenance comes from the document: exactly the fields it declares.
	wantOrigins := map[string]string{
		"stages.implement.prompt": "project",
		"stages.fix.prompt":       "project",
		"stages.plan.gate":        "project",
		"budgets.fix_cycles":      "project",
	}
	if len(resolved.FieldOrigins) != len(wantOrigins) {
		t.Errorf("FieldOrigins = %v, want exactly %v", resolved.FieldOrigins, wantOrigins)
	}
	for key, want := range wantOrigins {
		if got, ok := resolved.FieldOrigins[key]; !ok || got != want {
			t.Errorf("FieldOrigins[%q] = %q (%v), want %q", key, got, ok, want)
		}
	}
	if resolved.Pack.ContentHash == "" {
		t.Error("inherited resolution must carry a ContentHash over the project layer")
	}
}

// An heir may carry a directory name of its own; the base stays builtin.
func TestProjectSource_HeirWithNewName(t *testing.T) {
	t.Parallel()
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(projectFiles("my-flow", map[string]string{
			"overrides.yaml":       "base: base-pack@^1\n",
			"prompts/implement.md": "my implement",
		})),
	)
	resolved, err := source.ResolveForCommit(t.Context(), "my-flow", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForCommit: %v", err)
	}
	if resolved.Pack.Pack.Name != "my-flow" {
		t.Errorf("name = %q, want the directory's my-flow", resolved.Pack.Pack.Name)
	}
	if resolved.Origin != OriginProjectOverBuiltin {
		t.Errorf("origin = %q, want project+builtin", resolved.Origin)
	}
	// The base's version satisfies the ref's constraint (inherited version).
	if resolved.Pack.Pack.Version != "1.2.0" {
		t.Errorf("version = %q, want the base's inherited 1.2.0", resolved.Pack.Pack.Version)
	}
	if _, err := source.ResolveForCommit(t.Context(), "my-flow@^2", repoPath, baseCommit); err == nil {
		t.Error("constraint ^2 must reject the base's 1.2.0")
	}
}

// The task's three distinguishable errors (both files; prompt path outside
// the pack dir; base referencing a project pack) plus the rest of the
// sentinel classes — each asserted with errors.Is, not message text.
func TestProjectSource_ConfigurationErrors(t *testing.T) {
	t.Parallel()
	promptEscapeManifest := strings.Replace(replacementManifest, "prompt: prompts/spec.md", "prompt: ../outside.md", 1)
	tests := []struct {
		name    string
		ref     string
		project map[string]string
		wantErr error
	}{
		{
			name: "both files present",
			ref:  "base-pack",
			project: projectFiles("base-pack", map[string]string{
				"manifest.yaml":  replacementManifest,
				"overrides.yaml": "base: base-pack\n",
			}),
			wantErr: ErrBothPackFiles,
		},
		{
			name: "neither file present",
			ref:  "base-pack",
			project: projectFiles("base-pack", map[string]string{
				"prompts/spec.md": "orphan prompt",
			}),
			wantErr: ErrNoPackFile,
		},
		{
			name: "prompt path escapes the pack dir",
			ref:  "base-pack",
			project: projectFiles("base-pack", map[string]string{
				"manifest.yaml": promptEscapeManifest,
			}),
			wantErr: ErrPromptEscapesDir,
		},
		{
			name: "base names a project pack",
			ref:  "my-flow",
			project: mergeProjectFiles(
				projectFiles("my-flow", map[string]string{
					"overrides.yaml":       "base: base-pack@^1\n",
					"prompts/implement.md": "x",
				}),
				projectFiles("base-pack", map[string]string{
					"manifest.yaml":   replacementManifest,
					"prompts/spec.md": "y",
				}),
			),
			wantErr: ErrBaseNotBuiltin,
		},
		{
			name: "base names no builtin",
			ref:  "my-flow",
			project: projectFiles("my-flow", map[string]string{
				"overrides.yaml": "base: no-such-pack\n",
			}),
			wantErr: ErrBaseBuiltinNotFound,
		},
		{
			name: "fork is not allowed",
			ref:  "my-flow",
			project: projectFiles("my-flow", map[string]string{
				"overrides.yaml": "base: base-pack\nfork: true\n",
			}),
			wantErr: ErrForkNotAllowed,
		},
		{
			name: "manifest name mismatches directory",
			ref:  "my-pack",
			project: projectFiles("my-pack", map[string]string{
				"manifest.yaml":   strings.Replace(replacementManifest, "name: base-pack", "name: other-name", 1),
				"prompts/spec.md": "s",
			}),
			wantErr: ErrPackNameMismatch,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			source := NewProjectSource(builtinSourceWithBasePack(t), fakeTreeFromProject(testCase.project))
			_, err := source.ResolveForCommit(t.Context(), testCase.ref, repoPath, baseCommit)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error = %v, want errors.Is(err, %v)", err, testCase.wantErr)
			}
		})
	}
}

// mergeProjectFiles unions two project-file maps.
func mergeProjectFiles(maps ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, source := range maps {
		for path, body := range source {
			merged[path] = body
		}
	}
	return merged
}

// Strict decoding: an overrides document carrying keys the schema does not
// define (checks, approvals, transitions inside a stage patch) is refused,
// not silently ignored — a dropped "checks:" block would masquerade as an
// accepted weakening attempt. Same for a manifest's unknown key.
func TestProjectSource_StrictYAML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		project map[string]string
		wantSub string
	}{
		{
			name: "overrides with a checks block",
			project: projectFiles("base-pack", map[string]string{
				"overrides.yaml": "base: base-pack\nchecks: {required: [go-test]}\n",
			}),
			wantSub: "checks",
		},
		{
			name: "overrides with an approvals block",
			project: projectFiles("base-pack", map[string]string{
				"overrides.yaml": "base: base-pack\napprovals: []\n",
			}),
			wantSub: "approvals",
		},
		{
			name: "stage patch with transitions",
			project: projectFiles("base-pack", map[string]string{
				"overrides.yaml": "base: base-pack\nstages:\n  implement:\n    transitions:\n      - to: review\n",
			}),
			wantSub: "transitions",
		},
		{
			name: "manifest with an unknown key",
			project: projectFiles("base-pack", map[string]string{
				"manifest.yaml":   strings.Replace(replacementManifest, "entry: spec", "entry: spec\nunknown_key: 1", 1),
				"prompts/spec.md": "s",
			}),
			wantSub: "unknown_key",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			source := NewProjectSource(builtinSourceWithBasePack(t), fakeTreeFromProject(testCase.project))
			_, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit)
			if err == nil {
				t.Fatalf("strict decoding must reject the document; got nil error")
			}
			if !strings.Contains(err.Error(), testCase.wantSub) {
				t.Fatalf("error = %v, want it to name %q", err, testCase.wantSub)
			}
		})
	}
}

// A symlink entry or an oversized one is refused from the tree listing,
// before any content is read.
func TestProjectSource_TreeShapeLimits(t *testing.T) {
	t.Parallel()
	base := fakeTreeFromProject(projectFiles("base-pack", map[string]string{
		"manifest.yaml":   replacementManifest,
		"prompts/spec.md": "s",
	}))

	t.Run("symlink entry", func(t *testing.T) {
		t.Parallel()
		tree := fakeCommitTree{files: map[string]fakeTreeFile{}}
		for path, file := range base.files {
			tree.files[path] = file
		}
		tree.files[ProjectPacksDir+"/base-pack/prompts/link.md"] = fakeTreeFile{mode: "120000", content: []byte("../outside")}
		source := NewProjectSource(builtinSourceWithBasePack(t), tree)
		_, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit)
		if !errors.Is(err, ErrPackSymlink) {
			t.Fatalf("error = %v, want ErrPackSymlink", err)
		}
	})

	t.Run("oversized entry", func(t *testing.T) {
		t.Parallel()
		tree := fakeCommitTree{files: map[string]fakeTreeFile{}}
		for path, file := range base.files {
			tree.files[path] = file
		}
		// A blob whose committed size exceeds the per-file limit; the listing
		// reports the true size the way ls-tree -l would.
		tree.files[ProjectPacksDir+"/base-pack/prompts/huge.md"] = fakeTreeFile{
			mode:    "100644",
			content: append(make([]byte, MaxProjectPackFileBytes+1), 'x'),
		}
		source := NewProjectSource(builtinSourceWithBasePack(t), tree)
		_, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit)
		if !errors.Is(err, ErrPackFileTooLarge) {
			t.Fatalf("error = %v, want ErrPackFileTooLarge", err)
		}
	})
}

// ResolveForFloor defers the manifest path's validation so the runner's
// policy floor can name its rule first; ResolveForCommit surfaces the same
// defect as an error.
func TestProjectSource_ResolveForFloorDefersValidation(t *testing.T) {
	t.Parallel()
	// A manifest whose entry points at a stage that does not exist: invalid
	// under Validate, but parseable — the floor must see it first.
	brokenManifest := strings.Replace(replacementManifest, "entry: spec", "entry: nonexistent", 1)
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(projectFiles("base-pack", map[string]string{
			"manifest.yaml":   brokenManifest,
			"prompts/spec.md": "s",
		})),
	)

	floored, err := source.ResolveForFloor(t.Context(), "base-pack", repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ResolveForFloor: %v", err)
	}
	if floored.Origin != OriginProject {
		t.Errorf("origin = %q, want project", floored.Origin)
	}
	if floored.ValidateErr == nil {
		t.Fatal("ResolveForFloor must carry the deferred validation error")
	}

	if _, err := source.ResolveForCommit(t.Context(), "base-pack", repoPath, baseCommit); err == nil {
		t.Fatal("ResolveForCommit must apply the deferred validation")
	}
}

func TestProjectSource_ListProjectPacks(t *testing.T) {
	t.Parallel()
	source := NewProjectSource(
		builtinSourceWithBasePack(t),
		fakeTreeFromProject(mergeProjectFiles(
			projectFiles("replacer", map[string]string{
				"manifest.yaml":   replacementManifest,
				"prompts/spec.md": "s",
			}),
			projectFiles("heir", map[string]string{
				"overrides.yaml": "base: base-pack\n",
			}),
			projectFiles("broken", map[string]string{
				"manifest.yaml":  replacementManifest,
				"overrides.yaml": "base: base-pack\n",
			}),
			// A prompt that happens to be called manifest.yaml is a pack
			// file, not the pack's document.
			projectFiles("nested", map[string]string{
				"overrides.yaml":        "base: base-pack\n",
				"prompts/manifest.yaml": "not a manifest",
			}),
			map[string]string{ProjectPacksDir + "/README.md": "notes"},
		)),
	)
	listing, err := source.ListProjectPacks(t.Context(), repoPath, baseCommit)
	if err != nil {
		t.Fatalf("ListProjectPacks: %v", err)
	}
	byName := map[string]string{}
	for _, entry := range listing {
		byName[entry.Name] = entry.Kind
	}
	if byName["replacer"] != "manifest" {
		t.Errorf("replacer kind = %q, want manifest", byName["replacer"])
	}
	if byName["heir"] != "overrides" {
		t.Errorf("heir kind = %q, want overrides", byName["heir"])
	}
	if byName["broken"] != "invalid" {
		t.Errorf("broken kind = %q, want invalid (both files)", byName["broken"])
	}
	if byName["nested"] != "overrides" {
		t.Errorf("nested kind = %q, want overrides (a nested manifest.yaml is not the pack document)", byName["nested"])
	}
	if _, listed := byName["README.md"]; listed {
		t.Error("a file directly under .agentum/packs/ is not a pack")
	}
	if _, listed := byName["base-pack"]; listed {
		t.Error("the listing must contain only project packs, not builtin names")
	}
}

func TestGate_PausesForHuman(t *testing.T) {
	t.Parallel()
	for gate, want := range map[Gate]bool{
		GateAuto:           false,
		GateAutoIfClean:    false,
		GateAutoOnApproval: true,
		GateHumanApproval:  true,
		GateHumanFinal:     true,
		GateHumanEdit:      true,
		Gate("weird"):      false,
	} {
		if got := gate.PausesForHuman(); got != want {
			t.Errorf("PausesForHuman(%q) = %v, want %v", gate, got, want)
		}
	}
}
