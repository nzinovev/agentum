package pack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeStrict unmarshals a pack document rejecting unknown fields. A manifest
// or overrides file names what it means; a typo'd or speculative key ("checks:"
// in an overrides document, "transitions" inside a stage patch) must fail
// loudly instead of silently loading a zero value the caller then ignores —
// for overrides especially, a silently dropped "checks:" block would look like
// an accepted weakening attempt.
func decodeStrict(raw []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}

// Load reads and parses a pack directory: manifest.yaml first, then each
// non-terminal stage's prompt file. The result is structurally parsed but not
// yet semantically validated — call Validate to check the contract.
//
// Prompt file paths in the manifest are interpreted as relative to the pack
// directory and must not escape it (no ".." segments that leave the dir).
func Load(dir string) (*Pack, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("pack: resolve dir %q: %w", dir, err)
	}

	manifestPath := filepath.Join(abs, "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("pack: read manifest: %w", err)
	}

	var p Pack
	if err := decodeStrict(raw, &p); err != nil {
		return nil, fmt.Errorf("pack: parse manifest: %w", err)
	}
	p.Dir = abs

	if err := loadPrompts(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// loadPrompts reads each non-terminal stage's prompt file into the pack. It
// enforces that the resolved path stays inside the pack directory.
func loadPrompts(p *Pack) error {
	p.PromptText = make(map[string]string, len(p.Stages))
	for id, st := range p.Stages {
		s := st // copy for mutation
		if s.Terminal() {
			// Terminal stages are engine states and carry no prompt.
			p.Stages[id] = s
			continue
		}
		if s.Prompt == "" {
			return fmt.Errorf("pack: stage %q is non-terminal but has no prompt", id)
		}
		clean, err := safeJoin(p.Dir, s.Prompt)
		if err != nil {
			return fmt.Errorf("pack: stage %q prompt path %q: %w", id, s.Prompt, err)
		}
		text, err := os.ReadFile(clean)
		if err != nil {
			return fmt.Errorf("pack: stage %q read prompt %q: %w", id, s.Prompt, err)
		}
		s.promptText = string(text)
		p.PromptText[id] = string(text)
		p.Stages[id] = s
	}
	return nil
}

// safeJoin joins dir and rel, returning an error if rel escapes dir. The
// error wraps ErrPromptEscapesDir so callers (builtin loader and the project
// source alike) can classify the escape with errors.Is.
func safeJoin(dir, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("path escapes pack directory: %w", ErrPromptEscapesDir)
	}
	return filepath.Join(dir, cleaned), nil
}

// DirHash returns a deterministic digest of a directory's contents (relative
// file paths + file bytes, sorted by path, sha256). It is the one content-hash
// scheme for packs of every origin: DirSource hashes the shipped directory,
// ProjectSource hashes the materialized project directory before its temp
// location is removed. The runner's evidence records the value verbatim.
func DirHash(dir string) (string, error) {
	hasher := sha256.New()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("pack: walk dir %s: %w", dir, err)
	}
	sort.Strings(files)
	for _, filePath := range files {
		rel, relErr := filepath.Rel(dir, filePath)
		if relErr != nil {
			return "", fmt.Errorf("pack: rel %s under %s: %w", filePath, dir, relErr)
		}
		hasher.Write([]byte(rel))
		hasher.Write([]byte{0})
		content, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return "", fmt.Errorf("pack: read %s: %w", filePath, readErr)
		}
		hasher.Write(content)
		hasher.Write([]byte{0})
	}
	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum), nil
}
