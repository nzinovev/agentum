package taskinput

import (
	"strings"
	"testing"
)

// The reconcile-decision contract: the parse accepts exactly the three modes
// with a full-length HEAD, refuses everything else, and the destructive mode
// never parses without its explicit confirmation.

func TestParseReconcileDecision_AcceptsTheThreeModes(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	cases := []struct {
		name string
		body string
		want ReconcileDecision
	}{
		{
			name: "resume session",
			body: `{"mode":"resume_session","expected_head":"` + head + `"}`,
			want: ReconcileDecision{Mode: ReconcileResumeSession, ExpectedHead: head},
		},
		{
			name: "keep as checkpoint",
			body: `{"mode":"keep_as_checkpoint","expected_head":"` + head + `"}`,
			want: ReconcileDecision{Mode: ReconcileKeepAsCheckpoint, ExpectedHead: head},
		},
		{
			name: "discard with confirmation",
			body: `{"mode":"discard_to_checkpoint","expected_head":"` + head + `","confirm_uncommitted_loss":true}`,
			want: ReconcileDecision{Mode: ReconcileDiscardToCheckpoint, ExpectedHead: head, ConfirmUncommittedLoss: true},
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			decision, err := ParseReconcileDecision([]byte(testCase.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if decision != testCase.want {
				t.Errorf("decision = %+v, want %+v", decision, testCase.want)
			}
			// The canonical payload round-trips through the same parse.
			payload, marshalErr := decision.Marshal()
			if marshalErr != nil {
				t.Fatalf("marshal: %v", marshalErr)
			}
			reparsed, reparseErr := ParseReconcileDecision(payload)
			if reparseErr != nil {
				t.Fatalf("reparse canonical payload: %v", reparseErr)
			}
			if reparsed != decision {
				t.Errorf("canonical payload changed the decision: %+v", reparsed)
			}
		})
	}
}

func TestParseReconcileDecision_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"json null", "null"},
		{"broken json", `{"mode":`},
		{"unknown field", `{"mode":"resume_session","expected_head":"` + head + `","extra":1}`},
		{"unknown mode", `{"mode":"just_do_it","expected_head":"` + head + `"}`},
		{"missing mode", `{"expected_head":"` + head + `"}`},
		{"short head", `{"mode":"resume_session","expected_head":"abc123"}`},
		{"uppercase head", `{"mode":"resume_session","expected_head":"` + strings.ToUpper(head) + `"}`},
		{"missing head", `{"mode":"resume_session"}`},
		{"non-string head", `{"mode":"resume_session","expected_head":42}`},
		{"second json object", `{"mode":"resume_session","expected_head":"` + head + `"}{}`},
		{"discard without confirmation", `{"mode":"discard_to_checkpoint","expected_head":"` + head + `"}`},
		{"discard with false confirmation", `{"mode":"discard_to_checkpoint","expected_head":"` + head + `","confirm_uncommitted_loss":false}`},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseReconcileDecision([]byte(testCase.body)); err == nil {
				t.Fatalf("ParseReconcileDecision(%q) accepted a malformed body", testCase.body)
			}
		})
	}
}
