package pack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
