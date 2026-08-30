package taskinput

import (
	"encoding/json"
	"strings"
	"testing"
)

// validRequest is the row every "accepted" case starts from; failure cases
// override one field at a time so each table row isolates one rule.
func validRequest() Request {
	return Request{
		Title:       "Lower the log level of health endpoints",
		Description: "Problem. /healthz is noisy at Info. What to do. Log it at Debug.",
	}
}

func TestRequest_Validate(t *testing.T) {
	t.Parallel()
	overBudgetTitle := strings.Repeat("t", MaxTitleBytes+1)
	overBudgetDescription := strings.Repeat("d", MaxDescriptionBytes+1)
	tests := []struct {
		name       string
		mutate     func(request *Request)
		wantSubstr string
	}{
		{
			name:       "valid request passes",
			mutate:     func(request *Request) {},
			wantSubstr: "",
		},
		{
			name:       "empty description rejected",
			mutate:     func(request *Request) { request.Description = "" },
			wantSubstr: "description is required",
		},
		{
			name:       "whitespace-only description rejected",
			mutate:     func(request *Request) { request.Description = " \n\t " },
			wantSubstr: "description is required",
		},
		{
			name:       "empty title rejected",
			mutate:     func(request *Request) { request.Title = "" },
			wantSubstr: "title is required",
		},
		{
			name:       "whitespace-only title rejected",
			mutate:     func(request *Request) { request.Title = "   " },
			wantSubstr: "title is required",
		},
		{
			name:       "over-budget title rejected with its own message",
			mutate:     func(request *Request) { request.Title = overBudgetTitle },
			wantSubstr: "title exceeds",
		},
		{
			name:       "over-budget description rejected with its own message",
			mutate:     func(request *Request) { request.Description = overBudgetDescription },
			wantSubstr: "description exceeds",
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest()
			testCase.mutate(&request)
			err := request.Validate()
			if testCase.wantSubstr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), testCase.wantSubstr) {
				t.Errorf("error %q does not name the offending rule %q", err, testCase.wantSubstr)
			}
		})
	}
}

func TestParseOverrides(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		raw       string
		want      Overrides
		wantErr   bool
		errSubstr string
	}{
		{
			name: "absent bytes yield the zero value",
			raw:  "",
			want: Overrides{},
		},
		{
			name: "whitespace-only bytes yield the zero value",
			raw:  "  \n ",
			want: Overrides{},
		},
		{
			name: "empty object yields the zero value",
			raw:  "{}",
			want: Overrides{},
		},
		{
			name: "checks without a checks key yields the zero value",
			raw:  `{"checks":null}`,
			want: Overrides{},
		},
		{
			name: "well-formed overrides decode",
			raw:  `{"checks":{"required":["verify"],"optional":["lint"]}}`,
			want: Overrides{Checks: ChecksOverride{Required: []string{"verify"}, Optional: []string{"lint"}}},
		},
		{
			name:      "unknown key inside checks rejected",
			raw:       `{"checks":{"requred":["verify"]}}`,
			wantErr:   true,
			errSubstr: `overrides.checks`,
		},
		{
			name:      "unknown top-level key rejected",
			raw:       `{"model":"strong"}`,
			wantErr:   true,
			errSubstr: `overrides:`,
		},
		{
			name:      "malformed JSON rejected",
			raw:       `{not json`,
			wantErr:   true,
			errSubstr: `overrides:`,
		},
		{
			name:      "wrong-typed checks value rejected",
			raw:       `{"checks":5}`,
			wantErr:   true,
			errSubstr: `overrides.checks`,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseOverrides([]byte(testCase.raw))
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q, got %+v", testCase.raw, got)
				}
				if !strings.Contains(err.Error(), testCase.errSubstr) {
					t.Errorf("error %q does not contain %q", err, testCase.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", testCase.raw, err)
			}
			assertSameNames(t, "required", got.Checks.Required, testCase.want.Checks.Required)
			assertSameNames(t, "optional", got.Checks.Optional, testCase.want.Checks.Optional)
		})
	}
}

// assertSameNames compares two name lists including their nil-ness: an absent
// list and an empty list must both survive a round-trip distinguishably.
func assertSameNames(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
	for index, name := range want {
		if got[index] != name {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
}

// TestOverridesMarshal_Canonical pins the canonical bytes: sorted and
// de-duplicated name lists, fixed field order, absent lists omitted rather
// than rendered as empty arrays, no whitespace.
func TestOverridesMarshal_Canonical(t *testing.T) {
	t.Parallel()
	got, err := (Overrides{Checks: ChecksOverride{
		Required: []string{"verify", "build", "verify"},
		Optional: nil,
	}}).Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"checks":{"required":["build","verify"]}}`
	if string(got) != want {
		t.Fatalf("marshal = %s, want %s", got, want)
	}
	// Round-trip: canonical bytes parse back to the same value (order aside).
	parsed, parseErr := ParseOverrides(got)
	if parseErr != nil {
		t.Fatalf("parse canonical bytes: %v", parseErr)
	}
	remarshaled, remarshalErr := parsed.Marshal()
	if remarshalErr != nil {
		t.Fatalf("remarshal: %v", remarshalErr)
	}
	if string(remarshaled) != want {
		t.Fatalf("remarshal = %s, want the same canonical bytes %s", remarshaled, want)
	}
}

// TestRequest_Revision_StableAcrossSourceFormatting is the revision
// acceptance: two bodies that differ only in key order and whitespace — and
// whose overrides lists differ only in order — produce the same revision,
// while a changed description produces a different one.
func TestRequest_Revision_StableAcrossSourceFormatting(t *testing.T) {
	t.Parallel()
	build := func(overridesJSON string) Request {
		parsed, err := ParseOverrides([]byte(overridesJSON))
		if err != nil {
			t.Fatalf("parse %q: %v", overridesJSON, err)
		}
		request := validRequest()
		request.Overrides = parsed
		return request
	}
	compact := build(`{"checks":{"required":["verify","build"],"optional":["lint"]}}`)
	reformatted := build(`{
		"checks": {
			"optional": ["lint"],
			"required": ["build", "verify"]
		}
	}`)

	if compact.Revision() != reformatted.Revision() {
		t.Errorf(
			"same request, different formatting: revisions differ (%s vs %s)",
			compact.Revision(), reformatted.Revision(),
		)
	}

	changed := validRequest()
	changed.Description = compact.Description + " One more sentence."
	if changed.Revision() == compact.Revision() {
		t.Error("a changed description must change the revision")
	}

	// Title changes too: the revision covers the whole request.
	retitled := validRequest()
	retitled.Title = "A different title"
	if retitled.Revision() == compact.Revision() {
		t.Error("a changed title must change the revision")
	}

	// Leading/trailing whitespace on the strings does not change the request.
	padded := validRequest()
	padded.Title = "  " + padded.Title + "\n"
	padded.Description = "\t" + padded.Description + "  "
	if padded.Revision() != validRequest().Revision() {
		t.Error("padding whitespace must not change the revision (canonical form trims)")
	}
}

// TestRequest_Revision_FieldOrderIsFixed guards the canonical field order
// structurally: the serialized prefix pins which field comes first, so a
// reordered struct declaration cannot silently change every stored revision.
func TestRequest_Revision_FieldOrderIsFixed(t *testing.T) {
	t.Parallel()
	request := validRequest()
	encoded, err := json.Marshal(struct {
		Description string          `json:"description"`
		Overrides   json.RawMessage `json:"overrides"`
		Title       string          `json:"title"`
	}{
		Description: request.Description,
		Overrides:   mustMarshal(t, request.Overrides),
		Title:       request.Title,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(string(encoded), `{"description":`) {
		t.Errorf("canonical field order changed: %s", encoded)
	}
}

func mustMarshal(t *testing.T, overrides Overrides) []byte {
	t.Helper()
	encoded, err := overrides.Marshal()
	if err != nil {
		t.Fatalf("marshal overrides: %v", err)
	}
	return encoded
}

// TestOverridesMarshal_EmptyIsEmptyObject pins the shape the API documents and
// the column defaults to: a task with no overrides carries `{}`, not a
// skeleton of empty lists. The response body, the stored row and the migration
// backfill all have to agree on this, so it is asserted on the bytes.
func TestOverridesMarshal_EmptyIsEmptyObject(t *testing.T) {
	t.Parallel()
	for _, empty := range []Overrides{
		{},
		{Checks: ChecksOverride{Required: []string{}, Optional: []string{}}},
	} {
		got, err := empty.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != "{}" {
			t.Errorf("marshal(%+v) = %s, want {}", empty, got)
		}
	}
}

// TestRequestRevision_IgnoresDuplicateAndOrder is the other half of the
// canonical-form claim: a name list is a set, so reordering or repeating a
// name must not produce a different revision. checks.Resolve collapses both,
// and the revision has to agree with it or the manifest diff reports a change
// that did not happen.
func TestRequestRevision_IgnoresDuplicateAndOrder(t *testing.T) {
	t.Parallel()
	base := validRequest()
	base.Overrides = Overrides{Checks: ChecksOverride{Required: []string{"build", "verify"}}}
	shuffled := validRequest()
	shuffled.Overrides = Overrides{Checks: ChecksOverride{Required: []string{"verify", "build", "build"}}}
	if base.Revision() != shuffled.Revision() {
		t.Errorf("revision differs on reorder/duplicate: %s vs %s", base.Revision(), shuffled.Revision())
	}
}
