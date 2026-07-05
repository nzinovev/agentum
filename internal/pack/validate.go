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

	// api + identity
	if p.API != APIVersion {
		problems = append(problems, fmt.Sprintf("api must be %q, got %q", APIVersion, p.API))
	}
	if strings.TrimSpace(p.Pack.Name) == "" {
		problems = append(problems, "pack.name is required")
	}
	if !isSemver(p.Pack.Version) {
		problems = append(problems, fmt.Sprintf("pack.version must be semver MAJOR.MINOR.PATCH, got %q", p.Pack.Version))
	}

	// stages present at all
	if len(p.Stages) == 0 {
		problems = append(problems, "stages is empty")
	}

	// entry
	if p.Entry == "" {
		problems = append(problems, "entry is required")
	} else if _, ok := p.Stages[p.Entry]; !ok {
		problems = append(problems, fmt.Sprintf("entry %q is not defined in stages", p.Entry))
	}

	// per-stage + transition refs
	gateOK := map[Gate]bool{
		GateAuto: true, GateAutoIfClean: true, GateAutoOnApproval: true,
		GateHumanApproval: true, GateHumanFinal: true, GateHumanEdit: true,
	}
	for id, st := range p.Stages {
		if id == "" {
			problems = append(problems, "stage has an empty id")
			continue
		}
		if st.Terminal() {
			// Terminal stages are engine states (done/cancelled/failed). They
			// carry no prompt and no gate — there is no agent invocation to
			// control. Gate is ignored if present.
			if st.Prompt != "" {
				problems = append(problems, fmt.Sprintf("stage %q is terminal and must not declare a prompt", id))
			}
			continue
		}
		// Non-terminal: gate must be a known value.
		if !gateOK[st.Gate] {
			problems = append(problems, fmt.Sprintf("stage %q gate %q is not one of the six-value vocabulary", id, st.Gate))
		}
		if st.Prompt == "" {
			problems = append(problems, fmt.Sprintf("stage %q is non-terminal and requires a prompt", id))
		} else if p.PromptText[id] == "" {
			problems = append(problems, fmt.Sprintf("stage %q prompt %q loaded empty", id, st.Prompt))
		}
		for i, tr := range st.Transitions {
			if tr.To == "" {
				problems = append(problems, fmt.Sprintf("stage %q transition[%d].to is empty", id, i))
				continue
			}
			if _, ok := p.Stages[tr.To]; !ok {
				problems = append(problems, fmt.Sprintf("stage %q transition[%d].to %q is not a defined stage", id, i, tr.To))
			} else if tr.To == id {
				problems = append(problems, fmt.Sprintf("stage %q transition[%d].to %q is a self-loop", id, i, tr.To))
			}
		}
	}

	// memory scopes
	seen := map[MemoryScope]bool{}
	for _, sc := range p.Memory.Reads {
		switch sc {
		case ScopeProject, ScopeUser, ScopeOrg:
			if seen[sc] {
				problems = append(problems, fmt.Sprintf("memory.reads lists %q more than once", sc))
			}
			seen[sc] = true
		default:
			problems = append(problems, fmt.Sprintf("memory.reads %q is not one of {project, user, org}", sc))
		}
	}

	// budgets
	if p.Budgets.FixCycles < 0 {
		problems = append(problems, "budgets.fix_cycles must be non-negative")
	}
	if p.Budgets.AskToEdit < 0 {
		problems = append(problems, "budgets.ask_to_edit must be non-negative")
	}

	// reachability + terminal-exit (only meaningful once stages resolve)
	if len(problems) == 0 {
		if errs := validateGraph(p); len(errs) > 0 {
			problems = append(problems, errs...)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("pack: invalid: %s", joinErrors(problems))
	}
	return nil
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
