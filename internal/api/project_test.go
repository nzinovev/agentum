package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

func TestValidateGitRepo(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	if err := initGitRepo(repo); err != nil {
		t.Fatalf("setup: git init: %v", err)
	}

	notARepo := t.TempDir() // plain dir, no .git

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid work tree", repo, false},
		{"plain dir without git", notARepo, true},
		{"missing path", filepath.Join(t.TempDir(), "does-not-exist"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateGitRepo(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateGitRepo(%q) err = %v, wantErr = %v", tc.path, err, tc.wantErr)
			}
		})
	}
}

func TestToProjectResponse_NilRelatedProjects(t *testing.T) {
	t.Parallel()
	// A project with no related set must serialize as [] not null — the public
	// shape stays stable regardless of the DB-side default.
	got := toProjectResponse(sqlc.Project{RelatedProjects: nil})
	if got.RelatedProjects == nil {
		t.Fatal("RelatedProjects must be a non-nil empty slice when the DB value is nil")
	}
	if len(got.RelatedProjects) != 0 {
		t.Fatalf("RelatedProjects = %v, want empty", got.RelatedProjects)
	}
}

// initGitRepo turns dir into a real git work tree so validateGitRepo accepts it.
// Requires git on PATH; the test environment provides it.
func initGitRepo(dir string) error {
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@agentum"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &repoSetupError{args: args, out: out, err: err}
		}
	}
	return nil
}

type repoSetupError struct {
	args []string
	out  []byte
	err  error
}

func (e *repoSetupError) Error() string {
	return "git " + e.args[0] + ": " + e.err.Error() + " (" + string(e.out) + ")"
}
