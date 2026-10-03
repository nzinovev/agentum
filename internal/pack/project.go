package pack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectPacksDir is the in-repo directory a project's own packs live in:
// <repo>/.agentum/packs/<name>/{manifest.yaml | overrides.yaml}. It is read
// from the run's base_commit exactly like .agentum.yaml and the instruction
// files, so an agent cannot change the pack its own run executes.
const ProjectPacksDir = ".agentum/packs"

// Limits for reading a project pack out of a commit tree. A pack is a manifest
// plus a handful of markdown prompts; anything past these bounds is not a pack
// someone meant to run, and refusing it before reading keeps a hostile commit
// from turning the reader into a disk- or memory-pressure vector.
const (
	MaxProjectPackFileBytes  = 64 << 10
	MaxProjectPackTotalBytes = 256 << 10
	MaxProjectPackFiles      = 128
)

// Sentinel configuration errors. Every failure to read a project pack wraps
// exactly one of these, so callers distinguish the classes with errors.Is
// instead of parsing message text — the three the task names (both files at
// once, a prompt path outside the pack dir, a base referencing a project
// pack) must surface as three different errors in code, not just in prose.
var (
	// ErrBothPackFiles: a project pack directory carries manifest.yaml and
	// overrides.yaml at once. It must pick exactly one — a manifest alongside
	// an overrides document leaves "which graph runs" ambiguous.
	ErrBothPackFiles = errors.New("project pack has both manifest.yaml and overrides.yaml")
	// ErrNoPackFile: neither file present — the directory exists at the
	// commit but is not a pack.
	ErrNoPackFile = errors.New("project pack has neither manifest.yaml nor overrides.yaml")
	// ErrPromptEscapesDir: a prompt file path resolves outside the pack
	// directory (safeJoin; wraps every escape, builtin loading included).
	ErrPromptEscapesDir = errors.New("prompt path escapes the pack directory")
	// ErrPackSymlink: the pack tree at the commit contains a symlink entry.
	// A symlink is a reference to somewhere the pack does not control, and
	// the commit tree reports it by mode before any byte is read.
	ErrPackSymlink = errors.New("project pack entry is a symlink")
	// ErrPackFileTooLarge: one entry exceeds MaxProjectPackFileBytes.
	ErrPackFileTooLarge = errors.New("project pack entry exceeds the per-file size limit")
	// ErrPackTreeTooLarge: the pack exceeds the total-size or file-count limit.
	ErrPackTreeTooLarge = errors.New("project pack exceeds the total size or file-count limit")
	// ErrBaseNotBuiltin: an overrides document's base names a pack that this
	// project replaces with its own — even when a builtin of that name also
	// exists. The base of an inheritance is always a builtin pack; a shadowed
	// name would make "which graph is inherited" depend on resolution order.
	ErrBaseNotBuiltin = errors.New("base references a project pack; a base must be a builtin pack")
	// ErrBaseBuiltinNotFound: base names no builtin pack at all.
	ErrBaseBuiltinNotFound = errors.New("base references a builtin pack that does not exist")
	// ErrForkNotAllowed: fork is an upstream-registry concept (detach from a
	// distributed pack). A project pack is authored in the project already.
	ErrForkNotAllowed = errors.New("fork is not allowed in a project overrides document")
	// ErrPackNameMismatch: a manifest declares a pack.name other than its
	// directory's name. The directory name is the pack's identity — the ref
	// grammar, replacement semantics, and the listing all key on it.
	ErrPackNameMismatch = errors.New("project pack manifest name does not match its directory")
	// ErrPackNotFound: the ref names no builtin pack and the project ships no
	// pack under the name. The listing and detail surfaces map it to their
	// not-found answers; every other resolution failure is a configuration
	// error.
	ErrPackNotFound = errors.New("pack not found")
)

// TreeEntry is one file of a commit-tree listing, as the reader below returns
// it. It lives here (not in the worktree package) so pack stays a pure format
// package; the worktree adapter maps its own entry onto this shape.
type TreeEntry struct {
	Path string // repo-relative, forward slashes
	Mode string // git mode: "100644"/"100755" regular blobs, "120000" symlink
	Size int64  // blob size in bytes
}

// CommitTree reads files and directory listings from a commit's tree. The
// worktree manager satisfies it through a thin adapter; tests use in-memory
// fakes.
type CommitTree interface {
	FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error)
	ListTreeAtCommit(ctx context.Context, repoPath, commit, dir string) ([]TreeEntry, error)
}

// Resolved is a project-aware pack resolution: the pack, where its bytes came
// from, and — for inherited packs — which fields the project layer set.
// FieldOrigins is keyed "stages.<id>.prompt", "stages.<id>.gate",
// "stages.<id>.tier", "budgets.fix_cycles", "budgets.ask_to_edit", each with
// the value "project"; it is built from what the overrides document itself
// declares, so an override that happens to equal the base value still shows.
// Nil for builtin and project-manifest packs (their origin is uniform).
type Resolved struct {
	Pack         *Pack
	Origin       Origin
	FieldOrigins map[string]string

	// ValidateErr carries the manifest-path validation error when resolution
	// deliberately skipped validation (ResolveForFloor, used by the runner so
	// the policy floor names its rule before the validator names the defect).
	// Nil everywhere else.
	ValidateErr error
}

// ProjectSource resolves a pack reference for one project checkout at one
// commit. A project pack under .agentum/packs/<name>/ shadows the builtin of
// the same name (manifest.yaml = replacement) or wraps a builtin base
// (overrides.yaml = inheritance: prompts, stage gates/tiers, budgets; the
// graph — stages, transitions, approvals — comes from the base and cannot be
// patched). A name absent from the project resolves to the builtin source.
// Heirs may carry any directory name (.agentum/packs/my-flow/ with
// base: backend-development is a valid new pack name); a project pack whose
// name equals its base's replaces that builtin in this project.
type ProjectSource struct {
	Builtin *DirSource
	Reader  CommitTree
}

// NewProjectSource builds a ProjectSource over a builtin source and a commit
// reader. A nil reader disables project packs (every ref resolves builtin) —
// the shape tests and partially wired servers use.
func NewProjectSource(builtin *DirSource, reader CommitTree) *ProjectSource {
	return &ProjectSource{Builtin: builtin, Reader: reader}
}

// ResolveForCommit resolves ref to a loaded, validated pack. The project layer
// wins over the builtin of the same name; a name with no project pack falls
// back to the builtin source. A project manifest that fails validation is an
// error here — the deferred-validation shape is ResolveForFloor's, below.
func (source *ProjectSource) ResolveForCommit(ctx context.Context, ref, repoPath, baseCommit string) (*Resolved, error) {
	resolved, err := source.ResolveForFloor(ctx, ref, repoPath, baseCommit)
	if err != nil {
		return nil, err
	}
	if resolved.ValidateErr != nil {
		return nil, resolved.ValidateErr
	}
	return resolved, nil
}

// ResolveForFloor resolves ref the same way but defers the manifest path's
// Validate: the runner checks the policy floor first, so a floor violation is
// reported with its rule id and layer instead of the validator's wording. The
// deferred validation error rides on Resolved.ValidateErr; the builtin and
// inheritance paths validate during resolution as always (DirSource validates
// directly, and the override resolver re-validates its merged result), so
// ValidateErr can only be set for a project manifest pack.
func (source *ProjectSource) ResolveForFloor(ctx context.Context, ref, repoPath, baseCommit string) (*Resolved, error) {
	name, constraint, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	if source.Reader == nil {
		// No commit reader wired: project packs cannot be consulted and every
		// ref resolves builtin (the partially wired server and test shape).
		base, baseErr := source.Builtin.Resolve(ctx, ref)
		if baseErr != nil {
			return nil, fmt.Errorf("project pack %q: %w: %w", name, ErrPackNotFound, baseErr)
		}
		return &Resolved{Pack: base, Origin: OriginBuiltin}, nil
	}
	packDir := ProjectPacksDir + "/" + name
	entries, err := source.Reader.ListTreeAtCommit(ctx, repoPath, baseCommit, packDir)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: list %s at %s: %w", name, packDir, baseCommit, err)
	}
	if len(entries) == 0 {
		// No project pack under this name: the builtin source answers. Its
		// not-found failure wraps ErrPackNotFound so the catalog surfaces can
		// tell "no such pack" from a configuration error.
		base, baseErr := source.Builtin.Resolve(ctx, ref)
		if baseErr != nil {
			return nil, fmt.Errorf("project pack %q: %w: %w", name, ErrPackNotFound, baseErr)
		}
		return &Resolved{Pack: base, Origin: OriginBuiltin}, nil
	}

	// The tree listing is the trust boundary for shape: mode and size arrive
	// from the commit object itself, before any content is read.
	if err := validateProjectPackTree(name, packDir, entries); err != nil {
		return nil, err
	}
	hasManifest, hasOverrides := packFilePresent(entries, packDir, "manifest.yaml"), packFilePresent(entries, packDir, "overrides.yaml")
	switch {
	case hasManifest && hasOverrides:
		return nil, fmt.Errorf("project pack %q: %w", name, ErrBothPackFiles)
	case !hasManifest && !hasOverrides:
		return nil, fmt.Errorf("project pack %q: %w", name, ErrNoPackFile)
	}

	// Materialize the committed bytes into a temp directory so Load and
	// LoadOverrides — with their prompt-path containment — run unchanged over
	// the project tree. The directory holds exactly the validated entries, so
	// no symlink or outside file can appear in it.
	materialized, err := materializePack(ctx, source.Reader, repoPath, baseCommit, packDir, entries)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(materialized) }()

	if hasManifest {
		return source.resolveProjectManifest(materialized, ref, name, constraint)
	}
	return source.resolveProjectOverrides(ctx, materialized, ref, name, constraint, repoPath, baseCommit)
}

// resolveProjectManifest loads a replacement pack. Validation is deferred to
// the caller (ResolveForCommit applies it; the runner floors first).
func (source *ProjectSource) resolveProjectManifest(materializedDir, ref, name, constraint string) (*Resolved, error) {
	loaded, err := Load(materializedDir)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: %w", name, err)
	}
	if loaded.Pack.Name != name {
		return nil, fmt.Errorf("project pack %q: %w: manifest declares %q", name, ErrPackNameMismatch, loaded.Pack.Name)
	}
	if !versionSatisfies(loaded.Pack.Version, constraint) {
		return nil, fmt.Errorf("project pack %q: version %s does not satisfy constraint %q", name, loaded.Pack.Version, constraint)
	}
	contentHash, hashErr := DirHash(materializedDir)
	if hashErr != nil {
		return nil, fmt.Errorf("project pack %q: %w", name, hashErr)
	}
	loaded.BaseRef = ref
	loaded.Origin = OriginProject
	loaded.ContentHash = contentHash
	loaded.Dir = "" // the materialized dir is removed on return; ContentHash carries identity
	validateErr := loaded.Validate()
	if validateErr != nil {
		validateErr = fmt.Errorf("project pack %q: %w", name, validateErr)
	}
	return &Resolved{Pack: loaded, Origin: OriginProject, ValidateErr: validateErr}, nil
}

// resolveProjectOverrides layers an overrides document over a builtin base.
// The base is resolved ONLY through the builtin source; a base naming another
// project pack is refused — including a name this project shadows with its own
// pack (replacement means the builtin of that name is not what runs here, so
// inheriting "it" would be ambiguous). The heir's identity is its directory
// name; version and persona come from the base.
func (source *ProjectSource) resolveProjectOverrides(ctx context.Context, materializedDir, ref, name, constraint, repoPath, baseCommit string) (*Resolved, error) {
	overrides, err := LoadOverrides(materializedDir)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: %w", name, err)
	}
	if overrides.Fork {
		return nil, fmt.Errorf("project pack %q: %w", name, ErrForkNotAllowed)
	}
	baseName, _, err := parseRef(overrides.Base)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: base: %w", name, err)
	}
	if baseName != name {
		// A base other than the pack's own name must name a builtin that this
		// project does not shadow: if .agentum/packs/<baseName>/ exists, the
		// name belongs to a project pack here and the inheritance is refused.
		shadowed, shadowErr := source.Reader.ListTreeAtCommit(ctx, repoPath, baseCommit, ProjectPacksDir+"/"+baseName)
		if shadowErr != nil {
			return nil, fmt.Errorf("project pack %q: check base %q: %w", name, baseName, shadowErr)
		}
		if len(shadowed) > 0 {
			return nil, fmt.Errorf("project pack %q: base %q: %w", name, baseName, ErrBaseNotBuiltin)
		}
	}
	// The base itself must exist among the builtins — decided by the builtin
	// source's layout, before resolution, so "no such builtin" is its own
	// error rather than a generic resolution failure.
	if _, statErr := os.Stat(filepath.Join(source.Builtin.Root, baseName, "manifest.yaml")); statErr != nil {
		return nil, fmt.Errorf("project pack %q: base %q: %w", name, baseName, ErrBaseBuiltinNotFound)
	}
	base, err := source.Builtin.Resolve(ctx, overrides.Base)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: base %q: %w", name, overrides.Base, err)
	}
	// The ref's version constraint applies to the base's version — the heir
	// inherits it (a pack that tracks backend-development@^0 must not silently
	// pick up a base outside that major).
	if !versionSatisfies(base.Pack.Version, constraint) {
		return nil, fmt.Errorf("project pack %q: base version %s does not satisfy constraint %q", name, base.Pack.Version, constraint)
	}
	resolvedPack, err := Resolve(base, overrides)
	if err != nil {
		return nil, fmt.Errorf("project pack %q: %w", name, err)
	}
	contentHash, hashErr := DirHash(materializedDir)
	if hashErr != nil {
		return nil, fmt.Errorf("project pack %q: %w", name, hashErr)
	}
	resolvedPack.Pack.Name = name // the directory is the heir's identity
	resolvedPack.Origin = OriginProjectOverBuiltin
	resolvedPack.ContentHash = contentHash
	// ContentHash covers the project layer only; Resolve set BaseRef to the
	// base ref, which identifies the builtin side alongside it.
	return &Resolved{
		Pack:         resolvedPack,
		Origin:       OriginProjectOverBuiltin,
		FieldOrigins: overrideFieldOrigins(overrides),
	}, nil
}

// overrideFieldOrigins records which fields the overrides document declares,
// keyed per Resolved's contract. Built from the document itself (not a diff of
// values): an override equal to the base value is still an authored override.
func overrideFieldOrigins(overrides *Overrides) map[string]string {
	origins := make(map[string]string)
	for stage := range overrides.Prompts {
		origins["stages."+stage+".prompt"] = "project"
	}
	for stage, patch := range overrides.Stages {
		if patch.Gate != nil {
			origins["stages."+stage+".gate"] = "project"
		}
		if patch.Tier != nil {
			origins["stages."+stage+".tier"] = "project"
		}
	}
	if overrides.Budgets != nil {
		if overrides.Budgets.FixCycles != nil {
			origins["budgets.fix_cycles"] = "project"
		}
		if overrides.Budgets.AskToEdit != nil {
			origins["budgets.ask_to_edit"] = "project"
		}
	}
	if len(origins) == 0 {
		return nil
	}
	return origins
}

// validateProjectPackTree enforces the shape limits over the commit listing:
// regular blobs only (a symlink is a reference outside the pack's control),
// every path inside the pack directory, and per-file / total / count caps.
func validateProjectPackTree(name, packDir string, entries []TreeEntry) error {
	if len(entries) > MaxProjectPackFiles {
		return fmt.Errorf("project pack %q: %w: %d entries, limit %d", name, ErrPackTreeTooLarge, len(entries), MaxProjectPackFiles)
	}
	prefix := packDir + "/"
	var totalBytes int64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Path, prefix) || strings.Contains(entry.Path, "..") {
			return fmt.Errorf("project pack %q: entry %q lies outside the pack directory", name, entry.Path)
		}
		switch entry.Mode {
		case "100644", "100755":
		case "120000":
			return fmt.Errorf("project pack %q: entry %q: %w", name, entry.Path, ErrPackSymlink)
		default:
			return fmt.Errorf("project pack %q: entry %q has unsupported mode %q", name, entry.Path, entry.Mode)
		}
		if entry.Size > MaxProjectPackFileBytes {
			return fmt.Errorf("project pack %q: entry %q: %w: %d bytes, limit %d", name, entry.Path, ErrPackFileTooLarge, entry.Size, MaxProjectPackFileBytes)
		}
		totalBytes += entry.Size
	}
	if totalBytes > MaxProjectPackTotalBytes {
		return fmt.Errorf("project pack %q: %w: %d bytes, limit %d", name, ErrPackTreeTooLarge, totalBytes, MaxProjectPackTotalBytes)
	}
	return nil
}

// packFilePresent reports whether the pack's root file (manifest.yaml or
// overrides.yaml) is among the listed entries.
func packFilePresent(entries []TreeEntry, packDir, file string) bool {
	want := packDir + "/" + file
	for _, entry := range entries {
		if entry.Path == want {
			return true
		}
	}
	return false
}

// materializePack writes the validated tree entries' committed bytes into a
// fresh temp directory, recreating their relative layout.
func materializePack(ctx context.Context, reader CommitTree, repoPath, baseCommit, packDir string, entries []TreeEntry) (string, error) {
	tempDir, err := os.MkdirTemp("", "agentum-project-pack-")
	if err != nil {
		return "", fmt.Errorf("project pack %q: materialize: %w", packDir, err)
	}
	prefix := packDir + "/"
	for _, entry := range entries {
		content, readErr := reader.FileAtCommit(ctx, repoPath, baseCommit, entry.Path)
		if readErr != nil {
			_ = os.RemoveAll(tempDir)
			return "", fmt.Errorf("project pack: read %s at %s: %w", entry.Path, baseCommit, readErr)
		}
		relativePath := filepath.FromSlash(strings.TrimPrefix(entry.Path, prefix))
		target := filepath.Join(tempDir, relativePath)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			_ = os.RemoveAll(tempDir)
			return "", fmt.Errorf("project pack %q: materialize %s: %w", packDir, entry.Path, err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			_ = os.RemoveAll(tempDir)
			return "", fmt.Errorf("project pack %q: materialize %s: %w", packDir, entry.Path, err)
		}
	}
	return tempDir, nil
}

// ResolveBuiltin resolves ref against the builtin source alone — the catalog
// surface for callers not bound to a project. The origin is always builtin.
func (source *ProjectSource) ResolveBuiltin(ctx context.Context, ref string) (*Resolved, error) {
	base, err := source.Builtin.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPackNotFound, err)
	}
	return &Resolved{Pack: base, Origin: OriginBuiltin}, nil
}

// ListBuiltin lists the builtin source's packs (the catalog surface without a
// project).
func (source *ProjectSource) ListBuiltin(ctx context.Context) ([]Meta, error) {
	return source.Builtin.List(ctx)
}

// ProjectPackEntry is one project pack the commit tree advertises, for the
// pack listing surface. Kind says which document the directory carries;
// "invalid" marks a directory that declares both (or the listing failed to
// classify) — it is reported rather than hidden, so the listing never shows a
// shadowed builtin as quietly gone.
type ProjectPackEntry struct {
	Name string
	Kind string // "manifest" | "overrides" | "invalid"
}

// ListProjectPacks names every pack under .agentum/packs/ at the commit. It
// does not resolve them — the listing is a directory scan, and resolution
// (with its validation and floor checks) happens per pack on demand.
func (source *ProjectSource) ListProjectPacks(ctx context.Context, repoPath, commit string) ([]ProjectPackEntry, error) {
	if source.Reader == nil {
		return nil, nil
	}
	entries, err := source.Reader.ListTreeAtCommit(ctx, repoPath, commit, ProjectPacksDir)
	if err != nil {
		return nil, fmt.Errorf("list %s at %s: %w", ProjectPacksDir, commit, err)
	}
	prefix := ProjectPacksDir + "/"
	byName := map[string]*ProjectPackEntry{}
	for _, entry := range entries {
		rest := strings.TrimPrefix(entry.Path, prefix)
		if rest == "" || strings.HasPrefix(rest, "/") {
			continue
		}
		// A blob directly under .agentum/packs/ (a README, a stray file) is not
		// a pack: only a directory names one.
		name, inside, nested := strings.Cut(rest, "/")
		if name == "" || !nested {
			continue
		}
		record, ok := byName[name]
		if !ok {
			record = &ProjectPackEntry{Name: name}
			byName[name] = record
		}
		// Matched against the path inside the pack directory, not by suffix: a
		// nested prompts/manifest.yaml is an ordinary pack file and must not
		// change what kind of pack this is.
		switch inside {
		case "manifest.yaml":
			if record.Kind == "overrides" {
				record.Kind = "invalid"
			} else {
				record.Kind = "manifest"
			}
		case "overrides.yaml":
			if record.Kind == "manifest" {
				record.Kind = "invalid"
			} else {
				record.Kind = "overrides"
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	listing := make([]ProjectPackEntry, 0, len(names))
	for _, name := range names {
		listing = append(listing, *byName[name])
	}
	return listing, nil
}
