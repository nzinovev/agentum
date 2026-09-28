package taskinput

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseContinuation_EmptyShapesAreTheEmptyContinuation pins the
// compatibility contract: every pre-payload call shape — no body, whitespace
// only, JSON null, an empty object, and a whitespace-only text — decodes to the
// empty continuation, never to an error, so a continue without new text keeps
// working exactly as it did before the text existed.
func TestParseContinuation_EmptyShapesAreTheEmptyContinuation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"no body", ""},
		{"whitespace body", "  \n\t "},
		{"json null body", "null"},
		{"empty object", "{}"},
		{"absent text field", `{}`},
		{"whitespace-only text", `{"text":"   \n\t"}`},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			continuation, err := ParseContinuation([]byte(testCase.body))
			if err != nil {
				t.Fatalf("ParseContinuation(%q) = %v; the empty shape must not error", testCase.body, err)
			}
			if continuation.Text != "" {
				t.Errorf("ParseContinuation(%q) text = %q; the empty shape decodes to no text", testCase.body, continuation.Text)
			}
		})
	}
}

// TestParseContinuation_AcceptsText: a present, meaningful text round-trips
// verbatim — whitespace around it is what emptiness is judged by, but the text
// itself is never trimmed or rewritten on its way to the model.
func TestParseContinuation_AcceptsText(t *testing.T) {
	t.Parallel()
	continuation, err := ParseContinuation([]byte(`{"text":"  Use PostgreSQL 17. No new table.  "}`))
	if err != nil {
		t.Fatalf("ParseContinuation: %v", err)
	}
	if continuation.Text != "  Use PostgreSQL 17. No new table.  " {
		t.Errorf("text = %q; the stored text must be verbatim, untrimmed", continuation.Text)
	}
}

// TestParseContinuation_RejectsMalformedInput covers every refusal shape in
// one table: the decode must name a broken contract, not quietly drop a key or
// fold a wrong type into an empty text.
func TestParseContinuation_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	overBudget := strings.Repeat("а", MaxContinuationTextBytes+1)
	tests := []struct {
		name string
		body string
	}{
		{"broken json", `{"text":`},
		{"unknown field", `{"text":"ok","answers":["nope"]}`},
		{"null text", `{"text":null}`},
		{"number text", `{"text":42}`},
		{"object text", `{"text":{"a":1}}`},
		{"array text", `{"text":["a"]}`},
		{"second json object", `{"text":"a"}{"text":"b"}`},
		{"trailing garbage", `{"text":"a"} xyz`},
		{"over-budget text", marshalForTest(t, overBudget)},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseContinuation([]byte(testCase.body)); err == nil {
				t.Fatalf("ParseContinuation(%q) accepted malformed input", testCase.body)
			}
		})
	}
}

// TestParseContinuation_RejectsInvalidUTF8: encoding/json replaces invalid
// UTF-8 with U+FFFD instead of failing, so the explicit validity check is the
// only refusal point — without it a mojibake text would reach the model and be
// recorded as though the author wrote it.
func TestParseContinuation_RejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	// Valid JSON with an invalid UTF-8 byte inside the string: the escape-free
	// byte 0xff survives the decoder as a replacement char, and the explicit
	// check must refuse it.
	body := []byte("{\"text\":\"bad \xff byte\"}")
	if _, err := ParseContinuation(body); err == nil {
		t.Fatal("ParseContinuation accepted invalid UTF-8 text")
	}
}

// TestParseContinuation_TextAtTheBudgetLimit: the budget is inclusive — a text
// of exactly MaxContinuationTextBytes decodes, one byte more does not — and it
// is judged before the emptiness normalization, so an over-budget
// whitespace-only text is refused as over budget rather than normalized into
// the empty continuation and accepted with a 200.
func TestParseContinuation_TextAtTheBudgetLimit(t *testing.T) {
	t.Parallel()
	atLimit := marshalForTest(t, strings.Repeat("x", MaxContinuationTextBytes))
	if _, err := ParseContinuation([]byte(atLimit)); err != nil {
		t.Fatalf("a text of exactly the budget must decode: %v", err)
	}
	overLimit := marshalForTest(t, strings.Repeat("x", MaxContinuationTextBytes+1))
	if _, err := ParseContinuation([]byte(overLimit)); err == nil {
		t.Fatal("a text one byte over the budget must be refused")
	}
	overLimitWhitespace := marshalForTest(t, strings.Repeat(" ", MaxContinuationTextBytes+1))
	if _, err := ParseContinuation([]byte(overLimitWhitespace)); err == nil {
		t.Fatal("a whitespace-only text one byte over the budget must be refused, not normalized to the empty continuation")
	}
	withinBudgetWhitespace := marshalForTest(t, strings.Repeat(" ", MaxContinuationTextBytes))
	if _, err := ParseContinuation([]byte(withinBudgetWhitespace)); err != nil {
		t.Fatalf("a whitespace-only text within the budget is the empty continuation, not an error: %v", err)
	}
}

// TestContinuationMarshal_CanonicalForms: the payload stored on the job is the
// canonical object — {} for no text, {"text": …} for text — so two
// identically-valued continues store identical bytes, and the empty form is
// byte-identical to what a pre-payload job row already carries.
func TestContinuationMarshal_CanonicalForms(t *testing.T) {
	t.Parallel()
	empty, err := (Continuation{}).Marshal()
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if !bytes.Equal(empty, []byte("{}")) {
		t.Errorf("empty continuation marshals to %s; want {}", empty)
	}
	text := Continuation{Text: "answer"}
	encoded, err := text.Marshal()
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("canonical marshal is not valid JSON: %v", err)
	}
	if decoded["text"] != "answer" {
		t.Errorf("canonical marshal = %s; want {\"text\":\"answer\"}", encoded)
	}
	roundTrip, err := ParseContinuation(encoded)
	if err != nil {
		t.Fatalf("canonical marshal does not re-parse: %v", err)
	}
	if roundTrip.Text != text.Text {
		t.Errorf("round trip text = %q; want %q", roundTrip.Text, text.Text)
	}
}

// marshalForTest encodes text as {"text": …} without the test hand-rolling
// JSON escaping.
func marshalForTest(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(Continuation{Text: text})
	if err != nil {
		t.Fatalf("marshal fixture text: %v", err)
	}
	return string(encoded)
}

// TestContinuationLimits_RelativeSizes pins the relationship between the two
// budgets: the transport cap must exceed the worst-case JSON escaping of an
// in-budget text, or a valid Russian or control-char-heavy text would be
// rejected as malformed transport rather than judged on its decoded size.
func TestContinuationLimits_RelativeSizes(t *testing.T) {
	t.Parallel()
	if MaxContinueBodyBytes <= MaxContinuationTextBytes {
		t.Fatalf("body budget %d must exceed the text budget %d", MaxContinueBodyBytes, MaxContinuationTextBytes)
	}
	if MaxContinueBodyBytes < 6*MaxContinuationTextBytes {
		t.Fatalf("body budget %d leaves no headroom for per-character escaping of %d bytes of text",
			MaxContinueBodyBytes, MaxContinuationTextBytes)
	}
}
