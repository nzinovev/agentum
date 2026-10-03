// Package policy holds the host's policy floor: the rules every pack a run
// executes must satisfy, checked on the assembled pack before the run starts
// (and, for builtin packs, at server boot). The floor lives in host
// configuration and host code — never in the project repository — so a commit
// that brings a pack cannot also bring the terms of that pack's inspection.
//
// The floor adds an early, named refusal; it does not replace the runtime
// enforcement (capability withholding, monotonic check resolution, the FSM's
// final-review gate), which keeps holding whatever this package says.
package policy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/pack"
)

// Rule ids, stable for events and API responses.
const (
	// RuleApprovalPrecedesSourceWrite: a pack with source-writing stages
	// (implementer or fixer roles) declares a source_write approval that
	// precedes them on every path, on a stage whose gate stops for a human.
	RuleApprovalPrecedesSourceWrite = "approval_precedes_source_write"
	// RuleFinalReviewRequired: the graph completes only through a terminal
	// stage — engine states the runtime routes into awaiting_final_review.
	RuleFinalReviewRequired = "final_review_required"
	// RuleMandatoryChecksImmutable: the pack's check requests name registered
	// checks and never downgrade one the registry holds required.
	RuleMandatoryChecksImmutable = "mandatory_checks_immutable"
	// RuleCapabilitiesWithinHost: every capability token the pack declares
	// (pack-level and stage-level) falls inside the host's capability set.
	RuleCapabilitiesWithinHost = "capabilities_within_host"
)

// Layer names where an offending value came from, so a violation tells the
// operator which side to fix.
type Layer string

const (
	// LayerBuiltin: the value came from a builtin pack manifest (the base of
	// an inheritance included — overrides cannot patch the graph, the check
	// requests, or the capability list).
	LayerBuiltin Layer = "builtin pack"
	// LayerProject: the value came from the project's own manifest.
	LayerProject Layer = "project pack"
)

// Violation is one floor rule broken, with the layer the offending value came
// from and a message naming the concrete field.
type Violation struct {
	Rule    string
	Layer   Layer
	Message string
}

// String renders the violation for logs and failure messages:
// "<rule> (<layer>): <message>".
func (violation Violation) String() string {
	return fmt.Sprintf("%s (%s): %s", violation.Rule, violation.Layer, violation.Message)
}

// Target is the assembled pack the floor evaluates, with the inputs the rules
// need: where its bytes came from, which fields a project layer set, the
// host's capability set, and — when available — the project's check registry.
type Target struct {
	Pack *pack.Pack
	// Origin is the resolver-assigned origin (builtin / project /
	// project+builtin); it decides layer attribution.
	Origin pack.Origin
	// FieldOrigins carries the inherited pack's provenance (which fields the
	// overrides document set); nil for builtin and project-manifest packs.
	FieldOrigins map[string]string
	// HostCaps overrides the floor's configured set for this evaluation when
	// non-empty (evaluations share one floor; the field exists so a boot check
	// and a run check cannot drift on the input).
	HostCaps []caps.Category
	// Registry is the project's check registry read from the run's
	// base_commit. Nil skips rule 3 — the boot check over builtin packs has no
	// registry, and for builtin packs the monotonic checks.Resolve at run time
	// is the guarantee.
	Registry *checks.Registry
}

// Floor is the host's policy floor. It carries the host's capability set —
// the configured host set, or the adapter's declared set when nothing is
// configured — and no switch to turn rules off: a floor with an off switch is
// a suggestion.
type Floor struct {
	HostCaps []caps.Category
}

// NewFloor builds the floor over the host's capability set.
func NewFloor(hostCaps []caps.Category) *Floor {
	return &Floor{HostCaps: hostCaps}
}

// Check evaluates every rule against the target and returns the violations,
// rule by rule; an empty result admits the pack. Rules tolerate a structurally
// broken pack (the caller may floor before validating) by skipping what they
// cannot walk — the validator's report covers those defects.
func (floor *Floor) Check(target Target) []Violation {
	hostCaps := target.HostCaps
	if len(hostCaps) == 0 {
		hostCaps = floor.HostCaps
	}
	target.HostCaps = hostCaps
	var violations []Violation
	violations = append(violations, ruleApprovalPrecedesSourceWrite(target)...)
	violations = append(violations, ruleFinalReviewRequired(target)...)
	violations = append(violations, ruleMandatoryChecksImmutable(target)...)
	violations = append(violations, ruleCapabilitiesWithinHost(target)...)
	return violations
}

// graphLayer names the layer that declared the graph (stages, transitions,
// approvals): the builtin base for an inherited pack — overrides cannot patch
// the graph — and the project manifest otherwise.
func graphLayer(target Target) Layer {
	if target.Origin == pack.OriginProject {
		return LayerProject
	}
	return LayerBuiltin
}

// fieldLayer names the layer a single patched field came from: the project
// layer when the overrides provenance records it, else the layer the rest of
// the pack came from.
func fieldLayer(target Target, field string) Layer {
	if origin, patched := target.FieldOrigins[field]; patched && origin == "project" {
		return LayerProject
	}
	return graphLayer(target)
}

// ruleApprovalPrecedesSourceWrite evaluates rule 1 in three parts: a pack with
// source-writing stages declares a source_write approval; every entry path to
// a source-writing stage passes the approval's stage; and the approval's stage
// carries a gate that stops for a human. The third part is load-bearing: an
// approval on an auto-gated stage lets the runner slide past it with no
// run_approvals row, and the run then stalls at the first source-writing stage
// with source-write held back.
func ruleApprovalPrecedesSourceWrite(target Target) []Violation {
	sourceStages := target.Pack.SourceWritingStages()
	if len(sourceStages) == 0 {
		// No source-writing stage: nothing the approval must precede. The
		// withholding never engages for such a pack.
		return nil
	}
	sort.Strings(sourceStages)
	var violations []Violation
	approval, hasApproval := target.Pack.SourceWriteApproval()
	if !hasApproval {
		return []Violation{{
			Rule:    RuleApprovalPrecedesSourceWrite,
			Layer:   graphLayer(target),
			Message: fmt.Sprintf("pack has source-writing stages (%s) but declares no source_write approval", strings.Join(sourceStages, ", ")),
		}}
	}
	for _, stageID := range target.Pack.SourceWriteGateLeaks() {
		violations = append(violations, Violation{
			Rule:  RuleApprovalPrecedesSourceWrite,
			Layer: graphLayer(target),
			Message: fmt.Sprintf("source-writing stage %q is reachable from entry %q without passing the approval stage %q",
				stageID, target.Pack.Entry, approval.Stage),
		})
	}
	if stage, defined := target.Pack.Stages[approval.Stage]; defined && !stage.Gate.PausesForHuman() {
		violations = append(violations, Violation{
			Rule:  RuleApprovalPrecedesSourceWrite,
			Layer: fieldLayer(target, "stages."+approval.Stage+".gate"),
			Message: fmt.Sprintf("approval stage %q gate %q does not stop for a human; the approval row would never be written",
				approval.Stage, stage.Gate),
		})
	}
	return violations
}

// ruleFinalReviewRequired asserts the graph completes only through a terminal
// stage. The runtime routes every terminal completion into
// awaiting_final_review, and the FSM reaches done only through human approval
// — so the static property is that an entry-reachable terminal exists and
// carries no prompt (a terminal is an engine state, not an agent invocation).
// Pack validation enforces the same property at load time; this rule restates
// it under its floor name so the invariant has a named owner here too.
func ruleFinalReviewRequired(target Target) []Violation {
	if target.Pack.Entry == "" {
		// A broken entry is the validator's report; there is no graph to walk.
		return nil
	}
	if _, entryDefined := target.Pack.Stages[target.Pack.Entry]; !entryDefined {
		return nil
	}
	reachable := map[string]bool{target.Pack.Entry: true}
	queue := []string{target.Pack.Entry}
	hasTerminal := false
	var violations []Violation
	for len(queue) > 0 {
		stageID := queue[0]
		queue = queue[1:]
		stage := target.Pack.Stages[stageID]
		if stage.Terminal() {
			hasTerminal = true
			if stage.Prompt != "" {
				violations = append(violations, Violation{
					Rule:  RuleFinalReviewRequired,
					Layer: graphLayer(target),
					Message: fmt.Sprintf("terminal stage %q declares a prompt; a terminal is an engine state the run routes to awaiting_final_review, not an invocation",
						stageID),
				})
			}
			continue
		}
		for _, transition := range stage.Transitions {
			if _, defined := target.Pack.Stages[transition.To]; defined && !reachable[transition.To] {
				reachable[transition.To] = true
				queue = append(queue, transition.To)
			}
		}
	}
	if !hasTerminal {
		violations = append(violations, Violation{
			Rule:  RuleFinalReviewRequired,
			Layer: graphLayer(target),
			Message: fmt.Sprintf("no terminal stage is reachable from entry %q; the run can never reach awaiting_final_review",
				target.Pack.Entry),
		})
	}
	return violations
}

// ruleMandatoryChecksImmutable evaluates rule 3 over the pack's check
// requests: every name must exist in the project's registry — the same
// refusal checks.Resolve applies at run time, caught before the run starts —
// and a check the registry holds required must not appear in the pack's
// optional list, the one YAML-expressible form of "drop this mandatory
// check". Runtime resolution stays monotonic regardless (a required baseline
// item never drops to optional); this rule turns the attempt into a named
// refusal. Checks come from the manifest layer; overrides cannot patch them.
func ruleMandatoryChecksImmutable(target Target) []Violation {
	if target.Registry == nil {
		return nil
	}
	layer := graphLayer(target)
	var violations []Violation
	for _, name := range target.Pack.Checks.Required {
		if _, known := target.Registry.Get(name); !known {
			violations = append(violations, Violation{
				Rule:    RuleMandatoryChecksImmutable,
				Layer:   layer,
				Message: fmt.Sprintf("checks.required names %q, which the project registry (.agentum.yaml) does not define", name),
			})
		}
	}
	for _, name := range target.Pack.Checks.Optional {
		definition, known := target.Registry.Get(name)
		if !known {
			violations = append(violations, Violation{
				Rule:    RuleMandatoryChecksImmutable,
				Layer:   layer,
				Message: fmt.Sprintf("checks.optional names %q, which the project registry (.agentum.yaml) does not define", name),
			})
			continue
		}
		if definition.Required {
			violations = append(violations, Violation{
				Rule:  RuleMandatoryChecksImmutable,
				Layer: layer,
				Message: fmt.Sprintf("checks.optional lists %q, which the project registry holds required; a mandatory check cannot be downgraded",
					name),
			})
		}
	}
	return violations
}

// ruleCapabilitiesWithinHost evaluates rule 4: every capability token the
// pack declares — pack-level and stage-level — falls inside the host's
// capability set. At run time the profile intersection silently drops tokens
// the host does not carry; the floor turns that into a loud early refusal, so
// a pack written for a wider host fails to start instead of running with less
// than it declared. mcp.* and skill.* tokens are refused here for the same
// reason the adapter refuses them in a profile: no declared enforcement.
func ruleCapabilitiesWithinHost(target Target) []Violation {
	hostCategories := make(map[caps.Category]bool, len(target.HostCaps))
	for _, category := range target.HostCaps {
		hostCategories[category] = true
	}
	layer := graphLayer(target)
	var violations []Violation
	for _, token := range target.Pack.Capabilities {
		if !hostCategories[caps.CategoryOf(caps.Token(token))] {
			violations = append(violations, Violation{
				Rule:  RuleCapabilitiesWithinHost,
				Layer: layer,
				Message: fmt.Sprintf("pack capability %q is outside the host capability set (%s)",
					token, categoryNames(target.HostCaps)),
			})
		}
	}
	stageIDs := make([]string, 0, len(target.Pack.Stages))
	for stageID := range target.Pack.Stages {
		stageIDs = append(stageIDs, stageID)
	}
	sort.Strings(stageIDs)
	for _, stageID := range stageIDs {
		for _, token := range target.Pack.Stages[stageID].Capabilities {
			if !hostCategories[caps.CategoryOf(caps.Token(token))] {
				violations = append(violations, Violation{
					Rule:  RuleCapabilitiesWithinHost,
					Layer: layer,
					Message: fmt.Sprintf("stage %q capability %q is outside the host capability set (%s)",
						stageID, token, categoryNames(target.HostCaps)),
				})
			}
		}
	}
	return violations
}

// categoryNames renders the host set for violation messages.
func categoryNames(categories []caps.Category) string {
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, string(category))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
