package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type routeNodeProgress struct {
	State         string           `json:"state"`
	PassSequences []int32          `json:"pass_sequences"`
	Steps         []routeStepState `json:"steps,omitempty"`
}

type routeStepState struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
}

type routeEdgeProgress struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition"`
	Count     int    `json:"count"`
}

type runRouteProgress struct {
	Nodes           map[string]routeNodeProgress `json:"nodes"`
	Edges           []routeEdgeProgress          `json:"edges"`
	CurrentStep     string                       `json:"current_step,omitempty"`
	InvocationSteps map[string]string            `json:"invocation_steps"`
}

// routeProgress reads durable transitions and attempts for the pinned graph.
// Retries within a stage do not add an edge; only a transition event does.
func (api *API) routeProgress(ctx context.Context, run sqlc.Run, graph *runRouteGraph) (*runRouteProgress, error) {
	invocations, err := api.queries.ListStageInvocationsForRun(ctx, sqlc.ListStageInvocationsForRunParams{RunID: run.ID, TenantID: run.TenantID})
	if err != nil {
		return nil, err
	}
	transitions, err := api.queries.ListRouteTransitionsForRun(ctx, sqlc.ListRouteTransitionsForRunParams{
		RunID: sql.NullString{String: run.ID, Valid: true}, TenantID: run.TenantID,
	})
	if err != nil {
		return nil, err
	}
	approvals, err := api.queries.ListApprovalsForRun(ctx, sqlc.ListApprovalsForRunParams{TenantID: run.TenantID, RunID: run.ID})
	if err != nil {
		return nil, err
	}
	revisions, err := api.queries.ListArtifactRevisionsForRun(ctx, sqlc.ListArtifactRevisionsForRunParams{RunID: run.ID, TenantID: run.TenantID})
	if err != nil {
		return nil, err
	}
	return routeProgressOf(run, graph, invocations, transitions, approvals, revisions), nil
}

// routeProgressOf derives the read model from stored decisions. The graph
// stays generic: approval placement and terminal nodes come from the pack.
func routeProgressOf(run sqlc.Run, graph *runRouteGraph, invocations []sqlc.StageInvocation, transitions []sqlc.Event, approvals []sqlc.RunApproval, revisions []sqlc.ArtifactRevision) *runRouteProgress {
	progress := &runRouteProgress{
		Nodes: make(map[string]routeNodeProgress, len(graph.Nodes)),
		Edges: []routeEdgeProgress{}, InvocationSteps: map[string]string{},
	}
	byStage := make(map[string][]sqlc.StageInvocation)
	for _, invocation := range invocations {
		byStage[invocation.Stage] = append(byStage[invocation.Stage], invocation)
	}
	approvalByName := make(map[string]sqlc.RunApproval)
	for _, approval := range approvals {
		approvalByName[approval.Name] = approval
	}
	for _, node := range graph.Nodes {
		if node.Terminal && run.CurrentStage.Valid && run.CurrentStage.String == node.ID {
			progress.CurrentStep = "final review"
		}
		state := "not_reached"
		attempts := byStage[node.ID]
		if len(attempts) > 0 {
			state = "done"
		}
		if run.CurrentStage.Valid && run.CurrentStage.String == node.ID {
			switch run.State {
			case "running":
				state = "running"
			case "paused_gate", "awaiting_final_review":
				state = "waiting"
			case "paused_open_questions":
				state = "questions"
			case "paused_user_stop":
				state = "stopped"
			case "failed":
				state = "failed"
			case "cancelled":
				state = "cancelled"
				if run.CancelReason == "rejected_at_plan" || run.CancelReason == "rejected_at_final_review" {
					state = "rejected"
				}
			case "done":
				state = "accepted"
			}
		} else if run.State == "done" && state == "not_reached" {
			state = "not_taken"
		}
		passSequences := []int32{}
		for _, invocation := range attempts {
			if !invocation.ResumeOf.Valid {
				passSequences = append(passSequences, invocation.Sequence)
			}
		}
		nodeProgress := routeNodeProgress{State: state, PassSequences: passSequences}
		for _, approvalRef := range graph.Approvals {
			if approvalRef.Stage != node.ID {
				continue
			}
			approval, decided := approvalByName[approvalRef.Name]
			planState := "not_reached"
			gateState := "not_reached"
			editsState := "not_reached"
			if len(attempts) > 0 {
				planState = "done"
			}
			if run.CurrentStage.Valid && run.CurrentStage.String == node.ID && !decided {
				switch run.State {
				case "running":
					planState = "running"
				case "paused_gate":
					planState = "done"
					gateState = "waiting"
				case "paused_open_questions":
					planState = "questions"
				case "paused_user_stop":
					planState = "stopped"
				case "failed":
					planState = "failed"
				case "cancelled":
					planState = "cancelled"
				}
			}
			if decided {
				gateState = approval.Decision
				if approval.Decision == "approved" {
					for _, invocation := range attempts {
						if !invocation.StartedAt.Before(approval.CreatedAt) {
							editsState = "done"
							if !invocation.FinishedAt.Valid {
								editsState = "running"
							}
						}
					}
					if state == "running" && editsState == "not_reached" {
						editsState = "running"
					}
				}
			}
			gateLabel := approvalRef.Unlocks + " approval"
			if decided && approval.Decision == "approved" {
				revisionNumber := 0
				for index := len(revisions) - 1; index >= 0; index-- {
					revision := revisions[index]
					if revision.Name == approvalRef.Stage+"/"+approvalRef.Artifact {
						revisionNumber++
						if revision.ID == approval.ArtifactRevisionID.String {
							break
						}
					}
				}
				if revisionNumber > 0 {
					gateLabel = fmt.Sprintf("Approved · rev %d", revisionNumber)
				} else {
					gateLabel = "Approved"
				}
			}
			if decided && approval.Decision == "rejected" {
				gateLabel = "Rejected"
			}
			if !approvalRef.WithinStage {
				label := fmt.Sprintf("approve %s → %s", approvalRef.Artifact, approvalRef.Unlocks)
				if decided {
					label += " · " + gateLabel
				}
				nodeProgress.Steps = []routeStepState{{ID: "approval", Label: label, State: gateState}}
				continue
			}
			nodeProgress.Steps = []routeStepState{
				{ID: "short_plan", Label: "Short plan · read-only", State: planState},
				{ID: "approval", Label: gateLabel, State: gateState},
				{ID: "source_edits", Label: "Source edits", State: editsState},
			}
			if run.CurrentStage.Valid && run.CurrentStage.String == node.ID {
				if decided && approval.Decision == "approved" {
					progress.CurrentStep = "edits"
				} else if run.State == "paused_gate" {
					progress.CurrentStep = approvalRef.Unlocks + " approval"
				} else {
					progress.CurrentStep = "short plan"
				}
			}
			for _, invocation := range attempts {
				step := "short plan"
				if decided && !invocation.StartedAt.Before(approval.CreatedAt) && approval.Decision == "approved" {
					step = "edits"
				}
				progress.InvocationSteps[invocation.ID] = step
			}
		}
		progress.Nodes[node.ID] = nodeProgress
	}
	counts := make(map[string]int)
	for _, event := range transitions {
		var edge struct {
			From      string `json:"from"`
			To        string `json:"to"`
			Condition string `json:"condition"`
		}
		if json.Unmarshal(event.Payload, &edge) == nil && edge.From != "" && edge.To != "" {
			counts[edge.From+"\x00"+edge.To+"\x00"+edge.Condition]++
		}
	}
	for _, node := range graph.Nodes {
		for _, edge := range node.Transitions {
			progress.Edges = append(progress.Edges, routeEdgeProgress{
				From: node.ID, To: edge.To, Condition: edge.Condition,
				Count: counts[node.ID+"\x00"+edge.To+"\x00"+edge.Condition],
			})
		}
	}
	return progress
}
