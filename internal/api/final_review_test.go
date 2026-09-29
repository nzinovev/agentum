package api

import (
	"reflect"
	"testing"

	"github.com/nzinovev/agentum/internal/manifest"
)

// TestFinalReviewChecksFrom pins what the reviewer sees at the gate about
// "checked": the check outcome from the manifest's checks section, and the
// project config that defined the checks — so an empty registry and an
// operator's unapplied local config are visible where the decision is made.
func TestFinalReviewChecksFrom(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name string
		body manifest.Body
		want *finalReviewChecks
	}{
		{name: "nothing recorded", body: manifest.Body{}, want: nil},
		{
			name: "empty registry with an unapplied local config",
			body: manifest.Body{
				Checks: &manifest.CheckEvidence{Commit: "c1", Ran: true, MandatoryPassed: true},
				Context: &manifest.ContextEvidence{ProjectConfig: &manifest.ProjectConfigEvidence{
					File: ".agentum.yaml", PresentAtBase: false, CheckoutChange: "added", CheckoutHash: "h2",
				}},
			},
			want: &finalReviewChecks{
				Commit: "c1", Ran: true, MandatoryPassed: true,
				Config: &finalReviewProjectConfig{File: ".agentum.yaml", PresentAtBase: false, CheckoutChange: "added"},
			},
		},
		{
			name: "results without config evidence",
			body: manifest.Body{Checks: &manifest.CheckEvidence{
				Commit: "c1", Ran: true, MandatoryPassed: false,
				Results: []manifest.CheckResult{
					{Name: "build", Required: true, Status: "failed", Stdout: "noise"},
					{Name: "lint", Status: "passed"},
				},
			}},
			want: &finalReviewChecks{
				Commit: "c1", Ran: true, MandatoryPassed: false,
				Results: []finalReviewCheckResult{
					{Name: "build", Required: true, Status: "failed"},
					{Name: "lint", Status: "passed"},
				},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			if got := finalReviewChecksFrom(scenario.body); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("checks block = %+v, want %+v", got, scenario.want)
			}
		})
	}
}
