package pack

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Source resolves pack references to loaded, validated packs. A reference is
// one of:
//
//	"name"            any version
//	"name@^MAJOR"     latest within MAJOR (lock-major, override layer 1)
//	"name@X.Y.Z"      exact version
//
// The dogfooding MVP ships a DirSource over a flat packs/<name>/ layout (one
// version per name). A multi-version registry (multiple versions per name,
// true "latest within major" selection) is deferred — DirSource checks the
// single available version against the constraint and rejects mismatches.
type Source interface {
	Resolve(ctx context.Context, ref string) (*Pack, error)
}

// DirSource serves packs from a filesystem directory laid out as
// <root>/<name>/manifest.yaml.
type DirSource struct {
	Root string
}

// NewDirSource returns a Source rooted at root. The directory is not read until
// Resolve is called.
func NewDirSource(root string) *DirSource { return &DirSource{Root: root} }

// List returns the identity block of every pack under the root, sorted by
// name — the catalog listing surface. A dot-directory or a directory without
// a manifest.yaml is not a pack and is skipped; a directory that carries a
// manifest.yaml and fails to load is an error, not a skip: the
// listing feeds the boot floor check, and hiding a broken pack from it would
// hide exactly the pack the check exists to refuse. The result is not
// validated (Resolve does that per ref); a listing that showed only valid
// packs would quietly disagree with what Resolve accepts.
func (s *DirSource) List(ctx context.Context) ([]Meta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.Root) == "" {
		// An empty root is "no builtin packs configured" (the zero-value
		// config the boot check tolerates), not a broken directory: a
		// configured-but-missing root below is the error.
		return nil, nil
	}
	dirs, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, fmt.Errorf("pack source: list %s: %w", s.Root, err)
	}
	var metas []Meta
	for _, dir := range dirs {
		if !dir.IsDir() || strings.HasPrefix(dir.Name(), ".") {
			continue
		}
		// A directory without a manifest.yaml is not a pack (a .git beside
		// the packs, lost+found, a scratch folder) and Resolve never reads
		// it either. Only a directory that claims to be a pack and fails to
		// load is the error.
		if _, statErr := os.Stat(filepath.Join(s.Root, dir.Name(), "manifest.yaml")); errors.Is(statErr, fs.ErrNotExist) {
			continue
		}
		p, loadErr := Load(filepath.Join(s.Root, dir.Name()))
		if loadErr != nil {
			return nil, fmt.Errorf("pack source: pack %q: %w", dir.Name(), loadErr)
		}
		if p.Pack.Name != dir.Name() {
			return nil, fmt.Errorf("pack source: manifest at %s declares name %q, expected %q", dir.Name(), p.Pack.Name, dir.Name())
		}
		metas = append(metas, p.Pack)
	}
	sort.Slice(metas, func(left, right int) bool { return metas[left].Name < metas[right].Name })
	return metas, nil
}

func (s *DirSource) Resolve(ctx context.Context, ref string) (*Pack, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, constraint, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, name)
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err != nil {
		return nil, fmt.Errorf("pack source: pack %q not found at %s: %w", name, dir, err)
	}
	p, err := Load(dir)
	if err != nil {
		return nil, err
	}
	if p.Pack.Name != name {
		return nil, fmt.Errorf("pack source: manifest at %s declares name %q, expected %q", dir, p.Pack.Name, name)
	}
	if !versionSatisfies(p.Pack.Version, constraint) {
		return nil, fmt.Errorf("pack source: %s version %s does not satisfy constraint %q", name, p.Pack.Version, constraint)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("pack source: base pack %s failed validation: %w", name, err)
	}
	p.BaseRef = ref
	p.Origin = OriginBuiltin
	contentHash, hashErr := DirHash(p.Dir)
	if hashErr != nil {
		return nil, fmt.Errorf("pack source: hash pack %s: %w", name, hashErr)
	}
	p.ContentHash = contentHash
	return p, nil
}

// parseRef splits "name", "name@^MAJOR", or "name@X.Y.Z" into its parts.
func parseRef(ref string) (name, constraint string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("pack ref is empty")
	}
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		name = ref[:i]
		constraint = ref[i+1:]
	} else {
		name = ref
	}
	if name == "" {
		return "", "", fmt.Errorf("pack ref %q has empty name", ref)
	}
	if constraint != "" && !isConstraint(constraint) {
		return "", "", fmt.Errorf("pack ref %q has malformed constraint %q", ref, constraint)
	}
	return name, constraint, nil
}

// isConstraint accepts "" (any), "^N" (lock major), or "X.Y.Z" (exact).
func isConstraint(c string) bool {
	if c == "" {
		return true
	}
	if strings.HasPrefix(c, "^") {
		_, err := strconv.Atoi(c[1:])
		return err == nil
	}
	return isSemver(c)
}

// versionSatisfies reports whether version meets the constraint.
func versionSatisfies(version, constraint string) bool {
	if constraint == "" {
		return true
	}
	if strings.HasPrefix(constraint, "^") {
		wantMajor, err := strconv.Atoi(constraint[1:])
		if err != nil {
			return false
		}
		return majorOf(version) == wantMajor
	}
	return version == constraint
}

// majorOf returns the MAJOR component of a "MAJOR.MINOR.PATCH" version, or -1
// if it cannot be parsed.
func majorOf(v string) int {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return -1
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		return -1
	}
	return n
}
