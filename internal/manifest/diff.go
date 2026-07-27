package manifest

import (
	"reflect"
	"sort"
)

// Diff is the structural comparison between two manifests. It surfaces the
// sections whose inputs differ in a way that meaningfully changes what the
// agent would do — a different pack version, different prompt revision,
// different model, different effective capability profile, different memory
// slice, different input artifact revisions, different check set, or different
// git base. Outputs (artifacts the run produced) and human decisions are NOT
// compared — those are the *results* of the run, not its inputs.
//
// The Diff is the input to a higher-level "are these two runs comparable"
// decision. The caller decides what to do with each field (warn, block,
// auto-resolve).
type Diff struct {
	Input               *SectionDelta `json:"input,omitempty"`
	Project             *SectionDelta `json:"project,omitempty"`
	Pack                *SectionDelta `json:"pack,omitempty"`
	Prompts             *SectionDelta `json:"prompts,omitempty"`
	Adapter             *SectionDelta `json:"adapter,omitempty"`
	Model               *SectionDelta `json:"model,omitempty"`
	Capabilities        *SectionDelta `json:"capabilities,omitempty"`
	Memory              *SectionDelta `json:"memory,omitempty"`
	InputArtifacts      *SectionDelta `json:"input_artifacts,omitempty"`
	Checks              *SectionDelta `json:"checks,omitempty"`
	GitBase             *SectionDelta `json:"git_base,omitempty"`
	ExecutionCoordinate *SectionDelta `json:"execution_coordinate,omitempty"`
}

// SectionDelta is one section's comparison: a short reason string and a
// human-readable summary of what changed. Summary is for display; Reason is
// the stable machine identifier the UI branches on.
type SectionDelta struct {
	Reason  string `json:"reason"`
	Summary string `json:"summary,omitempty"`
}

// Empty reports whether the diff found no significant differences.
func (diff Diff) Empty() bool {
	return diff.Input == nil &&
		diff.Project == nil &&
		diff.Pack == nil &&
		diff.Prompts == nil &&
		diff.Adapter == nil &&
		diff.Model == nil &&
		diff.Capabilities == nil &&
		diff.Memory == nil &&
		diff.InputArtifacts == nil &&
		diff.Checks == nil &&
		diff.GitBase == nil &&
		diff.ExecutionCoordinate == nil
}

// DiffManifests compares two sealed manifest bodies and returns the
// significant input-level differences. A nil section in either body is treated
// as "not provided" — only fields present in both are compared, except where
// one side is nil and the other is set (that is a meaningful difference).
func DiffManifests(left, right Body) Diff {
	diff := Diff{}
	if delta := diffInputs(left.Input, right.Input); delta != nil {
		diff.Input = delta
	}
	if delta := diffProjects(left.Project, right.Project); delta != nil {
		diff.Project = delta
	}
	if delta := diffPacks(left.Pack, right.Pack); delta != nil {
		diff.Pack = delta
	}
	if delta := diffPrompts(left.Prompts, right.Prompts); delta != nil {
		diff.Prompts = delta
	}
	if delta := diffAdapters(left.Adapter, right.Adapter); delta != nil {
		diff.Adapter = delta
	}
	if delta := diffModels(left.Model, right.Model); delta != nil {
		diff.Model = delta
	}
	if delta := diffCapabilities(left.Capabilities, right.Capabilities); delta != nil {
		diff.Capabilities = delta
	}
	if delta := diffMemory(left.Memory, right.Memory); delta != nil {
		diff.Memory = delta
	}
	if delta := diffInputArtifacts(left.Artifacts, right.Artifacts); delta != nil {
		diff.InputArtifacts = delta
	}
	if delta := diffChecks(left.Checks, right.Checks); delta != nil {
		diff.Checks = delta
	}
	if delta := diffGitBase(left.Git, right.Git); delta != nil {
		diff.GitBase = delta
	}
	if delta := diffExecutionCoordinate(left.ExecutionCoordinate, right.ExecutionCoordinate); delta != nil {
		diff.ExecutionCoordinate = delta
	}
	return diff
}

// delta returns a SectionDelta when left and right differ in a meaningful way.
// `meaningful` is section-specific; the diff functions below define it.
func newDelta(reason, summary string) *SectionDelta {
	return &SectionDelta{Reason: reason, Summary: summary}
}

func diffInputs(left, right *InputEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "input-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.Revision != right.Revision {
		return newDelta("input-revision", "input payload hash differs")
	}
	if left.PipelineRef != right.PipelineRef {
		return newDelta("input-pipeline-ref", "pipeline_pack differs")
	}
	return nil
}

func diffProjects(left, right *ProjectEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "project-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.ProjectID != right.ProjectID {
		return newDelta("project-id", "project_id differs")
	}
	if left.BaseCommit != right.BaseCommit {
		return newDelta("project-base-commit", "base_commit differs")
	}
	return nil
}

func diffPacks(left, right *PackEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "pack-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.ContentHash != "" && right.ContentHash != "" && left.ContentHash != right.ContentHash {
		return newDelta("pack-hash", "pack content hash differs")
	}
	if left.Name != right.Name {
		return newDelta("pack-name", "pack name differs")
	}
	if left.Version != right.Version {
		return newDelta("pack-version", "pack version differs")
	}
	return nil
}

func diffPrompts(left, right []PromptRevision) *SectionDelta {
	leftMap := indexPrompts(left)
	rightMap := indexPrompts(right)
	if len(leftMap) == 0 && len(rightMap) == 0 {
		return nil
	}
	// Missing on one side or hash differs on the other = significant.
	for stage, leftHash := range leftMap {
		rightHash, ok := rightMap[stage]
		if !ok {
			return newDelta("prompt-set", "prompt for stage "+stage+" missing on right")
		}
		if leftHash != rightHash {
			return newDelta("prompt-hash", "prompt for stage "+stage+" differs")
		}
	}
	for stage := range rightMap {
		if _, ok := leftMap[stage]; !ok {
			return newDelta("prompt-set", "prompt for stage "+stage+" missing on left")
		}
	}
	return nil
}

func indexPrompts(prompts []PromptRevision) map[string]string {
	out := make(map[string]string, len(prompts))
	for _, prompt := range prompts {
		out[prompt.StageID] = prompt.Hash
	}
	return out
}

func diffAdapters(left, right *AdapterEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "adapter-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.Name != right.Name {
		return newDelta("adapter-name", "adapter name differs")
	}
	if left.Version != right.Version {
		return newDelta("adapter-version", "adapter version differs")
	}
	if !sameStringSet(left.DeclaredCapabilities, right.DeclaredCapabilities) {
		return newDelta("adapter-capabilities", "declared capabilities differ")
	}
	return nil
}

func diffModels(left, right *ModelEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "model-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.Model != right.Model {
		return newDelta("model-id", "model id differs")
	}
	if left.Tier != right.Tier {
		return newDelta("model-tier", "model tier differs")
	}
	return nil
}

func diffCapabilities(left, right *CapabilityProfile) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "capabilities-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if !sameStringSet(left.Declared, right.Declared) {
		return newDelta("capability-declared", "declared capability set differs")
	}
	if !sameStringSet(left.Granted, right.Granted) {
		return newDelta("capability-granted", "granted capability set differs")
	}
	return nil
}

func diffMemory(left, right *MemorySlice) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "memory-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if !sameStringSet(left.Hashes, right.Hashes) {
		return newDelta("memory-slice", "memory slice hashes differ")
	}
	if left.Entries != right.Entries {
		return newDelta("memory-count", "memory entry count differs")
	}
	return nil
}

func diffInputArtifacts(left, right *ArtifactEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "artifacts-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	leftInputs := indexArtifacts(left.Inputs)
	rightInputs := indexArtifacts(right.Inputs)
	if !reflect.DeepEqual(leftInputs, rightInputs) {
		return newDelta("input-artifacts", "input artifact revision set differs")
	}
	return nil
}

func indexArtifacts(refs []ArtifactRef) map[string]string {
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		// Key by Name (canonical) and value by ContentHash. Two runs with the
		// same Name but different Hash = significant; same Name, same Hash =
		// the same input.
		out[ref.Name] = ref.ContentHash
	}
	return out
}

func diffChecks(left, right *CheckEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "checks-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.SetVersion != right.SetVersion {
		return newDelta("check-set-version", "check set version differs")
	}
	return nil
}

func diffGitBase(left, right *GitEvidence) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "git-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.BaseCommit != right.BaseCommit {
		return newDelta("git-base-commit", "base_commit differs")
	}
	return nil
}

func diffExecutionCoordinate(left, right *ExecutionCoordinate) *SectionDelta {
	if left == nil && right == nil {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "coordinate-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if left.DeliveryStep != right.DeliveryStep ||
		left.ExecutionUnit != right.ExecutionUnit ||
		left.Phase != right.Phase {
		return newDelta("coordinate", "execution coordinate differs")
	}
	return nil
}

// eitherNilDelta returns a delta when exactly one side is nil. Returns nil when
// both are nil (caller handles that earlier) or both are set.
func eitherNilDelta(leftNil, rightNil bool, reason string) *SectionDelta {
	if leftNil != rightNil {
		return newDelta(reason, "present on one side, missing on the other")
	}
	return nil
}

// sameStringSet reports whether two string slices contain the same elements,
// ignoring order and duplicates. Used for capability sets and memory hashes —
// "same elements" is what matters, not the order they were enumerated in.
func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		// Different lengths may still be the same set after dedup, so fall
		// through to the map comparison rather than returning false here.
	}
	leftSet := dedupeAndSort(left)
	rightSet := dedupeAndSort(right)
	if len(leftSet) != len(rightSet) {
		return false
	}
	for index, value := range leftSet {
		if rightSet[index] != value {
			return false
		}
	}
	return true
}

func dedupeAndSort(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
