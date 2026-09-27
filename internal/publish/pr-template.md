## Request

{{.Request.Title}}

{{.Request.Description}}

Input revision: `{{.Request.Revision}}`

## Delivery

Branch: `{{.Target.RemoteBranch}}`

Base branch: `{{.Target.BaseBranch}}`

Commits: `{{.BaseCommit}}..{{.ResultCommit}}`

## Plan
{{if .Plan.RevisionID}}
Artifact: `{{.Plan.Name}}`

Revision: `{{.Plan.RevisionID}}`

Content hash: `{{.Plan.ContentHash}}`

Approved by: `{{.Plan.ApprovedBy}}` at {{timestamp .Plan.ApprovedAt}}
{{else}}
No approved plan revision was recorded.
{{end}}
## Checks
{{if .Checks.Ran}}
| Check | Required | Status | Duration (ms) |
|---|---|---|---|
{{range .Checks.Checks}}| {{cell .Name}} | {{.Required}} | {{cell .Status}} | {{.DurationMs}} |
{{end}}{{else}}
The project declares no checks.
{{end}}
Checked commit: `{{.Checks.Commit}}`

Registry revision: `{{.Checks.RegistryRevision}}`

Check set version: `{{.Checks.SetVersion}}`

## Review

Verdict: {{if .Review.Verdict}}{{.Review.Verdict}}{{else}}not recorded{{end}}

Completed fix cycles: {{.Review.FixCycles}}

## Evidence

Run: `{{.Run.ID}}`; sealed: {{.Evidence.Sealed}}; complete: {{.Evidence.Complete}}; missing: {{if .Evidence.Missing}}{{join .Evidence.Missing ", "}}{{else}}none{{end}}.

The branch was checked before publication. A human performs the merge.
