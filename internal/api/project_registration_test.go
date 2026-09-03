package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The registration contract these tests pin is the one the identity scheme
// exists for: a repository is the same project wherever it currently lives on
// disk, a second working copy does not capture runs that started in the first,
// and a relocation carries unfinished runs with it. They run against a real
// Postgres (dbtest) and real git repositories in temp directories, because
// both sides of that contract — the unique key and the git probe — are exactly
// what a fake would smooth over.

const (
	testTenantID = "1bd6f9a6-9c62-4a08-b3d4-ce5fd1b91c02"
	testUserID   = "2ce7c0b7-ad73-4b19-a4e5-df60e2ca2d03"
)

// registrationHarness bundles one database, one API, and the caller's
// principal — everything a registration test needs.
type registrationHarness struct {
	api       *API
	queries   *sqlc.Queries
	t         *testing.T
	principal authz.Principal
}

func newRegistrationHarness(t *testing.T) *registrationHarness {
	t.Helper()
	handle := dbtest.Store(t)
	return &registrationHarness{
		api:       New(handle.Store.DB, handle.Queries, slog.New(slog.DiscardHandler), nil),
		queries:   handle.Queries,
		t:         t,
		principal: authz.Principal{TenantID: testTenantID, UserID: testUserID},
	}
}

// registerProject posts the registration body and returns the decoded
// response, failing the test on a non-201 answer.
func (harness *registrationHarness) registerProject(repoPath string) projectResponse {
	harness.t.Helper()

	requestBody, err := json.Marshal(map[string]any{
		"repo_path": repoPath,
		"name":      "harness project",
	})
	if err != nil {
		harness.t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(string(requestBody)))
	request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
	recorder := httptest.NewRecorder()
	harness.api.handleCreateProject(recorder, request)
	if recorder.Code != http.StatusCreated {
		harness.t.Fatalf("register %s: status %d, body %s", repoPath, recorder.Code, recorder.Body.String())
	}
	var response projectResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		harness.t.Fatalf("decode response: %v", err)
	}
	return response
}

// initRegistrationRepo turns dir into a committed git work tree with a unique
// root commit (the marker keeps two fixtures from hashing to the same SHA).
func initRegistrationRepo(t *testing.T, dir, marker string) {
	t.Helper()
	runRegistrationGit(t, dir, "init", "--quiet", "--initial-branch=main")
	runRegistrationGit(t, dir, "config", "user.email", "test@agentum")
	runRegistrationGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+marker+"\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runRegistrationGit(t, dir, "add", "README.md")
	runRegistrationGit(t, dir, "commit", "--quiet", "-m", "init "+marker)
}

func runRegistrationGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
}

// seedRun creates a run of the given project pinned to checkoutPath; state
// "done" marks it terminal so a rebind must leave it alone.
func (harness *registrationHarness) seedRun(projectID, state, checkoutPath string) sqlc.Task {
	harness.t.Helper()
	run, err := harness.queries.CreateTask(context.Background(), sqlc.CreateTaskParams{
		TenantID: testTenantID, UserID: testUserID, ProjectID: projectID, PipelinePack: "test@1",
		Title: "run", Description: "seed", Overrides: []byte("{}"), BaseRef: "HEAD",
	})
	if err != nil {
		harness.t.Fatalf("create task: %v", err)
	}
	if state == "done" {
		if _, err := harness.queries.UpdateTaskState(context.Background(), sqlc.UpdateTaskStateParams{
			ID: run.ID, TenantID: testTenantID, State: "done",
		}); err != nil {
			harness.t.Fatalf("finish task: %v", err)
		}
	}
	if checkoutPath != "" {
		if _, err := harness.queries.SetCheckoutPath(context.Background(), sqlc.SetCheckoutPathParams{
			ID: run.ID, TenantID: testTenantID, CheckoutPath: checkoutPath,
		}); err != nil {
			harness.t.Fatalf("pin checkout: %v", err)
		}
	}
	return run
}

// TestProjectRegistration_SamePathTwiceReturnsOneProject: idempotency moved
// from the path to the identity, but the user-visible contract is unchanged —
// the same repository registered twice is one project.
func TestProjectRegistration_SamePathTwiceReturnsOneProject(t *testing.T) {
	harness := newRegistrationHarness(t)

	repo := t.TempDir()
	initRegistrationRepo(t, repo, "idempotent")

	first := harness.registerProject(repo)
	second := harness.registerProject(repo)

	if second.ID != first.ID {
		t.Fatalf("re-registration created a second project: %s vs %s", second.ID, first.ID)
	}
	if second.RepoIdentity == "" || second.RepoIdentity != first.RepoIdentity {
		t.Fatalf("identity drifted on re-registration: %q vs %q", second.RepoIdentity, first.RepoIdentity)
	}
	if second.PreviousRepoPath != "" || second.RunsAwaitingPreviousCheckout != 0 {
		t.Fatalf("same-path registration reported a move: %+v", second)
	}
}

// TestProjectRegistration_MoveKeepsIdentityAndHistory: the acceptance case the
// identity exists for — a repository moved on disk and registered from its new
// location is the SAME project (run history stays attached), and unfinished
// runs follow the working copy, because the copy is one and it relocated.
func TestProjectRegistration_MoveKeepsIdentityAndHistory(t *testing.T) {
	harness := newRegistrationHarness(t)

	original := t.TempDir()
	initRegistrationRepo(t, original, "move")

	first := harness.registerProject(original)
	activeRun := harness.seedRun(first.ID, "running", original)
	terminalRun := harness.seedRun(first.ID, "done", original)

	moved := filepath.Join(t.TempDir(), "moved-repo")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move repo: %v", err)
	}

	second := harness.registerProject(moved)

	if second.ID != first.ID {
		t.Fatalf("a moved repository became a new project: %s vs %s", second.ID, first.ID)
	}
	if second.RepoPath != moved {
		t.Fatalf("repo_path = %q, want the new location %q", second.RepoPath, moved)
	}
	if second.RepoIdentity != first.RepoIdentity {
		t.Fatalf("identity changed across the move: %q vs %q", second.RepoIdentity, first.RepoIdentity)
	}
	if second.PreviousRepoPath != original {
		t.Fatalf("response must name the previous path %q, got %+v", original, second)
	}
	if second.RunsAwaitingPreviousCheckout != 0 {
		t.Fatalf("a relocation has no runs awaiting the gone copy; got %d", second.RunsAwaitingPreviousCheckout)
	}

	// The unfinished run follows the working copy; the terminal run keeps the
	// historical fact of where its work happened.
	rebound, err := harness.queries.GetTask(context.Background(), sqlc.GetTaskParams{ID: activeRun.ID, TenantID: testTenantID})
	if err != nil {
		t.Fatalf("load active run: %v", err)
	}
	if rebound.CheckoutPath != moved {
		t.Fatalf("active run checkout_path = %q, want the new location %q", rebound.CheckoutPath, moved)
	}
	terminal, err := harness.queries.GetTask(context.Background(), sqlc.GetTaskParams{ID: terminalRun.ID, TenantID: testTenantID})
	if err != nil {
		t.Fatalf("load terminal run: %v", err)
	}
	if terminal.CheckoutPath != original {
		t.Fatalf("terminal run checkout_path = %q, must keep the historical %q", terminal.CheckoutPath, original)
	}
}

// TestProjectRegistration_SecondCopyLeavesRunsWhereTheyStarted: registering
// the same repository from a second clone must not capture runs that started
// in the first copy — the response names the split instead, so the user learns
// about it now, not when a continuation lands somewhere unexpected.
func TestProjectRegistration_SecondCopyLeavesRunsWhereTheyStarted(t *testing.T) {
	harness := newRegistrationHarness(t)

	first := t.TempDir()
	initRegistrationRepo(t, first, "first-copy")
	second := t.TempDir()
	runRegistrationGit(harness.t, second, "clone", "--quiet", first, filepath.Join(second, "repo"))
	secondCopy := filepath.Join(second, "repo")

	firstResponse := harness.registerProject(first)
	activeRun := harness.seedRun(firstResponse.ID, "running", first)

	secondResponse := harness.registerProject(secondCopy)

	if secondResponse.ID != firstResponse.ID {
		t.Fatal("a clone of the same history must be the same project")
	}
	if secondResponse.RepoPath != secondCopy {
		t.Fatalf("repo_path = %q, want the second copy %q", secondResponse.RepoPath, secondCopy)
	}
	if secondResponse.PreviousRepoPath != first {
		t.Fatalf("response must name the previous copy %q, got %+v", first, secondResponse)
	}
	if secondResponse.RunsAwaitingPreviousCheckout != 1 {
		t.Fatalf("runs awaiting previous checkout = %d, want 1", secondResponse.RunsAwaitingPreviousCheckout)
	}

	pinnedRun, err := harness.queries.GetTask(context.Background(), sqlc.GetTaskParams{ID: activeRun.ID, TenantID: testTenantID})
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if pinnedRun.CheckoutPath != first {
		t.Fatalf("a second copy must not capture a started run: checkout_path = %q, want %q", pinnedRun.CheckoutPath, first)
	}
}

// TestProjectRegistration_RejectsUnusableWorkingCopies: each refusal is a 400
// whose message names the fix — the refusal must reach the caller at
// registration, where it is actionable, not at the first run.
func TestProjectRegistration_RejectsUnusableWorkingCopies(t *testing.T) {
	harness := newRegistrationHarness(t)

	rejected := []struct {
		name     string
		path     string
		wantText string
	}{
		{"plain directory", t.TempDir(), "not inside a git work tree"},
		{"repository without commits", emptyRepository(t), "no commits"},
		{"linked worktree", linkedWorktreeOf(t), "register the main work tree"},
	}
	for _, rejection := range rejected {
		t.Run(rejection.name, func(t *testing.T) {
			requestBody, err := json.Marshal(map[string]any{"repo_path": rejection.path, "name": "x"})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(string(requestBody)))
			request = request.WithContext(authz.WithPrincipal(request.Context(), harness.principal))
			recorder := httptest.NewRecorder()
			harness.api.handleCreateProject(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), rejection.wantText) {
				t.Fatalf("message %q does not name the fix (%q)", recorder.Body.String(), rejection.wantText)
			}
		})
	}
}

func emptyRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runRegistrationGit(t, dir, "init", "--quiet")
	return dir
}

func linkedWorktreeOf(t *testing.T) string {
	t.Helper()
	main := t.TempDir()
	initRegistrationRepo(t, main, "main-tree")
	linked := filepath.Join(t.TempDir(), "linked")
	runRegistrationGit(t, main, "worktree", "add", "--quiet", linked)
	return linked
}
