package publish

import (
	"fmt"
	"strings"
)

func renderMinimalBody(delivery Delivery) string {
	var body strings.Builder
	fmt.Fprintf(&body, "## Request\n\n%s\n\n%s\n\n## Delivery\n\nBranch: `%s`\n\nCommits: `%s..%s`\n\n## Checks\n\n", delivery.Request.Title, delivery.Request.Description, delivery.Target.RemoteBranch, delivery.BaseCommit, delivery.ResultCommit)
	if !delivery.Checks.Ran {
		body.WriteString("The project declares no checks.\n")
	} else {
		body.WriteString("| Check | Required | Status | Duration (ms) |\n|---|---|---|---|\n")
		for _, check := range delivery.Checks.Checks {
			fmt.Fprintf(&body, "| %s | %t | %s | %d |\n", tableCell(check.Name), check.Required, tableCell(check.Status), check.DurationMs)
		}
	}
	fmt.Fprintf(&body, "\nChecked commit: `%s`\n\n## Evidence\n\nRun: `%s`; sealed: %t; complete: %t; missing: %s.\n\nThe branch was checked before publication. A human performs the merge.\n", delivery.Checks.Commit, delivery.Run.ID, delivery.Evidence.Sealed, delivery.Evidence.Complete, strings.Join(delivery.Evidence.Missing, ", "))
	return body.String()
}

func tableCell(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ").Replace(value)
}
