package agent

import (
	"os"
	"testing"
)

// TestIngest_NDJSONFixture feeds the captured opencode stream through the
// ingest path and asserts the stream-derived state: sessionID capture,
// telemetry accumulation across two step-finishes, snapshot, activity, and
// opaque forwarding of an unknown future event.
func TestIngest_NDJSONFixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/sample.ndjson")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	st := &invokeState{}
	var chunks []string
	for _, line := range splitNDJSON(string(data)) {
		if line == "" {
			continue
		}
		ev, _ := st.ingest([]byte(line))
		if ev != nil && ev.Kind == EventStream {
			chunks = append(chunks, ev.Chunk)
		}
	}

	if want := "ses_0cbeb1097ffeOspARButu4sJM2"; st.sessionID != want {
		t.Errorf("sessionID = %q, want %q", st.sessionID, want)
	}
	// Two step-finishes: 7396+7408 total, 24 reasoning, cache read 4352+7296.
	if st.telemetry.Tokens.Total != 14804 {
		t.Errorf("tokens.total = %d, want 14804", st.telemetry.Tokens.Total)
	}
	if st.telemetry.Tokens.Reasoning != 24 {
		t.Errorf("tokens.reasoning = %d, want 24", st.telemetry.Tokens.Reasoning)
	}
	if st.telemetry.Tokens.CacheRead != 11648 {
		t.Errorf("tokens.cache.read = %d, want 11648", st.telemetry.Tokens.CacheRead)
	}
	if st.telemetry.Cost != 0.001 {
		t.Errorf("cost = %v, want 0.001", st.telemetry.Cost)
	}
	if st.snapshot != "2f94a8bb02683a65f8f3d70ce28c3725463c8759" {
		t.Errorf("snapshot = %q", st.snapshot)
	}
	if len(st.activity) != 1 {
		t.Fatalf("activity len = %d, want 1", len(st.activity))
	}
	a := st.activity[0]
	if a.Tool != "write" || a.Status != "completed" || a.Target != "/tmp/hello.txt" {
		t.Errorf("activity = %+v, want write/completed//tmp/hello.txt", a)
	}
	// One text chunk ("done") + one opaque unknown-event chunk.
	if len(chunks) != 2 {
		t.Fatalf("stream chunks = %v, want [done, <event:unknown_future_event>]", chunks)
	}
	if chunks[0] != "done" {
		t.Errorf("chunk[0] = %q, want done", chunks[0])
	}
	if chunks[1] != "[event:unknown_future_event]" {
		t.Errorf("chunk[1] = %q, want opaque event marker", chunks[1])
	}
}

// splitNDJSON splits the fixture on newlines.
func splitNDJSON(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
