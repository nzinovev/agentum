package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/routing"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

const (
	// checkFeedbackOutputBytes caps one failed check's output in the routing
	// block, and checkFeedbackTotalBytes caps all of them together: the tail
	// of a test run names the failure, while the whole stream would crowd out
	// the rest of the agent's context.
	checkFeedbackOutputBytes = 8 << 10
	checkFeedbackTotalBytes  = 24 << 10

	// checkFeedbackCondition labels the edge from the writing stage back to
	// the delivery boundary in transition evidence; it is not a pack
	// condition, and no pack can declare it.
	checkFeedbackCondition = "project_checks_failed"
)

// checkFeedback is the one automatic round a delivery attempt gets when a
// mandatory project check fails: the failed checks go back to the agent that
// wrote the code, and the run returns straight to the delivery boundary.
type checkFeedback struct {
	// returnStage is the terminal stage whose checks failed. The writing
	// stage's next stage is this one, not its pack transition, so the fix is
	// checked without another review cycle; the human final review still
	// sees it.
	returnStage string
	report      checks.Report
}

// checkFeedbackEntry is where the round enters the loop: the writing stage,
// and its saved session when it has one.
type checkFeedbackEntry struct {
	stage    string
	session  string
	feedback *checkFeedback
}

// claimCheckFeedback spends the delivery attempt's round, if it is still
// unspent and an agent wrote the code under check. It returns nil when the
// failure must stand: the round is spent, or no implementer or fixer ran.
// The round is claimed in the database before the agent runs, so a crash or a
// pause between here and the invocation cannot give the run a second round.
func (runner *Runner) claimCheckFeedback(ctx context.Context, run stageRun, report checks.Report, returnStage string) (*checkFeedbackEntry, error) {
	writer, found, err := runner.latestWritingInvocation(ctx, run)
	if err != nil || !found {
		return nil, err
	}
	if _, claimErr := runner.store.ClaimCheckFeedbackRound(ctx, sqlc.ClaimCheckFeedbackRoundParams{
		ID: run.record.ID, TenantID: run.record.TenantID,
	}); claimErr != nil {
		if errors.Is(claimErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim check feedback round: %w", claimErr)
	}
	runner.emit(ctx, run.record, EvCheckFeedbackSent, map[string]any{
		"stage":           writer.Stage,
		"invocation":      writer.ID,
		"session_resumed": writer.SessionID.Valid && writer.SessionID.String != "",
		"commit":          report.Commit,
		"failed":          report.FailedMandatory(),
	})
	return &checkFeedbackEntry{
		stage:    writer.Stage,
		session:  writer.SessionID.String,
		feedback: &checkFeedback{returnStage: returnStage, report: report},
	}, nil
}

// latestWritingInvocation finds the last invocation of a stage that writes
// source — the agent whose code the checks just ran on. The latest invocation
// overall is usually the reviewer, which cannot write and so cannot fix.
func (runner *Runner) latestWritingInvocation(ctx context.Context, run stageRun) (sqlc.StageInvocation, bool, error) {
	invocations, err := runner.store.ListStageInvocationsForRun(ctx, sqlc.ListStageInvocationsForRunParams{
		RunID: run.record.ID, TenantID: run.record.TenantID,
	})
	if err != nil {
		return sqlc.StageInvocation{}, false, fmt.Errorf("list invocations for check feedback: %w", err)
	}
	for index := len(invocations) - 1; index >= 0; index-- {
		stage, inPack := run.runPack.Stages[invocations[index].Stage]
		if !inPack {
			continue
		}
		if role := pack.EffectiveRole(invocations[index].Stage, stage); role == "implementer" || role == "fixer" {
			return invocations[index], true, nil
		}
	}
	return sqlc.StageInvocation{}, false, nil
}

// returnToDeliveryBoundary ends the feedback round: the writing stage
// finished, so the run goes back to the terminal stage to run the checks
// again instead of following the stage's pack transition.
func (runner *Runner) returnToDeliveryBoundary(ctx context.Context, run stageRun, stageID string, feedback *checkFeedback) stageOutcome {
	runner.emitStageTransition(ctx, run.record, verdictPayload{
		From: stageID, To: feedback.returnStage, Condition: checkFeedbackCondition,
	})
	runner.recordTransitionEvidence(ctx, run.record, manifest.TransitionRecord{
		From: stageID, To: feedback.returnStage, Condition: checkFeedbackCondition, At: time.Now().UTC(),
	})
	return stageOutcome{nextStage: feedback.returnStage}
}

// checkFailuresRef renders the failed mandatory checks for the routing block,
// each output cut to its end within the per-check and total budgets.
func checkFailuresRef(report checks.Report) *routing.CheckFailuresRef {
	ref := &routing.CheckFailuresRef{Commit: report.Commit}
	budget := checkFeedbackTotalBytes
	for _, outcome := range report.Outcomes {
		if !outcome.Item.Required || outcome.Status == checks.StatusPass {
			continue
		}
		output := tailBytes(checkOutputText(outcome), min(checkFeedbackOutputBytes, budget))
		budget -= len(output)
		ref.Failed = append(ref.Failed, routing.CheckFailure{
			Name: outcome.Item.Definition.Name, Status: string(outcome.Status),
			ExitCode: outcome.ExitCode, Reason: outcome.Reason, Output: output,
		})
	}
	return ref
}

// checkOutputText joins a check's streams, stderr first, labelled only when
// both carry something.
func checkOutputText(outcome checks.Outcome) string {
	stderr := strings.TrimSpace(outcome.Stderr)
	stdout := strings.TrimSpace(outcome.Stdout)
	switch {
	case stderr != "" && stdout != "":
		return "stderr:\n" + stderr + "\n\nstdout:\n" + stdout
	case stderr != "":
		return stderr
	default:
		return stdout
	}
}

// tailBytes keeps the last maxBytes of text, starting on a rune boundary, and
// says how much it cut.
func tailBytes(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	if maxBytes <= 0 {
		return fmt.Sprintf("...[%d bytes omitted: the output budget for this block is spent]", len(text))
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return fmt.Sprintf("...[cut %d bytes]\n", start) + text[start:]
}
