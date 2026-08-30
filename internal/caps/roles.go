package caps

import (
	"sort"
	"strings"
)

// Role is the named template that selects a stage's baseline capability set.
// The runner derives a role per stage (from the pack's optional stage.role
// field, falling back to the stage id by convention) and applies the role's
// template as the scoped base of the effective profile.
//
// Roles are the only place path/command scopes live. Packs and stages declare
// scope-less categories; the role refines them into the concrete boundary the
// runtime enforces. This keeps packs portable across runtimes.
type Role string

const (
	// RoleAnalyst reads code and writes only its own artifacts. Used by spec,
	// design, and other analytical stages that must not mutate source.
	RoleAnalyst Role = "analyst"
	// RoleReviewer reads code, history, and delivery state and writes only its
	// own review artifact. It cannot change tracked source or delivery refs.
	RoleReviewer Role = "reviewer"
	// RoleImplementer changes the working tree (source + commits inside the
	// task worktree). Cannot touch delivery refs, the network, or secrets.
	RoleImplementer Role = "implementer"
	// RoleFixer is the implementer variant scoped to the edit_targets of a
	// fix cycle. Same baseline as implementer; the runner may further narrow
	// the fs.write scope at invocation time.
	RoleFixer Role = "fixer"
	// RoleSystem is the orchestrator's own role for delivery-ref and
	// artifact-store operations (checkpoints, result_commit capture, branch
	// cleanup). It is NEVER assigned to an agent invocation — deny-by-default
	// keeps git.delivery out of every agent profile.
	RoleSystem Role = "system"
)

// KnownRoles is the complete set of roles a pack may declare. Used by the pack
// validator to reject typos. RoleSystem is intentionally absent here — packs
// cannot assign it to a stage.
var KnownRoles = []Role{RoleAnalyst, RoleReviewer, RoleImplementer, RoleFixer}

// IsKnownRole reports whether a role is one a pack may declare on a stage.
// RoleSystem returns false: it is orchestrator-internal only.
func IsKnownRole(role Role) bool {
	for _, known := range KnownRoles {
		if role == known {
			return true
		}
	}
	return false
}

// DeriveRole maps a stage id to a role by naming convention, used when a pack
// stage omits an explicit role. The convention covers the standard stage names
// (spec/analyze/design → analyst; review → reviewer; implement → implementer;
// fix → fixer). Anything unrecognized defaults to the most conservative role:
// analyst (read-only + own artifacts).
func DeriveRole(stageID string) Role {
	switch {
	case stageID == "":
		return RoleAnalyst
	case containsWord(stageID, "review"):
		return RoleReviewer
	case containsWord(stageID, "implement"), containsWord(stageID, "build"):
		return RoleImplementer
	case containsWord(stageID, "fix"), containsWord(stageID, "patch"):
		return RoleFixer
	default:
		// spec, analyze, design, plan, research, and anything else → analyst.
		// Analyst is the conservative default: read + own artifacts, nothing
		// else. A pack that needs more for an unusual stage declares role
		// explicitly.
		return RoleAnalyst
	}
}

// containsWord reports whether stageID contains the given word as a substring.
// Kept loose (substring, not tokenized) so "pre_review" and "review-v2" both
// match "review"; the convention is a fallback, not a security boundary.
func containsWord(stageID, word string) bool {
	return strings.Contains(stageID, word)
}

// RoleTemplate is the capability template a role contributes to the effective
// profile. Scopes carry a literal "${worktree}" placeholder the runtime
// substitutes with the invocation's absolute worktree root before enforcement.
const worktreeScope = "${worktree}"

// RoleTemplate returns the scoped capability tokens the role grants as a base.
// The result is the *base* — Effective still intersects it with the pack,
// stage, host, and per-invocation inputs. A role never includes net.fetch,
// secret.*, mcp.*, or git.delivery; those are granted only by an explicit
// per-invocation grant (and survive only if every other input allows them).
//
// Scopes use the "${worktree}" placeholder; the runtime substitutes the
// invocation's absolute worktree root. The placeholder is exported via
// WorktreeScope so adapters and tests can reference it without duplicating the
// literal.
func RoleTemplate(role Role) []Token {
	switch role {
	case RoleAnalyst, RoleReviewer:
		// Both read the codebase and the git state, and write only their own
		// stage artifact. Reviewer additionally cannot change tracked source
		// or delivery refs — same token set, the difference is enforced by the
		// runtime denying fs.write outside artifact.write and denying git.write
		// outright (reviewer omits it).
		return []Token{
			Token(CatFsRead),
			Token(CatArtifactWrite) + ":" + ArtifactScope + "/**",
			Token(CatGitRead),
		}
	case RoleImplementer, RoleFixer:
		// Implementer/fixer change source inside their worktree, commit to the
		// task branch, and run project tooling. Scopes pin fs.write and
		// git.write to the worktree so an implementer cannot edit outside it.
		// git.delivery is deliberately absent.
		return []Token{
			Token(CatFsRead),
			Token(CatFsWrite) + ":" + worktreeScope + "/**",
			Token(CatArtifactWrite) + ":" + ArtifactScope + "/**",
			Token(CatGitRead),
			Token(CatGitWrite) + ":" + worktreeScope,
			Token(CatExecBash),
		}
	case RoleSystem:
		// Orchestrator-internal. Never returned to an agent invocation.
		return []Token{
			Token(CatGitDelivery),
			Token(CatFsWrite) + (":${artifact-root}/**"),
			Token(CatGitWrite),
		}
	default:
		return nil
	}
}

// WorktreeScope is the placeholder RoleTemplate uses for worktree-relative
// scopes; the runtime substitutes it with the invocation's absolute worktree
// root before enforcement.
const WorktreeScope = worktreeScope

// ArtifactScope is the placeholder RoleTemplate uses for the per-invocation
// artifact directory; the runtime substitutes it before enforcement. Pinned to
// the artifact dir so an analyst/reviewer can write result.json + declared
// artifacts without gaining source writes.
const ArtifactScope = "${artifact-root}"

// sortedRoles is a stable ordered view of KnownRoles for diagnostics and tests.
func sortedRoles() []Role {
	out := append([]Role(nil), KnownRoles...)
	sort.Slice(out, func(left, right int) bool { return out[left] < out[right] })
	return out
}

var _ = sortedRoles // reserved for future diagnostics
