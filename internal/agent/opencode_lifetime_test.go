package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/models"
)

// Subprocess contract tests for the opencode adapter.
//
// These run in normal CI: instead of a real opencode binary they re-exec the
// test binary itself as a fake agent that speaks just enough of the protocol
// (one NDJSON line, then a result.json). That is enough to pin the properties
// no unit test over rendered structs can reach:
//
//   - the agent keeps running after Invoke returns (the run outlives the call);
//   - the hard timeout terminates it;
//   - the idle cap terminates it;
//   - caller cancellation terminates it;
//   - exactly one Wait reaps it (asserted by running under -race).
//
// The real-binary contract — does opencode actually load our config and honor
// it — is a separate, opt-in job; see docs/capabilities.md.

const (
	fakeModeEnv     = "AGENTUM_FAKE_OPENCODE"
	fakeArtifactEnv = "AGENTUM_FAKE_ARTIFACT_DIR"
	fakeDelayEnv    = "AGENTUM_FAKE_DELAY_MS"
	// fakeRunCounterEnv names a file runFakeAgent increments once per
	// execution, so tests can assert how many times a runtime invocation was
	// actually spawned (a refused start must leave it at zero).
	fakeRunCounterEnv = "AGENTUM_FAKE_RUN_COUNTER"
	// fakeConfigDumpEnv names a file the fake writes its OPENCODE_CONFIG_CONTENT
	// into, so a test can assert what boundary the child was actually started
	// under rather than what the caller believed it passed.
	fakeConfigDumpEnv = "AGENTUM_FAKE_CONFIG_DUMP"
	// fakeArgvDumpEnv names a file the fake writes the argv it was started
	// with (JSON-encoded), so a test can assert the flags that reached the
	// process — the contract the typed options render into.
	fakeArgvDumpEnv = "AGENTUM_FAKE_ARGV"
	// fakePwdDumpEnv names a file the fake writes its PWD into: opencode
	// takes its working directory from PWD, so the child's own value is what
	// decides which project a session runs in.
	fakePwdDumpEnv = "AGENTUM_FAKE_PWD"
)

// Fake agent behaviours.
const (
	// fakeWorks emits a line, works for the configured delay, then emits a
	// second line and writes result.json.
	fakeWorks = "works"
	// fakeSilent emits one line and then produces nothing for the configured
	// delay — the shape the idle cap exists for.
	fakeSilent = "silent"
	// fakeMute emits nothing at all and outlives any test deadline — the
	// shape the model check's timeout and the first-event watchdog exist for.
	fakeMute = "mute"
	// fakeQuiet emits nothing for the configured delay, then a first line and
	// a normal completion — a late but live runtime.
	fakeQuiet = "quiet"
	// fakeDie exits non-zero before writing anything — the model check's
	// error outcome.
	fakeDie = "die"
	// fakeProviderError emits a structured provider failure without a result file.
	fakeProviderError = "provider-error"
)

// TestMain doubles as the fake agent's entry point. It must intercept before
// the testing framework parses flags, because the adapter invokes the binary
// with opencode's argv ("run --standalone …", "serve …", or "--version"), not
// with test flags. The argv decides which side of the runtime the fake plays,
// so an Invoke that probes the version and the catalog first reaches the
// right fake for each call.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runFakeServer())
		case "--version":
			os.Exit(runFakeVersion(os.Getenv(fakeVersionEnv)))
		}
	}
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		os.Exit(runFakeAgent(mode))
	}
	// The fake server settles after one read; polling at the production
	// pace would only slow every probe down.
	settleInterval = 10 * time.Millisecond
	os.Exit(m.Run())
}

// fakeListing is one listing answer the fake server gives: a status, a body,
// or a refusal to answer at all.
type fakeListing struct {
	body string
	// partial, when set, is the answer to the second read — a plugin that
	// has registered some records but not all of them yet.
	partial string
	// exitCode, when non-zero, makes the server exit before it listens.
	exitCode int
	// hang makes the server never report an address.
	hang bool
}

// runFakeServer plays `opencode serve`: it listens on a loopback port, prints
// opencode's "server listening on" line, requires the basic-auth password from
// OPENCODE_SERVER_PASSWORD, and answers the two listings from the modes in its
// environment. Each listing answers with an empty envelope on its first read,
// the way the real server answers before its plugins settle.
func runFakeServer() int {
	models := fakeModelsListing(os.Getenv(fakeCatalogEnv))
	skills := fakeSkillListing(os.Getenv(fakeDebugSkillEnv))
	// The process only knows which listing it serves once a request arrives,
	// so a mode that refuses to start applies when either listing asks for it.
	for _, listing := range []fakeListing{models, skills} {
		if listing.exitCode != 0 {
			fmt.Fprintln(os.Stderr, "fake runtime failure")
			return listing.exitCode
		}
		if listing.hang {
			time.Sleep(30 * time.Second)
			return 0
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 1
	}
	password := os.Getenv("OPENCODE_SERVER_PASSWORD")
	var reads sync.Map
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, given, ok := r.BasicAuth()
		if !ok || user != runtimeServerUser || given != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var listing fakeListing
		switch r.URL.Path {
		case modelListPath:
			listing = models
		case skillListPath:
			listing = skills
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		count, _ := reads.LoadOrStore(r.URL.Path, new(atomic.Int32))
		switch read := count.(*atomic.Int32).Add(1); {
		case read == 1:
			fmt.Fprint(w, `{"location":{"directory":"/wt"},"data":[]}`)
		case read == 2 && listing.partial != "":
			fmt.Fprint(w, listing.partial)
		default:
			fmt.Fprint(w, listing.body)
		}
	})
	fmt.Printf("server listening on http://%s\n", listener.Addr())
	_ = http.Serve(listener, handler)
	return 0
}

// fakeDebugSkillEnv selects the behaviour of the skill-list fake: well-formed
// JSON, malformed JSON, hang, or non-zero exit.
const fakeDebugSkillEnv = "AGENTUM_FAKE_DEBUG_SKILL"

// fakeSkillListing builds the skill-list side of the ContextProber contract:
// the API's skill envelope, or a failure shape, by mode.
func fakeSkillListing(mode string) fakeListing {
	switch mode {
	case fakeDebugMalformed:
		return fakeListing{body: `[{not json`}
	case fakeDebugNoData:
		return fakeListing{body: `{"location":{"directory":"/wt"}}`}
	case fakeDebugExit:
		return fakeListing{exitCode: 2}
	case fakeDebugHang:
		return fakeListing{hang: true}
	case fakeDebugSettling:
		return fakeListing{body: fakeDebugSkillOutput, partial: fakeDebugSkillPartial}
	default: // fakeDebugWellformed and any unknown mode
		return fakeListing{body: fakeDebugSkillOutput}
	}
}

// fakeDebugSkill modes.
const (
	fakeDebugWellformed = "wellformed"
	fakeDebugMalformed  = "malformed"
	fakeDebugNoData     = "nodata"
	fakeDebugSettling   = "settling"
	fakeDebugExit       = "exit"
	fakeDebugHang       = "hang"
)

// fakeDebugSkillPartial is the listing before the user's skills have loaded:
// only the bundled one.
const fakeDebugSkillPartial = `{"location":{"directory":"/wt"},"data":[
  {"id":"customize-opencode","name":"customize-opencode","description":"Customize opencode","path":"/opt/opencode/skills/customize-opencode/SKILL.md","content":"MARKER-BODY-SHOULD-NOT-LEAK"}
]}`

// fakeDebugSkillOutput is the envelope the well-formed fake emits, in the
// shape of `GET /api/skill`. It carries a marker body so a test can assert the
// body is hashed and discarded (never returned in the SkillRef).
const fakeDebugSkillOutput = `{"location":{"directory":"/wt"},"data":[
  {"id":"customize-opencode","name":"customize-opencode","description":"Customize opencode","path":"/opt/opencode/skills/customize-opencode/SKILL.md","content":"MARKER-BODY-SHOULD-NOT-LEAK"},
  {"id":"user-skill","name":"user-skill","description":"A user skill","path":"/home/user/.agents/skills/user-skill/SKILL.md","content":"user body"}
]}`

// runFakeAgent plays the agent side of the adapter contract.
func runFakeAgent(mode string) int {
	if counterPath := os.Getenv(fakeRunCounterEnv); counterPath != "" {
		count := 0
		if raw, err := os.ReadFile(counterPath); err == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				count = parsed
			}
		}
		_ = os.WriteFile(counterPath, []byte(strconv.Itoa(count+1)), 0o600)
	}
	if dumpPath := os.Getenv(fakeConfigDumpEnv); dumpPath != "" {
		_ = os.WriteFile(dumpPath, []byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), 0o600)
	}
	if pwdPath := os.Getenv(fakePwdDumpEnv); pwdPath != "" {
		_ = os.WriteFile(pwdPath, []byte(os.Getenv("PWD")), 0o600)
	}
	if argvPath := os.Getenv(fakeArgvDumpEnv); argvPath != "" {
		if raw, marshalErr := json.Marshal(os.Args); marshalErr == nil {
			_ = os.WriteFile(argvPath, raw, 0o600)
		}
	}
	delay := 100 * time.Millisecond
	if raw := os.Getenv(fakeDelayEnv); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			delay = time.Duration(parsed) * time.Millisecond
		}
	}
	switch mode {
	case fakeDie:
		fmt.Fprintln(os.Stderr, "fake model failure")
		return 2
	case fakeProviderError:
		fmt.Println(`{"type":"error","sessionID":"ses_fake","error":{"name":"APIError","data":{"message":"subscription does not include this model","statusCode":429,"responseHeaders":{"set-cookie":"secret"}}}}`)
		return 0
	case fakeMute:
		// Block long enough that every bounded waiter gives up first; the
		// caller's kill is what ends this process.
		time.Sleep(30 * time.Second)
		return 0
	case fakeQuiet:
		// Totally silent for the delay, then a late first line and a normal
		// completion — the shape that proves a disabled or retired
		// first-event watchdog leaves the run alone.
		time.Sleep(delay)
	}
	fmt.Println(`{"type":"text","sessionID":"ses_fake","part":{"type":"text","text":"starting"}}`)
	time.Sleep(delay)

	if mode == fakeWorks {
		fmt.Println(`{"type":"text","sessionID":"ses_fake","part":{"type":"text","text":"done"}}`)
	}
	artifactDir := os.Getenv(fakeArtifactEnv)
	if artifactDir == "" {
		return 1
	}
	result := []byte(`{"schema_version":"1","status":"complete","summary":"fake run"}`)
	if err := os.WriteFile(filepath.Join(artifactDir, "result.json"), result, 0o644); err != nil {
		return 1
	}
	return 0
}

// fakeInvocation wires the adapter to the test binary as its agent, and returns
// the adapter plus the invocation to run. profile carries the timeouts under
// test; an empty profile means no caps and no limits. The selection names a
// model because Invoke refuses an empty one outright, and it names one the
// fixture catalog lists because the same fake answers the catalog probe and
// Invoke checks the selection against that listing.
func fakeInvocation(t *testing.T, mode string, delay time.Duration, profile caps.Profile) (*OpencodeAdapter, Invocation) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	workdir := t.TempDir()
	artifactDir := filepath.Join(workdir, ".agentum", "artifacts", "spec")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}

	t.Setenv(fakeModeEnv, mode)
	t.Setenv(fakeArtifactEnv, artifactDir)
	t.Setenv(fakeDelayEnv, strconv.Itoa(int(delay.Milliseconds())))

	return NewOpencodeAdapter(self), Invocation{
		Workdir:     workdir,
		ArtifactDir: artifactDir,
		Prompt:      "fake stage",
		Profile:     profile,
		Model: models.Selection{
			Tier:    "strong",
			Options: models.Options{Model: "opencode/muse-spark-1.3-contributor-free"},
		},
	}
}

// drain consumes the adapter stream and returns the terminal outcome: the
// parsed result on success, or the terminal error.
func drain(t *testing.T, events <-chan Event) (*Result, error) {
	t.Helper()
	var (
		result *Result
		failed error
	)
	for event := range events {
		switch event.Kind {
		case EventResult:
			result = event.Result
		case EventError:
			failed = event.Err
		case EventStream:
		}
	}
	return result, failed
}

// shrinkKillGrace shortens the terminate-then-force escalation for the duration
// of one test, so a timeout assertion does not wait out the production grace
// period. Safe because every test using it is serial (they all call t.Setenv).
func shrinkKillGrace(t *testing.T) {
	t.Helper()
	original := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = original })
}

// TestInvoke_SubprocessOutlivesInvokeReturn is the regression test for the run
// context being released at the wrong seam. Invoke returns as soon as the
// process starts; if the context that binds the subprocess is cancelled at that
// point, the agent is killed milliseconds into its work and the run always ends
// in "cancelled", whatever the configured timeout says.
func TestInvoke_SubprocessOutlivesInvokeReturn(t *testing.T) {
	adapter, invocation := fakeInvocation(t, fakeWorks, 700*time.Millisecond, caps.Profile{})

	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)
	if failed != nil {
		t.Fatalf("run failed after Invoke returned: %v", failed)
	}
	if result == nil {
		t.Fatal("no result event; the agent did not run to completion")
	}
	if result.SessionID != "ses_fake" {
		t.Errorf("SessionID = %q, want ses_fake", result.SessionID)
	}
	if result.Status != StatusComplete {
		t.Errorf("status = %q, want complete", result.Status)
	}
}

// TestInvoke_ReportsRuntimeFailure preserves the provider or process cause
// when no result file exists and omits response headers from the error.
func TestInvoke_ReportsRuntimeFailure(t *testing.T) {
	for _, testCase := range []struct {
		name string
		mode string
		want string
	}{
		{name: "provider error", mode: fakeProviderError, want: "subscription does not include this model (HTTP 429)"},
		{name: "process exit", mode: fakeDie, want: "exit status 2"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			adapter, invocation := fakeInvocation(t, testCase.mode, 0, caps.Profile{})
			events, err := adapter.Invoke(t.Context(), invocation)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			result, failed := drain(t, events)
			if result != nil || failed == nil || !strings.Contains(failed.Error(), testCase.want) {
				t.Fatalf("result = %+v, error = %v; want %q", result, failed, testCase.want)
			}
			if strings.Contains(failed.Error(), "result.json") || strings.Contains(failed.Error(), "set-cookie") {
				t.Errorf("runtime error replaced or exposed headers: %v", failed)
			}
		})
	}
}

// TestInvoke_HardTimeoutStopsRun: the profile's hard cap terminates a run that
// overruns it, and says so.
func TestInvoke_HardTimeoutStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeWorks, 10*time.Second,
		caps.Profile{HardTimeout: 300 * time.Millisecond})

	started := time.Now()
	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite the hard timeout: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; the hard timeout did not fire")
	}
	if !strings.Contains(failed.Error(), "hard timeout") {
		t.Errorf("error = %q, want it to name the hard timeout", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; the timeout did not terminate the process tree", elapsed)
	}
}

// TestInvoke_IdleTimeoutStopsRun: an agent that goes quiet past the idle cap is
// terminated, and the terminal error names the idle cap rather than a generic
// cancellation.
func TestInvoke_IdleTimeoutStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeSilent, 10*time.Second,
		caps.Profile{IdleTimeout: 300 * time.Millisecond})

	started := time.Now()
	events, err := adapter.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite the idle cap: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; the idle cap did not fire")
	}
	if !strings.Contains(failed.Error(), "idle cap") {
		t.Errorf("error = %q, want it to name the idle cap", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; the idle cap did not terminate the process tree", elapsed)
	}
}

// TestInvoke_CallerCancellationStopsRun: the §5.1 cancellation seam. The runner
// cancels the context to abort a stage; the agent must go with it.
func TestInvoke_CallerCancellationStopsRun(t *testing.T) {
	shrinkKillGrace(t)
	adapter, invocation := fakeInvocation(t, fakeWorks, 10*time.Second, caps.Profile{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	events, err := adapter.Invoke(ctx, invocation)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	result, failed := drain(t, events)

	if result != nil {
		t.Fatalf("run produced a result despite cancellation: %+v", result)
	}
	if failed == nil {
		t.Fatal("run ended without an error event; cancellation did not fire")
	}
	if !strings.Contains(failed.Error(), "cancelled") {
		t.Errorf("error = %q, want it to report cancellation", failed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("run took %s; cancellation did not terminate the process tree", elapsed)
	}
}

// TestInvoke_CleansUpConfigAfterRun: the per-invocation config directory is
// released once the run ends — but not before, since it backs the child's
// permissions for as long as the child lives.
func TestInvoke_CleansUpConfigAfterRun(t *testing.T) {
	adapter, invocation := fakeInvocation(t, fakeWorks, 50*time.Millisecond, caps.Profile{})

	plan, err := adapter.prepareEnforcement(invocation)
	if err != nil {
		t.Fatalf("prepareEnforcement: %v", err)
	}
	configRoot := filepath.Dir(plan.configDir)
	plan.cleanup()
	before := countAdapterConfigDirs(t, configRoot)

	events, invokeErr := adapter.Invoke(context.Background(), invocation)
	if invokeErr != nil {
		t.Fatalf("Invoke: %v", invokeErr)
	}
	if _, failed := drain(t, events); failed != nil {
		t.Fatalf("run failed: %v", failed)
	}

	if after := countAdapterConfigDirs(t, configRoot); after != before {
		t.Errorf("config dirs before=%d after=%d; the run leaked its config directory", before, after)
	}
}

// countAdapterConfigDirs counts the adapter's per-invocation config directories
// under root.
func countAdapterConfigDirs(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "agentum-opencode-") {
			count++
		}
	}
	return count
}
