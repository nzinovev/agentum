package pack

import (
	"testing"
)

func TestParseCondition_UnconditionalEmpty(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"", "   ", "\t\n"} {
		condition, err := ParseCondition(text)
		if err != nil {
			t.Fatalf("ParseCondition(%q): %v", text, err)
		}
		if !condition.IsUnconditional() {
			t.Errorf("ParseCondition(%q): not unconditional", text)
		}
		matched, err := condition.Match(ConditionInput{})
		if err != nil || !matched {
			t.Errorf("ParseCondition(%q): empty must match anything (matched=%v err=%v)", text, matched, err)
		}
	}
}

func TestParseCondition_EnumTerms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		subject Subject
		literal string
	}{
		{"verdict approved", `verdict == "approved"`, SubjectVerdict, "approved"},
		{"verdict changes_requested", `verdict == "changes_requested"`, SubjectVerdict, "changes_requested"},
		{"status complete", `status == "complete"`, SubjectStatus, "complete"},
		{"status partial", `status == "partial"`, SubjectStatus, "partial"},
		{"status blocked", `status == "blocked"`, SubjectStatus, "blocked"},
		{"verdict no spaces", `verdict=="approved"`, SubjectVerdict, "approved"},
		{"verdict extra spaces", `  verdict   ==   "approved"  `, SubjectVerdict, "approved"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			condition, err := ParseCondition(testCase.text)
			if err != nil {
				t.Fatalf("ParseCondition: %v", err)
			}
			if condition.Subject() != testCase.subject {
				t.Errorf("subject = %q, want %q", condition.Subject(), testCase.subject)
			}
			if condition.enumTerm != testCase.literal {
				t.Errorf("literal = %q, want %q", condition.enumTerm, testCase.literal)
			}
			matched, err := condition.Match(ConditionInput{
				Verdict: testCase.literal, Status: testCase.literal,
			})
			if err != nil || !matched {
				t.Errorf("match against equal input: matched=%v err=%v", matched, err)
			}
		})
	}
}

func TestParseCondition_CountTerms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		comparator Comparator
		intValue   int
		budget     bool
	}{
		{"eq integer", `fix_cycles == 2`, ComparatorEqual, 2, false},
		{"less", `fix_cycles < 3`, ComparatorLess, 3, false},
		{"leq", `fix_cycles <= 1`, ComparatorLessOrEqual, 1, false},
		{"greater", `fix_cycles > 0`, ComparatorGreater, 0, false},
		{"geq", `fix_cycles >= 2`, ComparatorGreaterOrEqual, 2, false},
		{"budget keyword", `fix_cycles < budget`, ComparatorLess, 0, true},
		{"no spaces", `fix_cycles==2`, ComparatorEqual, 2, false},
		{"extra spaces", `  fix_cycles   <=   budget  `, ComparatorLessOrEqual, 0, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			condition, err := ParseCondition(testCase.text)
			if err != nil {
				t.Fatalf("ParseCondition: %v", err)
			}
			if condition.Subject() != SubjectFixCycles {
				t.Errorf("subject = %q, want fix_cycles", condition.Subject())
			}
			if condition.comparator != testCase.comparator {
				t.Errorf("comparator = %q, want %q", condition.comparator, testCase.comparator)
			}
			if condition.isBudgetKeyword != testCase.budget {
				t.Errorf("isBudgetKeyword = %v, want %v", condition.isBudgetKeyword, testCase.budget)
			}
			if !testCase.budget && condition.intValue != testCase.intValue {
				t.Errorf("intValue = %d, want %d", condition.intValue, testCase.intValue)
			}
		})
	}
}

func TestParseCondition_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want string
	}{
		{"unknown subject", `temperature == "hot"`, "must start with one of"},
		{"subject prefix not keyword", `verdictx == "approved"`, "must start with one of"},
		{"verdict unknown literal", `verdict == "maybe"`, "not in the closed set"},
		{"status unknown literal", `status == "done"`, "not in the closed set"},
		{"verdict missing quotes", `verdict == approved`, "must be a double-quoted string"},
		{"verdict wrong comparator", `verdict != "approved"`, "only supports =="},
		{"verdict missing comparator", `verdict "approved"`, "must be followed by a comparator"},
		{"single equals", `verdict = "approved"`, "must be followed by a comparator"},
		{"fix_cycles negative", `fix_cycles == -1`, "non-negative integer"},
		{"fix_cycles leading zero", `fix_cycles == 02`, "canonical non-negative integer"},
		{"fix_cycles unknown comparator", `fix_cycles != 1`, "not one of"},
		{"fix_cycles non-numeric operand", `fix_cycles == maybe`, "non-negative integer"},
		{"fix_cycles missing operand", `fix_cycles ==`, "non-negative integer or"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCondition(testCase.text)
			if err == nil {
				t.Fatalf("ParseCondition(%q): want error containing %q, got nil", testCase.text, testCase.want)
			}
			if !containsSubstring(err.Error(), testCase.want) {
				t.Errorf("ParseCondition(%q): error = %q, want substring %q", testCase.text, err.Error(), testCase.want)
			}
		})
	}
}

func TestCondition_Match_FixCycles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		text    string
		input   ConditionInput
		matched bool
	}{
		{"eq true", `fix_cycles == 2`, ConditionInput{FixCycles: 2}, true},
		{"eq false", `fix_cycles == 2`, ConditionInput{FixCycles: 3}, false},
		{"less true", `fix_cycles < 2`, ConditionInput{FixCycles: 1}, true},
		{"less false", `fix_cycles < 2`, ConditionInput{FixCycles: 2}, false},
		{"leq true at bound", `fix_cycles <= 2`, ConditionInput{FixCycles: 2}, true},
		{"greater true", `fix_cycles > 1`, ConditionInput{FixCycles: 2}, true},
		{"geq true at bound", `fix_cycles >= 2`, ConditionInput{FixCycles: 2}, true},
		{"budget less true", `fix_cycles < budget`, ConditionInput{FixCycles: 1, Budget: 2}, true},
		{"budget less false at cap", `fix_cycles < budget`, ConditionInput{FixCycles: 2, Budget: 2}, false},
		{"budget eq true", `fix_cycles == budget`, ConditionInput{FixCycles: 2, Budget: 2}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			condition, err := ParseCondition(testCase.text)
			if err != nil {
				t.Fatalf("ParseCondition: %v", err)
			}
			matched, err := condition.Match(testCase.input)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if matched != testCase.matched {
				t.Errorf("Match = %v, want %v", matched, testCase.matched)
			}
		})
	}
}

func TestStage_NextTransition_FirstMatchWins(t *testing.T) {
	t.Parallel()
	stage := Stage{Transitions: []Transition{
		{To: "fix", Condition: `verdict == "changes_requested"`},
		{To: "done", Condition: `verdict == "approved"`},
		{To: "fallback", Condition: ""},
	}}
	if transition, ok, err := stage.NextTransition(ConditionInput{Verdict: "changes_requested"}); err != nil || !ok || transition.To != "fix" {
		t.Errorf("changes_requested: transition=%+v ok=%v err=%v, want fix", transition, ok, err)
	}
	if transition, ok, err := stage.NextTransition(ConditionInput{Verdict: "approved"}); err != nil || !ok || transition.To != "done" {
		t.Errorf("approved: transition=%+v ok=%v err=%v, want done", transition, ok, err)
	}
	// An unknown verdict falls through to the unconditional fallback.
	if transition, ok, err := stage.NextTransition(ConditionInput{Verdict: "unknown"}); err != nil || !ok || transition.To != "fallback" {
		t.Errorf("unknown verdict: transition=%+v ok=%v err=%v, want fallback", transition, ok, err)
	}
}

func TestStage_NextTransition_NoMatch(t *testing.T) {
	t.Parallel()
	stage := Stage{Transitions: []Transition{
		{To: "done", Condition: `verdict == "approved"`},
	}}
	if _, ok, err := stage.NextTransition(ConditionInput{Verdict: "changes_requested"}); err != nil || ok {
		t.Errorf("no-match: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestStage_SourcesVerdict(t *testing.T) {
	t.Parallel()
	unconditional := Stage{Transitions: []Transition{{To: "done", Condition: ""}}}
	if unconditional.SourcesVerdict() {
		t.Error("unconditional stage reports SourcesVerdict=true")
	}
	statusOnly := Stage{Transitions: []Transition{{To: "done", Condition: `status == "complete"`}}}
	if statusOnly.SourcesVerdict() {
		t.Error("status-only stage reports SourcesVerdict=true")
	}
	stage := Stage{Transitions: []Transition{
		{To: "fix", Condition: `verdict == "changes_requested"`},
		{To: "done", Condition: ""},
	}}
	if !stage.SourcesVerdict() {
		t.Error("verdict-conditional stage reports SourcesVerdict=false")
	}
}

func containsSubstring(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
