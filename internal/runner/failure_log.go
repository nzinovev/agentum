package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/store/sqlc"
	"github.com/nzinovev/agentum/internal/worktree"
)

const (
	// failureLogMaxBytes bounds runs.failure_log. The log is read on the run
	// page, not archived: past this size a reader needs the manifest anyway.
	failureLogMaxBytes = 16 << 10
	// failureLogTailLines is how much of a check's output or an adapter's
	// error the log keeps. The end of a stream is where test runners and
	// crashing processes say what went wrong.
	failureLogTailLines = 40
)

// mandatoryChecksFailedError carries the check report out of the delivery
// boundary so the failed-run log can show each failed check's output, while
// runs.error keeps the one-line message the run list shows.
type mandatoryChecksFailedError struct {
	report checks.Report
}

func (failure *mandatoryChecksFailedError) Error() string {
	return "mandatory project check failed: " + strings.Join(failure.report.FailedMandatory(), ", ")
}

// composeFailureLog builds the text the run page shows for a failed run: the
// cause, then what a person needs to decide the next step — where the work is
// kept, which commits it can restart from, what the last agent attempt did, and
// for a check failure the output of each failed check. Every section is
// best-effort: a read that fails drops its section, because the run is already
// failing and the log must not become a second failure.
func (runner *Runner) composeFailureLog(ctx context.Context, record sqlc.Run, cause error) string {
	sections := []string{cause.Error()}
	if section := runner.checkpointSection(ctx, record); section != "" {
		sections = append(sections, section)
	}
	if record.CheckoutPath != "" {
		sections = append(sections, "worktree: "+worktree.PathFor(record.CheckoutPath, record.ID))
	}
	if section := runner.lastInvocationSection(ctx, record); section != "" {
		sections = append(sections, section)
	}
	var checkFailure *mandatoryChecksFailedError
	if errors.As(cause, &checkFailure) {
		sections = append(sections, failedChecksSection(checkFailure.report))
	}
	return truncateFailureLog(strings.Join(sections, "\n\n"), failureLogMaxBytes)
}

func (runner *Runner) checkpointSection(ctx context.Context, record sqlc.Run) string {
	var labels []string
	if record.BaseCommit.Valid && record.BaseCommit.String != "" {
		labels = append(labels, "base "+shortCommit(record.BaseCommit.String))
	}
	checkpoints, err := runner.store.ListCheckpointsForRun(ctx, sqlc.ListCheckpointsForRunParams{RunID: record.ID, TenantID: record.TenantID})
	if err != nil {
		runner.log.Warn("failure log: list checkpoints", "run", record.ID, "error", err)
	}
	for _, checkpoint := range checkpoints {
		labels = append(labels, checkpoint.Label+" "+shortCommit(checkpoint.CommitSha))
	}
	if len(labels) == 0 {
		return ""
	}
	return "checkpoints: " + strings.Join(labels, ", ")
}

func (runner *Runner) lastInvocationSection(ctx context.Context, record sqlc.Run) string {
	invocation, err := runner.store.LatestStageForRun(ctx, sqlc.LatestStageForRunParams{RunID: record.ID, TenantID: record.TenantID})
	if err != nil {
		return ""
	}
	line := fmt.Sprintf("last invocation: %s #%d, cycle %d", invocation.Stage, invocation.Sequence, invocation.Cycle)
	if invocation.FinishedAt.Valid {
		line += ", ran " + invocation.FinishedAt.Time.Sub(invocation.StartedAt).Round(time.Second).String()
	} else {
		line += ", did not finish"
	}
	if invocation.StopReason.Valid && invocation.StopReason.String != "" {
		line += ", stop_reason " + invocation.StopReason.String
	}
	if invocation.Error == "" {
		return line
	}
	return line + "\n" + tailBlock("adapter error", invocation.Error)
}

func failedChecksSection(report checks.Report) string {
	var blocks []string
	for _, outcome := range report.Outcomes {
		if !outcome.Item.Required || outcome.Status == checks.StatusPass {
			continue
		}
		header := fmt.Sprintf("check %s: %s", outcome.Item.Definition.Name, outcome.Status)
		if outcome.Status == checks.StatusFail {
			header += fmt.Sprintf(" (exit %d)", outcome.ExitCode)
		}
		header += "\ncommand: " + strings.Join(outcome.Item.Definition.Command, " ")
		if outcome.Reason != "" {
			header += "\nreason: " + outcome.Reason
		}
		if output := strings.TrimSpace(outcome.Stderr); output != "" {
			header += "\n" + tailBlock("stderr", output)
		}
		if output := strings.TrimSpace(outcome.Stdout); output != "" {
			header += "\n" + tailBlock("stdout", output)
		}
		blocks = append(blocks, header)
	}
	commitLine := "checks ran on " + shortCommit(report.Commit)
	return commitLine + "\n\n" + strings.Join(blocks, "\n\n")
}

// tailBlock renders the last failureLogTailLines lines of text under a label,
// indented so they read as one quoted block inside the log.
func tailBlock(label, text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	heading := label + ":"
	if len(lines) > failureLogTailLines {
		lines = lines[len(lines)-failureLogTailLines:]
		heading = fmt.Sprintf("%s (last %d lines):", label, failureLogTailLines)
	}
	return heading + "\n  " + strings.Join(lines, "\n  ")
}

// truncateFailureLog cuts text to maxBytes and says how much was cut; a log
// that ends silently mid-line reads as the whole story.
func truncateFailureLog(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	kept := strings.ToValidUTF8(text[:maxBytes], "")
	return kept + fmt.Sprintf("\n…[truncated %d bytes]", len(text)-len(kept))
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
