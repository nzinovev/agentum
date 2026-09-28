package taskinput

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Byte budgets for a continuation. The decoded-text budget matches the
// description budget: both reach the model through the routing block's Task
// section, so the same ceiling applies. The body budget is the transport cap
// only — it is larger than the text budget so JSON escaping of a valid text
// (up to 6 bytes per character for an escaped control char) cannot push an
// in-budget text over the transport limit.
const (
	MaxContinuationTextBytes = 32 << 10
	MaxContinueBodyBytes     = 8 * MaxContinuationTextBytes
)

// Continuation is the typed body of a continue request: optional text the user
// adds to the run being resumed — an answer to an open question or extra
// context. One contract for both sides of the job queue: the API stores it as
// the continue job's payload, and the runner decodes the same bytes before the
// resumed invocation. The runner never imports the API package; this package is
// the shared shape.
type Continuation struct {
	Text string `json:"text"`
}

// ParseContinuation strictly decodes continue-request bytes. An empty body, a
// body of whitespace, JSON null, and {} all decode to the empty Continuation —
// a continue without new text, the shape every stored job without a payload
// already has. A present text must be a JSON string that is valid UTF-8 and
// within MaxContinuationTextBytes. The budget is checked before the emptiness
// normalization: an over-budget whitespace-only text is refused as over budget,
// not normalized into the empty continuation. A within-budget whitespace-only
// text is the empty continuation (the emptiness check trims, the stored text
// does not — the author's text is delivered verbatim, never rewritten).
// Anything else is an error: malformed JSON, an unknown field, a non-string
// text (null included), or a second JSON value after the first.
func ParseContinuation(body []byte) (Continuation, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return Continuation{}, nil
	}
	// RawMessage keeps the three text shapes distinguishable: absent, null, and
	// a value. Decoding straight into string would fold null into "" and make
	// the wrong-type refusal unexpressible.
	var fields struct {
		Text json.RawMessage `json:"text"`
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return Continuation{}, fmt.Errorf("continuation: %w", err)
	}
	// Exactly one JSON value: a second object after the first must not be
	// silently dropped.
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Continuation{}, errors.New("continuation: request body must contain exactly one JSON object")
	}
	rawText := bytes.TrimSpace(fields.Text)
	if len(rawText) == 0 {
		return Continuation{}, nil
	}
	if bytes.Equal(rawText, []byte("null")) {
		return Continuation{}, errors.New("continuation: text must be a string, not null")
	}
	var text string
	if err := json.Unmarshal(rawText, &text); err != nil {
		return Continuation{}, fmt.Errorf("continuation: text must be a string: %w", err)
	}
	// Validity is checked on the raw token bytes, not on the decoded string:
	// the json decoder replaces invalid UTF-8 with U+FFFD instead of failing,
	// so by the time the value is a Go string the offending bytes are gone and
	// the decoded form always reads as valid.
	if !utf8.Valid(rawText) {
		return Continuation{}, errors.New("continuation: text is not valid UTF-8")
	}
	// The budget check precedes the emptiness normalization below: 32 KiB of
	// spaces is over budget first, meaningless second.
	if len(text) > MaxContinuationTextBytes {
		return Continuation{}, fmt.Errorf("continuation: text exceeds %d bytes (got %d)", MaxContinuationTextBytes, len(text))
	}
	if strings.TrimSpace(text) == "" {
		return Continuation{}, nil
	}
	return Continuation{Text: text}, nil
}

// Marshal returns the canonical job-payload JSON: {"text": …} for a
// continuation with text, {} for one without. The empty form is the same bytes
// the queue treats as "no payload content", so a continue without text stores
// exactly what a pre-payload job row already carries.
func (continuation Continuation) Marshal() ([]byte, error) {
	if strings.TrimSpace(continuation.Text) == "" {
		return []byte("{}"), nil
	}
	return json.Marshal(continuation)
}
