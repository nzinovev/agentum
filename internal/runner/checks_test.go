package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestRunner_DeliveryChecks proves the orchestrator runs its own project checks
// at the delivery boundary (the terminal stage) and blocks delivery on a
// mandatory failure. The check registry is committed into the repo so the
// worktree — branched from base_commit — carries it; the executor runs each
// check there. `true`/`false` are real binaries on the CI/linux host these
// runner tests target (the runner package is linux-built).
func TestRunner_DeliveryChecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		checksYAML string
		wantState  string
		wantErr    bool
	}{
		{
			name:       "passing mandatory check reaches final gate",
			checksYAML: "api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n    required: true\n",
			wantState:  "awaiting_memory_commit",
		},
		{
			name:       "failing mandatory check blocks delivery",
			checksYAML: "api: agentum/v1\nchecks:\n  - name: build\n    command: [\"false\"]\n    required: true\n",
			wantState:  "failed",
			wantErr:    true,
		},
		{
			name: "failing optional check does not block delivery",
			checksYAML: "api: agentum/v1\nchecks:\n" +
				"  - name: build\n    command: [\"true\"]\n    required: true\n" +
				"  - name: lint\n    command: [\"false\"]\n",
			wantState: "awaiting_memory_commit",
		},
		{
			name:       "no registry means no checks and delivery proceeds",
			checksYAML: "",
			wantState:  "awaiting_memory_commit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := initRepoWithCommit(repo); err != nil {
				t.Fatalf("setup repo: %v", err)
			}
			if tc.checksYAML != "" {
				if err := os.WriteFile(filepath.Join(repo, checks.ConfigFile), []byte(tc.checksYAML), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := gitCommitAll(repo, "add project checks"); err != nil {
					t.Fatalf("commit checks: %v", err)
				}
			}

			// Single auto stage → terminal: one run job reaches the terminal
			// stage, where the delivery checks are enforced.
			taskPack := scriptPack("spec", map[string]pack.Stage{
				"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
				"done": {},
			})
			task := sqlc.Task{ID: "Tc", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
			proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
			store := newFakeStore(task, proj)
			adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
				"spec": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
			}}
			executor := checks.NewExecutor(checks.ExecutorDeps{})
			runner := New(Deps{
				Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
				AgentName: "opencode", CheckExec: executor,
			})

			handleErr := runner.Handle(context.Background(), job("run", "Tc", "tn", "us"))
			if tc.wantErr && handleErr == nil {
				t.Fatal("expected Handle to return an error for the blocked delivery, got nil")
			}
			if !tc.wantErr && handleErr != nil {
				t.Fatalf("unexpected Handle error: %v", handleErr)
			}
			if got := store.taskState(); got != tc.wantState {
				t.Fatalf("state = %q, want %q", got, tc.wantState)
			}
		})
	}
}

// TestRunner_DeliveryChecksUnknownPackCheckFails proves a pack referencing an
// unregistered check name fails the task rather than running an arbitrary
// command — the registry is the only source of commands.
func TestRunner_DeliveryChecksUnknownPackCheckFails(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	// Registry defines only "build"; the pack requires the unknown "nope".
	if err := os.WriteFile(filepath.Join(repo, checks.ConfigFile), []byte(
		"api: agentum/v1\nchecks:\n  - name: build\n    command: [\"true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitCommitAll(repo, "add checks"); err != nil {
		t.Fatalf("commit checks: %v", err)
	}

	taskPack := scriptPack("spec", map[string]pack.Stage{
		"spec": {Gate: pack.GateAuto, Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
		"done": {},
	})
	taskPack.Checks = pack.CheckPolicy{Required: []string{"nope"}}
	task := sqlc.Task{ID: "Tu", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "P"}
	store := newFakeStore(task, proj)
	adapter := &scriptAdapter{scripts: map[string]agent.ResultJSON{
		"spec": {SchemaVersion: "1", Status: agent.StatusComplete},
	}}
	executor := checks.NewExecutor(checks.ExecutorDeps{})
	runner := New(Deps{
		Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter,
		AgentName: "opencode", CheckExec: executor,
	})

	if err := runner.Handle(context.Background(), job("run", "Tu", "tn", "us")); err == nil {
		t.Fatal("expected Handle error for unregistered pack check name")
	}
	if got := store.taskState(); got != "failed" {
		t.Fatalf("state = %q, want failed (unknown check must block before execution)", got)
	}
}

// TestPackAndTaskCheckRequests covers the pure mapping helpers that turn the
// pack policy and the task input JSON into checks.Request lists.
func TestPackAndTaskCheckRequests(t *testing.T) {
	t.Run("pack policy", func(t *testing.T) {
		requests := packCheckRequests(&pack.Pack{Checks: pack.CheckPolicy{
			Required: []string{"build"}, Optional: []string{"lint"},
		}})
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests, got %d", len(requests))
		}
		if !requests[0].Required || requests[0].Name != "build" {
			t.Errorf("build should be required: %+v", requests[0])
		}
		if requests[1].Required || requests[1].Name != "lint" {
			t.Errorf("lint should be optional: %+v", requests[1])
		}
		if packCheckRequests(nil) != nil {
			t.Error("nil pack must yield nil requests")
		}
	})
	t.Run("task input", func(t *testing.T) {
		requests := taskCheckRequests(json.RawMessage(`{"checks":{"required":["t1"],"optional":["t2"]}}`))
		if len(requests) != 2 {
			t.Fatalf("expected 2 requests, got %d", len(requests))
		}
		if !requests[0].Required || requests[0].Name != "t1" {
			t.Errorf("t1 should be required: %+v", requests[0])
		}
		if taskCheckRequests(nil) != nil {
			t.Error("nil input must yield nil")
		}
		if taskCheckRequests(json.RawMessage(`{not json`)) != nil {
			t.Error("malformed input must yield nil")
		}
	})
}

// gitCommitAll stages every change under dir and commits it, so the worktree
// (branched from the repo HEAD) carries the committed files.
func gitCommitAll(dir, message string) error {
	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", message}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if _, err := cmd.CombinedOutput(); err != nil {
			return err
		}
	}
	return nil
}
