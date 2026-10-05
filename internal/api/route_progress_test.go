package api

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestRouteProgressUsesTransitionEvents protects cycle counts from retries
// of the same stage and keeps an untaken terminal pending during a fix cycle.
func TestRouteProgressUsesTransitionEvents(t *testing.T) {
	run := sqlc.Run{State: "running", CurrentStage: sql.NullString{String: "fix", Valid: true}}
	graph := &runRouteGraph{Nodes: []packStageView{
		{ID: "review", Transitions: []packTransitionView{{To: "fix", Condition: `verdict == "changes_requested"`}, {To: "done", Condition: `verdict == "approved"`}}},
		{ID: "fix", Transitions: []packTransitionView{{To: "review"}}},
		{ID: "done", Terminal: true},
	}}
	history := []sqlc.StageInvocation{{ID: "one", Stage: "review", Sequence: 1}, {ID: "retry", Stage: "review", Sequence: 2}, {ID: "fix", Stage: "fix", Sequence: 3}}
	events := []sqlc.Event{{Payload: json.RawMessage(`{"from":"review","to":"fix","condition":"verdict == \"changes_requested\""}`)}}
	progress := routeProgressOf(run, graph, history, events, nil, nil)
	if progress.Nodes["done"].State != "not_reached" || progress.Nodes["fix"].State != "running" || len(progress.Nodes["review"].PassSequences) != 2 {
		t.Errorf("node progress = %+v", progress.Nodes)
	}
	if progress.Edges[0].Count != 1 || progress.Edges[1].Count != 0 {
		t.Errorf("transition counts = %+v", progress.Edges)
	}
}

// TestRouteProgressWithinStageSteps associates attempts with the approval
// decision and names the approved plan revision in the gate step.
func TestRouteProgressWithinStageSteps(t *testing.T) {
	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionTime := before.Add(time.Minute)
	late := decisionTime.Add(time.Minute)
	run := sqlc.Run{State: "running", CurrentStage: sql.NullString{String: "implement", Valid: true}}
	graph := &runRouteGraph{Approvals: []packApprovalRef{{Name: "short_plan", Stage: "implement", Artifact: "plan.md", Unlocks: "source_write", WithinStage: true}}, Nodes: []packStageView{{ID: "implement"}}}
	history := []sqlc.StageInvocation{{ID: "plan", Stage: "implement", Sequence: 1, StartedAt: before, FinishedAt: sql.NullTime{Time: decisionTime, Valid: true}}, {ID: "edits", Stage: "implement", Sequence: 2, StartedAt: late}}
	approvals := []sqlc.RunApproval{{Name: "short_plan", Decision: "approved", ArtifactRevisionID: sql.NullString{String: "second", Valid: true}, CreatedAt: decisionTime}}
	revisions := []sqlc.ArtifactRevision{{ID: "second", Name: "implement/plan.md"}, {ID: "first", Name: "implement/plan.md"}}
	progress := routeProgressOf(run, graph, history, nil, approvals, revisions)
	steps := progress.Nodes["implement"].Steps
	if progress.CurrentStep != "edits" || progress.InvocationSteps["plan"] != "short plan" || progress.InvocationSteps["edits"] != "edits" || steps[1].Label != "Approved · rev 2" || steps[2].State != "running" {
		t.Errorf("within-stage progress = %+v", progress)
	}
}

// TestRouteProgressGateWaitsOnlyAtGate keeps a completed attempt from making
// the approval look actionable while the run is at questions or a failure.
func TestRouteProgressGateWaitsOnlyAtGate(t *testing.T) {
	graph := &runRouteGraph{
		Approvals: []packApprovalRef{{Name: "short_plan", Stage: "implement", Artifact: "plan.md", Unlocks: "source_write", WithinStage: true}},
		Nodes:     []packStageView{{ID: "implement"}},
	}
	history := []sqlc.StageInvocation{{ID: "plan", Stage: "implement", Sequence: 1, FinishedAt: sql.NullTime{Time: time.Now(), Valid: true}}}
	for _, testCase := range []struct {
		state    string
		wantPlan string
		wantGate string
	}{
		{state: "paused_gate", wantPlan: "done", wantGate: "waiting"},
		{state: "paused_open_questions", wantPlan: "questions", wantGate: "not_reached"},
		{state: "failed", wantPlan: "failed", wantGate: "not_reached"},
		{state: "running", wantPlan: "running", wantGate: "not_reached"},
	} {
		t.Run(testCase.state, func(t *testing.T) {
			run := sqlc.Run{State: testCase.state, CurrentStage: sql.NullString{String: "implement", Valid: true}}
			steps := routeProgressOf(run, graph, history, nil, nil, nil).Nodes["implement"].Steps
			if steps[0].State != testCase.wantPlan || steps[1].State != testCase.wantGate {
				t.Errorf("steps for %s = %+v", testCase.state, steps)
			}
		})
	}
}
