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
//   - Missing       — subsystems that did not contribute (explicit)
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

// CapabilityProfile is the effective capability set granted to the invocation.
// Today it is the pack∩stage declared subset (declared = passed at MVP). When
// Epic 6 lands it grows a Grant field (the operator-granted subset).
type CapabilityProfile struct {
	Declared []string `json:"declared"`
	Granted  []string `json:"granted,omitempty"`
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

// CheckEvidence is the versioned set of checks + their results. Empty until
// project checks land; the manifest explicitly records this under Missing.
type CheckEvidence struct {
	SetVersion string        `json:"set_version,omitempty"`
	Results    []CheckResult `json:"results,omitempty"`
}

// CheckResult is one check outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// HumanDecision is one human gate decision.
type HumanDecision struct {
	Stage     string    `json:"stage"`
	Gate      string    `json:"gate"`
	Decision  string    `json:"decision"` // approved | rejected | edited
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
		merged.Capabilities = patch.Capabilities
	}
	if patch.Memory != nil {
		merged.Memory = patch.Memory
	}
	if patch.Artifacts != nil {
		merged.Artifacts = mergeArtifactEvidence(merged.Artifacts, patch.Artifacts)
	}
	if patch.Checks != nil {
		merged.Checks = patch.Checks
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
	return merged
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

func firstNonEmpty(preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}
