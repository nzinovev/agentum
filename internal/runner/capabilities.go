package runner

import (
	"encoding/json"
	"time"

	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
)

// computeProfile derives the effective capability profile for one stage
// invocation: host support ∩ pack capabilities ∩ (stage capabilities or
// inherited pack) ∩ role template, minus an explicit withholding. The role
// comes from the stage's explicit role field, falling back to
// caps.DeriveRole(stageID). The result is deny-by-default: a stage whose
// pack/stage omit a category receives nothing for it.
//
// withheld removes categories AFTER the four-way intersection (ADR 0003 D3).
// The runner passes caps.SourceWriteCategories while the run's source_write
// approval is pending, so fs.write / git.write / exec.bash are refused even
// though the role and the pack grant them — the capability layer of the
// plan-approval lock. withheldReason is recorded on the profile's Source so an
// audit reader sees why the grants were narrowed. Empty withheld reproduces the
// pre-ADR-0003 profile byte-for-byte.
//
// hardBudget and idleBudget are the configured per-invocation timeouts applied
// via the adapter's ctx (zero = no cap). They are taken as-is: config owns the
// policy decision about whether a cap applies; the profile is just the carrier.
func (runner *Runner) computeProfile(
	taskPack *pack.Pack,
	stageID string,
	stage pack.Stage,
	supported []caps.Category,
	hardBudget, idleBudget time.Duration,
	withheld []caps.Category,
	withheldReason string,
) caps.Profile {
	packTokens := toTokens(taskPack.Capabilities)
	stageTokens := toTokens(stage.Capabilities)
	if len(stageTokens) == 0 {
		// Inheritance: a stage that does not declare its own subset inherits
		// the pack's ceiling. Resolved here rather than inside caps so the
		// intersection stays a pure four-way AND.
		stageTokens = packTokens
	}
	role := caps.Role(stage.Role)
	if role == "" {
		role = caps.DeriveRole(stageID)
	}
	profile := caps.Effective(caps.Input{
		Host:           supported,
		Pack:           packTokens,
		Stage:          stageTokens,
		Role:           role,
		HardTimeout:    hardBudget,
		IdleTimeout:    idleBudget,
		Withheld:       withheld,
		WithheldReason: withheldReason,
	})
	// Contract floor: every stage MUST write result.json to its artifact dir —
	// that file is the orchestrator's signal to advance, pause, or gate. So
	// artifact.write to the per-invocation artifact dir is not a grantable
	// capability a pack or stage can withhold; it is injected after the
	// intersection, like the routing block. The ${artifact-root} placeholder is
	// substituted by the adapter with the invocation's absolute artifact dir.
	return withArtifactFloor(profile)
}

// withArtifactFloor returns profile with artifact.write scoped to the artifact
// dir guaranteed present. Idempotent: a profile that already grants artifact
// write (any scope) is returned unchanged so an operator-granted broader scope
// is preserved.
func withArtifactFloor(profile caps.Profile) caps.Profile {
	if profile.Has(caps.CatArtifactWrite) {
		return profile
	}
	floor := caps.Token(string(caps.CatArtifactWrite) + ":" + caps.ArtifactScope + "/**")
	profile.Grants = append(profile.Grants, floor)
	profile.Source.Invocation = append(profile.Source.Invocation, floor)
	return caps.Normalize(profile)
}

// toTokens converts the pack/stage []string declaration to caps.Token. Empty
// strings are dropped (defensive against malformed manifests).
func toTokens(declared []string) []caps.Token {
	out := make([]caps.Token, 0, len(declared))
	for _, entry := range declared {
		if entry == "" {
			continue
		}
		out = append(out, caps.Token(entry))
	}
	return out
}

// marshalProfile encodes a profile for storage on the stage_invocations row.
// Returns nil bytes (→ NULL column) for an empty profile so the column
// distinguishes "no profile computed" from "computed to grant nothing" — both
// are valid, and the manifest's missing-section is the place that records the
// former.
func marshalProfile(profile caps.Profile) []byte {
	if profile.IsEmpty() {
		return nil
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return nil
	}
	return encoded
}

// profileTokens renders a profile's grants as the []string the routing block
// template consumes. Used so the routing block shows the agent the exact
// capability set in force.
func profileTokens(profile caps.Profile) []string {
	out := make([]string, 0, len(profile.Grants))
	for _, granted := range profile.Grants {
		out = append(out, string(granted))
	}
	return out
}
