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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

// recordingArtifactStore is an artifacts.Store that records what was ingested.
// Put is exercised by the capture path; Current/GetBytes return the most
// recently Put bytes for a name so the verdict-read path (buildTransitionContext)
// can resolve a verdict.json without a live database. The other read methods
// remain "not used" stubs that fail loudly if called.
type recordingArtifactStore struct {
	mu   sync.Mutex
	puts []artifacts.PutParams
	// currentByName maps a revision name to its most-recent Put bytes, so
	// Current + GetBytes can serve the verdict artifact the capture path
	// ingested. Updated on every Put.
	currentByName map[string][]byte
	// kindByName remembers each name's kind so ListCurrent can report it
	// (priorStageRefs filters revisions by kind).
	kindByName map[string]string
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
	if store.currentByName == nil {
		store.currentByName = make(map[string][]byte)
	}
	// Stash a copy so later mutation of params.Bytes cannot retroactively
	// change what GetBytes returns.
	stashed := make([]byte, len(params.Bytes))
	copy(stashed, params.Bytes)
	store.currentByName[params.Name] = stashed
	if store.kindByName == nil {
		store.kindByName = make(map[string]string)
	}
	store.kindByName[params.Name] = params.Kind
	// A stable, distinct id + content hash per ingest so the manifest-ref
	// assertions can distinguish revisions by more than name. Hash mirrors the
	// real store: the bytes determine the hash.
	return artifacts.Revision{
		ID:          "rev-" + params.Name,
		Name:        params.Name,
		ContentHash: artifacts.Hash(params.Bytes),
	}, nil
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

func (store *recordingArtifactStore) Get(_ context.Context, _ string, revisionID string) (artifacts.Revision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	// The Put-generated id is "rev-<name>"; reverse it so Get/GetBytes agree
	// with Current's returned id for the same name.
	for name := range store.currentByName {
		if "rev-"+name == revisionID {
			return artifacts.Revision{ID: revisionID, Name: name}, nil
		}
	}
	return artifacts.Revision{}, artifacts.ErrNoCurrentRevision
}
func (store *recordingArtifactStore) GetBytes(_ context.Context, _ string, revisionID string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for name, bytes := range store.currentByName {
		if "rev-"+name == revisionID {
			stashed := make([]byte, len(bytes))
			copy(stashed, bytes)
			return stashed, nil
		}
	}
	return nil, errors.New("not found")
}
func (*recordingArtifactStore) Reader(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (*recordingArtifactStore) CopyTo(context.Context, string, string, io.Writer) (int64, error) {
	return 0, errors.New("not used")
}
func (store *recordingArtifactStore) Current(_ context.Context, _ string, _ string, name string) (artifacts.Revision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.currentByName[name]; ok {
		return artifacts.Revision{ID: "rev-" + name, Name: name}, nil
	}
	return artifacts.Revision{}, artifacts.ErrNoCurrentRevision
}
func (*recordingArtifactStore) ListForTask(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}
func (store *recordingArtifactStore) ListCurrent(context.Context, string, string) ([]artifacts.Revision, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	names := make([]string, 0, len(store.currentByName))
	for name := range store.currentByName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]artifacts.Revision, 0, len(names))
	for _, name := range names {
		out = append(out, artifacts.Revision{
			ID:   "rev-" + name,
			Name: name,
			Kind: store.kindByName[name],
		})
	}
	return out, nil
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
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{}}
	runner := New(Deps{
		Store:     events,
		Artifacts: store,
		Adapter:   adapter,
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
func (fixture *captureFixture) capture(t *testing.T, declared ...agent.Artifact) ([]manifest.ArtifactRef, error) {
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

	if _, err := fixture.capture(t, agent.Artifact{Path: "docs/spec.md", Kind: "spec"}); err != nil {
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

	_, err := fixture.capture(t, agent.Artifact{Path: outside, Kind: "spec"})
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

	_, err := fixture.capture(t, agent.Artifact{Path: "../outside/secret.txt"})
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

	_, err := fixture.capture(t,
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

	if _, err := fixture.capture(t, agent.Artifact{Path: "docs/never-written.md"}); err != nil {
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

	_, err := fixture.capture(t, agent.Artifact{Path: "escape/secret.txt"})
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

	if _, err := fixture.capture(t, agent.Artifact{Path: "config.yaml"}); err != nil {
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

	if _, err := fixture.capture(t, agent.Artifact{Path: filepath.Join(fixture.outsideDir, "secret.txt")}); err != nil {
		t.Fatalf("captureStageOutputs with a nil store = %v, want nil", err)
	}
}

// TestCaptureStageOutputs_RecordsOutputRefsForStoredRevisions is D3: a stage
// that captures two artifacts must yield two manifest refs carrying the
// revision id and content hash the store returned, plus result.json's own ref.
// Before D3 the revision returned by Put was discarded, so the manifest's
// artifacts.outputs was always empty and the diff endpoint always reported no
// difference between runs.
func TestCaptureStageOutputs_RecordsOutputRefsForStoredRevisions(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	writeInWorktree(t, fixture.worktreeDir, "docs/spec.md", "the spec")
	writeInWorktree(t, fixture.worktreeDir, "src/main.go", "package main")

	refs, err := fixture.capture(t,
		agent.Artifact{Path: "docs/spec.md", Kind: "spec"},
		agent.Artifact{Path: "src/main.go", Kind: "code"},
	)
	if err != nil {
		t.Fatalf("captureStageOutputs: %v", err)
	}
	// result.json + the two declared artifacts.
	if len(refs) != 3 {
		t.Fatalf("captured %d refs, want 3 (result.json + 2 declared): %+v", len(refs), refs)
	}
	byName := make(map[string]manifest.ArtifactRef, len(refs))
	for _, ref := range refs {
		byName[ref.Name] = ref
	}
	for _, name := range []string{fixtureStage + "/result.json", "docs/spec.md", "src/main.go"} {
		ref, ok := byName[name]
		if !ok {
			t.Errorf("missing ref for %q", name)
			continue
		}
		if ref.RevisionID == "" {
			t.Errorf("ref %q has empty revision id; the manifest would point at nothing", name)
		}
		if ref.ContentHash == "" {
			t.Errorf("ref %q has empty content hash; the manifest could not verify the bytes", name)
		}
		if ref.Stage != fixtureStage {
			t.Errorf("ref %q stage = %q, want %q", name, ref.Stage, fixtureStage)
		}
	}
}

// TestCaptureStageOutputs_ReflectedPutRecordsNoRef guards the input side of D3:
// a Put the store refuses (here, a secret-shaped artifact under reject policy)
// must not produce a manifest ref, because the revision it would reference was
// never stored. The stage still succeeds — the gap is on the event stream — but
// the manifest records only revisions that actually exist.
func TestCaptureStageOutputs_RefusedPutRecordsNoRef(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fixture.store.failWith = artifacts.ErrSecretDetected
	writeInWorktree(t, fixture.worktreeDir, "docs/spec.md", "the spec")

	refs, err := fixture.capture(t, agent.Artifact{Path: "docs/spec.md", Kind: "spec"})
	if err != nil {
		t.Fatalf("captureStageOutputs: %v, want nil for a refused Put", err)
	}
	for _, ref := range refs {
		if ref.Name == "docs/spec.md" {
			t.Errorf("refused Put produced a manifest ref %+v; the revision was never stored", ref)
		}
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

// fakeManifestService is a manifestService that records calls and can be told
// to fail. Used to exercise the evidence-gap path (D5) without a database.
type fakeManifestService struct {
	mu          sync.Mutex
	addEvidence []manifest.Body
	gaps        []manifest.EvidenceGap
	sealed      bool
	// addErr, when set, is returned from AddEvidence.
	addErr error
	// checksCommitValue is returned by ChecksCommit; "" means none recorded.
	checksCommitValue string
	// checksCommitErr, when set, is returned from ChecksCommit — the "the
	// verified commit could not be read" case, distinct from "none recorded".
	checksCommitErr error
}

func (service *fakeManifestService) AddEvidence(_ context.Context, _, _ string, patch manifest.Body) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.addErr != nil {
		return service.addErr
	}
	service.addEvidence = append(service.addEvidence, patch)
	return nil
}

func (service *fakeManifestService) Seal(_ context.Context, _, _, _ string, _ manifest.SealReason) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.sealed = true
	return nil
}

func (service *fakeManifestService) RecordGap(_ context.Context, _, _ string, gap manifest.EvidenceGap) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.gaps = append(service.gaps, gap)
	return nil
}

// checksCommit is the recorded checks commit, or "" when none was set. The E3
// teardown divergence test sets this to drive verifyDeliveryCommitBinding.
func (service *fakeManifestService) ChecksCommit(_ context.Context, _, _ string) (string, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.checksCommitErr != nil {
		return "", service.checksCommitErr
	}
	return service.checksCommitValue, nil
}

func (service *fakeManifestService) gapSections() []string {
	service.mu.Lock()
	defer service.mu.Unlock()
	out := make([]string, 0, len(service.gaps))
	for _, gap := range service.gaps {
		out = append(out, gap.Section)
	}
	return out
}

// TestCompleteStageEvidence_AddEvidenceFailureRecordsGapAndSurvives: when
// AddEvidence fails mid-run, the runner must record the failure as an
// EvidenceGap on the manifest rather than swallow it, and the stage must
// survive. A sealed manifest that swallows the failure (degrading silently) is
// worse than an absent record because a reviewer cannot tell the two apart.
//
// The successful attempt's evidence is one write covering three sections, so a
// failure records a gap against EACH of them: a reviewer asks whether the
// artifacts section is trustworthy, not which call failed.
func TestCompleteStageEvidence_AddEvidenceFailureRecordsGapAndSurvives(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fake := &fakeManifestService{addErr: errors.New("db connection lost")}
	fixture.runner.mfst = fake

	task := sqlc.Task{ID: fixtureTask, TenantID: "tenant-1", UserID: "user-1"}
	taskPack := scriptPack(fixtureStage, map[string]pack.Stage{
		fixtureStage: {Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "done"}}},
	})
	run := stageRun{task: task, taskPack: taskPack}

	// Must not panic or fail the stage; the run continues.
	outputs := []manifest.ArtifactRef{{
		Name: fixtureStage + "/result.json", RevisionID: "rev-1", ContentHash: "h",
		Stage: fixtureStage, InvocationID: "inv-1",
	}}
	fixture.runner.completeStageEvidence(context.Background(), run, fixtureStage, "inv-1", agent.Telemetry{}, outputs)

	gaps := fake.gapSections()
	recorded := make(map[string]bool, len(gaps))
	for _, section := range gaps {
		recorded[section] = true
	}
	for _, want := range []string{"invocations", "context", "artifacts"} {
		if !recorded[want] {
			t.Errorf("no gap recorded for the %q section: %v", want, gaps)
		}
	}
	if len(gaps) != 3 {
		t.Errorf("AddEvidence failure recorded %d gaps, want one per covered section: %v", len(gaps), gaps)
	}
}

// TestCompleteStageEvidence_WritesOnce pins the fold: a successful attempt
// leaves ONE manifest transaction carrying the invocation close, the artifact
// outputs and the context section. AddEvidence is a full-document
// read-modify-write under the row lock, so an extra call is an extra decode
// and re-encode of a body that grows for the life of the run — and three
// separate writes could leave the attempt closed with its artifacts missing.
func TestCompleteStageEvidence_WritesOnce(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fake := &fakeManifestService{}
	fixture.runner.mfst = fake

	task := sqlc.Task{ID: fixtureTask, TenantID: "tenant-1", UserID: "user-1"}
	taskPack := scriptPack(fixtureStage, map[string]pack.Stage{
		fixtureStage: {Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "done"}}},
	})
	run := stageRun{task: task, taskPack: taskPack}
	outputs := []manifest.ArtifactRef{{
		Name: fixtureStage + "/result.json", RevisionID: "rev-1", ContentHash: "h",
		Stage: fixtureStage, InvocationID: "inv-1",
	}}

	telemetry := agent.Telemetry{Cost: 0.25}
	telemetry.Tokens.Total = 1234
	fixture.runner.completeStageEvidence(context.Background(), run, fixtureStage, "inv-1", telemetry, outputs)

	if len(fake.addEvidence) != 1 {
		t.Fatalf("manifest writes = %d, want 1: %+v", len(fake.addEvidence), fake.addEvidence)
	}
	patch := fake.addEvidence[0]
	if len(patch.Invocations) != 1 || patch.Invocations[0].InvocationID != "inv-1" {
		t.Errorf("patch carries no invocation close: %+v", patch.Invocations)
	}
	if patch.Invocations[0].Telemetry == nil || patch.Invocations[0].Telemetry.Tokens.Total != 1234 {
		t.Errorf("measured telemetry must reach the record: %+v", patch.Invocations[0].Telemetry)
	}
	if patch.Artifacts == nil || len(patch.Artifacts.Outputs) != 1 {
		t.Errorf("patch carries no artifact outputs: %+v", patch.Artifacts)
	}
	if patch.Context == nil {
		t.Error("patch carries no context section")
	}
}

// TestRecordInitialEvidence_FailureFailsTheTask is D5's exception: the initial
// evidence is the run's provenance root, so a failure there must propagate
// (the caller fails the task through failTask) rather than degrade.
// recordInitialEvidence returns the error; this test pins that it does, so the
// caller can fail the run.
func TestRecordInitialEvidence_FailureFailsTheTask(t *testing.T) {
	t.Parallel()
	fixture := newCaptureFixture(t)
	fixture.runner.mfst = &fakeManifestService{addErr: errors.New("db connection lost")}

	task := sqlc.Task{ID: fixtureTask, TenantID: "tenant-1", UserID: "user-1"}
	project := sqlc.Project{ID: "proj-1"}
	taskPack := scriptPack(fixtureStage, map[string]pack.Stage{
		fixtureStage: {Gate: pack.GateAuto, Transitions: []pack.Transition{{To: "done"}}},
	})

	err := fixture.runner.recordInitialEvidence(context.Background(), task, project, taskPack)
	if err == nil {
		t.Fatal("recordInitialEvidence with a failing manifest service returned nil; the provenance root must fail rather than degrade")
	}
}
