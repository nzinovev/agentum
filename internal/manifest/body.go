package manifest

import (
	"encoding/json"
	"fmt"
	"time"
)

// schemaVersion is the body schema version this build understands. Stored on
// every manifest body so a future incompatible change can be detected at read
// time. Bumped only on a structural change; adding optional fields does not
// require a bump (unknown fields are forward-compatible).
const schemaVersion = "1"

// Body is the manifest body shape. Each top-level field is one evidence
// section. A zero-valued field is treated as "not provided" by the merge
// logic; the slice / pointer fields are nil unless set.
//
// Sections map 1:1 to the task brief:
//
//   - Input         — input task + its revision
//   - Project       — project + source commit
//   - Pack          — pack name, version, content hash
//   - Prompts       — prompt revisions the adapter saw
//   - Adapter       — adapter name, version, declared capabilities
//   - Model         — model + tier
//   - Capabilities  — effective capability profile (pack ∩ stage ∩ grant)
//   - Memory        — memory slice pulled into the run
//   - Artifacts     — input + output artifact revisions
//   - Checks        — check set version + their results
//   - HumanGates    — human gate decisions
//   - Git           — branch, checkpoint, result commits
//   - ExecutionCoordinate — optional (delivery step / execution unit / phase)
//   - Missing       — subsystems that did not contribute (derived at seal)
//   - EvidenceGaps  — evidence the orchestrator tried and failed to write
//   - EvidenceComplete — set at seal: false when any section is degraded
type Body struct {
	Schema              string               `json:"schema_version"`
	Input               *InputEvidence       `json:"input,omitempty"`
	Project             *ProjectEvidence     `json:"project,omitempty"`
	Pack                *PackEvidence        `json:"pack,omitempty"`
	Prompts             []PromptRevision     `json:"prompts,omitempty"`
	Adapter             *AdapterEvidence     `json:"adapter,omitempty"`
	Model               *ModelEvidence       `json:"model,omitempty"`
	Capabilities        *CapabilityProfile   `json:"capabilities,omitempty"`
	Memory              *MemorySlice         `json:"memory,omitempty"`
	Artifacts           *ArtifactEvidence    `json:"artifacts,omitempty"`
	Checks              *CheckEvidence       `json:"checks,omitempty"`
	HumanGates          []HumanDecision      `json:"human_gates,omitempty"`
	Git                 *GitEvidence         `json:"git,omitempty"`
	ExecutionCoordinate *ExecutionCoordinate `json:"execution_coordinate,omitempty"`
	Missing             []string             `json:"missing,omitempty"`
	EvidenceGaps        []EvidenceGap        `json:"evidence_gaps,omitempty"`
	EvidenceComplete    *bool                `json:"evidence_complete,omitempty"`
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

// InputEvidence records the input task and its revision (the immutable content
// hash of the input payload, so two runs with the same title but different
// inputs are distinguishable).
type InputEvidence struct {
	TaskID      string          `json:"task_id"`
	Title       string          `json:"title"`
	Input       json.RawMessage `json:"input"`
	Revision    string          `json:"revision"`                // hash of Input
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
// rendered prompt text; StageID is the pack stage it belonged to.
type PromptRevision struct {
	StageID string `json:"stage_id"`
	Hash    string `json:"hash"`
}

// AdapterEvidence records the adapter (opencode / claude-code …) and its
// declared capabilities. The Version is the adapter binary version the
// orchestrator shells out to.
type AdapterEvidence struct {
	Name                 string   `json:"name"`
	Version              string   `json:"version,omitempty"`
	DeclaredCapabilities []string `json:"declared_capabilities,omitempty"`
}

// ModelEvidence records the model + tier the invocation used.
type ModelEvidence struct {
	Tier      string `json:"tier"`
	Model     string `json:"model"`
	AgentName string `json:"agent_name,omitempty"`
}

// CapabilityProfile records the capability evidence for a run. Declared is the
// pack-wide ceiling; Effective is the per-invocation profile the runtime
// enforced (host ∩ pack ∩ stage ∩ role), keyed by stage so each invocation's
// deny-by-default baseline is reconstructible. Granted is retained for the
// pre-Epic-6 callers that wrote a flat list.
type CapabilityProfile struct {
	Declared  []string                 `json:"declared"`
	Granted   []string                 `json:"granted,omitempty"`
	Effective []StageCapabilityProfile `json:"effective,omitempty"`
}

// StageCapabilityProfile is the effective profile snapshot for one stage
// invocation. It mirrors the fields the runtime enforced, so a later review or
// cross-run diff can reconstruct "what could this invocation do" without
// re-deriving the intersection. Profile is the raw caps.Profile JSON.
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

// ArtifactRef is one artifact revision reference inside the manifest.
type ArtifactRef struct {
	Name        string `json:"name"`
	Kind        string `json:"kind,omitempty"`
	RevisionID  string `json:"revision_id"`
	ContentHash string `json:"content_hash"`
	Stage       string `json:"stage,omitempty"`
}

// CheckEvidence is the versioned set of project checks + their results. The
// orchestrator runs the resolved set itself (not the agent) at the delivery
// boundary, against the commit recorded here, and records the outcome as
// evidence a final review reconstructs. MandatoryPassed is the delivery gate: a
// false value blocked the task from reaching successful final delivery.
type CheckEvidence struct {
	SetVersion       string        `json:"set_version,omitempty"`
	RegistryRevision string        `json:"registry_revision,omitempty"`
	Commit           string        `json:"commit,omitempty"`
	Profile          string        `json:"profile,omitempty"`
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

// MissingSections reports the evidence sections that are absent in this body.
// Derived from the body's actual shape rather than asserted once at init, so a
// stale claim (e.g. "capabilities missing" on a body that carries a populated
// capabilities section) cannot survive to seal time. The sections covered are
// the ones the run is expected to produce; Input / Project / Pack / Adapter /
// Git are written once at init and their absence is a gap, not a "missing
// subsystem," so they are not reported here.
//
// Note that memory is genuinely absent until Epic 1 wires it; a derived missing
// list keeps reporting it, correctly, rather than hiding it.
func (body Body) MissingSections() []string {
	missing := make([]string, 0, 8)
	if body.Memory == nil {
		missing = append(missing, "memory")
	}
	if len(body.HumanGates) == 0 {
		missing = append(missing, "human_gates")
	}
	if body.Artifacts == nil {
		missing = append(missing, "artifacts")
	}
	if body.Checks == nil {
		missing = append(missing, "checks")
	}
	if body.Capabilities == nil {
		missing = append(missing, "capabilities")
	}
	if len(body.Prompts) == 0 {
		missing = append(missing, "prompts")
	}
	return missing
}

// encodeBody marshals a Body to canonical JSON. Used by AddEvidence and Seal.
// The map ordering produced by encoding/json is stable for our shape (struct
// fields are encoded in declaration order), so two callers with the same Body
// produce identical bytes — important for byte-level manifest comparison.
func encodeBody(body Body) ([]byte, error) {
	if body.Schema == "" {
		body.Schema = schemaVersion
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	return encoded, nil
}

// decodeBody unmarshals a manifest body. Unknown fields are ignored
// (forward-compatible).
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
	}
	return body, nil
}

// mergeBodies returns a Body that combines existing with patch. Pointer and
// scalar fields from patch overwrite existing when set; slice fields are
// appended (de-duplicated for slices of structs with an identifying field).
// The Missing list is unioned.
//
// mergeBodies never partially mutates existing — it builds a fresh Body.
func mergeBodies(existing Body, patch Body) Body {
	merged := existing
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
	if len(patch.Prompts) > 0 {
		merged.Prompts = appendUniquePrompt(merged.Prompts, patch.Prompts)
	}
	if patch.Adapter != nil {
		merged.Adapter = patch.Adapter
	}
	if patch.Model != nil {
		merged.Model = patch.Model
	}
	if patch.Capabilities != nil {
		merged.Capabilities = mergeCapabilityProfile(merged.Capabilities, patch.Capabilities)
	}
	if patch.Memory != nil {
		merged.Memory = patch.Memory
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
	// incomplete", which a pointer (rather than a bool) exists to express.
	return merged
}

// appendUniqueEvidenceGap appends gaps not already in base, matched by
// (Section, Stage, Reason) so the same failure recorded twice (a retry that
// failed the same way) does not duplicate. At is taken from the addition so
// the latest occurrence is kept on a duplicate.
func appendUniqueEvidenceGap(base []EvidenceGap, additions []EvidenceGap) []EvidenceGap {
	out := make([]EvidenceGap, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for index, present := range out {
			if present.Section == addition.Section && present.Stage == addition.Stage && present.Reason == addition.Reason {
				out[index].At = addition.At
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

// appendUniquePrompt appends prompts from additions that are not already in
// base (matched by StageID). Returns a new slice; base is not mutated.
func appendUniquePrompt(base []PromptRevision, additions []PromptRevision) []PromptRevision {
	out := make([]PromptRevision, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if present.StageID == addition.StageID && present.Hash == addition.Hash {
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
// RevisionID).
func appendUniqueArtifactRef(base []ArtifactRef, additions []ArtifactRef) []ArtifactRef {
	out := make([]ArtifactRef, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		found := false
		for _, present := range base {
			if present.RevisionID == addition.RevisionID && present.RevisionID != "" {
				found = true
				break
			}
			if present.Name == addition.Name && present.ContentHash == addition.ContentHash && present.ContentHash != "" {
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

// mergeCapabilityProfile combines existing with patch. Declared and Granted
// overwrite (latest wins — the pack-wide ceiling is set once at run start and
// does not change per stage); Effective appends, de-duplicating by Stage so
// each invocation's snapshot accumulates across the run.
func mergeCapabilityProfile(existing *CapabilityProfile, patch *CapabilityProfile) *CapabilityProfile {
	if existing == nil {
		return patch
	}
	merged := &CapabilityProfile{
		Declared:  existing.Declared,
		Granted:   existing.Granted,
		Effective: append([]StageCapabilityProfile(nil), existing.Effective...),
	}
	if patch.Declared != nil {
		merged.Declared = patch.Declared
	}
	if patch.Granted != nil {
		merged.Granted = patch.Granted
	}
	merged.Effective = appendUniqueStageCapability(merged.Effective, patch.Effective)
	return merged
}

// appendUniqueStageCapability appends entries whose Stage is not already
// present (a re-run of the same stage replaces the prior snapshot — the latest
// invocation's profile is the authoritative one for that stage).
func appendUniqueStageCapability(base []StageCapabilityProfile, additions []StageCapabilityProfile) []StageCapabilityProfile {
	out := make([]StageCapabilityProfile, 0, len(base)+len(additions))
	out = append(out, base...)
	for _, addition := range additions {
		replaced := false
		for index, present := range out {
			if present.Stage == addition.Stage {
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
// one. MandatoryPassed is the OR of the two — once mandatory checks passed, a
// later partial patch must not flip it back.
func mergeCheckEvidence(existing *CheckEvidence, patch *CheckEvidence) *CheckEvidence {
	if existing == nil {
		return patch
	}
	merged := &CheckEvidence{
		SetVersion:       firstNonEmpty(patch.SetVersion, existing.SetVersion),
		RegistryRevision: firstNonEmpty(patch.RegistryRevision, existing.RegistryRevision),
		Commit:           firstNonEmpty(patch.Commit, existing.Commit),
		Profile:          firstNonEmpty(patch.Profile, existing.Profile),
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
