// Package caps is the provider-neutral capability model every agent runtime
// (opencode, claude-code, …) is subject to.
//
// Design rules enforced by this package:
//
//   - **Deny by default.** Anything not present in the effective Profile is
//     forbidden. There is no "implicit grant"; an empty input set is an empty
//     profile.
//   - **Effective = intersection.** The effective profile for one invocation is
//     the intersection of four inputs: the runtime's supported set (what the
//     adapter can technically enforce), the pack's declared set, the stage's
//     declared set, and the role template (optionally widened by a per-run
//     grant). A capability absent from any input is absent from the result.
//   - **Roles carry scope.** Path and command scopes live on the role template
//     (e.g. an implementer's fs.write is scoped to its worktree). The pack and
//     stage declare categories without scope; the role refines them. This keeps
//     pack manifests portable across runtimes while the runtime owns the
//     concrete boundary.
//   - **Delivery refs are never agent-granted.** The git.delivery capability is
//     the orchestrator's own (checkpoints, result_commit capture, branch
//     cleanup). No agent role includes it; deny-by-default keeps it out of every
//     agent invocation regardless of what a pack declares.
//
// v1 scope: this model protects against *accidental* agent actions in a
// single-owner local runtime, not against a malicious process. The documented
// escape paths live in docs/capabilities.md.
package caps

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Token is one capability grant, optionally scoped. Canonical forms:
//
//   - Category only:        "fs.read", "exec.bash", "net.fetch"
//   - Path-scoped:          "fs.write:src/**", "artifact.write"
//   - Named entity:         "secret.github_token", "mcp.github"
//   - Delivery (system):    "git.delivery"
//
// The part before the first ':' is the Key() used for category-level
// intersection. For named entities (secret.*, mcp.*) the whole token is the
// key — each named entity is a distinct capability, never a scope of a broader
// one.
type Token string

// Key returns the capability category used for intersection matching. Scoped
// tokens drop the scope ("fs.write:src/**" → "fs.write"); named-entity tokens
// keep their identity ("secret.github_token" → "secret.github_token").
func (token Token) Key() string {
	if idx := strings.IndexByte(string(token), ':'); idx >= 0 {
		return string(token[:idx])
	}
	return string(token)
}

// Scope returns the path scope portion of a scoped token, or "" when the token
// is unscoped or is a named entity. Used by adapters that translate the scope
// into a runtime-specific permission rule.
func (token Token) Scope() string {
	if idx := strings.IndexByte(string(token), ':'); idx >= 0 {
		return string(token[idx+1:])
	}
	return ""
}

// Category is the categorical name of a capability (the Key space). Used by
// adapters to declare the set of categories they can technically enforce.
type Category string

const (
	CatFsRead        Category = "fs.read"
	CatFsWrite       Category = "fs.write"
	CatArtifactWrite Category = "artifact.write"
	CatExecBash      Category = "exec.bash"
	CatGitRead       Category = "git.read"
	CatGitWrite      Category = "git.write"
	CatGitDelivery   Category = "git.delivery" // orchestrator-only; never in an agent role
	CatNetFetch      Category = "net.fetch"
	CatSkill         Category = "skill" // ADR 0002 D7: inert seam. No role grants it, so no
	// effective profile carries it and MVP behaviour is unchanged. An adapter
	// that cannot enforce it (the opencode adapter does not list it in Supported)
	// refuses to start a profile that carries skill.* — the same answer mcp.*
	// gets, for the same reason. The token exists so narrowing the skill set
	// later becomes a config change (permission.skill rule list) rather than a
	// model redesign.
)

// CategoryOf reports the enforcement category a token belongs to — the value
// an enforcer declares support for. Scoped tokens drop the scope
// ("fs.write:src/**" → "fs.write"); named-entity tokens (secret.*/mcp.*)
// collapse to their meta-category, since an enforcer declares mechanism support
// (env scrub / mcp config) rather than per-entity support.
func CategoryOf(token Token) Category { return enforcementCategory(token) }

// Profile is the effective capability set granted to one invocation, plus the
// resource limits the runtime enforces for it. The zero value grants nothing
// and imposes no limits — a safe deny-by-default baseline.
type Profile struct {
	// Grants is the de-duplicated, lexically-sorted set of capability tokens
	// the invocation may exercise. Anything not in this set is denied.
	Grants []Token `json:"grants,omitempty"`

	// HardTimeout is the wall-clock cap on the invocation. Zero means no cap.
	// The runtime applies this via context cancellation.
	HardTimeout time.Duration `json:"hard_timeout,omitempty"`
	// IdleTimeout is the cap on time without observable progress (a stream
	// chunk). Zero means no cap.
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`

	// Source records the inputs that produced this profile (host / pack /
	// stage / role / invocation), for audit. Free-form labels; the runner
	// fills them when it computes Effective.
	Source Sources `json:"source,omitempty"`
}

// Sources records which inputs contributed to an effective profile and what
// each carried. Stored as audit evidence so a later review can reconstruct why
// the invocation saw exactly these capabilities.
type Sources struct {
	Host       string  `json:"host,omitempty"`       // runtime/adapter name
	Pack       []Token `json:"pack,omitempty"`       // pack-level declarations
	Stage      []Token `json:"stage,omitempty"`      // stage-level subset
	Role       Role    `json:"role"`                 // role template applied
	Invocation []Token `json:"invocation,omitempty"` // per-run grant additions
}

// Has reports whether the profile grants the given category (by Key). A scoped
// grant satisfies the unscoped query; a named-entity token matches only itself.
func (profile Profile) Has(category Category) bool {
	for _, granted := range profile.Grants {
		if CategoryOf(granted) == category {
			return true
		}
	}
	return false
}

// HasToken reports whether the profile grants the exact token (scope included).
// Use Has for category-level checks; HasToken for a specific scoped or named
// grant.
func (profile Profile) HasToken(token Token) bool {
	return slices.Contains(profile.Grants, token)
}

// IsEmpty reports whether the profile grants nothing and imposes no timeout.
// An empty profile is a valid deny-by-default profile — it is the result when
// no input granted any category.
func (profile Profile) IsEmpty() bool {
	return len(profile.Grants) == 0 && profile.HardTimeout == 0 && profile.IdleTimeout == 0
}

// Normalize returns a copy of profile with grants de-duplicated and sorted, so
// two profiles built from the same tokens (in any order) compare byte-equal.
// Callers that mutate Grants after Effective (e.g. injecting a contract floor)
// use this to keep the stored representation canonical.
func Normalize(profile Profile) Profile { return profile.normalized() }

// normalized returns a copy of the grants de-duplicated and sorted. Used by
// Effective to produce a canonical representation so two equal inputs yield
// byte-identical stored profiles.
func (profile Profile) normalized() Profile {
	out := Profile{
		Grants:      dedupSort(profile.Grants),
		HardTimeout: profile.HardTimeout,
		IdleTimeout: profile.IdleTimeout,
		Source:      profile.Source,
	}
	out.Source.Pack = dedupSort(out.Source.Pack)
	out.Source.Stage = dedupSort(out.Source.Stage)
	out.Source.Invocation = dedupSort(out.Source.Invocation)
	return out
}

func dedupSort(tokens []Token) []Token {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[Token]struct{}, len(tokens))
	out := make([]Token, 0, len(tokens))
	for _, token := range tokens {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	slices.Sort(out)
	return out
}

// ErrUnenforceable is returned by EnforceableBy when the profile grants a
// capability the runtime cannot technically enforce. Missing lists the
// unsupported categories.
var ErrUnenforceable = errors.New("caps: profile grants capabilities the runtime cannot enforce")

// Unsupported wraps ErrUnenforceable with the list of categories the declared
// enforcer cannot guarantee. Returned by EnforceableBy so callers can record
// the gap as audit evidence before refusing to start the invocation.
type Unsupported struct {
	Missing []Category
}

func (unsupported *Unsupported) Error() string {
	missingNames := make([]string, 0, len(unsupported.Missing))
	for _, category := range unsupported.Missing {
		missingNames = append(missingNames, string(category))
	}
	return fmt.Sprintf("caps: unsupported capabilities: %s", strings.Join(missingNames, ", "))
}

func (unsupported *Unsupported) Unwrap() error { return ErrUnenforceable }

// EnforceableBy reports whether every granted capability belongs to a category
// the enforcer can technically enforce. Supported is the enforcer's declared
// category set (e.g. the opencode adapter's Supported()). A profile that grants
// a capability whose category is missing from supported is unenforceable: the
// runtime cannot honor the deny-by-default promise for the unsupported tools, so
// the invocation must not start.
//
// Returns (nil, true) when the profile is enforceable. Returns an
// *Unsupported (wrapping ErrUnenforceable) listing the unsupported categories
// otherwise.
func (profile Profile) EnforceableBy(supported []Category) error {
	supportedSet := make(map[Category]struct{}, len(supported))
	for _, category := range supported {
		supportedSet[category] = struct{}{}
	}
	missing := make([]Category, 0)
	for _, granted := range profile.Grants {
		category := CategoryOf(granted)
		if _, found := supportedSet[category]; !found {
			missing = append(missing, category)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return &Unsupported{Missing: missing}
	}
	return nil
}

// summary returns a short human-readable rendering of the grants for log lines
// and audit events. Stable order (the grants are already sorted).
func (profile Profile) summary() string {
	if len(profile.Grants) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(profile.Grants))
	for _, granted := range profile.Grants {
		names = append(names, string(granted))
	}
	return strings.Join(names, ", ")
}
