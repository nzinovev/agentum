package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// recordingArtifactStore is an artifacts.Store that records what was ingested.
// Only Put is exercised by the capture path; the read methods are present to
// satisfy the interface and fail loudly if the capture path ever calls one.
type recordingArtifactStore struct {
	mu   sync.Mutex
	puts []artifacts.PutParams
	// failWith, when set, is returned from every Put.
	failWith error
}

func (store *recordingArtifactStore) Put(_ context.Context, params artifacts.PutParams) (artifacts.Revision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failWith != nil {
		return artifacts.Revision{}, store.failWith
	}
	store.puts = append(store.puts, params)
	return artifacts.Revision{ID: "rev", Name: params.Name}, nil
}

func (store *recordingArtifactStore) names() []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]string, 0, len(store.puts))
	for _, put := range store.puts {
		out = append(out, put.Name)
	}
	return out
}

func (*recordingArtifactStore) Get(context.Context, string, string) (artifacts.Revision, error) {
	return artifacts.Revision{}, errors.New("not used")
}
func (*recordingArtifactStore) GetBytes(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("not used")
}
func (*recordingArtifactStore) Reader(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (*recordingArtifactStore) CopyTo(context.Context, string, string, io.Writer) (int64, error) {
	return 0, errors.New("not used")
}
func (*recordingArtifactStore) Current(context.Context, string, string, string) (artifacts.Revision, error) {
	return artifacts.Revision{}, errors.New("not used")
}
func (*recordingArtifactStore) ListForTask(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}
func (*recordingArtifactStore) ListCurrent(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}
func (*recordingArtifactStore) ListForInvocation(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}

// captureFixture builds a runner wired to a recording store, plus a stageRun
// over a temp worktree with a populated artifact dir.
type captureFixture struct {
	runner      *Runner
	store       *recordingArtifactStore
	events      *fakeStore
	run         stageRun
	artifactDir string
	worktreeDir string
	outsideDir  string
}

const (
	fixtureTask  = "task-1"
	fixtureStage = "spec"
)

func newCaptureFixture(t *testing.T) *captureFixture {
	t.Helper()
	parent := t.TempDir()
	worktreeDir := filepath.Join(parent, "worktree")
	outsideDir := filepath.Join(parent, "outside")
	if err := os.MkdirAll(worktreeDir, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("host secret"), 0o644); err != nil {
		t.Fatalf("write host secret: %v", err)
	}

	artifactDir := worktree.ArtifactDir(worktreeDir, fixtureTask, fixtureStage)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "result.json"),
		[]byte(`{"schema_version":"1","status":"complete"}`), 0o644); err != nil {
		t.Fatalf("write result.json: %v", err)
	}

	task := sqlc.Task{ID: fixtureTask, TenantID: "tenant-1", UserID: "user-1"}
	events := newFakeStore(task, sqlc.Project{})
	store := &recordingArtifactStore{}
	runner := New(Deps{
		Store:     events,
		Artifacts: store,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &captureFixture{
		runner: runner, store: store, events: events,
		run: stageRun{
			task:     task,
			worktree: &worktree.Worktree{Root: worktreeDir},
		},
		artifactDir: artifactDir,
		worktreeDir: worktreeDir,
		outsideDir:  outsideDir,
	}
}

// capture runs captureStageOutputs for a result declaring the given artifacts.
func (fixture *captureFixture) capture(t *testing.T, declared ...agent.Artifact) error {
	t.Helper()
	return fixture.runner.captureStageOutputs(
		context.Background(), fixture.run, fixtureStage, "inv-1", fixture.artifactDir,
		&agent.ResultJSON{Status: "complete", Artifacts: declared},
	)
}

// emittedReasons returns the reasons on every artifact-rejected event recorded.
func (fixture *captureFixture) emittedReasons() []string {
	fixture.events.mu.Lock()
	defer fixture.events.mu.Unlock()
	reasons := make([]string, 0)
	for _, event := range fixture.events.events {
		if event.Type == EvArtifactRejected {
			reasons = append(reasons, string(event.Payload))
		}
	}
	return reasons
}

// TestCaptureStageOutputs_IngestsDeclaredArtifacts is the baseline: ordinary
// declarations are captured under their worktree-relative names, alongside
// result.json under "<stage>/result.json".
func TestCaptureStageOutputs_IngestsDeclaredArtifacts(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	writeInWorktree(t, fixture.worktreeDir, "docs/spec.md", "the spec")

	if err := fixture.capture(t, agent.Artifact{Path: "docs/spec.md", Kind: "spec"}); err != nil {
		t.Fatalf("captureStageOutputs: %v", err)
	}
	got := fixture.store.names()
	want := []string{fixtureStage + "/result.json", "docs/spec.md"}
	if len(got) != len(want) {
		t.Fatalf("captured %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("captured[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

// TestCaptureStageOutputs_RejectsPathOutsideTheWorktree is the P0. An
// agent-declared absolute path is untrusted input that the orchestrator would
// otherwise read with its own privileges and store as durable, API-readable
// evidence.
func TestCaptureStageOutputs_RejectsPathOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	outside := filepath.Join(fixture.outsideDir, "secret.txt")

	err := fixture.capture(t, agent.Artifact{Path: outside, Kind: "spec"})
	if !errors.Is(err, ErrArtifactEscapesWorktree) {
		t.Fatalf("captureStageOutputs = %v, want ErrArtifactEscapesWorktree", err)
	}
	assertNoHostFileCaptured(t, fixture, "host secret")
	if reasons := fixture.emittedReasons(); len(reasons) != 1 || !strings.Contains(reasons[0], "escapes_worktree") {
		t.Errorf("emitted rejection events = %v, want one naming escapes_worktree", reasons)
	}
}

// TestCaptureStageOutputs_RejectsTraversal: the ../ form of the same escape.
func TestCaptureStageOutputs_RejectsTraversal(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)

	err := fixture.capture(t, agent.Artifact{Path: "../outside/secret.txt"})
	if !errors.Is(err, ErrArtifactEscapesWorktree) {
		t.Fatalf("captureStageOutputs = %v, want ErrArtifactEscapesWorktree", err)
	}
	assertNoHostFileCaptured(t, fixture, "host secret")
}

// TestCaptureStageOutputs_RejectionIsAllOrNothing: a breach must not leave the
// legitimate half of the stage's output in the store. The escape is declared
// second, so a per-artifact guard would already have ingested the first one and
// left the run looking partially successful.
func TestCaptureStageOutputs_RejectionIsAllOrNothing(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	writeInWorktree(t, fixture.worktreeDir, "docs/spec.md", "the spec")

	err := fixture.capture(t,
		agent.Artifact{Path: "docs/spec.md", Kind: "spec"},
		agent.Artifact{Path: filepath.Join(fixture.outsideDir, "secret.txt")},
	)
	if !errors.Is(err, ErrArtifactEscapesWorktree) {
		t.Fatalf("captureStageOutputs = %v, want ErrArtifactEscapesWorktree", err)
	}
	if names := fixture.store.names(); len(names) != 0 {
		t.Errorf("captured %v after a rejection; the capture is not all-or-nothing", names)
	}
}

// TestCaptureStageOutputs_MissingFileIsNotABreach: an agent that declares a file
// it never wrote has a contract gap. The run continues and result.json is still
// captured — conflating this with an escape would fail stages routinely.
func TestCaptureStageOutputs_MissingFileIsNotABreach(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)

	if err := fixture.capture(t, agent.Artifact{Path: "docs/never-written.md"}); err != nil {
		t.Fatalf("captureStageOutputs = %v, want nil for a declared-but-absent file", err)
	}
	names := fixture.store.names()
	if len(names) != 1 || names[0] != fixtureStage+"/result.json" {
		t.Errorf("captured %v, want just the result.json revision", names)
	}
}

// TestCaptureStageOutputs_RejectsLinkEscape closes the escape a lexical check
// cannot see: the agent has write access inside its own worktree, so it can
// plant a link and declare the link.
//
// The link target is relative, which is the shape that actually exercises the
// containment walk — an absolute target is refused on sight, so it would pass
// this test without proving the traversal is checked. On Windows, where
// unprivileged symlink creation is off, the fallback is a directory junction,
// whose target must be absolute; that case is worth having anyway, since
// filepath.EvalSymlinks does not follow junctions at all.
func TestCaptureStageOutputs_RejectsLinkEscape(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	link := filepath.Join(fixture.worktreeDir, "escape")
	if err := os.Symlink("../outside", link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skipf("cannot create symlink: %v", err)
		}
		if out, mkErr := exec.Command("cmd", "/c", "mklink", "/J", link, fixture.outsideDir).CombinedOutput(); mkErr != nil {
			t.Skipf("cannot create a junction: %v (%s)", mkErr, out)
		}
	}
	// Control: the host file really is reachable through the link, so the
	// rejection below is the guard working rather than a missing file.
	if _, err := os.ReadFile(filepath.Join(link, "secret.txt")); err != nil {
		t.Fatalf("control read through the link failed: %v", err)
	}

	err := fixture.capture(t, agent.Artifact{Path: "escape/secret.txt"})
	if !errors.Is(err, ErrArtifactEscapesWorktree) {
		t.Fatalf("captureStageOutputs = %v, want ErrArtifactEscapesWorktree", err)
	}
	assertNoHostFileCaptured(t, fixture, "host secret")
}

// TestCaptureStageOutputs_RefusedPutDoesNotFailTheStage: a Put the store
// refuses (a credential-shaped artifact under a reject policy) is a gap, not a
// breach — nothing was read that should not have been. The stage survives and
// the refusal is on the event stream, since an absent revision alone is
// indistinguishable from an artifact the agent never wrote.
func TestCaptureStageOutputs_RefusedPutDoesNotFailTheStage(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fixture.store.failWith = artifacts.ErrSecretDetected
	writeInWorktree(t, fixture.worktreeDir, "config.yaml", "token: ghp_x")

	if err := fixture.capture(t, agent.Artifact{Path: "config.yaml"}); err != nil {
		t.Fatalf("captureStageOutputs = %v, want nil for a refused Put", err)
	}
	reasons := fixture.emittedReasons()
	if len(reasons) == 0 {
		t.Fatal("a refused Put emitted no rejection event")
	}
	for _, reason := range reasons {
		if !strings.Contains(reason, "secret_detected") {
			t.Errorf("rejection event %q does not name secret_detected", reason)
		}
	}
}

// TestCaptureStageOutputs_NilStoreIsANoop keeps the unit-test wiring honest:
// runners built without an artifact store must not fail stages.
func TestCaptureStageOutputs_NilStoreIsANoop(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fixture.runner.art = nil

	if err := fixture.capture(t, agent.Artifact{Path: filepath.Join(fixture.outsideDir, "secret.txt")}); err != nil {
		t.Fatalf("captureStageOutputs with a nil store = %v, want nil", err)
	}
}

// assertNoHostFileCaptured verifies the escaping content never reached the
// store, whatever name it might have been filed under.
func assertNoHostFileCaptured(t *testing.T, fixture *captureFixture, content string) {
	t.Helper()
	fixture.store.mu.Lock()
	defer fixture.store.mu.Unlock()
	for _, put := range fixture.store.puts {
		if strings.Contains(string(put.Bytes), content) {
			t.Fatalf("host file content reached the artifact store under name %q", put.Name)
		}
	}
}

func writeInWorktree(t *testing.T, root, relPath, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}
