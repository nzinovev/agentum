package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDescriptionTemplate(test *testing.T) {
	delivery := Delivery{
		Run: RunRef{ID: "run-123"}, Request: RequestRef{Title: "Task title", Description: "Task details", Revision: "input-hash"},
		Target: Target{RemoteBranch: "agentum/run-123", BaseBranch: "release/1.2"}, BaseCommit: "base-sha", ResultCommit: "result-sha",
		Plan:   PlanRef{Name: "plan/plan.md", RevisionID: "plan-revision", ContentHash: "plan-hash", ApprovedBy: "human-id", ApprovedAt: time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)},
		Review: ReviewRef{Verdict: "approved", FixCycles: 2}, Evidence: EvidenceRef{Sealed: true, Missing: []string{"memory", "context"}},
		Checks: ChecksSeal{Ran: true, Commit: "result-sha", RegistryRevision: "registry-hash", SetVersion: "set-hash", Checks: []CheckSummary{{Name: "unit|tests\nall", Required: true, Status: "passed", DurationMs: 123}}},
	}
	body, err := RenderDescription(delivery)
	if err != nil {
		test.Fatal(err)
	}
	for _, expected := range []string{"## Request", "Task title", "Task details", "input-hash", "## Delivery", "agentum/run-123", "release/1.2", "base-sha..result-sha", "## Plan", "plan/plan.md", "plan-revision", "plan-hash", "human-id", "2026-09-20T10:00:00Z", "## Checks", "| unit\\|tests all | true | passed | 123 |", "registry-hash", "set-hash", "## Review", "Verdict: approved", "Completed fix cycles: 2", "## Evidence", "run-123", "sealed: true", "complete: false", "memory, context", "A human performs the merge."} {
		if !strings.Contains(string(body), expected) {
			test.Errorf("missing %q in %s", expected, body)
		}
	}
	delivery.Checks.Ran = false
	delivery.Plan = PlanRef{}
	delivery.Review = ReviewRef{}
	body, err = RenderDescription(delivery)
	if err != nil {
		test.Fatal(err)
	}
	for _, expected := range []string{"The project declares no checks.", "No approved plan revision was recorded.", "Verdict: not recorded"} {
		if !strings.Contains(string(body), expected) {
			test.Errorf("missing %q", expected)
		}
	}
	if strings.Contains(string(body), "| Check |") {
		test.Fatal("absent checks rendered as a checks table")
	}
}

func TestGitHubPublishesStoredDescription(test *testing.T) {
	for _, existing := range []bool{false, true} {
		test.Run(fmt.Sprint(existing), func(test *testing.T) {
			const storedText = "Scanned publication text: [REDACTED]\n"
			writes := 0
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if existing {
						_, _ = w.Write([]byte(`[{"number":42,"state":"open","draft":true,"head":{"ref":"agentum/run-one","label":"owner:agentum/run-one"}}]`))
					} else {
						_, _ = w.Write([]byte(`[]`))
					}
					return
				}
				writes++
				var payload struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					test.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if payload.Body != storedText {
					test.Errorf("provider rerendered stored text: %q", payload.Body)
				}
				if r.Method == http.MethodPost {
					w.WriteHeader(http.StatusCreated)
				}
				writePullRequest(w, "open", true)
			})
			digest := sha256.Sum256([]byte(storedText))
			delivery.Description = DescriptionRef{Text: storedText, RevisionID: "scanned-revision", ContentHash: hex.EncodeToString(digest[:])}
			delivery.Request.Description = "Unscanned input must not become the body"
			if _, err := publisher.Publish(test.Context(), delivery); err != nil {
				test.Fatal(err)
			}
			if writes != 1 {
				test.Fatalf("writes = %d", writes)
			}
			delivery.Description.Text = "tampered"
			publisher.push = func(context.Context, Delivery, string) error {
				test.Fatal("push before description validation")
				return nil
			}
			_, err := publisher.Publish(test.Context(), delivery)
			if code, _ := Classify(err); code != ReasonDescriptionInvalid {
				test.Fatalf("tampered description: %v", err)
			}
		})
	}
}
