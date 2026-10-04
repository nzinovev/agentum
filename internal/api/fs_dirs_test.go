package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/authz"
)

func listFSDirs(t *testing.T, api *API, requestPath, remoteAddress string, principal bool) (int, fsDirsResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/fs/dirs"+requestPath, nil)
	request.RemoteAddr = remoteAddress
	request.Host = "localhost:8080"
	if principal {
		request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
			TenantID: testTenantID, UserID: testUserID,
		}))
	}
	recorder := httptest.NewRecorder()
	api.handleListFSDirs(recorder, request)
	var response fsDirsResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode directory response: %v", err)
		}
	}
	return recorder.Code, response
}

func TestListFSDirs_RejectsReboundHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	request := httptest.NewRequest(http.MethodGet, "/api/v1/fs/dirs", nil)
	request.RemoteAddr = "127.0.0.1:4000"
	request.Host = "attacker.example:8080"
	request = request.WithContext(authz.WithPrincipal(request.Context(), authz.Principal{
		TenantID: testTenantID, UserID: testUserID,
	}))
	recorder := httptest.NewRecorder()
	New(nil, nil, nil, nil).handleListFSDirs(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestLoopbackHost(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		host string
		want bool
	}{
		{host: "localhost:8080", want: true},
		{host: "127.0.0.1:8080", want: true},
		{host: "[::1]:8080", want: true},
		{host: "[::1]", want: true},
		{host: "attacker.example:8080"},
		{host: "localhost.attacker.example:8080"},
	} {
		t.Run(testCase.host, func(t *testing.T) {
			if got := loopbackHost(testCase.host); got != testCase.want {
				t.Errorf("loopbackHost(%q) = %v, want %v", testCase.host, got, testCase.want)
			}
		})
	}
}

func TestListFSDirs_AccessAndHomeBoundary(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("HOME", homePath)
	insidePath := filepath.Join(homePath, "inside")
	if err := os.Mkdir(insidePath, 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := t.TempDir()
	if err := os.Symlink(outsidePath, filepath.Join(homePath, "outside-link")); err != nil {
		t.Fatal(err)
	}
	api := New(nil, nil, nil, nil)
	testCases := []struct {
		name          string
		requestPath   string
		remoteAddress string
		principal     bool
		wantStatus    int
	}{
		{name: "missing principal", remoteAddress: "127.0.0.1:4000", wantStatus: http.StatusUnauthorized},
		{name: "remote peer", remoteAddress: "192.0.2.1:4000", principal: true, wantStatus: http.StatusForbidden},
		{name: "path outside home", requestPath: "?path=" + outsidePath, remoteAddress: "127.0.0.1:4000", principal: true, wantStatus: http.StatusForbidden},
		{name: "symlink outside home", requestPath: "?path=" + filepath.Join(homePath, "outside-link"), remoteAddress: "[::1]:4000", principal: true, wantStatus: http.StatusForbidden},
		{name: "relative path", requestPath: "?path=inside", remoteAddress: "127.0.0.1:4000", principal: true, wantStatus: http.StatusBadRequest},
		{name: "inside home", requestPath: "?path=" + insidePath, remoteAddress: "[::1]:4000", principal: true, wantStatus: http.StatusOK},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			status, _ := listFSDirs(t, api, testCase.requestPath, testCase.remoteAddress, testCase.principal)
			if status != testCase.wantStatus {
				t.Errorf("status = %d, want %d", status, testCase.wantStatus)
			}
		})
	}
	status, response := listFSDirs(t, api, "", "127.0.0.1:4000", true)
	if status != http.StatusOK || response.Path != homePath || response.Parent != "" {
		t.Errorf("home response = (%d, %+v)", status, response)
	}
	if len(response.Directories) != 1 || response.Directories[0].Path != insidePath {
		t.Errorf("home directories = %+v, want only inside", response.Directories)
	}
	status, response = listFSDirs(t, api, "?path="+insidePath, "127.0.0.1:4000", true)
	if status != http.StatusOK || response.Parent != homePath {
		t.Errorf("inside response = (%d, %+v)", status, response)
	}
}

func TestListFSDirs_GitLabels(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("HOME", homePath)
	committedPath := filepath.Join(homePath, "committed")
	emptyPath := filepath.Join(homePath, "empty")
	shallowPath := filepath.Join(homePath, "shallow")
	for _, directoryPath := range []string{committedPath, emptyPath} {
		if err := os.Mkdir(directoryPath, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initRegistrationRepo(t, committedPath, "directory-label")
	runRegistrationGit(t, emptyPath, "init", "--quiet", "--initial-branch=main")
	runRegistrationGit(t, homePath, "clone", "--quiet", "--depth=1", "file://"+committedPath, shallowPath)

	status, response := listFSDirs(t, New(nil, nil, nil, nil), "", "127.0.0.1:4000", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	byName := make(map[string]fsDirEntry)
	for _, directory := range response.Directories {
		byName[directory.Name] = directory
	}
	for _, testCase := range []struct {
		name       string
		shallow    bool
		hasCommits bool
	}{
		{name: "committed", hasCommits: true},
		{name: "empty"},
		{name: "shallow", shallow: true, hasCommits: true},
	} {
		entry, present := byName[testCase.name]
		if !present || !entry.IsGit || entry.Shallow != testCase.shallow || entry.HasCommits != testCase.hasCommits || entry.RegisteredProjectID != "" {
			t.Errorf("%s = %+v (present %v)", testCase.name, entry, present)
		}
	}
}

func TestListFSDirs_RegisteredProjectID(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("HOME", homePath)
	repositoryPath := filepath.Join(homePath, "registered")
	if err := os.Mkdir(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	initRegistrationRepo(t, repositoryPath, "registered-directory")
	harness := newRegistrationHarness(t)
	project := harness.registerProject(repositoryPath)
	status, response := listFSDirs(t, harness.api, "", "127.0.0.1:4000", true)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(response.Directories) != 1 || response.Directories[0].RegisteredProjectID != project.ID {
		t.Errorf("directory registration = %+v, want %s", response.Directories, project.ID)
	}
}
