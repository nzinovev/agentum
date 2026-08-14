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
	problems = append(problems, p.validateApprovals()...)

	// reachability + terminal-exit + bounded cycles (only meaningful once the
	// structural checks already passed, so we can assume refs resolve and
	// conditions parse).
	if len(problems) == 0 {
		problems = append(problems, validateGraph(p)...)
		problems = append(problems, validateBoundedCycles(p)...)
		// Approval reachability depends on the graph being well-formed, so it
		// runs last (ADR 0003 D3 layer 3 — static, advisory; the runtime lock
		// in computeProfile is the real guarantee).
		problems = append(problems, validateApprovalReachability(p)...)
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
	problems = append(problems, validateConditionalEdges(id, stage)...)
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

// validateConditionalEdges enforces the D6 condition rules per stage:
//
//  1. Every non-empty condition parses under D1's grammar with a known subject
//     and a literal from that subject's closed set. ParseCondition already does
//     the closed-set check; a parse failure is reported with the stage id.
//  2. At most one unconditional transition per stage, and it must be last — a
//     fallback declared before a conditional edge makes that edge dead.
//  3. Totality: a stage with conditional transitions must end with an
//     unconditional fallback OR exhaustively cover one closed enum subject
//     (every member of verdict/status appears with ==). fix_cycles conditions
//     can never establish coverage, so a stage using them requires the fallback.
//
// The rules run in addition to validateTransitions (targets exist, no
// self-loops) and accumulate problems without short-circuiting.
func validateConditionalEdges(id string, stage Stage) []string {
	var problems []string
	hasConditional := false
	unconditionalIndex := -1
	verdictLiteralsSeen := map[string]bool{}
	statusLiteralsSeen := map[string]bool{}
	hasFixCycles := false
	for index, transition := range stage.Transitions {
		condition, parseErr := ParseCondition(transition.Condition)
		if parseErr != nil {
			problems = append(problems, fmt.Sprintf("stage %q transition[%d].condition: %v", id, index, parseErr))
			continue
		}
		if condition.IsUnconditional() {
			if unconditionalIndex >= 0 {
				problems = append(problems, fmt.Sprintf("stage %q has more than one unconditional transition", id))
			}
			unconditionalIndex = index
			continue
		}
		hasConditional = true
		switch condition.subject {
		case SubjectVerdict:
			verdictLiteralsSeen[condition.enumTerm] = true
		case SubjectStatus:
			statusLiteralsSeen[condition.enumTerm] = true
		case SubjectFixCycles:
			hasFixCycles = true
		}
	}
	// Rule 2: the unconditional edge (if any) must be last. A fallback before a
	// conditional edge would shadow it — the first-match-wins resolver never
	// reaches the conditional.
	if unconditionalIndex >= 0 && hasConditional && unconditionalIndex < len(stage.Transitions)-1 {
		problems = append(problems, fmt.Sprintf("stage %q unconditional transition at [%d] must be last (a fallback before a conditional edge makes the edge dead)", id, unconditionalIndex))
	}
	// Rule 3: totality. A branching stage needs a fallback OR an exhaustive
	// enum cover. fix_cycles alone never covers.
	if hasConditional && unconditionalIndex < 0 {
		if hasFixCycles && !enumComplete(verdictLiteralsSeen, verdictLiterals) && !enumComplete(statusLiteralsSeen, statusLiterals) {
			problems = append(problems, fmt.Sprintf("stage %q uses fix_cycles conditions without a fallback and without an exhaustive verdict/status cover", id))
		}
		if !enumComplete(verdictLiteralsSeen, verdictLiterals) && !enumComplete(statusLiteralsSeen, statusLiterals) {
			problems = append(problems, fmt.Sprintf("stage %q conditional transitions do not cover verdict/status exhaustively and there is no fallback", id))
		}
	}
	return problems
}

// enumComplete reports whether seen covers every member of the closed literal
// set. The verdict/status subjects are the only enum subjects; fix_cycles is
// never passed here.
func enumComplete(seen map[string]bool, closed map[string]bool) bool {
	if len(seen) == 0 {
		return false
	}
	for literal := range closed {
		if !seen[literal] {
			return false
		}
	}
	return true
}

// validateBoundedCycles enforces D6 rule 4: every non-trivial strongly
// connected component reachable from entry must contain at least one fixer-role
// stage, and budgets.fix_cycles >= 1. Per-component, not whole-graph: a pack
// with one bounded loop and one unbounded loop must be rejected.
//
// The existing validateGraph walk cannot do this — it is a reachability DFS
// over a visited map and never observes a back-edge. This function runs a
// Tarjan SCC decomposition over the entry-reachable subgraph. A non-trivial
// component (size > 1, or a self-loop) is a cycle; self-loops are already
// rejected by validateTransitions, so here a non-trivial component has size > 1.
// The implication is one-way: a budget without a cycle stays valid.
// packs/minimal and testdata/minimal declare fix_cycles with a linear graph
// and must keep validating.
func validateBoundedCycles(p *Pack) []string {
	reachable := reachableStages(p)
	components := tarjanSCC(p, reachable)
	var problems []string
	for _, component := range components {
		if !isCyclicComponent(component, p) {
			continue
		}
		hasFixer := false
		for _, stageID := range component {
			if EffectiveRole(stageID, p.Stages[stageID]) == "fixer" {
				hasFixer = true
				break
			}
		}
		if !hasFixer {
			problems = append(problems, fmt.Sprintf("cycle %v has no fixer-role stage; a loop with no budget-boundable role cannot validate", component))
		}
		if p.Budgets.FixCycles < 1 {
			problems = append(problems, fmt.Sprintf("cycle %v requires budgets.fix_cycles >= 1", component))
		}
	}
	return problems
}

// reachableStages returns the set of stage ids reachable from entry, including
// entry itself. Terminal stages (no transitions) contribute no successors.
func reachableStages(p *Pack) map[string]bool {
	visited := map[string]bool{}
	var walk func(string)
	walk = func(stageID string) {
		if visited[stageID] {
			return
		}
		visited[stageID] = true
		for _, transition := range p.Stages[stageID].Transitions {
			walk(transition.To)
		}
	}
	walk(p.Entry)
	return visited
}

// isCyclicComponent reports whether a Tarjan component is a cycle. A component
// of size > 1 is always cyclic. A single-node component is cyclic only if the
// node has a self-edge — already rejected by validateTransitions, so this
// returns false for singletons. Kept defensive so a future relaxation of the
// self-loop rule does not silently let a self-loop bypass the budget check.
func isCyclicComponent(component []string, p *Pack) bool {
	if len(component) > 1 {
		return true
	}
	if len(component) == 0 {
		return false
	}
	stageID := component[0]
	for _, transition := range p.Stages[stageID].Transitions {
		if transition.To == stageID {
			return true
		}
	}
	return false
}

// tarjanSCC returns the strongly connected components of the entry-reachable
// subgraph, restricted to nodes in reachable. Each component is a slice of
// stage ids; the order of components and of ids within a component is not
// specified. The iterative form is used (no recursion) so a deep pack graph
// cannot overflow the goroutine stack.
func tarjanSCC(p *Pack, reachable map[string]bool) [][]string {
	// Iterative Tarjan. The classic recursive algorithm tracks an index and a
	// low-link per node plus an on-stack flag; we mirror that with explicit
	// frames so the depth of the DFS is bounded by available heap, not stack.
	type frame struct {
		stageID    string
		successors []string
		cursor     int
	}
	var index int32
	indices := map[string]int32{}
	lowLinks := map[string]int32{}
	onStack := map[string]bool{}
	var stack []string
	var components [][]string

	successorsOf := func(stageID string) []string {
		var successors []string
		for _, transition := range p.Stages[stageID].Transitions {
			if reachable[transition.To] {
				successors = append(successors, transition.To)
			}
		}
		return successors
	}

	var frames []frame
	for startID := range reachable {
		if alreadyIndexed(startID, indices) {
			continue
		}
		frames = append(frames, frame{stageID: startID, successors: successorsOf(startID)})
		for len(frames) > 0 {
			top := &frames[len(frames)-1]
			if top.cursor == 0 {
				index++
				indices[top.stageID] = index
				lowLinks[top.stageID] = index
				stack = append(stack, top.stageID)
				onStack[top.stageID] = true
			}
			if top.cursor < len(top.successors) {
				successor := top.successors[top.cursor]
				top.cursor++
				if !alreadyIndexed(successor, indices) {
					frames = append(frames, frame{stageID: successor, successors: successorsOf(successor)})
				} else if onStack[successor] {
					if indices[successor] < lowLinks[top.stageID] {
						lowLinks[top.stageID] = indices[successor]
					}
				}
				continue
			}
			// All successors visited: this frame is done.
			if lowLinks[top.stageID] == indices[top.stageID] {
				component := []string{}
				for {
					node := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[node] = false
					component = append(component, node)
					if node == top.stageID {
						break
					}
				}
				components = append(components, component)
			}
			frames = frames[:len(frames)-1]
			if len(frames) > 0 {
				parent := &frames[len(frames)-1]
				if lowLinks[top.stageID] < lowLinks[parent.stageID] {
					lowLinks[parent.stageID] = lowLinks[top.stageID]
				}
			}
		}
	}
	return components
}

// alreadyIndexed reports whether stageID has been assigned an SCC index.
func alreadyIndexed(stageID string, indices map[string]int32) bool {
	_, present := indices[stageID]
	return present
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

// validateApprovals checks each approval declaration is well-formed: names are
// non-empty and unique; the stage is defined and non-terminal (a terminal stage
// has no gate to advance past); the artifact is a bare file name (no path
// separators — it lives in the stage's orchestrator-constructed artifact dir);
// and unlocks is a known member of the closed set. Collected into the existing
// multi-error pass so a pack author sees every issue at once.
func (p *Pack) validateApprovals() []string {
	if len(p.Approvals) == 0 {
		return nil // a pack with no approvals behaves exactly as today
	}
	var problems []string
	seenNames := make(map[string]bool, len(p.Approvals))
	sourceWriteCount := 0
	for index, approval := range p.Approvals {
		if strings.TrimSpace(approval.Name) == "" {
			problems = append(problems, fmt.Sprintf("approvals[%d].name is empty", index))
		} else if seenNames[approval.Name] {
			problems = append(problems, fmt.Sprintf("approvals lists name %q more than once", approval.Name))
		}
		seenNames[approval.Name] = true

		stage, stageDefined := p.Stages[approval.Stage]
		if !stageDefined {
			problems = append(problems, fmt.Sprintf("approvals[%d].stage %q is not a defined stage", index, approval.Stage))
		} else if stage.Terminal() {
			// A terminal stage has no outgoing transition; a human cannot
			// "advance past" it, so it cannot be an approval gate.
			problems = append(problems, fmt.Sprintf("approvals[%d].stage %q is terminal and cannot host an approval gate", index, approval.Stage))
		}

		if approval.Artifact == "" {
			problems = append(problems, fmt.Sprintf("approvals[%d].artifact is empty", index))
		} else if strings.ContainsAny(approval.Artifact, `/\`) {
			// The artifact names a file inside the stage's artifact dir, not a
			// path. A separator here would let an approval name a file outside
			// the orchestrator-constructed dir, re-opening the containment seam.
			problems = append(problems, fmt.Sprintf("approvals[%d].artifact %q must be a bare file name, not a path", index, approval.Artifact))
		}

		if !knownUnlocks[Unlock(approval.Unlocks)] {
			problems = append(problems, fmt.Sprintf("approvals[%d].unlocks %q is not one of the known unlock names {source_write}", index, approval.Unlocks))
		} else if Unlock(approval.Unlocks) == UnlockSourceWrite {
			// SourceWriteApproval() returns the first source_write approval; a
			// second one would be silently inert (the runner never reads it).
			// At most one is allowed so the declaration and the runtime agree.
			sourceWriteCount++
			if sourceWriteCount > 1 {
				problems = append(problems, "approvals declares more than one source_write approval; at most one is allowed")
			}
		}
	}
	return problems
}

// validateApprovalReachability enforces ADR 0003 D3 layer 3: when a
// source_write approval exists, every path from entry to a source-writing stage
// (implementer or fixer role) must pass through the approval stage. This is a
// static, advisory check that catches authoring mistakes at load time — the
// real guarantee is the capability withholding in computeProfile, which holds
// even if this rule is wrong. It runs only after the graph is known well-formed
// (every stage reachable, no dangling refs).
func validateApprovalReachability(p *Pack) []string {
	sourceApproval, hasSourceApproval := p.SourceWriteApproval()
	if !hasSourceApproval {
		return nil
	}
	sourceStages := p.SourceWritingStages()
	if len(sourceStages) == 0 {
		return nil // nothing to protect; the withholding would be a no-op
	}
	// Walk from entry; a stage is "reachable without approval" if there is a
	// path from entry to it that never traverses the approval stage. The
	// approval stage itself is the gate, so we stop expanding through it.
	approvalStage := sourceApproval.Stage
	reachableWithoutApproval := map[string]bool{}
	var walk func(stageID string)
	walk = func(stageID string) {
		if reachableWithoutApproval[stageID] {
			return
		}
		reachableWithoutApproval[stageID] = true
		if stageID == approvalStage {
			return // do not expand through the gate
		}
		for _, transition := range p.Stages[stageID].Transitions {
			walk(transition.To)
		}
	}
	walk(p.Entry)

	var problems []string
	for _, stageID := range sourceStages {
		if reachableWithoutApproval[stageID] {
			problems = append(problems, fmt.Sprintf("source-writing stage %q is reachable from entry %q without passing the approval stage %q", stageID, p.Entry, approvalStage))
		}
	}
	return problems
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
