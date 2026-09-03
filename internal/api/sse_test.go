package api

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// frameData decodes the data: line of a written frame back into a map, so the
// assertions read facts rather than byte-exact strings.
func frameData(t *testing.T, frame string) map[string]json.RawMessage {
	t.Helper()
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(frame), &data); err != nil {
		t.Fatalf("decode frame data %q: %v", frame, err)
	}
	return data
}

func frameFact(t *testing.T, data map[string]json.RawMessage, key string) string {
	t.Helper()
	raw, exists := data[key]
	if !exists {
		t.Fatalf("frame data carries no %q: %v", key, data)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %q: %v", key, err)
	}
	return value
}

func TestWriteSSEFrame(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, sqlc.Event{
		ID: 42, Type: "stage.stopped",
		TaskID:  sql.NullString{String: "t1", Valid: true},
		Payload: json.RawMessage(`{"stop_reason":"gate"}`),
		Actor:   "system",
	}); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "id: 42\n") {
		t.Errorf("missing id frame; got:\n%s", got)
	}
	if !strings.Contains(got, "event: stage.stopped\n") {
		t.Errorf("missing event frame; got:\n%s", got)
	}
	data := frameData(t, lineAfter(t, got, "data: "))
	if frameFact(t, data, "stop_reason") != "gate" {
		t.Errorf("producer payload lost: %v", data)
	}
	if frameFact(t, data, "task_id") != "t1" {
		t.Errorf("run id not mixed in: %v", data)
	}
	if frameFact(t, data, "actor") != "system" {
		t.Errorf("actor not mixed in: %v", data)
	}
}

// TestWriteSSEFrame_MixInNeverRewrites pins the precedence: a producer that
// recorded its own wording for a mixed-in key keeps it — the frame supplements
// the payload, it does not edit it.
func TestWriteSSEFrame_MixInNeverRewrites(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, sqlc.Event{
		ID: 7, Type: "task.state_changed",
		TaskID:  sql.NullString{String: "row-id", Valid: true},
		Payload: json.RawMessage(`{"task_id":"producer-id"}`),
		Actor:   "human",
	}); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	data := frameData(t, lineAfter(t, rec.Body.String(), "data: "))
	if frameFact(t, data, "task_id") != "producer-id" {
		t.Errorf("producer task_id rewritten: %v", data)
	}
	if frameFact(t, data, "actor") != "human" {
		t.Errorf("actor missing: %v", data)
	}
}

// TestWriteSSEFrame_TenantGlobalEventOmitsRunID: an event with no run carries
// no empty task_id placeholder — the frame says nothing rather than saying
// "".
func TestWriteSSEFrame_TenantGlobalEventOmitsRunID(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, sqlc.Event{ID: 3, Type: "run.log", Payload: nil, Actor: "system"}); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	data := frameData(t, lineAfter(t, rec.Body.String(), "data: "))
	if _, exists := data["task_id"]; exists {
		t.Errorf("tenant-global event carries a task_id: %v", data)
	}
	if frameFact(t, data, "actor") != "system" {
		t.Errorf("actor missing: %v", data)
	}
}

func TestWriteSSEFrame_EmptyPayload(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	if err := writeSSEFrame(rec, sqlc.Event{ID: 1, Type: "task.state_changed", Payload: nil, Actor: "human"}); err != nil {
		t.Fatalf("writeSSEFrame: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `data: {`) {
		t.Errorf("nil payload must default to an object; got:\n%s", rec.Body.String())
	}
}

// lineAfter returns the trimmed line of frame that follows the given prefix
// marker ("data: ").
func lineAfter(t *testing.T, frame, marker string) string {
	t.Helper()
	markerIndex := strings.Index(frame, marker)
	if markerIndex < 0 {
		t.Fatalf("frame has no %q line; got:\n%s", marker, frame)
	}
	rest := frame[markerIndex+len(marker):]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		rest = rest[:newline]
	}
	return strings.TrimSpace(rest)
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
