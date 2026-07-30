package pack

import (
	"errors"
	"fmt"
	"strings"
)

// Validate checks the loaded pack against the §4.4 contract. It returns nil
// when the pack is sound, or an error whose message lists every problem found
// (multi-error) so a pack author sees all issues at once.
func (p *Pack) Validate() error {
	var problems []string
	problems = append(problems, p.validateIdentity()...)
	problems = append(problems, p.validateEntry()...)
	problems = append(problems, p.validateStages()...)
	problems = append(problems, p.validateMemory()...)
	problems = append(problems, p.validateBudgets()...)
	problems = append(problems, validateCheckPolicy(p.Checks)...)

	// reachability + terminal-exit (only meaningful once the structural checks
	// already passed, so we can assume refs resolve).
	if len(problems) == 0 {
		problems = append(problems, validateGraph(p)...)
	}

	if len(problems) > 0 {
		return fmt.Errorf("pack: invalid: %s", joinErrors(problems))
	}
	return nil
}

// validateIdentity checks api, pack name, pack version, and that stages exist.
func (p *Pack) validateIdentity() []string {
	var problems []string
	if p.API != APIVersion {
		problems = append(problems, fmt.Sprintf("api must be %q, got %q", APIVersion, p.API))
	}
	if strings.TrimSpace(p.Pack.Name) == "" {
		problems = append(problems, "pack.name is required")
	}
	if !isSemver(p.Pack.Version) {
		problems = append(problems, fmt.Sprintf("pack.version must be semver MAJOR.MINOR.PATCH, got %q", p.Pack.Version))
	}
	if len(p.Stages) == 0 {
		problems = append(problems, "stages is empty")
	}
	return problems
}

// validateEntry checks the entry stage is declared and defined.
func (p *Pack) validateEntry() []string {
	if p.Entry == "" {
		return []string{"entry is required"}
	}
	if _, ok := p.Stages[p.Entry]; !ok {
		return []string{fmt.Sprintf("entry %q is not defined in stages", p.Entry)}
	}
	return nil
}

// validateStages checks every stage's gate, role, prompt, and transitions.
func (p *Pack) validateStages() []string {
	gateOK := map[Gate]bool{
		GateAuto: true, GateAutoIfClean: true, GateAutoOnApproval: true,
		GateHumanApproval: true, GateHumanFinal: true, GateHumanEdit: true,
	}
	// knownStageRoles mirrors caps.KnownRoles. Inlined to keep the pack
	// package a pure data format with no import of caps; the two must stay
	// in sync. A stage's role selects the capability-profile template.
	knownStageRoles := map[string]bool{
		"analyst": true, "reviewer": true, "implementer": true, "fixer": true,
	}
	var problems []string
	for id, stage := range p.Stages {
		problems = append(problems, p.validateStage(id, stage, gateOK, knownStageRoles)...)
	}
	return problems
}

// validateStage checks one stage. Terminal stages are engine states (no prompt,
// no gate); non-terminal stages require a known gate, a valid role, and a loaded
// prompt, and each transition must point at a defined, non-self stage.
func (p *Pack) validateStage(id string, stage Stage, gateOK map[Gate]bool, knownStageRoles map[string]bool) []string {
	if id == "" {
		return []string{"stage has an empty id"}
	}
	if stage.Terminal() {
		if stage.Prompt != "" {
			return []string{fmt.Sprintf("stage %q is terminal and must not declare a prompt", id)}
		}
		return nil
	}
	var problems []string
	if !gateOK[stage.Gate] {
		problems = append(problems, fmt.Sprintf("stage %q gate %q is not one of the six-value vocabulary", id, stage.Gate))
	}
	if stage.Role != "" && !knownStageRoles[stage.Role] {
		// The role selects the capability-profile template (see internal/caps).
		// The set is mirrored here so the pack validates without importing caps;
		// the two lists must stay in sync.
		problems = append(problems, fmt.Sprintf("stage %q role %q is not one of {analyst, reviewer, implementer, fixer}", id, stage.Role))
	}
	problems = append(problems, p.validateStagePrompt(id, stage)...)
	problems = append(problems, validateTransitions(id, stage, p.Stages)...)
	return problems
}

// validateStagePrompt checks a non-terminal stage's prompt is declared and
// loaded non-empty.
func (p *Pack) validateStagePrompt(id string, stage Stage) []string {
	if stage.Prompt == "" {
		return []string{fmt.Sprintf("stage %q is non-terminal and requires a prompt", id)}
	}
	if p.PromptText[id] == "" {
		return []string{fmt.Sprintf("stage %q prompt %q loaded empty", id, stage.Prompt)}
	}
	return nil
}

// validateTransitions checks each transition target is a defined, non-self stage.
func validateTransitions(id string, stage Stage, stages map[string]Stage) []string {
	var problems []string
	for index, transition := range stage.Transitions {
		if transition.To == "" {
			problems = append(problems, fmt.Sprintf("stage %q transition[%d].to is empty", id, index))
			continue
		}
		if _, ok := stages[transition.To]; !ok {
			problems = append(problems, fmt.Sprintf("stage %q transition[%d].to %q is not a defined stage", id, index, transition.To))
		} else if transition.To == id {
			problems = append(problems, fmt.Sprintf("stage %q transition[%d].to %q is a self-loop", id, index, transition.To))
		}
	}
	return problems
}

// validateMemory checks the declared memory scopes are known and unique.
func (p *Pack) validateMemory() []string {
	var problems []string
	seen := map[MemoryScope]bool{}
	for _, scope := range p.Memory.Reads {
		switch scope {
		case ScopeProject, ScopeUser, ScopeOrg:
			if seen[scope] {
				problems = append(problems, fmt.Sprintf("memory.reads lists %q more than once", scope))
			}
			seen[scope] = true
		default:
			problems = append(problems, fmt.Sprintf("memory.reads %q is not one of {project, user, org}", scope))
		}
	}
	return problems
}

// validateBudgets checks the recursion caps are non-negative.
func (p *Pack) validateBudgets() []string {
	var problems []string
	if p.Budgets.FixCycles < 0 {
		problems = append(problems, "budgets.fix_cycles must be non-negative")
	}
	if p.Budgets.AskToEdit < 0 {
		problems = append(problems, "budgets.ask_to_edit must be non-negative")
	}
	return problems
}

// validateGraph checks (1) every stage is reachable from entry, and (2) at
// least one terminal stage is reachable. These only run when the per-stage /
// transition-ref checks already passed, so we can assume refs resolve.
func validateGraph(p *Pack) []string {
	var problems []string

	// reachability from entry
	visited := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		for _, tr := range p.Stages[id].Transitions {
			walk(tr.To)
		}
	}
	walk(p.Entry)

	for id := range p.Stages {
		if !visited[id] {
			problems = append(problems, fmt.Sprintf("stage %q is not reachable from entry %q", id, p.Entry))
		}
	}

	// at least one reachable terminal
	hasTerminal := false
	for id := range visited {
		if p.Stages[id].Terminal() {
			hasTerminal = true
			break
		}
	}
	if !hasTerminal {
		problems = append(problems, "no terminal stage is reachable from entry (pipeline has no exit)")
	}

	return problems
}

// isSemver accepts MAJOR.MINOR.PATCH with non-negative integers. Pre-release
// tags and the rest of full semver are deferred — packs need simple,
// comparable versions now.
func isSemver(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			// disallow leading zeros and empty segments
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
		n, err := parseUint(part)
		if err != nil || n < 0 {
			return false
		}
	}
	return true
}

func parseUint(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

func joinErrors(msgs []string) string {
	return strings.Join(msgs, "; ")
}

// validateCheckPolicy checks the pack's check references are well-formed: names
// are non-empty, unique within each list, and not listed as both required and
// optional. It does NOT check the names exist in a project registry — the pack
// is portable across projects, so that cross-check belongs to checks.Resolve at
// run time.
func validateCheckPolicy(policy CheckPolicy) []string {
	var problems []string
	seenRequired := make(map[string]bool, len(policy.Required))
	for index, name := range policy.Required {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, fmt.Sprintf("checks.required[%d] is empty", index))
			continue
		}
		if seenRequired[name] {
			problems = append(problems, fmt.Sprintf("checks.required lists %q more than once", name))
		}
		seenRequired[name] = true
	}
	seenOptional := make(map[string]bool, len(policy.Optional))
	for index, name := range policy.Optional {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, fmt.Sprintf("checks.optional[%d] is empty", index))
			continue
		}
		if seenRequired[name] {
			problems = append(problems, fmt.Sprintf("checks: %q appears in both required and optional", name))
		}
		if seenOptional[name] {
			problems = append(problems, fmt.Sprintf("checks.optional lists %q more than once", name))
		}
		seenOptional[name] = true
	}
	return problems
}
