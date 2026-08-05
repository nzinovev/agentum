package pack

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Subject is the closed set of values a condition may read. Adding a member is
// an additive format change; the validator's closed-set check (validate.go)
// must learn the new subject's literals in the same change.
type Subject string

const (
	// SubjectVerdict reads the reviewer's own verdict.json, parsed by the
	// orchestrator (not the agent's prose). Literals: approved,
	// changes_requested.
	SubjectVerdict Subject = "verdict"
	// SubjectStatus reads result.json.status of the stage that is transitioning.
	// Literals: complete, partial, blocked.
	SubjectStatus Subject = "status"
	// SubjectFixCycles reads the durable fix-cycle counter of the current run.
	// Comparators: < <= > >= ==; the right-hand side is an integer or the
	// keyword `budget` (= budgets.fix_cycles). A fix_cycles condition can never
	// establish transition totality, so a stage using it requires a fallback.
	SubjectFixCycles Subject = "fix_cycles"
)

// verdictLiterals and statusLiterals are the closed literal sets for the enum
// subjects. They are the source of truth for both the parser (accept) and the
// validator's totality check (cover). Keep them in sync with the agent result
// / verdict contracts.
var (
	verdictLiterals = map[string]bool{
		"approved":          true,
		"changes_requested": true,
	}
	statusLiterals = map[string]bool{
		"complete": true,
		"partial":  true,
		"blocked":  true,
	}
)

// Comparator is the closed set of comparison operators a count_term may use.
// Enum terms are always ==, so the comparator only applies to fix_cycles.
type Comparator string

const (
	ComparatorEqual          Comparator = "=="
	ComparatorLess           Comparator = "<"
	ComparatorLessOrEqual    Comparator = "<="
	ComparatorGreater        Comparator = ">"
	ComparatorGreaterOrEqual Comparator = ">="
)

// Condition is one parsed transition condition per the D1 grammar:
//
//	condition := enum_term | count_term
//	enum_term  := ("verdict" | "status") "==" '"' literal '"'
//	count_term := "fix_cycles" ("<" | "<=" | ">" | ">=" | "==") (integer | "budget")
//
// The zero Condition is the unconditional edge: it matches every input. An
// empty condition string parses to the zero value, so existing single-target
// packs (no condition field) stay valid and unconditional.
type Condition struct {
	raw string

	subject Subject
	// enumTerm holds the literal for a verdict/status term. Empty for a
	// fix_cycles term and for the unconditional zero value.
	enumTerm string
	// comparator is set for a fix_cycles term; empty otherwise.
	comparator Comparator
	// intValue holds the parsed integer for a fix_cycles term whose right-hand
	// side is an integer literal. Zero when the term uses the `budget` keyword
	// or for non-fix_cycles terms.
	intValue int
	// isBudgetKeyword is true for a fix_cycles term whose right-hand side is
	// the keyword `budget` (resolves to budgets.fix_cycles at match time).
	isBudgetKeyword bool
}

// IsUnconditional reports whether this is the zero-value always-match edge.
func (condition Condition) IsUnconditional() bool {
	return condition.subject == ""
}

// Subject returns the parsed subject, or "" for the unconditional edge. Used by
// the validator's totality / coverage checks and by Stage.SourcesVerdict.
func (condition Condition) Subject() Subject { return condition.subject }

// Match reports whether input satisfies the condition. It is pure: it reads
// only the passed scalars. An unconditional edge matches anything. The budget
// keyword resolves to input.Budget, so `fix_cycles < budget` is "still under
// the cap". A fix_cycles term with the > / >= comparator reads input.FixCycles.
func (condition Condition) Match(input ConditionInput) (bool, error) {
	if condition.IsUnconditional() {
		return true, nil
	}
	switch condition.subject {
	case SubjectVerdict:
		return condition.enumTerm == input.Verdict, nil
	case SubjectStatus:
		return condition.enumTerm == input.Status, nil
	case SubjectFixCycles:
		right := condition.intValue
		if condition.isBudgetKeyword {
			right = input.Budget
		}
		switch condition.comparator {
		case ComparatorEqual:
			return input.FixCycles == right, nil
		case ComparatorLess:
			return input.FixCycles < right, nil
		case ComparatorLessOrEqual:
			return input.FixCycles <= right, nil
		case ComparatorGreater:
			return input.FixCycles > right, nil
		case ComparatorGreaterOrEqual:
			return input.FixCycles >= right, nil
		default:
			return false, fmt.Errorf("condition: %q: unknown comparator %q", condition.raw, condition.comparator)
		}
	default:
		// Post-validation this is unreachable; reaching it means the validator
		// was bypassed. Surface it rather than silently not matching.
		return false, fmt.Errorf("condition: %q: unknown subject %q", condition.raw, condition.subject)
	}
}

// ConditionInput is the scalar bundle Match reads. The runner fills it from
// durable state (verdict artifact, result.json, the cycle counter, the budget)
// before evaluating a stage's transitions. All fields are value types so the
// struct copies cheaply and has no aliasing.
type ConditionInput struct {
	Verdict   string // the parsed verdict.json verdict ("approved" | "changes_requested")
	Status    string // the transitioning stage's result.json status
	FixCycles int    // fixer-stage entries already made in this run
	Budget    int    // budgets.fix_cycles
}

// ParseCondition parses text under the D1 grammar. Whitespace is normalised
// (trimmed and runs collapsed) before matching, so `verdict=="approved"` and
// `verdict == "approved"` are equivalent. An empty (whitespace-only) string
// yields the unconditional zero value — this is how a transition with no
// condition field behaves.
func ParseCondition(text string) (Condition, error) {
	normalised := normaliseWhitespace(text)
	if normalised == "" {
		return Condition{raw: text}, nil
	}

	subject, rest, ok := splitSubject(normalised)
	if !ok {
		return Condition{}, fmt.Errorf("condition: %q: must start with one of {verdict, status, fix_cycles}", text)
	}
	switch Subject(subject) {
	case SubjectVerdict, SubjectStatus:
		return parseEnumTerm(text, subject, rest)
	case SubjectFixCycles:
		return parseCountTerm(text, rest)
	default:
		return Condition{}, fmt.Errorf("condition: %q: unknown subject %q (want verdict, status, or fix_cycles)", text, subject)
	}
}

// parseEnumTerm parses `<comparator> "<literal>"` for a verdict/status term.
// The comparator must be ==; negation is intentionally absent (D1) so totality
// stays decidable.
func parseEnumTerm(raw, subject, rest string) (Condition, error) {
	comparator, operand, ok := splitComparator(rest)
	if !ok {
		return Condition{}, fmt.Errorf("condition: %q: %s must be followed by a comparator and a quoted literal", raw, subject)
	}
	if Comparator(comparator) != ComparatorEqual {
		return Condition{}, fmt.Errorf("condition: %q: %s only supports == (got %q)", raw, subject, comparator)
	}
	literal, ok := unquote(operand)
	if !ok {
		return Condition{}, fmt.Errorf("condition: %q: %s literal must be a double-quoted string", raw, subject)
	}
	literals := statusLiterals
	if Subject(subject) == SubjectVerdict {
		literals = verdictLiterals
	}
	if !literals[literal] {
		return Condition{}, fmt.Errorf("condition: %q: %s literal %q is not in the closed set", raw, subject, literal)
	}
	return Condition{raw: raw, subject: Subject(subject), enumTerm: literal}, nil
}

// parseCountTerm parses `<comparator> (integer | "budget")` for a fix_cycles
// term. The integer must be non-negative; a leading sign is rejected so
// `fix_cycles == -1` is a parse error, not a silently-accepted edge case.
func parseCountTerm(raw, rest string) (Condition, error) {
	comparator, operand, ok := splitComparator(rest)
	if !ok {
		return Condition{}, fmt.Errorf("condition: %q: fix_cycles must be followed by a comparator and an operand", raw)
	}
	switch Comparator(comparator) {
	case ComparatorEqual, ComparatorLess, ComparatorLessOrEqual, ComparatorGreater, ComparatorGreaterOrEqual:
	default:
		return Condition{}, fmt.Errorf("condition: %q: fix_cycles comparator %q is not one of < <= > >= ==", raw, comparator)
	}
	condition := Condition{raw: raw, subject: SubjectFixCycles, comparator: Comparator(comparator)}
	if operand == budgetKeyword {
		condition.isBudgetKeyword = true
		return condition, nil
	}
	value, err := parseNonNegInt(operand)
	if err != nil {
		return Condition{}, fmt.Errorf("condition: %q: fix_cycles operand must be a non-negative integer or %q: %w", raw, budgetKeyword, err)
	}
	condition.intValue = value
	return condition, nil
}

const budgetKeyword = "budget"

// parseNonNegInt rejects leading signs and leading zeros (mirroring validate's
// isSemver discipline) so the operand is a canonical non-negative integer.
func parseNonNegInt(text string) (int, error) {
	if text == "" || len(text) > 1 && text[0] == '0' {
		return 0, errors.New("not a canonical non-negative integer")
	}
	for _, character := range text {
		if character < '0' || character > '9' {
			return 0, errors.New("not a non-negative integer")
		}
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("parse: %w", err)
	}
	return value, nil
}

// splitSubject separates the leading subject keyword from the rest. The subject
// is the longest prefix that is one of the known keywords; this lets the
// comparator splitter handle the remainder uniformly.
func splitSubject(text string) (subject, rest string, ok bool) {
	for _, candidate := range []string{string(SubjectFixCycles), string(SubjectVerdict), string(SubjectStatus)} {
		if text == candidate || strings.HasPrefix(text, candidate) && isTermBoundary(text[len(candidate)]) {
			return candidate, strings.TrimLeft(text[len(candidate):], " "), true
		}
	}
	return "", "", false
}

// isTermBoundary reports whether the byte at the subject/Rest junction is a
// legal separator (whitespace or a comparator starter). A subject keyword must
// not be a prefix of a longer identifier — `verdictx` must not parse as
// `verdict`.
func isTermBoundary(character byte) bool {
	if character == ' ' {
		return true
	}
	return isComparatorStart(character)
}

func isComparatorStart(character byte) bool {
	switch character {
	case '=', '<', '>':
		return true
	}
	return false
}

// notEqualToken is the rejected negation form. The grammar excludes != so
// totality stays decidable (D1); recognising it here lets parseEnumTerm /
// parseCountTerm produce a precise "not supported" error instead of a generic
// "missing comparator" message that would mislead a pack author who typed !=.
const notEqualToken = "!="

// splitComparator separates the leading comparator from its operand. Accepts
// ==, <=, >=, <, >; recognises != only to reject it with a precise error. A
// single `=` is rejected (the grammar requires ==) so a typo does not silently
// parse.
func splitComparator(text string) (comparator, operand string, ok bool) {
	text = strings.TrimLeft(text, " ")
	if text == "" {
		return "", "", false
	}
	switch {
	case strings.HasPrefix(text, notEqualToken):
		return notEqualToken, strings.TrimLeft(text[len(notEqualToken):], " "), true
	case strings.HasPrefix(text, string(ComparatorEqual)):
		return string(ComparatorEqual), strings.TrimLeft(text[len(ComparatorEqual):], " "), true
	case strings.HasPrefix(text, string(ComparatorLessOrEqual)):
		return string(ComparatorLessOrEqual), strings.TrimLeft(text[len(ComparatorLessOrEqual):], " "), true
	case strings.HasPrefix(text, string(ComparatorGreaterOrEqual)):
		return string(ComparatorGreaterOrEqual), strings.TrimLeft(text[len(ComparatorGreaterOrEqual):], " "), true
	case strings.HasPrefix(text, string(ComparatorLess)):
		return string(ComparatorLess), strings.TrimLeft(text[len(ComparatorLess):], " "), true
	case strings.HasPrefix(text, string(ComparatorGreater)):
		return string(ComparatorGreater), strings.TrimLeft(text[len(ComparatorGreater):], " "), true
	}
	return "", "", false
}

// unquote strips one layer of surrounding double quotes. The grammar requires
// the literal be quoted so a literal cannot run into the next token; a missing
// close quote is a parse error rather than a silent accept.
func unquote(text string) (string, bool) {
	if len(text) < 2 || text[0] != '"' || text[len(text)-1] != '"' {
		return "", false
	}
	return text[1 : len(text)-1], true
}

// normaliseWhitespace trims and collapses internal runs of spaces to one. Tabs
// and newlines are not part of the grammar; treat them as spaces so a pasted
// condition with odd whitespace still parses.
func normaliseWhitespace(text string) string {
	fields := strings.Fields(text)
	return strings.Join(fields, " ")
}

// NextTransition returns the first transition whose condition matches input, in
// declaration order. The first-match-wins rule supplies disjunction and
// precedence: a stage declares its edges most-specific first, fallback last.
// Returns (zero, false, nil) when no edge matches — the validator (D6 rule 3)
// makes this unreachable for a valid pack, so a caller that hits it treats the
// transition as a broken invariant.
func (stage Stage) NextTransition(input ConditionInput) (Transition, bool, error) {
	for _, transition := range stage.Transitions {
		condition, err := ParseCondition(transition.Condition)
		if err != nil {
			// A condition that fails to parse at match time means the validator
			// was bypassed; surface it precisely rather than skipping the edge.
			return Transition{}, false, fmt.Errorf("stage transition %q: %w", transition.Condition, err)
		}
		matched, err := condition.Match(input)
		if err != nil {
			return Transition{}, false, fmt.Errorf("stage transition %q: %w", transition.Condition, err)
		}
		if matched {
			return transition, true, nil
		}
	}
	return Transition{}, false, nil
}

// SourcesVerdict reports whether any of the stage's transitions carries a
// verdict condition. The runner uses it to decide whether to render the
// verdict contract into the routing block (commit 7) without string-scanning
// the condition text — parsing once here is the source of truth.
func (stage Stage) SourcesVerdict() bool {
	for _, transition := range stage.Transitions {
		condition, err := ParseCondition(transition.Condition)
		if err != nil {
			continue
		}
		if condition.Subject() == SubjectVerdict {
			return true
		}
	}
	return false
}
