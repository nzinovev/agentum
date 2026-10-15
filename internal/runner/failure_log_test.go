package runner

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestComposeFailureLog pins what the run page can tell a person about a
// failed run: the cause first, then where to restart from and why the last
// agent attempt ended. A missing source drops its section instead of failing.
func TestComposeFailureLog(t *testing.T) {
	started := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		record      sqlc.Run
		checkpoints []sqlc.RunCheckpoint
		invocations []sqlc.StageInvocation
		want        []string
		wantAbsent  []string
	}{
		{
			name:       "cause only",
			record:     sqlc.Run{ID: "run-1", TenantID: "tn"},
			want:       []string{"pack resolve: boom"},
			wantAbsent: []string{"checkpoints:", "worktree:", "last invocation:"},
		},
		{
			name: "checkpoints, worktree and the adapter error tail",
			record: sqlc.Run{ID: "run-1", TenantID: "tn", CheckoutPath: "/repo",
				BaseCommit: sql.NullString{String: "3f9a2c1e8b07", Valid: true}},
			checkpoints: []sqlc.RunCheckpoint{{Label: "post-plan", CommitSha: "5b7e2d1f09a4"}},
			invocations: []sqlc.StageInvocation{{
				Stage: "implement", Sequence: 2, Kind: "stage", StartedAt: started,
				FinishedAt: sql.NullTime{Time: started.Add(213 * time.Second), Valid: true},
				StopReason: sql.NullString{String: "adapter_error", Valid: true},
				Error:      "opencode exited: exit status 1: panic: nil pointer",
			}},
			want: []string{
				"pack resolve: boom",
				"checkpoints: base 3f9a2c1, post-plan 5b7e2d1",
				"worktree: /repo/.agentum/worktrees/run-1",
				"last invocation: implement #2, cycle 0, ran 3m33s, stop_reason adapter_error",
				"adapter error:\n  opencode exited: exit status 1: panic: nil pointer",
			},
		},
		{
			name:   "an unfinished invocation says so",
			record: sqlc.Run{ID: "run-1", TenantID: "tn"},
			invocations: []sqlc.StageInvocation{{
				Stage: "review", Sequence: 4, Kind: "stage", StartedAt: started,
			}},
			want: []string{"last invocation: review #4, cycle 0, did not finish"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(tc.record, sqlc.Project{})
			store.checkpoints = tc.checkpoints
			store.invocations = tc.invocations
			runner := New(Deps{Store: store})
			got := runner.composeFailureLog(t.Context(), tc.record, errors.New("pack resolve: boom"))
			for _, fragment := range tc.want {
				if !strings.Contains(got, fragment) {
					t.Errorf("log lacks %q:\n%s", fragment, got)
				}
			}
			for _, fragment := range tc.wantAbsent {
				if strings.Contains(got, fragment) {
					t.Errorf("log carries %q:\n%s", fragment, got)
				}
			}
		})
	}
}

// The end of a stream is where a test runner or a crash says what went wrong,
// so a long output keeps its last lines and says that it did.
func TestTailBlockKeepsLastLines(t *testing.T) {
	var lines []string
	for i := range failureLogTailLines + 5 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	got := tailBlock("stderr", strings.Join(lines, "\n"))
	if !strings.HasPrefix(got, fmt.Sprintf("stderr (last %d lines):\n  line 5\n", failureLogTailLines)) {
		t.Errorf("tail heading or first kept line wrong:\n%s", got)
	}
	if !strings.HasSuffix(got, fmt.Sprintf("line %d", failureLogTailLines+4)) {
		t.Errorf("tail lost the last line:\n%s", got)
	}
}

// A log cut without a marker reads as the whole story; the marker also must
// not split a multi-byte rune.
func TestTruncateFailureLog(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxBytes int
		want     string
	}{
		{name: "fits", text: "short", maxBytes: 16, want: "short"},
		{name: "cut", text: "abcdefgh", maxBytes: 4, want: "abcd\n…[truncated 4 bytes]"},
		{name: "cut inside a rune", text: "abcé", maxBytes: 4, want: "abc\n…[truncated 2 bytes]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateFailureLog(tc.text, tc.maxBytes); got != tc.want {
				t.Errorf("truncateFailureLog = %q, want %q", got, tc.want)
			}
		})
	}
}
