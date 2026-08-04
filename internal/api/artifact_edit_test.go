package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/authz"
)

// editArtifactStore is an artifacts.Store fake for the edit-endpoint tests.
// It records Puts, returns a configurable Current revision / error, and a
// configurable Put error so each status-mapping case is exercisable. The read
// methods beyond Current are present to satisfy the interface and fail loudly
// if the edit path ever calls one.
type editArtifactStore struct {
	current    artifacts.Revision
	hasCurrent bool
	currentErr error
	putErr     error
	lastPut    artifacts.PutParams
}

func (store *editArtifactStore) Put(_ context.Context, params artifacts.PutParams) (artifacts.Revision, error) {
	if store.putErr != nil {
		return artifacts.Revision{}, store.putErr
	}
	store.lastPut = params
	return artifacts.Revision{
		ID: "rev-new", Name: params.Name, ContentHash: artifacts.Hash(params.Bytes),
		Kind: params.Kind, Actor: params.Actor, IsCurrent: true,
	}, nil
}

func (store *editArtifactStore) Current(_ context.Context, _, _, _ string) (artifacts.Revision, error) {
	if store.currentErr != nil {
		return artifacts.Revision{}, store.currentErr
	}
	if !store.hasCurrent {
		return artifacts.Revision{}, artifacts.ErrNoCurrentRevision
	}
	return store.current, nil
}

func (store *editArtifactStore) Get(context.Context, string, string) (artifacts.Revision, error) {
	return artifacts.Revision{}, errors.New("not used")
}
func (store *editArtifactStore) GetBytes(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("not used")
}
func (store *editArtifactStore) Reader(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (store *editArtifactStore) CopyTo(_ context.Context, _, _ string, writer io.Writer) (int64, error) {
	n, err := writer.Write([]byte("current content"))
	return int64(n), err
}
func (store *editArtifactStore) ListForTask(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}
func (store *editArtifactStore) ListCurrent(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}
func (store *editArtifactStore) ListForInvocation(context.Context, string, string) ([]artifacts.Revision, error) {
	return nil, errors.New("not used")
}

// newEditAPI builds an API wired only with the artifact store (no DB), the
// minimum the edit handlers need. The principal is injected via the context,
// mirroring how the server boundary does it.
func newEditAPI(store artifacts.Store) *API {
	return New(nil, nil, nil, nil, WithArtifactStore(store))
}

func editRequest(method, target string, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	return request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: "tenant-1", UserID: "user-1",
	}))
}

// TestArtifactPut_SuccessfulEditCreatesHumanRevisionNoInvocation is the core D7
// invariant: a human edit creates a revision with actor = human and no source
// invocation, and two humans editing concurrently produce a 409 for the loser
// rather than a lost update. This case covers the happy path.
func TestArtifactPut_SuccessfulEditCreatesHumanRevisionNoInvocation(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{
		hasCurrent: true,
		current:    artifacts.Revision{ID: "rev-1", Name: "spec.md", ContentHash: "h-old", Kind: "spec"},
	}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md",
		`{"content":"new spec","expected_revision_id":"rev-1"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if store.lastPut.Actor != artifacts.ActorHuman {
		t.Errorf("Actor = %q, want human", store.lastPut.Actor)
	}
	if store.lastPut.Source != "" {
		t.Errorf("Source = %q, want empty (a human edit has no invocation)", store.lastPut.Source)
	}
	if store.lastPut.ExpectedCurrentRevision != "rev-1" {
		t.Errorf("ExpectedCurrentRevision = %q, want rev-1", store.lastPut.ExpectedCurrentRevision)
	}
}

// TestArtifactPut_RevisionConflictMapsTo409 covers the optimistic-concurrency
// loser: the store rejects a Put whose precondition no longer holds, and the
// handler must surface that as 409 conflict — not a lost update, not a 500.
func TestArtifactPut_RevisionConflictMapsTo409(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{
		hasCurrent: true,
		current:    artifacts.Revision{ID: "rev-1", Name: "spec.md", Kind: "spec"},
		putErr:     artifacts.ErrRevisionConflict,
	}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md",
		`{"content":"new spec","expected_revision_id":"rev-1"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), codeConflict) {
		t.Errorf("body = %s, want code %q", recorder.Body.String(), codeConflict)
	}
}

// TestArtifactPut_SecretDetectedMapsTo422 covers the reject-on-secret policy:
// the store refuses credential-shaped content, and the handler surfaces it as
// 422 so the caller knows the content was the problem, not the state.
func TestArtifactPut_SecretDetectedMapsTo422(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{
		hasCurrent: true,
		current:    artifacts.Revision{ID: "rev-1", Name: "spec.md", Kind: "spec"},
		putErr:     artifacts.ErrSecretDetected,
	}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md",
		`{"content":"token: ghp_x","expected_revision_id":"rev-1"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestArtifactGet_NoCurrentRevisionMapsTo404 covers the GET-on-missing case:
// an artifact the task has no revision of yet is a 404, matching the read-
// surface handlers' treatment of ErrNoCurrentRevision.
func TestArtifactGet_NoCurrentRevisionMapsTo404(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{currentErr: artifacts.ErrNoCurrentRevision}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodGet,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/never.md", "")
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactGet(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestArtifactPut_MissingPreconditionWhenCurrentExistsMapsTo428 pins the
// precondition policy: when the artifact already has a current revision, a PUT
// without expected_revision_id is rejected as 428 (precondition required). This
// is the safer choice — it prevents a blind overwrite of an existing artifact
// by a client that did not first GET the revision it is replacing.
func TestArtifactPut_MissingPreconditionWhenCurrentExistsMapsTo428(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{
		hasCurrent: true,
		current:    artifacts.Revision{ID: "rev-1", Name: "spec.md", Kind: "spec"},
	}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md",
		`{"content":"blind overwrite"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428; body=%s", recorder.Code, recorder.Body.String())
	}
	if store.lastPut.Name != "" {
		t.Error("a PUT rejected for a missing precondition reached the store; it must not")
	}
}

// TestArtifactPut_FirstCreateNeedsNoPrecondition covers the create case: an
// artifact with no current revision accepts a PUT with no precondition, since
// there is nothing to pin.
func TestArtifactPut_FirstCreateNeedsNoPrecondition(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{currentErr: artifacts.ErrNoCurrentRevision}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/new.md",
		`{"content":"fresh artifact","kind":"spec"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a first create; body=%s", recorder.Code, recorder.Body.String())
	}
	if store.lastPut.ExpectedCurrentRevision != "" {
		t.Errorf("create pinned a revision: %q", store.lastPut.ExpectedCurrentRevision)
	}
}

// TestArtifactPut_TransientCurrentErrorFailsHard is the fix for the failure
// mode where a transient Current() store error was collapsed into "no current
// revision," disabling the precondition and letting a blind PUT through. Any
// Current error other than ErrNoCurrentRevision must fail the request (500)
// before the precondition branching, so a store hiccup cannot become a silent
// overwrite. The artifact must not reach Put.
func TestArtifactPut_TransientCurrentErrorFailsHard(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{currentErr: errors.New("connection reset")}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md",
		`{"content":"blind overwrite, no precondition"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a transient Current error; body=%s", recorder.Code, recorder.Body.String())
	}
	if store.lastPut.Name != "" {
		t.Error("a PUT reached the store despite a transient Current error; the precondition was disabled")
	}
}

// TestArtifactPut_PreconditionOnCreateMapsTo409 covers the mismatch: a PUT that
// pins a revision against an artifact that has none is a conflict, not a create.
func TestArtifactPut_PreconditionOnCreateMapsTo409(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{currentErr: artifacts.ErrNoCurrentRevision}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodPut,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/new.md",
		`{"content":"fresh","expected_revision_id":"rev-x"}`)
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactPut(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestArtifactGet_ReturnsRevisionIdHeader verifies the GET handler surfaces the
// revision id in X-Revision-Id so a client can use it as the precondition for a
// subsequent PUT — without it, the optimistic-concurrency loop has no entry.
func TestArtifactGet_ReturnsRevisionIdHeader(t *testing.T) {
	t.Parallel()
	store := &editArtifactStore{
		hasCurrent: true,
		current:    artifacts.Revision{ID: "rev-1", Name: "spec.md", ContentHash: "h1", Kind: "spec"},
	}
	apiInst := newEditAPI(store)

	request := editRequest(http.MethodGet,
		"/api/v1/tasks/task-1/invocations/inv-1/artifacts/spec.md", "")
	recorder := httptest.NewRecorder()
	apiInst.handleArtifactGet(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Revision-Id"); got != "rev-1" {
		t.Errorf("X-Revision-Id = %q, want rev-1", got)
	}
}
