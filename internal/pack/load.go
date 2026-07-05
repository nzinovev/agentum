package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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
	if err := yaml.Unmarshal(raw, &p); err != nil {
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
		s.setPromptText(string(text))
		p.PromptText[id] = string(text)
		p.Stages[id] = s
	}
	return nil
}

// safeJoin joins dir and rel, returning an error if rel escapes dir.
func safeJoin(dir, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("path escapes pack directory")
	}
	return filepath.Join(dir, cleaned), nil
}
