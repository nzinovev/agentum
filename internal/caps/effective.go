package caps

import "time"

// Input is what the runner hands to Effective to compute one invocation's
// capability profile. Every field is an independent deny-list: a capability
// absent from any field is absent from the result.
type Input struct {
	// Host is the set of enforcement categories the runtime (adapter) can
	// technically guarantee. A capability whose category is not in Host is
	// unenforceable and dropped — the runtime cannot honor deny-by-default for
	// tools it cannot control.
	Host []Category

	// Pack is the pack-level capability ceiling. Scope-less category tokens
	// (e.g. "fs.write") grant their whole category to stages below.
	Pack []Token
	// Stage is the stage-level subset. The runner resolves inheritance
	// (empty stage declarations inherit the pack set) before calling Effective,
	// so an empty Stage here means "this stage declares nothing" — pure
	// intersection denies every category.
	Stage []Token

	// Role selects the scoped base template. The role is the only input that
	// carries path/command scopes; everything else is scope-less categories or
	// named entities.
	Role Role
	// Invocation is the per-run grant addition. It can widen the role (e.g.
	// grant net.fetch to a spec stage that researches online). It still must
	// survive pack ∩ stage ∩ host.
	Invocation []Token

	// HardTimeout and IdleTimeout are optional per-invocation caps. Zero means
	// "no cap from this input"; the effective cap is the minimum non-zero value
	// across the inputs the runner considered.
	HardTimeout time.Duration
	IdleTimeout time.Duration
}

// Effective computes the effective capability profile as the intersection of
// all inputs. The algorithm:
//
//  1. Base = RoleTemplate(role) ∪ Invocation — the capabilities the role
//     grants (scoped) plus anything the run explicitly adds.
//  2. For each base token, keep it iff its enforcement category is supported
//     by Host AND its grant is matched by both Pack and Stage.
//  3. Timeouts: the minimum non-zero HardTimeout / IdleTimeout wins; all-zero
//     means no cap.
//
// The result is normalized (grants de-duplicated and sorted) so two equal
// inputs produce byte-identical stored profiles.
func Effective(input Input) Profile {
	base := unionTokens(RoleTemplate(input.Role), input.Invocation)

	hostSupported := make(map[Category]struct{}, len(input.Host))
	for _, category := range input.Host {
		hostSupported[category] = struct{}{}
	}

	grants := make([]Token, 0, len(base))
	for _, token := range base {
		category := enforcementCategory(token)
		if _, supported := hostSupported[category]; !supported {
			continue
		}
		if !grantedBy(token, input.Pack) {
			continue
		}
		if !grantedBy(token, input.Stage) {
			continue
		}
		grants = append(grants, token)
	}

	profile := Profile{
		Grants:      grants,
		HardTimeout: input.HardTimeout,
		IdleTimeout: input.IdleTimeout,
		Source: Sources{
			Host:       categoriesLabel(input.Host),
			Pack:       append([]Token(nil), input.Pack...),
			Stage:      append([]Token(nil), input.Stage...),
			Role:       input.Role,
			Invocation: append([]Token(nil), input.Invocation...),
		},
	}
	return profile.normalized()
}

// unionTokens merges two token slices, de-duplicating. Order is not preserved —
// the caller normalizes the final profile.
func unionTokens(left, right []Token) []Token {
	seen := make(map[Token]struct{}, len(left)+len(right))
	out := make([]Token, 0, len(left)+len(right))
	for _, token := range left {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	for _, token := range right {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

// grantedBy reports whether candidates grants the given token. A candidate
// grants a scoped token when it carries the same category unscoped (e.g.
// "fs.write" grants "fs.write:src/**"); a named-entity token is granted only by
// an exact match.
func grantedBy(token Token, candidates []Token) bool {
	for _, candidate := range candidates {
		if candidate == token {
			return true
		}
		// A scoped token ("fs.write:${worktree}/**") is granted by an unscoped
		// candidate of the same category ("fs.write"). The reverse (scoped
		// candidate granting an unscoped token) does not apply here: roles are
		// the only scoped source.
		if token.Scope() != "" && candidate.Scope() == "" && candidate.Key() == token.Key() {
			return true
		}
	}
	return false
}

// enforcementCategory returns the category an enforcer must support to honor
// the token. Scoped tokens drop the scope; named-entity tokens
// (secret.*/mcp.*/skill.*) collapse to their meta-category, since the enforcer
// declares mechanism support (env scrub / mcp config / skill permission map)
// rather than per-entity support.
//
// skill.* joins the vocabulary as an inert seam (ADR 0002 D7): no role template
// grants it and the opencode adapter does not list CatSkill in Supported, so a
// profile carrying it is unenforceable and the invocation refuses to start. The
// category exists so a future adapter can declare support and render a
// permission.skill rule list without a model change.
func enforcementCategory(token Token) Category {
	raw := string(token)
	switch {
	case raw == "secret" || startsWithNamed(raw, "secret."):
		return "secret"
	case raw == "mcp" || startsWithNamed(raw, "mcp."):
		return "mcp"
	case raw == "skill" || startsWithNamed(raw, "skill."):
		return CatSkill
	}
	return Category(token.Key())
}

// startsWithNamed reports whether raw begins with prefix followed by at least
// one character (so "secret." alone does not count as a named entity).
func startsWithNamed(raw, prefix string) bool {
	if len(raw) <= len(prefix) {
		return false
	}
	return raw[:len(prefix)] == prefix
}

// categoriesLabel renders the host support set as a stable comma-joined label
// for the audit Source. Kept simple — the full token lists carry the detail.
func categoriesLabel(categories []Category) string {
	out := make([]string, 0, len(categories))
	for _, category := range categories {
		out = append(out, string(category))
	}
	if len(out) == 0 {
		return ""
	}
	// Stable order without importing sort here; the input is small and the
	// caller-provided order is itself meaningful (it mirrors the enforcer's
	// declaration).
	joined := ""
	for index, name := range out {
		if index > 0 {
			joined += ","
		}
		joined += name
	}
	return joined
}
