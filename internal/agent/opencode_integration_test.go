//go:build integration

// The tests in this file drive a real opencode subprocess and are excluded from
// CI (which has no opencode binary and no credentials). Run locally:
//
//	go test -tags integration ./internal/agent/ -run TestOpencodeLive -v
//
// They cover two contracts:
//
//   - F.2 reference adapter: invoke → stream → session-id capture → resume →
//     result.json read+parse.
//   - Capability enforcement: does the installed opencode load the permission
//     config this adapter generates, and does it obey it? Everything else about
//     capability profiles is asserted against rendered structs; this is the only
//     place the answer comes from the runtime itself.
//
// Each enforcement test asks for exactly one action. That is deliberate: a
// prompt combining a denied action with an allowed one made opencode stall with
// no output on either stream, and a stalled run answers nothing.
package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
)

// requireOpencode skips the test when no opencode binary is installed.
func requireOpencode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skipf("opencode not on PATH: %v", err)
	}
}

// liveWorktree creates a workdir shaped like the real thing: a git repository
// with one tracked source file and the per-stage artifact directory beneath it.
// The git part matters — opencode derives its project root from the git root,
// and both the relative permission patterns and `external_directory` are judged
// against that root, so a bare temp dir would not exercise the same boundary a
// task worktree does.
func liveWorktree(t *testing.T, stage string) (workdir, artifactDir, sourcePath string) {
	t.Helper()
	workdir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")

	sourcePath = filepath.Join(workdir, "main.go")
	if err := os.WriteFile(sourcePath, []byte(liveSource), 0o644); err != nil {
		t.Fatalf("seed source file: %v", err)
	}
	run("add", "main.go")
	run("-c", "user.email=live@test", "-c", "user.name=live", "commit", "-q", "-m", "base")

	// The runner adds .agentum/ to the repo's local excludes; mirror that so the
	// live run covers a git-ignored artifact tree.
	excludePath := filepath.Join(workdir, ".git", "info", "exclude")
	if existing, err := os.ReadFile(excludePath); err == nil {
		_ = os.WriteFile(excludePath, append(existing, []byte("\n.agentum/\n")...), 0o644)
	}

	artifactDir = filepath.Join(workdir, ".agentum", "task-1", ".ag-artifacts", stage)
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	return workdir, artifactDir, sourcePath
}

const liveSource = "package main\n\nfunc main() {}\n"

// liveProfile builds the effective profile for a role, with the caps a real
// invocation carries. The caps are not decoration: a misconfigured opencode
// blocks forever without writing a byte to either stream, so the idle cap is
// what turns a broken setup into a fast, named failure.
func liveProfile(t *testing.T, role caps.Role) caps.Profile {
	t.Helper()
	declared := []caps.Token{caps.Token(caps.CatFsRead), caps.Token(caps.CatArtifactWrite)}
	if role == caps.RoleImplementer || role == caps.RoleFixer {
		declared = append(declared,
			caps.Token(caps.CatFsWrite), caps.Token(caps.CatGitWrite), caps.Token(caps.CatExecBash))
	}
	profile := caps.Effective(caps.Input{
		Host: opencodeSupported, Pack: declared, Stage: declared, Role: role,
	})
	profile.HardTimeout = 4 * time.Minute
	profile.IdleTimeout = 90 * time.Second
	return profile
}

// invoke runs one live invocation and returns the terminal result, or the
// terminal error. Unlike a plain helper it does not fail the test on an error:
// an enforcement test may legitimately expect the agent to end up unable to
// finish, and the assertion belongs on the filesystem, not on the stream.
func invoke(t *testing.T, inv Invocation) (*Result, error) {
	t.Helper()
	events, err := NewOpencodeAdapter("opencode").Invoke(context.Background(), inv)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var (
		result *Result
		failed error
		stream strings.Builder
	)
	for event := range events {
		switch event.Kind {
		case EventStream:
			stream.WriteString(event.Chunk)
		case EventResult:
			result = event.Result
		case EventError:
			failed = event.Err
		}
	}
	t.Logf("agent said: %s", strings.TrimSpace(stream.String()))
	return result, failed
}

// TestOpencodeLive_InvokeStreamResult proves the adapter contract end to end:
// stream events, session-id capture, resume by session id, and result.json
// parsing.
func TestOpencodeLive_InvokeStreamResult(t *testing.T) {
	requireOpencode(t)
	workdir, artifactDir, _ := liveWorktree(t, "spec")
	routing := "## Stage\nspec\n\n" + ResultContractPreamble(artifactDir)

	first, failed := invoke(t, Invocation{
		Workdir:      workdir,
		ArtifactDir:  artifactDir,
		Prompt:       contractPrompt(artifactDir),
		RoutingBlock: routing,
		Profile:      liveProfile(t, caps.RoleAnalyst),
	})
	if failed != nil {
		t.Fatalf("first run: %v", failed)
	}
	if !strings.HasPrefix(first.SessionID, "ses_") {
		t.Errorf("first run: SessionID = %q, want a ses_ prefix", first.SessionID)
	}
	if first.Status == "" {
		t.Error("first run: result.json status empty — the contract file was not parsed")
	}
	t.Logf("first run: session=%s status=%q tokens=%d", first.SessionID, first.Status, first.Telemetry.Tokens.Total)

	second, failed := invoke(t, Invocation{
		Workdir:       workdir,
		ArtifactDir:   artifactDir,
		Prompt:        contractPrompt(artifactDir),
		RoutingBlock:  routing,
		ResumeSession: first.SessionID,
		Profile:       liveProfile(t, caps.RoleAnalyst),
	})
	if failed != nil {
		t.Fatalf("resume: %v", failed)
	}
	if second.SessionID != first.SessionID {
		t.Errorf("resume: session id changed: first=%s second=%s", first.SessionID, second.SessionID)
	}
}

// TestOpencodeLive_AnalystWritesItsArtifact is the allow half of the
// enforcement contract, and the one that went unverified the longest: the
// analyst profile grants `edit` only inside its artifact directory, and the
// agent must actually be able to produce result.json there.
//
// This is where an absolute permission scope fails silently — opencode
// normalises the target path relative to the project root before matching, so
// an absolute pattern never matches and the write is refused by the deny
// baseline while the config still reads correctly.
func TestOpencodeLive_AnalystWritesItsArtifact(t *testing.T) {
	requireOpencode(t)
	workdir, artifactDir, sourcePath := liveWorktree(t, "spec")

	result, failed := invoke(t, Invocation{
		Workdir:      workdir,
		ArtifactDir:  artifactDir,
		Prompt:       contractPrompt(artifactDir),
		RoutingBlock: "## Stage\nspec\n\n" + ResultContractPreamble(artifactDir),
		Profile:      liveProfile(t, caps.RoleAnalyst),
	})
	if failed != nil {
		t.Fatalf("analyst could not produce its artifact: %v", failed)
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "result.json")); err != nil {
		t.Errorf("result.json missing from the artifact dir: %v", err)
	}
	if result.Status == "" {
		t.Error("result.json parsed but status is empty")
	}
	assertSourceUnchanged(t, sourcePath)
}

// TestOpencodeLive_AnalystCannotEditSource is the deny half: the same profile
// must refuse a source edit even when the prompt asks for one directly.
// Asserted on the bytes on disk, because the agent's own account of what it did
// is precisely the thing capability profiles exist to stop trusting.
func TestOpencodeLive_AnalystCannotEditSource(t *testing.T) {
	requireOpencode(t)
	workdir, artifactDir, sourcePath := liveWorktree(t, "spec")

	_, _ = invoke(t, Invocation{
		Workdir:      workdir,
		ArtifactDir:  artifactDir,
		Prompt:       "Edit main.go so that main prints hello. Do nothing else.",
		RoutingBlock: "## Stage\nspec\n\n" + ResultContractPreamble(artifactDir),
		Profile:      liveProfile(t, caps.RoleAnalyst),
	})
	assertSourceUnchanged(t, sourcePath)
}

// TestOpencodeLive_ImplementerEditsSource is the counterpart that keeps the
// deny test honest: a profile that denies everything would pass the test above
// while making the pipeline useless. An implementer must still be able to work.
func TestOpencodeLive_ImplementerEditsSource(t *testing.T) {
	requireOpencode(t)
	workdir, artifactDir, sourcePath := liveWorktree(t, "implement")

	_, _ = invoke(t, Invocation{
		Workdir:      workdir,
		ArtifactDir:  artifactDir,
		Prompt:       "Edit main.go so that main prints hello. Do nothing else.",
		RoutingBlock: "## Stage\nimplement\n\n" + ResultContractPreamble(artifactDir),
		Profile:      liveProfile(t, caps.RoleImplementer),
	})

	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source after run: %v", err)
	}
	if string(after) == liveSource {
		t.Error("implementer profile did not permit a source edit; the worktree scope is not reaching opencode")
	}
}

// contractPrompt names the result.json path outright.
//
// The routing block already carries the path and the schema, but this model
// treats that as background: given "write your structured result" it produced a
// file of its own naming, and once it answered in prose without touching a tool
// at all. Naming the file in the instruction is what makes these tests measure
// enforcement rather than prompt adherence — the adherence problem is real and
// belongs to the pack prompts, not here.
func contractPrompt(artifactDir string) string {
	return "Create the file " + filepath.ToSlash(filepath.Join(artifactDir, "result.json")) +
		" containing JSON in which schema_version is the string 1 (not a number) and status is the string complete. Do nothing else."
}

// assertSourceUnchanged fails when the tracked source file was modified.
func assertSourceUnchanged(t *testing.T, sourcePath string) {
	t.Helper()
	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read source after run: %v", err)
	}
	if string(after) != liveSource {
		t.Errorf("tracked source was modified.\nbefore: %q\nafter:  %q", liveSource, after)
	}
}
