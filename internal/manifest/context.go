package manifest

import "time"

// ContextEvidence is the project-context channel record (ADR 0002): which
// instruction files were pinned and delivered, which skills the runtime had
// available, whether any instruction copy had to be restored after tampering,
// and which declared paths were absent at base_commit. It is the "what
// knowledge was in play" section — inputs, not outputs.
//
// The section is written on every run, even when empty: a project with no
// AGENTS.md and no skills gets a section that says exactly that. This is the
// "absent source is explicit" requirement, and it is only sound because the
// section is unconditional (it counts toward evidence completeness).
//
// Instruction and skill BODIES are never stored — only hashes and sizes. This
// keeps evidence compact and keeps third-party text out of a record that
// already has a secret-scanning story for artifacts.
type ContextEvidence struct {
	// Instructions is the set of pinned instruction files, one entry per
	// distinct (path, delivered_hash) pair. Append-merged across stages so a
	// re-pin under a retry collapses.
	Instructions []InstructionRef `json:"instructions,omitempty"`
	// Restorations is the tamper-and-reverse history: one entry per stage that
	// restored or removed a worktree instruction copy. Append-merged by
	// (stage, path, at).
	Restorations []InstructionRestoration `json:"restorations,omitempty"`
	// Skills is the set of skills the runtime had available, one entry per
	// distinct (name, hash) pair. Append-merged so a skill set that changed
	// between jobs shows up rather than hiding.
	Skills []SkillRef `json:"skills,omitempty"`
	// SkillsProbe is the outcome label of the skill enumeration: "ok",
	// "unsupported" (the adapter has no prober), or "failed: <reason>". A failed
	// probe additionally surfaces as an EvidenceGap on the body, making
	// evidence_complete false — the honest reading is that we do not know what
	// knowledge was in play.
	SkillsProbe string `json:"skills_probe,omitempty"`
	// Missing lists instruction paths declared in .agentum.yaml but absent at
	// base_commit. Recorded, not fatal: a project that declares a path that
	// does not exist yet still runs; the evidence says so.
	Missing []string `json:"missing,omitempty"`
}

// InstructionRef is one pinned instruction file's evidence. SourceHash is the
// sha256 of the ORIGINAL base_commit bytes (the file's identity, stable even
// when delivered truncated); DeliveredHash is the sha256 of the post-truncate
// bytes the model actually saw. Recording both is what makes truncation an
// attributable fact: a file whose SourceHash matches across runs but whose
// DeliveredHash differs was cut differently.
type InstructionRef struct {
	Path           string `json:"path"`
	Source         string `json:"source"`         // "runtime" | "declared"
	SourceHash     string `json:"source_hash"`    // sha256 of the base_commit bytes
	DeliveredHash  string `json:"delivered_hash"` // sha256 of what went to the model
	DeliveredBytes int    `json:"delivered_bytes"`
	Truncated      bool   `json:"truncated,omitempty"`
}

// InstructionRestoration is one tamper reversal (ADR 0002 D4 layer 2). The
// runner records a restoration whenever a pre-stage hash check found the
// worktree instruction copy had drifted from the pin and rewrote or removed it.
// FoundHash is the sha256 of the tampered content; empty when the file was
// absent. Restoration is orchestrator-authored, so it lands in the next
// checkpoint commit and shows in the delivery diff as a revert — the tamper and
// its reversal are both in the git lineage.
type InstructionRestoration struct {
	Stage     string    `json:"stage"`
	Path      string    `json:"path"`
	Action    string    `json:"action"` // "restored" | "removed"
	FoundHash string    `json:"found_hash,omitempty"`
	At        time.Time `json:"at"`
}

// SkillRef is one available skill in the context evidence. Mirrors the agent
// package's SkillRef shape but lives here to avoid an import cycle (the manifest
// never imports the agent package). Body is never stored — Hash and Bytes only.
type SkillRef struct {
	Name        string `json:"name"`
	Location    string `json:"location"`
	Description string `json:"description,omitempty"`
	Hash        string `json:"hash"`
	Bytes       int    `json:"bytes"`
}

// mergeContextEvidence combines two context sections. Instructions and Skills
// merge append-unique by (path, delivered_hash) and (name, hash); Restorations
// append by (stage, path, at); Missing is unioned; SkillsProbe keeps the worst
// outcome (a failed probe on either side stays failed — a retry that succeeded
// does not erase the fact that an earlier probe failed). A nil patch leaves the
// existing section untouched; a nil existing adopts the patch.
func mergeContextEvidence(existing, patch *ContextEvidence) *ContextEvidence {
	if patch == nil {
		return existing
	}
	if existing == nil {
		return patch
	}
	merged := &ContextEvidence{
		Instructions: appendUniqueInstructionRef(existing.Instructions, patch.Instructions),
		Restorations: appendUniqueRestoration(existing.Restorations, patch.Restorations),
		Skills:       appendUniqueSkillRef(existing.Skills, patch.Skills),
		SkillsProbe:  worstSkillsProbe(existing.SkillsProbe, patch.SkillsProbe),
		Missing:      appendUniqueString(existing.Missing, patch.Missing),
	}
	return merged
}

// appendUniqueInstructionRef appends refs not already in base, matched by
// (path, delivered_hash). A re-pin under a retry that delivered the same bytes
// collapses to one entry; a re-pin that truncated differently surfaces as a new
// entry.
func appendUniqueInstructionRef(base, additions []InstructionRef) []InstructionRef {
	if len(additions) == 0 {
		return base
	}
	seen := make(map[instructionRefKey]bool, len(base)+len(additions))
	out := make([]InstructionRef, 0, len(base)+len(additions))
	for _, ref := range base {
		seen[instructionRefKey{ref.Path, ref.DeliveredHash}] = true
		out = append(out, ref)
	}
	for _, ref := range additions {
		key := instructionRefKey{ref.Path, ref.DeliveredHash}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

type instructionRefKey struct {
	path          string
	deliveredHash string
}

// appendUniqueSkillRef appends skill refs not already in base, matched by
// (name, hash). A skill whose body changed between jobs adds a new entry rather
// than replacing the old one — the change is itself the evidence.
func appendUniqueSkillRef(base, additions []SkillRef) []SkillRef {
	if len(additions) == 0 {
		return base
	}
	seen := make(map[skillRefKey]bool, len(base)+len(additions))
	out := make([]SkillRef, 0, len(base)+len(additions))
	for _, ref := range base {
		seen[skillRefKey{ref.Name, ref.Hash}] = true
		out = append(out, ref)
	}
	for _, ref := range additions {
		key := skillRefKey{ref.Name, ref.Hash}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

type skillRefKey struct {
	name string
	hash string
}

// appendUniqueRestoration appends restorations not already in base, matched by
// (stage, path, at) so a restoration recorded once per stage does not duplicate
// under a retry that re-ran the same stage at the same time.
func appendUniqueRestoration(base, additions []InstructionRestoration) []InstructionRestoration {
	if len(additions) == 0 {
		return base
	}
	seen := make(map[restorationKey]bool, len(base)+len(additions))
	out := make([]InstructionRestoration, 0, len(base)+len(additions))
	for _, restoration := range base {
		seen[restorationKey{restoration.Stage, restoration.Path, restoration.At}] = true
		out = append(out, restoration)
	}
	for _, restoration := range additions {
		key := restorationKey{restoration.Stage, restoration.Path, restoration.At}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, restoration)
	}
	return out
}

type restorationKey struct {
	stage string
	path  string
	at    time.Time
}

// worstSkillsProbe keeps a failure if either side failed. A retry that succeeded
// does not erase the fact that an earlier probe failed: the run still passed
// through a window where the skill set was unknown.
func worstSkillsProbe(left, right string) string {
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	leftFailed := startsWithFailed(left)
	rightFailed := startsWithFailed(right)
	if leftFailed || rightFailed {
		// Prefer the more specific failure; if both failed, keep left.
		if leftFailed {
			return left
		}
		return right
	}
	return left
}

// startsWithFailed reports whether a probe label is a failure prefix.
func startsWithFailed(probe string) bool {
	return len(probe) >= len(ContextProbeFailedLabel) && probe[:len(ContextProbeFailedLabel)] == ContextProbeFailedLabel
}

// ContextProbeFailedLabel is the "failed:" prefix a failed probe label carries.
// Kept as a const so the manifest's worst-probe logic and the agent package's
// labels stay aligned without an import.
const ContextProbeFailedLabel = "failed:"
