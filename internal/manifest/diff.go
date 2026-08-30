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
	Context             *SectionDelta `json:"context,omitempty"`
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
		diff.Context == nil &&
		diff.InputArtifacts == nil &&
		diff.Checks == nil &&
		diff.GitBase == nil &&
		diff.ExecutionCoordinate == nil
}

// DiffManifests compares two sealed manifest bodies and returns the
// significant input-level differences. A nil section in either body is treated
// as "not provided" — only fields present in both are compared, except where
// one side is nil and the other is set (that is a meaningful difference).
//
// The per-attempt axes (prompts, model, capabilities, adapter) are recomputed
// from InvocationRecords() — the accessor that reads `invocations` on a
// schema-2 body and synthesizes the same shape from a schema-1 body — so a v1
// manifest and a v2 manifest of the same run diff to empty.
func DiffManifests(left, right Body) Diff {
	leftRecords := left.InvocationRecords()
	rightRecords := right.InvocationRecords()
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
	if delta := diffPrompts(leftRecords, rightRecords); delta != nil {
		diff.Prompts = delta
	}
	if delta := diffAdapters(left.Adapter, right.Adapter, leftRecords, rightRecords); delta != nil {
		diff.Adapter = delta
	}
	if delta := diffModels(leftRecords, rightRecords); delta != nil {
		diff.Model = delta
	}
	if delta := diffCapabilities(left.Capabilities, right.Capabilities, leftRecords, rightRecords); delta != nil {
		diff.Capabilities = delta
	}
	if delta := diffMemory(left.Memory, right.Memory); delta != nil {
		diff.Memory = delta
	}
	if delta := diffContext(left.Context, right.Context); delta != nil {
		diff.Context = delta
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

// invocationKey is the semantic coordinate of one attempt. Invocation ids are
// UUIDs and never equal between two runs, so the diff indexes records by
// (stage, cycle, ordinal) instead — the coordinate that IS shared when two
// runs made the same attempt.
//
// Ordinal is the third part because (stage, cycle) is NOT unique within a run:
// a resume inherits the resumed attempt's cycle while invokeStage still
// creates a fresh stage_invocations row, so a `continue` job leaves two
// records sharing a coordinate. Keying on the pair collapsed them and made the
// earlier attempt invisible on every per-attempt axis at once — the same "a
// coarse key erases an attempt" defect schema 2 exists to fix, one level up.
type invocationKey struct {
	Stage   string
	Cycle   int32
	Ordinal int32
}

// stageCycle is the (stage, cycle) coordinate an ordinal counts within.
type stageCycle struct {
	Stage string
	Cycle int32
}

// indexInvocations maps records by their (stage, cycle, ordinal) coordinate.
// Ordinal is derived at index time from Sequence — it is never stored in
// evidence and never appears in the API — so two attempts sharing a
// (stage, cycle) are compared pairwise in run order: a resume lines up with
// the other run's resume rather than with its original attempt. Records with
// no Sequence (synthesized from schema 1) sort stably and keep their append
// order.
func indexInvocations(records []InvocationEvidence) map[invocationKey]InvocationEvidence {
	ordered := append([]InvocationEvidence(nil), records...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].Sequence < ordered[right].Sequence
	})
	attempts := make(map[stageCycle]int32, len(ordered))
	out := make(map[invocationKey]InvocationEvidence, len(ordered))
	for _, record := range ordered {
		coordinate := stageCycle{Stage: record.Stage, Cycle: record.Cycle}
		ordinal := attempts[coordinate]
		attempts[coordinate] = ordinal + 1
		out[invocationKey{Stage: record.Stage, Cycle: record.Cycle, Ordinal: ordinal}] = record
	}
	return out
}

// sortedInvocationKeys returns the map's keys in a deterministic order, so a
// diff over several differing attempts reports the same reason every time.
func sortedInvocationKeys(indexed map[invocationKey]InvocationEvidence) []invocationKey {
	keys := make([]invocationKey, 0, len(indexed))
	for key := range indexed {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].Stage != keys[right].Stage {
			return keys[left].Stage < keys[right].Stage
		}
		if keys[left].Cycle != keys[right].Cycle {
			return keys[left].Cycle < keys[right].Cycle
		}
		return keys[left].Ordinal < keys[right].Ordinal
	})
	return keys
}

// sharedKeyDelta walks the shared (stage, cycle) keys in deterministic order
// and returns the first non-nil delta the per-field comparison produces, or
// nil. Shared keys are compared BEFORE set differences: the value difference
// is the more specific answer, and the attempt count alone is closer to a
// result than to an input.
func sharedKeyDelta(
	left, right map[invocationKey]InvocationEvidence,
	compare func(left, right InvocationEvidence) *SectionDelta,
) *SectionDelta {
	for _, key := range sortedInvocationKeys(left) {
		rightRecord, shared := right[key]
		if !shared {
			continue
		}
		if delta := compare(left[key], rightRecord); delta != nil {
			return delta
		}
	}
	return nil
}

// keySetDiffers reports whether the two indexes carry different key sets.
func keySetDiffers(left, right map[invocationKey]InvocationEvidence) bool {
	for key := range left {
		if _, shared := right[key]; !shared {
			return true
		}
	}
	for key := range right {
		if _, shared := left[key]; !shared {
			return true
		}
	}
	return false
}

// delta returns a SectionDelta when left and right differ in a meaningful way.
// `meaningful` is section-specific; the diff functions below define it.
func newDelta(reason, summary string) *SectionDelta {
	return &SectionDelta{Reason: reason, Summary: summary}
}

func diffInputs(left, right *InputEvidence) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "input-missing")
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
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "project-missing")
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
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "pack-missing")
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

// diffPrompts compares the per-attempt stage prompt hashes. The axis is
// deliberately stage_prompt_hash only: the rendered hash embeds the task id
// and absolute artifact paths, so two runs of the same task shape never
// produce equal rendered hashes, and wiring it in here would make every
// comparison report a difference.
func diffPrompts(left, right []InvocationEvidence) *SectionDelta {
	leftMap := indexInvocations(left)
	rightMap := indexInvocations(right)
	if len(leftMap) == 0 && len(rightMap) == 0 {
		return nil
	}
	if delta := sharedKeyDelta(leftMap, rightMap, func(leftRecord, rightRecord InvocationEvidence) *SectionDelta {
		if leftRecord.Prompt.StagePromptHash != rightRecord.Prompt.StagePromptHash {
			return newDelta("prompt-hash", "stage prompt hash differs")
		}
		return nil
	}); delta != nil {
		return delta
	}
	if keySetDiffers(leftMap, rightMap) {
		return newDelta("prompt-set", "attempt set differs")
	}
	return nil
}

// diffAdapters compares the per-attempt adapter facts plus the run-level
// declared set. The runtime version is compared as the SET observed across
// each run's invocations (derived from the records, not a run-level scalar — a
// run resumed after an upgrade genuinely has two): two runs on the same tier
// and model but different runtime builds differ on this axis and no other,
// which is exactly the question the axis exists to answer.
func diffAdapters(left, right *AdapterEvidence, leftRecords, rightRecords []InvocationEvidence) *SectionDelta {
	if left == nil && right == nil && len(leftRecords) == 0 && len(rightRecords) == 0 {
		return nil
	}
	if oneNilDelta := eitherNilDelta(left == nil, right == nil, "adapter-missing"); oneNilDelta != nil {
		return oneNilDelta
	}
	if delta := sharedKeyDelta(indexInvocations(leftRecords), indexInvocations(rightRecords),
		func(leftRecord, rightRecord InvocationEvidence) *SectionDelta {
			if leftRecord.Adapter.ID != rightRecord.Adapter.ID {
				return newDelta("adapter-id", "adapter id differs")
			}
			if leftRecord.Adapter.AdapterVersion != rightRecord.Adapter.AdapterVersion {
				return newDelta("adapter-version", "adapter version differs")
			}
			return nil
		}); delta != nil {
		return delta
	}
	if !sameStringSet(runtimeVersionSet(leftRecords), runtimeVersionSet(rightRecords)) {
		return newDelta("adapter-runtime-version", "runtime version set differs")
	}
	if !sameStringSet(adapterDeclaredCapabilities(left), adapterDeclaredCapabilities(right)) {
		return newDelta("adapter-capabilities", "declared capabilities differ")
	}
	return nil
}

// runtimeVersionSet collects the distinct runtime versions observed across a
// run's invocation records. Empty versions (a failed probe, or a synthesized
// schema-1 record) are skipped: absence is not a version.
func runtimeVersionSet(records []InvocationEvidence) []string {
	seen := make(map[string]struct{}, len(records))
	out := make([]string, 0, len(records))
	for _, record := range records {
		if record.Adapter.RuntimeVersion == "" {
			continue
		}
		if _, duplicate := seen[record.Adapter.RuntimeVersion]; duplicate {
			continue
		}
		seen[record.Adapter.RuntimeVersion] = struct{}{}
		out = append(out, record.Adapter.RuntimeVersion)
	}
	return out
}

// adapterDeclaredCapabilities reads the run-level declared set, tolerating a
// nil section on either side.
func adapterDeclaredCapabilities(section *AdapterEvidence) []string {
	if section == nil {
		return nil
	}
	return section.DeclaredCapabilities
}

// diffModels compares each shared attempt's model selection: tier, provider,
// model id, and the remaining options (task 6's variant lands there). The
// former run-level scalar comparison is gone with the run-level section — a
// "primary model" summary maintained by a merge function is what produced the
// overwritten-evidence defect in the first place.
func diffModels(left, right []InvocationEvidence) *SectionDelta {
	leftMap := indexInvocations(left)
	rightMap := indexInvocations(right)
	if len(leftMap) == 0 && len(rightMap) == 0 {
		return nil
	}
	if delta := sharedKeyDelta(leftMap, rightMap, func(leftRecord, rightRecord InvocationEvidence) *SectionDelta {
		if leftRecord.Model.Options.Model != rightRecord.Model.Options.Model {
			return newDelta("model-id", "model id differs")
		}
		if leftRecord.Model.Tier != rightRecord.Model.Tier {
			return newDelta("model-tier", "model tier differs")
		}
		if leftRecord.Model.Provider != rightRecord.Model.Provider {
			return newDelta("model-provider", "model provider differs")
		}
		if leftRecord.Model.Options != rightRecord.Model.Options {
			// Same model string, different remaining options (e.g. the
			// variant task 6 adds). Unreachable today — Options carries one
			// field — but wired so the field lands on the right axis.
			return newDelta("model-options", "model options differ")
		}
		return nil
	}); delta != nil {
		return delta
	}
	if keySetDiffers(leftMap, rightMap) {
		return newDelta("model-set", "attempt set differs")
	}
	return nil
}

// diffCapabilities compares the run-level declared/granted sets and each
// shared attempt's effective profile. The effective comparison is what makes
// "the second review of a fix cycle ran under a different profile" visible —
// schema 1 keyed it by stage and overwrote it.
func diffCapabilities(left, right *CapabilityProfile, leftRecords, rightRecords []InvocationEvidence) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "capabilities-missing")
	}
	if !sameStringSet(left.Declared, right.Declared) {
		return newDelta("capability-declared", "declared capability set differs")
	}
	if !sameStringSet(left.Granted, right.Granted) {
		return newDelta("capability-granted", "granted capability set differs")
	}
	if delta := sharedKeyDelta(indexInvocations(leftRecords), indexInvocations(rightRecords),
		func(leftRecord, rightRecord InvocationEvidence) *SectionDelta {
			if string(leftRecord.Capabilities.Profile) != string(rightRecord.Capabilities.Profile) {
				return newDelta("capability-effective", "effective capability profile differs")
			}
			return nil
		}); delta != nil {
		return delta
	}
	return nil
}

func diffMemory(left, right *MemorySlice) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "memory-missing")
	}
	if !sameStringSet(left.Hashes, right.Hashes) {
		return newDelta("memory-slice", "memory slice hashes differ")
	}
	if left.Entries != right.Entries {
		return newDelta("memory-count", "memory entry count differs")
	}
	return nil
}

// diffContext compares the project-context channel. Both instructions and
// skills are RUN INPUTS — which is what makes "same task, same commit, same
// config, different skills ⇒ the runs differ" answerable. The instruction axis
// compares path → delivered_hash; the skill axis compares the name → hash set.
// A change in either surfaces a delta; the absence of one side is itself
// significant.
func diffContext(left, right *ContextEvidence) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "context-missing")
	}
	if !reflect.DeepEqual(indexInstructions(left.Instructions), indexInstructions(right.Instructions)) {
		return newDelta("context-instructions", "pinned instruction set differs")
	}
	if !reflect.DeepEqual(indexSkills(left.Skills), indexSkills(right.Skills)) {
		return newDelta("context-skills", "skill set differs")
	}
	return nil
}

// indexInstructions maps each instruction ref to its delivered hash, keyed by
// path. Two runs with the same paths but different delivered hashes differ;
// same paths and hashes are the same input.
func indexInstructions(refs []InstructionRef) map[string]string {
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		out[ref.Path] = ref.DeliveredHash
	}
	return out
}

// indexSkills maps each skill name to its hash. A skill whose body changed
// between runs (same name, different hash) is a significant input change.
func indexSkills(skills []SkillRef) map[string]string {
	out := make(map[string]string, len(skills))
	for _, skill := range skills {
		out[skill.Name] = skill.Hash
	}
	return out
}

func diffInputArtifacts(left, right *ArtifactEvidence) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "artifacts-missing")
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
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "checks-missing")
	}
	if left.SetVersion != right.SetVersion {
		return newDelta("check-set-version", "check set version differs")
	}
	return nil
}

func diffGitBase(left, right *GitEvidence) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "git-missing")
	}
	if left.BaseCommit != right.BaseCommit {
		return newDelta("git-base-commit", "base_commit differs")
	}
	return nil
}

func diffExecutionCoordinate(left, right *ExecutionCoordinate) *SectionDelta {
	if left == nil || right == nil {
		return eitherNilDelta(left == nil, right == nil, "coordinate-missing")
	}
	if left.DeliveryStep != right.DeliveryStep ||
		left.ExecutionUnit != right.ExecutionUnit ||
		left.Phase != right.Phase {
		return newDelta("coordinate", "execution coordinate differs")
	}
	return nil
}

// eitherNilDelta returns a delta when exactly one side is nil, and nil when
// both are. Callers hand it the whole `left == nil || right == nil` branch, so
// past that branch both sides are known to be set.
func eitherNilDelta(leftNil, rightNil bool, reason string) *SectionDelta {
	if leftNil != rightNil {
		return newDelta(reason, "present on one side, missing on the other")
	}
	return nil
}

// sameStringSet reports whether two string slices contain the same elements,
// ignoring order and duplicates. Used for capability sets and memory hashes —
// "same elements" is what matters, not the order they were enumerated in.
// Lengths are deliberately NOT compared up front: different lengths may still
// be the same set after de-duplication, so the comparison is always made on
// the deduped forms.
func sameStringSet(left, right []string) bool {
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
