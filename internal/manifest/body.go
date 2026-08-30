package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nzinovev/agentum/internal/models"
)

// The body schema versions this build understands. A body stores its version
// so an incompatible change can be detected at read time; schema 2 moved the
// per-attempt evidence into one `invocations` section. Reads accept both 1 and
// 2; writes always emit 2 and never emit the legacy fields.
const (
	schemaVersionV1 = "1"
	schemaVersion   = "2"
)

// ErrUnsupportedSchema is wrapped by every schema-version rejection from
// decodeBody, so a caller can distinguish "a version this build cannot read"
// from a corrupt body.
var ErrUnsupportedSchema = errors.New("manifest: unsupported body schema_version")

// UnsupportedSchemaError names the version a body carried that this build
// neither reads nor writes. A body with an unknown version is refused rather
// than silently mis-decoded.
type UnsupportedSchemaError struct {
	Version string
}

func (schemaError *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("manifest: unsupported schema_version %q (this build reads %q and %q)", schemaError.Version, schemaVersionV1, schemaVersion)
}

func (schemaError *UnsupportedSchemaError) Unwrap() error { return ErrUnsupportedSchema }

// AdapterID is the execution adapter's stable id. Mirrored from
// internal/agent as a plain type — the manifest never imports the agent
// package (the same rule that produced the duplicated SkillRef); conversion
// is by string.
type AdapterID string

// TokenUsage is the per-invocation token breakdown. Mirrored from
// internal/agent's TotalTokens for the same no-import reason.
type TokenUsage struct {
	Total      int64 `json:"total"`
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Body is the manifest body shape. Each top-level field is one evidence
// section. A zero-valued field is treated as "not provided" by the merge
// logic; the slice / pointer fields are nil unless set.
//
// Sections map 1:1 to the task brief:
//
//   - Input         — input task + its revision
//   - Project       — project + source commit
//   - Pack          — pack name, version, content hash
//   - Invocations   — one record per stage ATTEMPT: adapter, model, prompts,
//     capability profile, telemetry (the unit of evidence)
//   - Adapter       — the wiring of the process that drove the run
//   - Capabilities  — the pack-wide declared ceiling
//   - Memory        — memory slice pulled into the run
//   - Context       — pinned project instructions + enumerated skills (ADR 0002)
//   - Artifacts     — input + output artifact revisions (outputs keyed by
//     invocation)
//   - Checks        — check set version + their results
//   - HumanGates    — human gate decisions
//   - Git           — branch, checkpoint, result commits
//   - ExecutionCoordinate — optional (delivery step / execution unit / phase)
//   - Transitions   — conditional transitions the run took
//   - Stops         — controlled stops the run hit
//   - Missing       — subsystems that did not contribute (derived at seal)
//   - EvidenceGaps  — evidence the orchestrator tried and failed to write
//   - EvidenceComplete — set at seal: false when any section is degraded
type Body struct {
	Schema              string               `json:"schema_version"`
	Input               *InputEvidence       `json:"input,omitempty"`
	Project             *ProjectEvidence     `json:"project,omitempty"`
	Pack                *PackEvidence        `json:"pack,omitempty"`
	Invocations         []InvocationEvidence `json:"invocations,omitempty"`
	Adapter             *AdapterEvidence     `json:"adapter,omitempty"`
	Capabilities        *CapabilityProfile   `json:"capabilities,omitempty"`
	Memory              *MemorySlice         `json:"memory,omitempty"`
	Context             *ContextEvidence     `json:"context,omitempty"`
	Artifacts           *ArtifactEvidence    `json:"artifacts,omitempty"`
	Checks              *CheckEvidence       `json:"checks,omitempty"`
	HumanGates          []HumanDecision      `json:"human_gates,omitempty"`
	Git                 *GitEvidence         `json:"git,omitempty"`
	ExecutionCoordinate *ExecutionCoordinate `json:"execution_coordinate,omitempty"`
	Transitions         []TransitionRecord   `json:"transitions,omitempty"`
	Stops               []StopRecord         `json:"stops,omitempty"`
	Missing             []string             `json:"missing,omitempty"`
	EvidenceGaps        []EvidenceGap        `json:"evidence_gaps,omitempty"`
	EvidenceComplete    *bool                `json:"evidence_complete,omitempty"`

	// Prompts is the schema-1 per-stage prompt list, retained READ-ONLY so a
	// sealed v1 manifest round-trips through decode with nothing dropped. A
	// writer touching it is a bug: schema 2 records prompts per invocation and
	// encodeBody never emits this field.
	Prompts []PromptRevision `json:"prompts,omitempty"`
	// Model is the schema-1 run-level model summary (including PerStage),
	// retained READ-ONLY for v1 round-trips. A writer touching it is a bug
	// (schema 2 deleted the run-level section and carries the model per
	// invocation).
	Model *ModelEvidence `json:"model,omitempty"`
}

// TransitionRecord is one conditional transition the run took. It makes the
// review ⇄ fix loop auditable from the manifest: each taken branch is recorded
// with its condition, the verdict that matched, and the prospective cycle of
// the target invocation. Cycle comes from the runner's Resolution (commit 6),
// not re-derived. Append-merged by (From, To, Condition, Cycle) so a
// re-resolution of the same edge under a retry collapses to one record.
type TransitionRecord struct {
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Condition string    `json:"condition,omitempty"`
	Cycle     int       `json:"cycle,omitempty"`
	Verdict   string    `json:"verdict,omitempty"`
	At        time.Time `json:"at"`
}

// StopRecord is one controlled stop the run hit: fix_budget_exhausted,
// verdict_unreadable, gate, adapter_error, etc. recordStopEvidence records
// every pause, not only the budget stop, so the manifest carries the full stop
// history; (Stage, Reason, Cycle) collapses repeats. Cycle is the prospective
// cycle at the stop point.
type StopRecord struct {
	Stage  string    `json:"stage"`
	Reason string    `json:"reason"`
	Cycle  int       `json:"cycle,omitempty"`
	At     time.Time `json:"at"`
}

// EvidenceGap records one evidence write the orchestrator attempted and failed.
// A sealed manifest carries the gaps so a reviewer can tell a section that was
// never produced from one that was attempted and degraded — the two are
// indistinguishable without this, and a silently degraded manifest is worse
// than an absent one because the reviewer has no way to know to distrust it.
type EvidenceGap struct {
	Section string    `json:"section"`
	Stage   string    `json:"stage,omitempty"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// newEmptyBody returns a Body with the schema version set and nothing else.
// This is the initial state for a fresh manifest.
func newEmptyBody() Body {
	return Body{Schema: schemaVersion}
}

// InputEvidence records the typed task request and its revision: the
// description the run exists to satisfy, the run overrides, and a canonical
// hash over {title, description, overrides} — so two runs with the same
// request hash equal regardless of how either request body was formatted, and
// the input-revision diff axis means what it always claimed.
type InputEvidence struct {
	TaskID      string          `json:"task_id"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Overrides   json.RawMessage `json:"overrides,omitempty"`
	Revision    string          `json:"revision"`                // canonical hash of {title, description, overrides}
	PipelineRef string          `json:"pipeline_pack,omitempty"` // tasks.pipeline_pack
}

// ProjectEvidence records the project and the source commit the run branched
// from.
type ProjectEvidence struct {
	ProjectID  string `json:"project_id"`
	RepoPath   string `json:"repo_path"`
	Name       string `json:"name"`
	BaseRef    string `json:"base_ref"`
	BaseCommit string `json:"base_commit"`
}

// PackEvidence records the resolved pack: the ref the caller passed, the
// version + content hash the source resolved to, and whether it was forked.
type PackEvidence struct {
	Ref         string `json:"ref"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	Forked      bool   `json:"forked,omitempty"`
}

// PromptRevision is one prompt the adapter saw. Hash is the sha256 of the
// rendered prompt text; StageID is the pack stage it belonged to. Schema 1
// only: schema 2 records both prompt hashes per invocation
// (InvocationPrompt). Retained read-only for v1 round-trips.
type PromptRevision struct {
	StageID string `json:"stage_id"`
	Hash    string `json:"hash"`
}

// InvocationEvidence is everything recorded about ONE attempt at a stage. The
// key is InvocationID (stage_invocations.id); Stage / Sequence / Cycle are
// coordinates for a reader, never merge keys — two records with the same stage
// and different ids are two records, always.
type InvocationEvidence struct {
	InvocationID string `json:"invocation_id"`
	Stage        string `json:"stage"`
	Sequence     int32  `json:"sequence"`
	Cycle        int32  `json:"cycle"`

	Adapter      InvocationAdapter    `json:"adapter"`
	Model        models.Selection     `json:"model"`
	Prompt       InvocationPrompt     `json:"prompt"`
	Capabilities InvocationCaps       `json:"capabilities"`
	Telemetry    *InvocationTelemetry `json:"telemetry,omitempty"`
	StopReason   string               `json:"stop_reason,omitempty"`
}

// InvocationAdapter is the execution target one attempt ran under. The three
// facts are distinct on purpose: our adapter implementation's version, and the
// external runtime's own version ("" when the probe failed).
type InvocationAdapter struct {
	ID             AdapterID `json:"id"`
	AdapterVersion string    `json:"adapter_version,omitempty"`
	RuntimeVersion string    `json:"runtime_version,omitempty"`
}

// InvocationPrompt carries the two prompt hashes of one attempt. Bodies are
// never stored — hashes only, as with instructions and skills. RenderedHash is
// what makes two attempts at the same stage distinguishable in evidence; it is
// deliberately NOT a diff axis (the routing block embeds the task id and
// absolute paths, so it never repeats across runs).
type InvocationPrompt struct {
	StagePromptHash string `json:"stage_prompt_hash"` // sha256 of the pack's stage prompt
	RenderedHash    string `json:"rendered_hash"`     // sha256 of prompt + "\n\n" + routing block
}

// InvocationCaps is the effective capability profile one attempt ran under:
// the role the profile was computed for and the raw caps.Profile JSON the
// runtime enforced.
type InvocationCaps struct {
	Role    string          `json:"role"`
	Profile json.RawMessage `json:"profile"`
}

// InvocationTelemetry is the cost summary of one attempt, recorded per
// invocation and only there.
type InvocationTelemetry struct {
	Tokens TokenUsage `json:"tokens"`
	Cost   float64    `json:"cost"`
}

// AdapterEvidence records the execution adapter wiring that drove the run:
// its id, our adapter implementation's version, the capability categories it
// declares, and the runtime probe outcome. The runtime VERSION is
// deliberately not duplicated here — a run resumed in a new process after a
// runtime upgrade genuinely has two runtime versions, and a run-level scalar
// could only lie about one of them; it lives per invocation.
type AdapterEvidence struct {
	ID                   AdapterID `json:"id,omitempty"`
	AdapterVersion       string    `json:"adapter_version,omitempty"`
	DeclaredCapabilities []string  `json:"declared_capabilities,omitempty"`
	// RuntimeProbe is the readiness probe outcome label: "ok" or
	// "failed: <reason>". Empty when never probed.
	RuntimeProbe string `json:"runtime_probe,omitempty"`

	// Name / Version are the schema-1 field names for ID / AdapterVersion,
	// retained READ-ONLY so a sealed v1 manifest round-trips with nothing
	// dropped. A writer touching them is a bug.
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// ModelEvidence records the model + tier the invocation used — the schema-1
// shape, retained READ-ONLY for v1 round-trips. A writer touching it is a bug:
// schema 2 carries the model per invocation.
type ModelEvidence struct {
	Tier      string       `json:"tier"`
	Model     string       `json:"model"`
	AgentName string       `json:"agent_name,omitempty"`
	PerStage  []StageModel `json:"per_stage,omitempty"`
}

// StageModel is the schema-1 per-stage model snapshot. Read-only retention.
type StageModel struct {
	Stage     string `json:"stage"`
	Tier      string `json:"tier"`
	Model     string `json:"model"`
	AgentName string `json:"agent_name,omitempty"`
}

// CapabilityProfile records the capability evidence for a run. Declared is the
// pack-wide ceiling, set once at run start and unchanged per attempt; the
// per-attempt effective profile lives on each invocation record. Granted is
// retained for the pre-Epic-6 callers that wrote a flat list.
type CapabilityProfile struct {
	Declared []string `json:"declared"`
	Granted  []string `json:"granted,omitempty"`

	// Effective is the schema-1 per-stage profile list, retained READ-ONLY for v1
	// round-trips. A writer touching it is a bug.
	Effective []StageCapabilityProfile `json:"effective,omitempty"`
}

// StageCapabilityProfile is the schema-1 effective-profile snapshot for one
// stage. Read-only retention.
type StageCapabilityProfile struct {
	Stage   string          `json:"stage"`
	Role    string          `json:"role"`
	Profile json.RawMessage `json:"profile"`
}

// MemorySlice is the memory pulled into the run. The Hashes list is the
// per-entry hash of each entry the agent saw, so two runs that pulled
// different decisions are distinguishable.
type MemorySlice struct {
	Scope   string   `json:"scope"`
	Hashes  []string `json:"hashes,omitempty"`
	Entries int      `json:"entries"`
}

// ArtifactEvidence groups the input and output artifact revisions the run
// consumed and produced. Each entry is a content hash + the revision id;
// outputs are indexed by (stage, name).
type ArtifactEvidence struct {
	Inputs  []ArtifactRef `json:"inputs,omitempty"`
	Outputs []ArtifactRef `json:"outputs,omitempty"`
}

// ArtifactRef is one artifact revision reference inside the manifest. Outputs
// carry the InvocationID that produced them: grouping by invocation answers
// "what did this attempt produce" without a second copy of the ledger. Inputs
// never do — the worktree sync runs once per job, before any invocation row
// exists, and attributing it to a "first invocation" would be a guess dressed
// as a fact.
type ArtifactRef struct {
	Name         string `json:"name"`
	Kind         string `json:"kind,omitempty"`
	RevisionID   string `json:"revision_id"`
	ContentHash  string `json:"content_hash"`
	Stage        string `json:"stage,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
}

// CheckEvidence is the versioned set of project checks + their results. The
// orchestrator runs the resolved set itself (not the agent) at the delivery
// boundary, against the commit recorded here, and records the outcome as
// evidence a final review reconstructs. MandatoryPassed is the delivery gate: a
// false value blocked the task from reaching successful final delivery.
//
// Ran reports whether any check actually executed. An empty set is a legitimate
// configuration (the project defines no checks), but recording it as
// mandatory_passed=true alone would read as "the gate ran and cleared it" when
// in fact nothing ran. Ran lets a reviewer distinguish "no checks defined" from
// "checks ran and passed"; it also gates IsEvidenceComplete so a checks section
// that ran nothing does not satisfy the completeness flag.
type CheckEvidence struct {
	SetVersion       string        `json:"set_version,omitempty"`
	RegistryRevision string        `json:"registry_revision,omitempty"`
	Commit           string        `json:"commit,omitempty"`
	Profile          string        `json:"profile,omitempty"`
	Ran              bool          `json:"ran"`
	MandatoryPassed  bool          `json:"mandatory_passed"`
	Results          []CheckResult `json:"results,omitempty"`
}

// CheckResult is one check outcome. It carries the definition revision (so a
// result is tied to the exact contract that ran), the exit code, the wall-clock
// duration, the capped stdout/stderr, and a reason for any non-pass status.
type CheckResult struct {
	Name               string `json:"name"`
	Required           bool   `json:"required,omitempty"`
	Status             string `json:"status"`
	ExitCode           int    `json:"exit_code,omitempty"`
	DurationMs         int64  `json:"duration_ms"`
	Stdout             string `json:"stdout,omitempty"`
	Stderr             string `json:"stderr,omitempty"`
	Reason             string `json:"reason,omitempty"`
	DefinitionRevision string `json:"definition_revision,omitempty"`
	Source             string `json:"source,omitempty"`
}

// HumanDecision is one human gate decision. Decision is one of: approved (a
// gate was passed), rejected (the run was cancelled), edited (a human edit at a
// human_edit gate — the edit is the approval), continued (a paused run was
// resumed past an open_questions or user_stop pause).
type HumanDecision struct {
	Stage     string    `json:"stage"`
	Gate      string    `json:"gate"`
	Decision  string    `json:"decision"` // approved | rejected | edited | continued
	Actor     string    `json:"actor"`
	Timestamp time.Time `json:"timestamp"`
}

// GitEvidence is the git lineage: branch, base commit, checkpoints, result
// commit. The runner fills this at boundaries (base resolve, post-stage
// checkpoint, terminal teardown).
type GitEvidence struct {
	Branch       string          `json:"branch"`
	BaseCommit   string          `json:"base_commit"`
	ResultCommit string          `json:"result_commit,omitempty"`
	Checkpoints  []CheckpointRef `json:"checkpoints,omitempty"`
}

// CheckpointRef is one orchestrator-owned checkpoint.
type CheckpointRef struct {
	Label  string `json:"label"`
	Commit string `json:"commit"`
}

// ExecutionCoordinate is the optional multi-step delivery coordinate. All
// three fields empty for a single-unit run; their absence is the signal that
// the coordinate does not apply.
type ExecutionCoordinate struct {
	DeliveryStep  string `json:"delivery_step,omitempty"`
	ExecutionUnit string `json:"execution_unit,omitempty"`
	Phase         string `json:"phase,omitempty"`
}

// expectedSections is the single source of truth for the evidence sections a
// run is expected to produce, the predicate that detects each section's
// presence, and whether an absent/degraded section counts toward the
// completeness flag. MissingSections and IsEvidenceComplete both derive from
// this list so they cannot drift — a third parallel condition (the D-review
// hazard this list replaces) would otherwise be the natural next mistake.
//
// countsTowardCompleteness is false only for memory: the memory subsystem is
// not wired in this build and its absence is a known, permanent gap rather than
// a degradation of this run's evidence. Counting it would make
// evidence_complete permanently false and conflate "subsystem not built" with
// "evidence degraded," which is the confusion the flag exists to dispel.
// MissingSections still reports memory honestly; only the completeness flag
// excludes it.
//
// The checks predicate requires Ran: a checks section that recorded no run (the
// project defines no checks) is a legitimate configuration, but it must not
// satisfy completeness — a reviewer reads evidence_complete as "the delivery
// gate ran," and an empty set is not that.
type expectedSection struct {
	name                     string
	present                  func(body *Body) bool
	countsTowardCompleteness bool
}

var expectedSections = []expectedSection{
	{name: "memory", present: func(body *Body) bool { return body.Memory != nil }, countsTowardCompleteness: false},
	{name: "context", present: func(body *Body) bool { return body.Context != nil }, countsTowardCompleteness: true},
	{name: "human_gates", present: func(body *Body) bool { return len(body.HumanGates) > 0 }, countsTowardCompleteness: true},
	{name: "artifacts", present: func(body *Body) bool { return body.Artifacts != nil }, countsTowardCompleteness: true},
	{name: "checks", present: func(body *Body) bool { return body.Checks != nil && body.Checks.Ran }, countsTowardCompleteness: true},
	{name: "capabilities", present: capabilitiesPresent, countsTowardCompleteness: true},
	{name: "invocations", present: invocationsPresent, countsTowardCompleteness: true},
}

// invocationsPresent reports whether the per-attempt evidence is there: at
// least one record, and every record carries a stage prompt hash (the one
// input-side hash an attempt cannot run without). Derived through
// InvocationRecords so a schema-1 body satisfies the same predicate via its
// legacy sections.
func invocationsPresent(body *Body) bool {
	records := body.InvocationRecords()
	if len(records) == 0 {
		return false
	}
	for _, record := range records {
		if record.Prompt.StagePromptHash == "" {
			return false
		}
	}
	return true
}

// capabilitiesPresent reports whether the pack-wide declared ceiling is set
// and every attempt recorded the profile it ran under. Derived through
// InvocationRecords so a schema-1 body satisfies the same predicate via its
// legacy effective list.
func capabilitiesPresent(body *Body) bool {
	if body.Capabilities == nil || len(body.Capabilities.Declared) == 0 {
		return false
	}
	for _, record := range body.InvocationRecords() {
		if len(record.Capabilities.Profile) == 0 {
			return false
		}
	}
	return true
}

// InvocationRecords is THE accessor for per-attempt evidence: it returns
// `invocations` when present and otherwise synthesizes records from a schema-1
// body's legacy sections (keyed by stage, cycle 0, empty invocation id). Every
// consumer — the diff, the completeness predicates — goes through it, so a v1
// manifest and a v2 manifest of the same run remain comparable without a
// second code path.
func (body Body) InvocationRecords() []InvocationEvidence {
	if len(body.Invocations) > 0 {
		return body.Invocations
	}
	return body.legacyInvocationRecords()
}

// legacyInvocationRecords synthesizes invocation records from the schema-1
// sections, with an empty invocation id (schema 1 never recorded one) and an
// empty RenderedHash (it never recorded that either). The adapter facts come
// from the run-level section.
//
// A stage contributes one record per DISTINCT prompt hash it carried, in the
// order schema 1 appended them, numbered as cycles 0…n-1. Schema 1's prompt
// list deduplicated on (stage, hash), so two entries for one stage are two
// genuinely different prompts — two attempts under an edited pack — and
// collapsing them to one record would delete evidence during the very upgrade
// that exists to stop evidence being deleted. The per-stage model and
// capability snapshots attach to the LAST record: schema 1 replaced those by
// stage on every write, so the surviving snapshot is the most recent attempt's.
// Earlier records carry the prompt hash alone, which is the honest reading —
// what those attempts ran under is what schema 1 overwrote.
func (body Body) legacyInvocationRecords() []InvocationEvidence {
	promptHashesByStage := make(map[string][]string, len(body.Prompts))
	for _, prompt := range body.Prompts {
		hashes := promptHashesByStage[prompt.StageID]
		duplicate := false
		for _, present := range hashes {
			if present == prompt.Hash {
				duplicate = true
				break
			}
		}
		if !duplicate {
			promptHashesByStage[prompt.StageID] = append(hashes, prompt.Hash)
		}
	}
	modelByStage := make(map[string]StageModel)
	if body.Model != nil {
		for _, stageModel := range body.Model.PerStage {
			if _, seen := modelByStage[stageModel.Stage]; !seen {
				modelByStage[stageModel.Stage] = stageModel
			}
		}
	}
	capsByStage := make(map[string]StageCapabilityProfile)
	if body.Capabilities != nil {
		for _, effective := range body.Capabilities.Effective {
			if _, seen := capsByStage[effective.Stage]; !seen {
				capsByStage[effective.Stage] = effective
			}
		}
	}
	stages := make([]string, 0, len(promptHashesByStage)+len(modelByStage)+len(capsByStage))
	seenStage := make(map[string]bool)
	for stage := range promptHashesByStage {
		if !seenStage[stage] {
			stages = append(stages, stage)
			seenStage[stage] = true
		}
	}
	for stage := range modelByStage {
		if !seenStage[stage] {
			stages = append(stages, stage)
			seenStage[stage] = true
		}
	}
	for stage := range capsByStage {
		if !seenStage[stage] {
			stages = append(stages, stage)
			seenStage[stage] = true
		}
	}
	sort.Strings(stages)

	var runAdapter InvocationAdapter
	if body.Adapter != nil {
		runAdapter = InvocationAdapter{ID: AdapterID(body.Adapter.Name), AdapterVersion: body.Adapter.Version}
	}
	records := make([]InvocationEvidence, 0, len(stages))
	for _, stage := range stages {
		hashes := promptHashesByStage[stage]
		// A stage known only from a model or capability snapshot still gets one
		// record — with no prompt hash, which is what the body actually says.
		attempts := len(hashes)
		if attempts == 0 {
			attempts = 1
		}
		for attempt := 0; attempt < attempts; attempt++ {
			record := InvocationEvidence{
				Stage:   stage,
				Cycle:   int32(attempt),
				Adapter: runAdapter,
			}
			if attempt < len(hashes) {
				record.Prompt = InvocationPrompt{StagePromptHash: hashes[attempt]}
			}
			// The surviving snapshots belong to the last attempt (schema 1
			// replaced them by stage on every write).
			if attempt == attempts-1 {
				if stageModel, found := modelByStage[stage]; found {
					record.Model = models.Selection{
						Tier:     stageModel.Tier,
						Provider: models.SplitProvider(stageModel.Model),
						Options:  models.Options{Model: stageModel.Model},
					}
				}
				if effective, found := capsByStage[stage]; found {
					record.Capabilities = InvocationCaps{Role: effective.Role, Profile: effective.Profile}
				}
			}
			records = append(records, record)
		}
	}
	return records
}

// CarriesLegacySections reports whether the body carries any schema-1-only
// section or field. The write path speaks one schema: the corrections endpoint
// rejects a patch that carries them rather than re-introducing shapes schema 2
// replaced.
func (body Body) CarriesLegacySections() bool {
	if len(body.Prompts) > 0 || body.Model != nil {
		return true
	}
	if body.Capabilities != nil && len(body.Capabilities.Effective) > 0 {
		return true
	}
	if body.Adapter != nil && (body.Adapter.Name != "" || body.Adapter.Version != "") {
		return true
	}
	return false
}

// upgradeLegacySections converts a schema-1 body into the schema-2 shape: the
// legacy sections become invocation records (via the same synthesis
// InvocationRecords performs), EVERY legacy field is cleared, and the schema
// version moves to 2. A body can never hold both shapes, which is why the
// guard is CarriesLegacySections rather than "did the synthesis produce
// records": a run that recorded its adapter section and then stopped before
// its first stage has legacy fields and no records, and stamping it schema 2
// while leaving adapter.name/version behind would store exactly the mixed body
// this function exists to prevent.
//
// A body with no legacy sections is returned unchanged apart from the version
// stamp, which makes this safe to call from every write path — the upgrade
// happens inside the same transaction, under the same row lock, as the write.
// The pointer sections are copied before they are cleared: the caller still
// holds those pointers, and an upgrade must not reach back into the body it
// was handed.
func upgradeLegacySections(body Body) Body {
	body.Schema = schemaVersion
	if !body.CarriesLegacySections() {
		return body
	}
	body.Invocations = append(body.Invocations, body.legacyInvocationRecords()...)
	body.Prompts = nil
	body.Model = nil
	if body.Capabilities != nil {
		capabilities := *body.Capabilities
		capabilities.Effective = nil
		body.Capabilities = &capabilities
	}
	if body.Adapter != nil {
		adapter := *body.Adapter
		adapter.Name = ""
		adapter.Version = ""
		body.Adapter = &adapter
	}
	return body
}

// MissingSections reports the evidence sections that are absent in this body.
// Derived from the body's actual shape via expectedSections rather than asserted
// once at init, so a stale claim (e.g. "capabilities missing" on a body that
// carries a populated capabilities section) cannot survive to seal time. The
// sections covered are the ones the run is expected to produce; Input / Project
// / Pack / Adapter / Git are written once at init and their absence is a gap,
// not a "missing subsystem," so they are not reported here.
//
// Note that memory is genuinely absent until Epic 1 wires it; a derived missing
// list keeps reporting it, correctly, rather than hiding it.
func (body Body) MissingSections() []string {
	missing := make([]string, 0, len(expectedSections))
	for _, section := range expectedSections {
		if !section.present(&body) {
			missing = append(missing, section.name)
		}
	}
	return missing
}

// IsEvidenceComplete reports whether this body's evidence is complete for the
// purposes of the seal-time flag. It differs from MissingSections in one
// deliberate way: `memory` is excluded (countsTowardCompleteness=false), because
// the memory subsystem is not wired in this build and its absence is a known,
// permanent gap — not a degradation of this run's evidence. Counting it would
// make the flag permanently false and conflate "subsystem not built" with
// "evidence degraded," which is exactly the confusion the flag exists to
// dispel. A reviewer reads `missing` for the honest list of gaps (memory
// included) and `evidence_complete` for whether the run's own evidence degraded.
//
// True when there are no evidence gaps and every section the run is expected to
// produce (excluding the unwired memory subsystem) is present. Both the section
// list and the per-section presence test come from expectedSections, so this and
// MissingSections share one source of truth and cannot drift apart.
func (body Body) IsEvidenceComplete() bool {
	if len(body.EvidenceGaps) > 0 {
		return false
	}
	for _, section := range expectedSections {
		if !section.countsTowardCompleteness {
			continue
		}
		if !section.present(&body) {
			return false
		}
	}
	return true
}

// encodeBody marshals a Body to canonical JSON. Used by AddEvidence and Seal.
// The write path speaks one schema: the body is upgraded to schema 2 first,
// so the legacy fields are never emitted. The map ordering produced by
// encoding/json is stable for our shape (struct fields are encoded in
// declaration order), so two callers with the same Body produce identical
// bytes — important for byte-level manifest comparison.
func encodeBody(body Body) ([]byte, error) {
	body = upgradeLegacySections(body)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	return encoded, nil
}

// decodeBody unmarshals a manifest body. Schema 1 and 2 both decode (a sealed
// manifest is readable forever, and its legacy sections are retained verbatim
// on the read-only fields); anything else is a typed error rather than a
// silent mis-decode. Unknown fields are ignored (forward-compatible).
func decodeBody(raw []byte) (Body, error) {
	var body Body
	if len(raw) == 0 {
		return newEmptyBody(), nil
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return Body{}, fmt.Errorf("decode body: %w", err)
	}
	if body.Schema == "" {
		body.Schema = schemaVersion
		return body, nil
	}
	if body.Schema != schemaVersionV1 && body.Schema != schemaVersion {
		return Body{}, &UnsupportedSchemaError{Version: body.Schema}
	}
	return body, nil
}

// mergeBodies returns a Body that combines existing with patch. Pointer and
// scalar fields from patch overwrite existing when set; slice fields are
// appended (de-duplicated for slices of structs with an identifying field).
// The Missing list is unioned.
//
// A schema-1 existing body is upgraded to schema 2 before the patch merges, so
// a body can never hold both shapes: the legacy sections become invocation
// records and are cleared in the same merge, under the same row lock
// AddEvidence already takes. Legacy fields on the PATCH are ignored — the
// corrections endpoint rejects a patch that carries them, and the runner
// writes none.
//
// mergeBodies never partially mutates existing — it builds a fresh Body.
func mergeBodies(existing Body, patch Body) Body {
	merged := upgradeLegacySections(existing)
	if patch.Schema != "" {
		merged.Schema = patch.Schema
	}
	if patch.Input != nil {
		merged.Input = patch.Input
	}
	if patch.Project != nil {
		merged.Project = patch.Project
	}
	if patch.Pack != nil {
		merged.Pack = patch.Pack
	}
	if len(patch.Invocations) > 0 {
		merged.Invocations = appendOrUpdateInvocation(merged.Invocations, patch.Invocations)
	}
	if patch.Adapter != nil {
		merged.Adapter = mergeAdapterEvidence(merged.Adapter, patch.Adapter)
	}
	if patch.Capabilities != nil {
		merged.Capabilities = mergeCapabilityProfile(merged.Capabilities, patch.Capabilities)
	}
	if patch.Memory != nil {
		merged.Memory = patch.Memory
	}
	if patch.Context != nil {
		merged.Context = mergeContextEvidence(merged.Context, patch.Context)
	}
	if patch.Artifacts != nil {
		merged.Artifacts = mergeArtifactEvidence(merged.Artifacts, patch.Artifacts)
	}
	if patch.Checks != nil {
		merged.Checks = mergeCheckEvidence(merged.Checks, patch.Checks)
	}
	if len(patch.HumanGates) > 0 {
		merged.HumanGates = appendUniqueHumanDecision(merged.HumanGates, patch.HumanGates)
	}
	if patch.Git != nil {
		merged.Git = mergeGitEvidence(merged.Git, patch.Git)
	}
	if patch.ExecutionCoordinate != nil {
		merged.ExecutionCoordinate = patch.ExecutionCoordinate
	}
	if len(patch.Transitions) > 0 {
		merged.Transitions = appendUniqueTransition(merged.Transitions, patch.Transitions)
	}
	if len(patch.Stops) > 0 {
		merged.Stops = appendUniqueStop(merged.Stops, patch.Stops)
	}
	if len(patch.Missing) > 0 {
		merged.Missing = appendUniqueString(merged.Missing, patch.Missing)
	}
	if len(patch.EvidenceGaps) > 0 {
		merged.EvidenceGaps = appendUniqueEvidenceGap(merged.EvidenceGaps, patch.EvidenceGaps)
	}
	// EvidenceComplete is intentionally NOT merged from patches: it is a seal-
	// time assertion the seal transaction sets once, against the body as it
	// then exists. A patch carrying it would be a caller overstepping; leaving
	// it unset keeps "not yet sealed" distinguishable from "sealed and
	// incomplete", which a pointer (rather than bool) exists to express.
	return merged
}

// mergeAdapterEvidence merges the run-level adapter section. The v2 fields
// overwrite when set; the deprecated schema-1 fields on the patch are dropped
// (a v1 body's Name/Version were already converted at upgrade).
func mergeAdapterEvidence(existing *AdapterEvidence, patch *AdapterEvidence) *AdapterEvidence {
	if existing == nil {
		return patch
	}
	merged := &AdapterEvidence{
		ID:                   existing.ID,
		AdapterVersion:       existing.AdapterVersion,
		DeclaredCapabilities: existing.DeclaredCapabilities,
		RuntimeProbe:         existing.RuntimeProbe,
	}
	if patch.ID != "" {
		merged.ID = patch.ID
	}
	if patch.AdapterVersion != "" {
		merged.AdapterVersion = patch.AdapterVersion
	}
	if patch.DeclaredCapabilities != nil {
		merged.DeclaredCapabilities = patch.DeclaredCapabilities
	}
	if patch.RuntimeProbe != "" {
		merged.RuntimeProbe = patch.RuntimeProbe
	}
	return merged
}

// appendOrUpdateInvocation merges invocation records matched ONLY on
// InvocationID, filling non-zero fields from the patch. Two records with the
// same stage and different ids are two records, always — the unit of evidence
// is the invocation, and keying by anything coarser (stage, attempt count)
// would erase an attempt, which is precisely the defect schema 2 exists to
// fix. This is the single behaviour the acceptance criteria test directly.
//
// The two-pass write (open before Invoke, close after the drain) relies on the
// fill-from-patch semantics: the close patch carries only telemetry and the
// stop reason, and everything the open pass recorded survives.
func appendOrUpdateInvocation(base []InvocationEvidence, additions []InvocationEvidence) []InvocationEvidence {
	out := make([]InvocationEvidence, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		// An empty invocation id (a synthesized schema-1 record) never matches:
		// two id-less records are two records, not one merged one.
		if addition.InvocationID == "" {
			out = append(out, addition)
			continue
		}
		found := false
		for index := range out {
			if out[index].InvocationID == addition.InvocationID {
				out[index] = fillInvocationEvidence(out[index], addition)
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

// fillInvocationEvidence overlays patch onto existing, taking every non-zero
// patch field. Zero-valued patch fields leave the recorded value alone.
func fillInvocationEvidence(existing InvocationEvidence, patch InvocationEvidence) InvocationEvidence {
	if patch.Stage != "" {
		existing.Stage = patch.Stage
	}
	if patch.Sequence != 0 {
		existing.Sequence = patch.Sequence
	}
	if patch.Cycle != 0 {
		existing.Cycle = patch.Cycle
	}
	if patch.Adapter.ID != "" {
		existing.Adapter = patch.Adapter
	}
	if patch.Model.Tier != "" || patch.Model.Options.Model != "" {
		existing.Model = patch.Model
	}
	if patch.Prompt.StagePromptHash != "" {
		existing.Prompt.StagePromptHash = patch.Prompt.StagePromptHash
	}
	if patch.Prompt.RenderedHash != "" {
		existing.Prompt.RenderedHash = patch.Prompt.RenderedHash
	}
	if patch.Capabilities.Role != "" || len(patch.Capabilities.Profile) > 0 {
		existing.Capabilities = patch.Capabilities
	}
	if patch.Telemetry != nil {
		existing.Telemetry = patch.Telemetry
	}
	if patch.StopReason != "" {
		existing.StopReason = patch.StopReason
	}
	return existing
}

// appendUniqueKeyed appends the additions whose key is not already present.
// base is copied first, so the caller's slice is neither aliased nor mutated,
// and each addition is matched against the GROWING result — an addition that
// repeats an earlier addition in the same patch collapses too. When an addition
// collides, refresh (when non-nil) updates the record already in the result;
// that is how the merges which keep the latest occurrence's timestamp are
// expressed. A nil refresh simply drops the collision, keeping the first record.
//
// The siblings that match against `base` alone (appendUniqueString,
// appendUniqueHumanDecision, appendUniqueCheckpoint, appendUniqueArtifactRef)
// are deliberately not expressed here: they let a repeat WITHIN one patch
// through, and collapsing that would change what they record. Two of them could
// not use a comparable key anyway — appendUniqueHumanDecision compares
// timestamps with time.Time.Equal, and appendUniqueArtifactRef matches on a
// conditional predicate rather than a fixed tuple.
func appendUniqueKeyed[T any, K comparable](base, additions []T, key func(T) K, refresh func(present *T, addition T)) []T {
	out := make([]T, 0, len(base)+len(additions))
	out = append(out, base...)
	position := make(map[K]int, len(out)+len(additions))
	for index, present := range out {
		presentKey := key(present)
		if _, seen := position[presentKey]; !seen {
			position[presentKey] = index // first occurrence wins, as a linear scan would
		}
	}
	for _, addition := range additions {
		additionKey := key(addition)
		index, seen := position[additionKey]
		if !seen {
			position[additionKey] = len(out)
			out = append(out, addition)
			continue
		}
		if refresh != nil {
			refresh(&out[index], addition)
		}
	}
	return out
}

// appendUniqueEvidenceGap appends gaps not already in base, matched by
// (Section, Stage, Reason) so the same failure recorded twice (a retry that
// failed the same way) does not duplicate. At is taken from the addition so
// the latest occurrence is kept on a duplicate.
func appendUniqueEvidenceGap(base []EvidenceGap, additions []EvidenceGap) []EvidenceGap {
	return appendUniqueKeyed(base, additions,
		func(gap EvidenceGap) evidenceGapKey {
			return evidenceGapKey{gap.Section, gap.Stage, gap.Reason}
		},
		func(present *EvidenceGap, addition EvidenceGap) { present.At = addition.At })
}

// evidenceGapKey identifies one recorded gap: the same section failing the same
// way at the same stage is one gap, however many times it was recorded.
type evidenceGapKey struct {
	section string
	stage   string
	reason  string
}

// appendUniqueHumanDecision appends decisions not already in base (matched by
// Stage + Decision + Timestamp).
func appendUniqueHumanDecision(base []HumanDecision, additions []HumanDecision) []HumanDecision {
	out := make([]HumanDecision, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if present.Stage == addition.Stage && present.Decision == addition.Decision && present.Timestamp.Equal(addition.Timestamp) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

// appendUniqueString appends strings not already in base.
func appendUniqueString(base []string, additions []string) []string {
	out := make([]string, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if present == addition {
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

// mergeArtifactEvidence merges two ArtifactEvidence. Inputs and Outputs are
// appended; entries matched by RevisionID are de-duplicated. Existing may be
// nil — the patch becomes the result.
func mergeArtifactEvidence(existing *ArtifactEvidence, patch *ArtifactEvidence) *ArtifactEvidence {
	if existing == nil {
		return patch
	}
	merged := &ArtifactEvidence{}
	merged.Inputs = appendUniqueArtifactRef(existing.Inputs, patch.Inputs)
	merged.Outputs = appendUniqueArtifactRef(existing.Outputs, patch.Outputs)
	return merged
}

// appendUniqueArtifactRef appends refs not already in base (matched by
// sameArtifactRef).
func appendUniqueArtifactRef(base []ArtifactRef, additions []ArtifactRef) []ArtifactRef {
	out := make([]ArtifactRef, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if sameArtifactRef(present, addition) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

// sameArtifactRef reports whether two refs describe the same recorded
// artifact. The revision id decides whenever both carry one: two distinct
// revisions are two records even when their bytes are identical, because
// artifacts.Put is a plain INSERT with no content de-duplication — a repeat
// attempt that produced a byte-identical output has its OWN revision row with
// its own source_invocation_id, and dropping the ref would leave the manifest
// naming the earlier attempt as the producer and disagreeing with
// artifact_revisions.
//
// The (name, content) fallback covers refs carrying no revision id, and it
// carries the invocation for the same reason. Inputs leave InvocationID empty
// on both sides (the worktree sync runs before any invocation row exists), so
// their de-duplication is unchanged.
func sameArtifactRef(present, addition ArtifactRef) bool {
	if present.RevisionID != "" && addition.RevisionID != "" {
		return present.RevisionID == addition.RevisionID
	}
	return present.ContentHash != "" &&
		present.Name == addition.Name &&
		present.ContentHash == addition.ContentHash &&
		present.InvocationID == addition.InvocationID
}

// mergeGitEvidence merges git lineage. Patch overwrites scalars (latest wins)
// and appends checkpoints (de-duplicated by Label).
func mergeGitEvidence(existing *GitEvidence, patch *GitEvidence) *GitEvidence {
	if existing == nil {
		return patch
	}
	merged := &GitEvidence{
		Branch:       firstNonEmpty(patch.Branch, existing.Branch),
		BaseCommit:   firstNonEmpty(patch.BaseCommit, existing.BaseCommit),
		ResultCommit: firstNonEmpty(patch.ResultCommit, existing.ResultCommit),
	}
	merged.Checkpoints = appendUniqueCheckpoint(existing.Checkpoints, patch.Checkpoints)
	return merged
}

// appendUniqueCheckpoint appends checkpoints not already in base (matched by Label).
func appendUniqueCheckpoint(base []CheckpointRef, additions []CheckpointRef) []CheckpointRef {
	out := make([]CheckpointRef, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if present.Label == addition.Label {
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

// appendUniqueTransition appends transitions not already in base, matched by
// (From, To, Condition, Cycle) so a re-resolution of the same edge under a
// retry collapses to one record. At is taken from the addition so the latest
// occurrence is kept on a duplicate.
func appendUniqueTransition(base []TransitionRecord, additions []TransitionRecord) []TransitionRecord {
	return appendUniqueKeyed(base, additions,
		func(record TransitionRecord) transitionKey {
			return transitionKey{record.From, record.To, record.Condition, record.Cycle}
		},
		func(present *TransitionRecord, addition TransitionRecord) { present.At = addition.At })
}

// transitionKey identifies one resolved edge: the same edge re-resolved under a
// retry in the same cycle is one record.
type transitionKey struct {
	from      string
	to        string
	condition string
	cycle     int
}

// appendUniqueStop appends stops not already in base, matched by
// (Stage, Reason, Cycle) so a repeat pause (e.g. budget re-hit on continue)
// collapses to one record. At is taken from the addition.
func appendUniqueStop(base []StopRecord, additions []StopRecord) []StopRecord {
	return appendUniqueKeyed(base, additions,
		func(record StopRecord) stopKey {
			return stopKey{record.Stage, record.Reason, record.Cycle}
		},
		func(present *StopRecord, addition StopRecord) { present.At = addition.At })
}

// stopKey identifies one pause: a repeat pause for the same reason at the same
// stage in the same cycle is one record.
type stopKey struct {
	stage  string
	reason string
	cycle  int
}

// mergeCapabilityProfile combines existing with patch. Declared and Granted
// overwrite (latest wins — the pack-wide ceiling is set once at run start and
// does not change per attempt; the per-attempt profile lives on the invocation
// records). The deprecated schema-1 Effective list on the patch is dropped:
// no writer produces it.
func mergeCapabilityProfile(existing *CapabilityProfile, patch *CapabilityProfile) *CapabilityProfile {
	if existing == nil {
		if patch == nil {
			return nil
		}
		// Copy before clearing: the caller still holds this pointer, and a
		// merge must not reach back into the patch it was handed. Every other
		// merge helper builds a fresh value; this one used to be the
		// exception, for no reason but the shortcut.
		adopted := *patch
		adopted.Effective = nil
		return &adopted
	}
	merged := &CapabilityProfile{
		Declared: existing.Declared,
		Granted:  existing.Granted,
	}
	if patch.Declared != nil {
		merged.Declared = patch.Declared
	}
	if patch.Granted != nil {
		merged.Granted = patch.Granted
	}
	return merged
}

func firstNonEmpty(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}

// mergeCheckEvidence combines two CheckEvidence. The set/registry/commit/profile
// scalars take the patch's value when set (the runner writes a fresh full set
// each delivery boundary), and results are merged de-duplicating by name so a
// re-run after resume replaces a prior result for the same check with the latest
// one. MandatoryPassed and Ran are the OR of the two — once mandatory checks
// passed (or once any check ran), a later partial patch must not flip it back.
func mergeCheckEvidence(existing *CheckEvidence, patch *CheckEvidence) *CheckEvidence {
	if existing == nil {
		return patch
	}
	merged := &CheckEvidence{
		SetVersion:       firstNonEmpty(patch.SetVersion, existing.SetVersion),
		RegistryRevision: firstNonEmpty(patch.RegistryRevision, existing.RegistryRevision),
		Commit:           firstNonEmpty(patch.Commit, existing.Commit),
		Profile:          firstNonEmpty(patch.Profile, existing.Profile),
		Ran:              existing.Ran || patch.Ran,
		MandatoryPassed:  existing.MandatoryPassed || patch.MandatoryPassed,
		Results:          appendUniqueCheckResult(existing.Results, patch.Results),
	}
	return merged
}

// appendUniqueCheckResult appends results whose Name is not already in base; a
// repeat name replaces the prior entry so the latest run's result wins.
func appendUniqueCheckResult(base []CheckResult, additions []CheckResult) []CheckResult {
	out := make([]CheckResult, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		replaced := false
		for index, present := range out {
			if present.Name == addition.Name {
				out[index] = addition
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, addition)
		}
	}
	return out
}
