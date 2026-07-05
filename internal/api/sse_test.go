package api

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestWriteSSEFrame(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, 42, "stage.stopped", json.RawMessage(`{"task_id":"t1"}`)); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "id: 42\n") {
		t.Errorf("missing id frame; got:\n%s", got)
	}
	if !strings.Contains(got, "event: stage.stopped\n") {
		t.Errorf("missing event frame; got:\n%s", got)
	}
	if !strings.Contains(got, `data: {"task_id":"t1"}`+"\n\n") {
		t.Errorf("missing data frame; got:\n%s", got)
	}
}

func TestWriteSSEFrame_EmptyPayload(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, 1, "task.state_changed", nil); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `data: {}`) {
		t.Errorf("nil payload must default to {}; got:\n%s", rec.Body.String())
	}
}

func TestParseLastEventID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"42", 42},
		{"9999999999", 9999999999},
		{"not-a-number", 0},
		{"-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := parseLastEventID(tc.in); got != tc.want {
				t.Errorf("parseLastEventID(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	// keep strconv import meaningful even if cases shrink
	_ = strconv.Itoa(0)
}

func TestStructuredErrorShape(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writeError(rec, 409, codeIllegalTransition, "engine: illegal transition running --start-->")

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != codeIllegalTransition {
		t.Errorf("code = %q, want %q", body.Error.Code, codeIllegalTransition)
	}
	if !strings.Contains(body.Error.Message, "illegal transition") {
		t.Errorf("message = %q", body.Error.Message)
	}
	if rec.Code != 409 {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestNotImplementedShape(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	notImplemented(rec, "Epic 2", "POST /tasks/{id}/invocations/{iid}/continue")
	if rec.Code != 501 {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != codeNotImplemented {
		t.Errorf("code = %q, want %q", body.Error.Code, codeNotImplemented)
	}
}
