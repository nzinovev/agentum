package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadOverrides reads an override document from dir: parses overrides.yaml,
// then reads each prompt file referenced under prompts: into memory. Prompt
// paths are relative to dir and must not escape it.
//
// The returned Overrides is intended for Resolve. Layer 1 (the base ref) is
// resolved separately by a Source.
func LoadOverrides(dir string) (*Overrides, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("overrides: resolve dir %q: %w", dir, err)
	}
	raw, err := os.ReadFile(filepath.Join(abs, "overrides.yaml"))
	if err != nil {
		return nil, fmt.Errorf("overrides: read overrides.yaml: %w", err)
	}
	var ov Overrides
	if err := yaml.Unmarshal(raw, &ov); err != nil {
		return nil, fmt.Errorf("overrides: parse overrides.yaml: %w", err)
	}
	ov.dir = abs
	if err := loadOverridePrompts(&ov); err != nil {
		return nil, err
	}
	return &ov, nil
}

func loadOverridePrompts(ov *Overrides) error {
	if len(ov.Prompts) == 0 {
		return nil
	}
	ov.promptText = make(map[string]string, len(ov.Prompts))
	for stage, rel := range ov.Prompts {
		clean, err := safeJoin(ov.dir, rel)
		if err != nil {
			return fmt.Errorf("overrides: prompt for stage %q path %q: %w", stage, rel, err)
		}
		text, err := os.ReadFile(clean)
		if err != nil {
			return fmt.Errorf("overrides: prompt for stage %q read %q: %w", stage, rel, err)
		}
		ov.promptText[stage] = string(text)
	}
	return nil
}

// Resolve applies overrides to base and returns a new, re-validated pack. It
// does not mutate base.
//
//   - Layer 2 (fork): recorded as Forked=true on the result. No behavioral
//     change at resolve time — a fork is a detached copy.
//   - Layer 3 (prompts): each entry replaces the named stage's prompt text.
//     The stage must exist; overriding a prompt for an unknown stage is an
//     error (a consumer mistake, not a silent skip).
//   - Layer 4 (params): stage gate/tier patches and budget patches apply on
//     top of the base. Pointers distinguish "set" from "leave unchanged".
//
// The result is re-validated: an override that breaks the contract (e.g.
// setting an invalid gate, or swapping a prompt to one that loads empty) is
// rejected. Resolve does not handle layer 1 — base must already be the loaded
// pack selected by a Source.
func Resolve(base *Pack, ov *Overrides) (*Pack, error) {
	if base == nil {
		return nil, fmt.Errorf("resolve: base pack is nil")
	}
	resolved, err := deepCopy(base)
	if err != nil {
		return nil, err
	}
	if ov != nil {
		resolved.BaseRef = ov.Base
		resolved.Forked = ov.Fork
		if err := applyPromptSwaps(resolved, ov); err != nil {
			return nil, err
		}
		if err := applyStagePatches(resolved, ov); err != nil {
			return nil, err
		}
		applyBudgetPatches(resolved, ov)
	}
	if err := resolved.Validate(); err != nil {
		return nil, fmt.Errorf("resolve: re-validation failed: %w", err)
	}
	return resolved, nil
}

func deepCopy(base *Pack) (*Pack, error) {
	// Round-trip through YAML for a value copy. The Pack has unexported fields
	// (stage.promptText) that yaml won't carry, so restore prompts explicitly.
	raw, err := yaml.Marshal(base)
	if err != nil {
		return nil, fmt.Errorf("resolve: copy base: %w", err)
	}
	var out Pack
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("resolve: copy base: %w", err)
	}
	out.Dir = base.Dir
	out.PromptText = make(map[string]string, len(base.PromptText))
	for id, st := range base.Stages {
		s := st
		s.setPromptText(base.PromptText[id])
		out.Stages[id] = s
		out.PromptText[id] = base.PromptText[id]
	}
	return &out, nil
}

func applyPromptSwaps(p *Pack, ov *Overrides) error {
	for stage, text := range ov.promptText {
		st, ok := p.Stages[stage]
		if !ok {
			return fmt.Errorf("resolve: override prompt references unknown stage %q", stage)
		}
		if st.Terminal() {
			return fmt.Errorf("resolve: cannot override prompt on terminal stage %q", stage)
		}
		// Keep the file path (Stage.Prompt) pointing at the override source for
		// traceability; the loaded text is what the agent receives. If the
		// override didn't supply a path (in-memory override), leave Prompt as-is.
		if rel := ov.Prompts[stage]; rel != "" {
			st.Prompt = rel
		}
		st.setPromptText(text)
		p.Stages[stage] = st
		p.PromptText[stage] = text
	}
	return nil
}

func applyStagePatches(p *Pack, ov *Overrides) error {
	for stage, patch := range ov.Stages {
		st, ok := p.Stages[stage]
		if !ok {
			return fmt.Errorf("resolve: override patches unknown stage %q", stage)
		}
		if patch.Gate != nil {
			st.Gate = *patch.Gate
		}
		if patch.Tier != nil {
			st.Tier = *patch.Tier
		}
		p.Stages[stage] = st
	}
	return nil
}

func applyBudgetPatches(p *Pack, ov *Overrides) {
	if ov.Budgets == nil {
		return
	}
	if ov.Budgets.FixCycles != nil {
		p.Budgets.FixCycles = *ov.Budgets.FixCycles
	}
	if ov.Budgets.AskToEdit != nil {
		p.Budgets.AskToEdit = *ov.Budgets.AskToEdit
	}
}

// HasMutation reports whether the overrides actually mutate the base (layers
// 3–4). A fork with no mutations is a pure detach. Useful for tooling that
// wants to distinguish "track upstream unchanged" from "customized."
func (ov *Overrides) HasMutation() bool {
	if ov == nil {
		return false
	}
	return len(ov.Prompts) > 0 || len(ov.Stages) > 0 || ov.Budgets != nil
}

// String is a short human form for diagnostics.
func (ov *Overrides) String() string {
	if ov == nil {
		return "<no overrides>"
	}
	parts := []string{fmt.Sprintf("base=%s", ov.Base)}
	if ov.Fork {
		parts = append(parts, "fork")
	}
	if n := len(ov.Prompts); n > 0 {
		parts = append(parts, fmt.Sprintf("prompts=%d", n))
	}
	if n := len(ov.Stages); n > 0 {
		parts = append(parts, fmt.Sprintf("stages=%d", n))
	}
	if ov.Budgets != nil {
		parts = append(parts, "budgets")
	}
	return strings.Join(parts, " ")
}
