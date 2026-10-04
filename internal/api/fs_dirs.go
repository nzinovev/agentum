package api

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type fsDirsResponse struct {
	Path        string       `json:"path"`
	Parent      string       `json:"parent"`
	Directories []fsDirEntry `json:"directories"`
}

type fsDirEntry struct {
	Name                string `json:"name"`
	Path                string `json:"path"`
	IsGit               bool   `json:"is_git"`
	Shallow             bool   `json:"shallow"`
	HasCommits          bool   `json:"has_commits"`
	RegisteredProjectID string `json:"registered_project_id"`
}

func (api *API) handleListFSDirs(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireAccess(w, r, authz.ActionFSDirs, "")
	if !ok {
		return
	}
	if !loopbackPeer(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, codeForbidden, "directory browsing requires a loopback connection")
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "home directory is unavailable")
		return
	}
	homePath, err := filepath.EvalSymlinks(home)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal, "home directory is unavailable")
		return
	}

	requestedPath := r.URL.Query().Get("path")
	if requestedPath == "" {
		requestedPath = homePath
	}
	if !filepath.IsAbs(requestedPath) {
		writeError(w, http.StatusBadRequest, codeBadInput, "path must be absolute")
		return
	}
	currentPath, err := filepath.EvalSymlinks(requestedPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeBadInput, "path is unavailable")
		return
	}
	if !withinHome(homePath, currentPath) {
		writeError(w, http.StatusForbidden, codeForbidden, "path is outside the home directory")
		return
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil || !currentInfo.IsDir() {
		writeError(w, http.StatusBadRequest, codeBadInput, "path is not an accessible directory")
		return
	}

	directoryEntries, err := os.ReadDir(currentPath)
	if err != nil {
		writeError(w, http.StatusForbidden, codeForbidden, "directory is not accessible")
		return
	}
	response := fsDirsResponse{Path: currentPath, Directories: make([]fsDirEntry, 0, len(directoryEntries))}
	if currentPath != homePath {
		response.Parent = filepath.Dir(currentPath)
	}
	for _, directoryEntry := range directoryEntries {
		entryPath := filepath.Join(currentPath, directoryEntry.Name())
		resolvedPath, resolveErr := filepath.EvalSymlinks(entryPath)
		if resolveErr != nil || !withinHome(homePath, resolvedPath) {
			continue
		}
		entryInfo, statErr := os.Stat(resolvedPath)
		if statErr != nil || !entryInfo.IsDir() {
			continue
		}
		isGit, shallow, hasCommits := probeGitDirectory(r.Context(), resolvedPath)
		response.Directories = append(response.Directories, fsDirEntry{
			Name: directoryEntry.Name(), Path: resolvedPath, IsGit: isGit,
			Shallow: shallow, HasCommits: hasCommits,
		})
	}

	registeredPaths, err := api.registeredDirectoryPaths(r.Context(), principal)
	if err != nil {
		if api.log != nil {
			logUnexpected(api.log, err, "ListProjects(fs dirs)")
		}
		writeError(w, http.StatusInternalServerError, codeInternal, "registered projects are unavailable")
		return
	}
	for index := range response.Directories {
		response.Directories[index].RegisteredProjectID = registeredPaths[response.Directories[index].Path]
	}
	writeJSON(w, http.StatusOK, response)
}

func loopbackPeer(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.Unmap().IsLoopback()
}

func withinHome(homePath, candidatePath string) bool {
	relativePath, err := filepath.Rel(homePath, candidatePath)
	return err == nil && relativePath != ".." && !strings.HasPrefix(relativePath, ".."+string(filepath.Separator))
}

func probeGitDirectory(ctx context.Context, directoryPath string) (bool, bool, bool) {
	if _, err := os.Lstat(filepath.Join(directoryPath, ".git")); err != nil {
		return false, false, false
	}
	probeContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rootOutput, err := fsGitOutput(probeContext, directoryPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, false, false
	}
	rootPath, err := filepath.EvalSymlinks(rootOutput)
	if err != nil || rootPath != directoryPath {
		return false, false, false
	}
	shallowOutput, shallowErr := fsGitOutput(probeContext, directoryPath, "rev-parse", "--is-shallow-repository")
	if shallowErr != nil {
		return true, false, false
	}
	_, commitErr := fsGitOutput(probeContext, directoryPath, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	return true, shallowOutput == "true", commitErr == nil
}

func fsGitOutput(ctx context.Context, directoryPath string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directoryPath}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0")
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

func (api *API) registeredDirectoryPaths(ctx context.Context, principal authz.Principal) (map[string]string, error) {
	registeredPaths := make(map[string]string)
	if api.queries == nil {
		return registeredPaths, nil
	}
	const pageSize int32 = 200
	for offset := int32(0); ; offset += pageSize {
		projects, err := api.queries.ListProjects(ctx, sqlc.ListProjectsParams{
			TenantID: principal.TenantID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, project := range projects {
			if project.UserID != principal.UserID {
				continue
			}
			resolvedPath, resolveErr := filepath.EvalSymlinks(project.RepoPath)
			if resolveErr == nil {
				registeredPaths[resolvedPath] = project.ID
			}
		}
		if len(projects) < int(pageSize) {
			return registeredPaths, nil
		}
	}
}
