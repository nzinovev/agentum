package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/models"
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

	checkIngestedState(t, st, chunks)
}

// checkIngestedState asserts the stream-derived fields the fixture must
// produce: sessionID, telemetry across two step-finishes, snapshot, the single
// write activity, and the expected stream chunks. Lifted out of the test body
// to keep TestIngest_NDJSONFixture a flat ingest loop with no assertion
// nesting.
func checkIngestedState(t *testing.T, st *invokeState, chunks []string) {
	t.Helper()
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
	activity := st.activity[0]
	if activity.Tool != "write" || activity.Status != "completed" || activity.Target != "/tmp/hello.txt" {
		t.Errorf("activity = %+v, want write/completed//tmp/hello.txt", activity)
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

// TestBuildOpencodeArgs_AggregateShape pins the argv contract: run --format
// json --auto --dir <workdir> [--session <id>] [--model <model>] <message>.
// The optional flags appear only when their selection field is populated — an
// empty model emits no --model, and the runtime's own default applies.
func TestBuildOpencodeArgs_AggregateShape(t *testing.T) {
	t.Parallel()
	base := Invocation{
		Workdir:      "/wt",
		Prompt:       "stage prompt",
		RoutingBlock: "routing block",
	}

	argv := buildOpencodeArgs("opencode", base)
	want := []string{"opencode", "run", "--format", "json", "--auto", "--dir", "/wt", "stage prompt\n\nrouting block"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("bare argv = %v; want %v", argv, want)
	}

	withModel := base
	withModel.Model = models.Selection{Tier: "strong", Options: models.Options{Model: "prov/model"}}
	argv = buildOpencodeArgs("opencode", withModel)
	if index := indexOfArg(argv, "--model"); index < 0 || argv[index+1] != "prov/model" {
		t.Errorf("argv = %v; want --model prov/model", argv)
	}

	withSession := withModel
	withSession.ResumeSession = "sess-1"
	argv = buildOpencodeArgs("opencode", withSession)
	sessionIndex := indexOfArg(argv, "--session")
	modelIndex := indexOfArg(argv, "--model")
	if sessionIndex < 0 || modelIndex < 0 || sessionIndex > modelIndex {
		t.Errorf("argv = %v; want --session before --model", argv)
	}
	if got := argv[len(argv)-1]; got != "stage prompt\n\nrouting block" {
		t.Errorf("last argv element = %q; want the prompt+routing message", got)
	}
}

// indexOfArg finds a flag's index in argv, or -1.
func indexOfArg(argv []string, flag string) int {
	for index, arg := range argv {
		if arg == flag {
			return index
		}
	}
	return -1
}

// TestInvoke_RefusesUndeclaredModelOption is the third refusal point: a
// selection carrying an option the descriptor does not declare is refused
// before any subprocess starts, and the error names BOTH the adapter id and
// the option. A narrowed descriptor supplies the negative case, because
// today's only option (model) is one this adapter declares — this is the
// mechanism test MVP task 6 inherits for --variant.
//
// Cannot use t.Parallel: it narrows the package-level descriptor (restored on
// cleanup), which other Describe() callers read.
func TestInvoke_RefusesUndeclaredModelOption(t *testing.T) {
	workdir := t.TempDir()
	original := opencodeDescriptor.ModelOptions
	opencodeDescriptor.ModelOptions = nil // the narrowed declaration: nothing is declared
	t.Cleanup(func() { opencodeDescriptor.ModelOptions = original })

	adapter := NewOpencodeAdapter("definitely-not-a-real-binary")
	invocation := Invocation{
		Workdir:     workdir,
		ArtifactDir: workdir,
		Prompt:      "stage prompt",
		Model:       models.Selection{Tier: "strong", Options: models.Options{Model: "prov/model"}},
	}

	events, err := adapter.Invoke(context.Background(), invocation)
	if err == nil {
		t.Fatal("Invoke must refuse a selection whose options the descriptor does not declare")
	}
	if events != nil {
		t.Error("no channel may be returned from a refused start")
	}
	if !strings.Contains(err.Error(), string(AdapterOpencode)) {
		t.Errorf("error must name the adapter id: %v", err)
	}
	if !strings.Contains(err.Error(), "unsupported model option") {
		t.Errorf("error must name the unsupported option: %v", err)
	}
}

// TestInvoke_AcceptsDeclaredOptions: the same selection against the real
// descriptor passes the option check (the start may still fail later on the
// missing binary — that is a different error, not an option refusal).
func TestInvoke_AcceptsDeclaredOptions(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	adapter := NewOpencodeAdapter("definitely-not-a-real-runtime-binary")
	invocation := Invocation{
		Workdir:     workdir,
		ArtifactDir: workdir,
		Prompt:      "stage prompt",
		Model:       models.Selection{Tier: "strong", Options: models.Options{Model: "prov/model"}},
	}
	_, err := adapter.Invoke(context.Background(), invocation)
	if err == nil {
		t.Fatal("expected the missing-binary error, not a successful start")
	}
	if strings.Contains(err.Error(), "unsupported model option") {
		t.Errorf("a declared option must not be refused: %v", err)
	}
}
