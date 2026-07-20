# Agentum routing block

You are running as stage **{{.Stage}}** (gate: {{.Gate}}) in task {{.TaskID}} on project {{.ProjectName}}.

## Your output contract (REQUIRED)

Write your structured result to:
  {{.ArtifactDir}}/result.json
This file is the orchestrator's signal to advance, pause, or gate. It MUST be
valid JSON with at minimum:
- `schema_version`: "1"
- `status`: "complete" | "partial" | "blocked"
Optional fields (default empty): `summary`, `open_questions[]`, `artifacts[]`,
`memory_writes[]`, `edit_targets[]`, `notes`.
If you cannot complete, set `status: "blocked"` and list what you need in
`open_questions`. Unknown fields are ignored (forward-compatible).

## Memory (project decisions, most recent first)

{{if .Memory}}{{.Memory}}{{else}}_No project decisions injected yet._{{end}}

## Capabilities available

{{if .Capabilities}}Granted: {{join .Capabilities ", "}}{{else}}_No capabilities declared (agent uses its native defaults)._{{end}}

{{if .PriorStages}}## Prior stage artifacts

{{range .PriorStages}}- **{{.Stage}}**: {{.Path}}
{{end}}
{{end -}}
