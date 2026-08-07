package caps

import (
	"errors"
	"testing"
	"time"
)

// TestProfile_DenyByDefault: with no inputs, the effective profile grants
// nothing. This is the load-bearing invariant of the whole model — an
// invocation cannot accidentally inherit a capability no input declared.
func TestProfile_DenyByDefault(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{})

	if !profile.IsEmpty() {
		t.Fatalf("empty inputs produced non-empty profile: %+v", profile)
	}
	if err := profile.EnforceableBy(nil); err != nil {
		t.Errorf("empty profile should be enforceable by any host, got %v", err)
	}
}

// TestProfile_RoleTemplatesAreScoped: the role is the only source of scope,
// and the system role carries git.delivery while no agent role does.
func TestProfile_RoleTemplatesAreScoped(t *testing.T) {
	t.Parallel()
	for _, role := range KnownRoles {
		assertRoleTokensGrantNothingDangerous(t, role)
	}
	// System role is the only one that carries git.delivery.
	systemTokens := RoleTemplate(RoleSystem)
	if !containsToken(systemTokens, Token(CatGitDelivery)) {
		t.Errorf("system role must grant git.delivery, got %v", systemTokens)
	}
}

// assertRoleTokensGrantNothingDangerous checks one agent role's template carries
// no empty template and no git.delivery / net.fetch / secret / mcp grant — those
// must stay orchestrator-owned or be explicit per-invocation grants.
func assertRoleTokensGrantNothingDangerous(t *testing.T, role Role) {
	t.Helper()
	tokens := RoleTemplate(role)
	if len(tokens) == 0 {
		t.Errorf("role %q has an empty template", role)
	}
	for _, token := range tokens {
		forbidden := forbiddenAgentCategories(token)
		if forbidden != "" {
			t.Errorf("agent role %q grants %s — %s", role, token, forbidden)
		}
	}
}

// forbiddenAgentCategories returns the reason a token is forbidden in an agent
// role template, or "" when the token is allowed.
func forbiddenAgentCategories(token Token) string {
	switch {
	case CategoryOf(token) == CatGitDelivery:
		return "delivery refs must stay orchestrator-owned"
	case CategoryOf(token) == CatNetFetch:
		return "network must be an explicit per-invocation grant"
	case enforcementCategory(token) == "secret":
		return "credentials must be an explicit per-invocation grant"
	case enforcementCategory(token) == "mcp":
		return "MCP must be an explicit per-invocation grant"
	case enforcementCategory(token) == CatSkill:
		// ADR 0002 D7: skill is allowed at runtime (opencode loads the user's
		// skills) but never granted by a role template — narrowing is a future
		// policy, and a role granting it would make the seam load-bearing before
		// any adapter declares support.
		return "skill narrowing is not a role-granted capability"
	}
	return ""
}

// TestEffective_IntersectionDropsAbsentCategories: a capability missing from
// any input is dropped, even if the role template grants it.
func TestEffective_IntersectionDropsAbsentCategories(t *testing.T) {
	t.Parallel()
	// Implementer role grants fs.write and exec.bash, but the pack only allows
	// fs.read+fs.write and the stage only allows fs.read. exec.bash must drop.
	profile := Effective(Input{
		Host:  []Category{CatFsRead, CatFsWrite, CatExecBash, CatGitRead, CatArtifactWrite},
		Pack:  []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatExecBash)},
		Stage: []Token{Token(CatFsRead), Token(CatFsWrite)},
		Role:  RoleImplementer,
	})

	if profile.Has(CatExecBash) {
		t.Errorf("exec.bash survived stage that omits it: %v", profile.Grants)
	}
	if !profile.Has(CatFsRead) {
		t.Errorf("fs.read dropped: %v", profile.Grants)
	}
	// fs.write survives only scoped to the worktree (the role's scope).
	if !profile.Has(CatFsWrite) {
		t.Errorf("fs.write dropped: %v", profile.Grants)
	}
	foundScoped := false
	for _, granted := range profile.Grants {
		if granted.Key() == string(CatFsWrite) {
			if granted.Scope() == "" {
				t.Errorf("fs.write lost its worktree scope: %q", granted)
			}
			foundScoped = true
		}
	}
	if !foundScoped {
		t.Errorf("fs.write grant missing entirely: %v", profile.Grants)
	}
}

// TestEffective_AnalystCannotWriteSource: analyst role grants read + own
// artifact only — no fs.write, no exec.bash, no git.write.
func TestEffective_AnalystCannotWriteSource(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host: allHostCategories,
		Pack: []Token{
			Token(CatFsRead), Token(CatFsWrite), Token(CatExecBash),
			Token(CatGitRead), Token(CatGitWrite), Token(CatArtifactWrite),
		},
		Stage: []Token{
			Token(CatFsRead), Token(CatFsWrite), Token(CatExecBash),
			Token(CatGitRead), Token(CatGitWrite), Token(CatArtifactWrite),
		},
		Role: RoleAnalyst,
	})
	if profile.Has(CatFsWrite) {
		t.Errorf("analyst must not write source, got fs.write in %v", profile.Grants)
	}
	if profile.Has(CatExecBash) {
		t.Errorf("analyst must not run commands, got exec.bash in %v", profile.Grants)
	}
	if profile.Has(CatGitWrite) {
		t.Errorf("analyst must not mutate git, got git.write in %v", profile.Grants)
	}
	if !profile.Has(CatFsRead) || !profile.Has(CatArtifactWrite) || !profile.Has(CatGitRead) {
		t.Errorf("analyst lost expected read/artifact/git.read: %v", profile.Grants)
	}
}

// TestEffective_ReviewerCannotMutateDeliveryRefs: reviewer shares analyst's
// read set and additionally must not change tracked source or delivery refs.
func TestEffective_ReviewerCannotMutateDeliveryRefs(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:  allHostCategories,
		Pack:  []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitWrite), Token(CatGitDelivery)},
		Stage: []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitWrite), Token(CatGitDelivery)},
		Role:  RoleReviewer,
	})
	if profile.Has(CatGitDelivery) {
		t.Errorf("reviewer must not touch delivery refs, got git.delivery in %v", profile.Grants)
	}
	if profile.Has(CatFsWrite) {
		t.Errorf("reviewer must not change tracked source, got fs.write in %v", profile.Grants)
	}
}

// TestEffective_ImplementerScopedToWorktree: implementer's fs.write and
// git.write carry the worktree scope; it cannot appear outside it.
func TestEffective_ImplementerScopedToWorktree(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:  allHostCategories,
		Pack:  []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitWrite), Token(CatExecBash)},
		Stage: []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitWrite), Token(CatExecBash)},
		Role:  RoleImplementer,
	})
	if !profile.Has(CatFsWrite) || !profile.Has(CatGitWrite) || !profile.Has(CatExecBash) {
		t.Fatalf("implementer lost expected grants: %v", profile.Grants)
	}
	for _, granted := range profile.Grants {
		if granted.Key() == string(CatFsWrite) && granted.Scope() == "" {
			t.Errorf("implementer fs.write must be scoped to worktree, got unscoped")
		}
		if granted.Key() == string(CatGitWrite) && granted.Scope() == "" {
			t.Errorf("implementer git.write must be scoped to worktree, got unscoped")
		}
	}
}

// TestEffective_NamedEntitiesRequireExactMatchEverywhere: secret/mcp grants
// must be exact in pack, stage, and invocation; a broad "secret" or "mcp"
// candidate does not satisfy a named token.
func TestEffective_NamedEntitiesRequireExactMatchEverywhere(t *testing.T) {
	t.Parallel()
	// Invocation adds a secret; pack and stage allow the named secret.
	withGrant := Effective(Input{
		Host:       append(allHostCategories, "secret", "mcp"),
		Pack:       []Token{Token(CatFsRead), "secret.github_token", "mcp.github"},
		Stage:      []Token{Token(CatFsRead), "secret.github_token", "mcp.github"},
		Role:       RoleAnalyst,
		Invocation: []Token{"secret.github_token", "mcp.github"},
	})
	if !withGrant.HasToken("secret.github_token") {
		t.Errorf("exact secret grant dropped: %v", withGrant.Grants)
	}
	if !withGrant.HasToken("mcp.github") {
		t.Errorf("exact mcp grant dropped: %v", withGrant.Grants)
	}

	// Pack allows the broad category but not the named entity → must drop.
	broadPack := Effective(Input{
		Host:       append(allHostCategories, "secret"),
		Pack:       []Token{Token(CatFsRead), "secret"},
		Stage:      []Token{Token(CatFsRead), "secret.github_token"},
		Role:       RoleAnalyst,
		Invocation: []Token{"secret.github_token"},
	})
	if broadPack.HasToken("secret.github_token") {
		t.Errorf("broad 'secret' pack candidate must not satisfy named token: %v", broadPack.Grants)
	}

	// Stage omits the named entity → must drop even if pack allows it.
	noStage := Effective(Input{
		Host:       append(allHostCategories, "mcp"),
		Pack:       []Token{Token(CatFsRead), "mcp.github"},
		Stage:      []Token{Token(CatFsRead)},
		Role:       RoleAnalyst,
		Invocation: []Token{"mcp.github"},
	})
	if noStage.HasToken("mcp.github") {
		t.Errorf("mcp grant survived a stage that omits it: %v", noStage.Grants)
	}
}

// TestEffective_TimeoutsTakeMinimum: the most restrictive non-zero timeout
// wins; all-zero means no cap.
func TestEffective_TimeoutsTakeMinimum(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:        allHostCategories,
		Pack:        []Token{Token(CatFsRead)},
		Stage:       []Token{Token(CatFsRead)},
		Role:        RoleAnalyst,
		HardTimeout: 30 * time.Second,
	})
	if profile.HardTimeout != 30*time.Second {
		t.Errorf("hard timeout = %v, want 30s", profile.HardTimeout)
	}
}

// TestEnforceableBy_UnsupportedCapabilityRejected: a profile granting a
// capability the host cannot enforce is rejected with the gap listed. Effective
// already intersects with Host, so this check is defense-in-depth: it lets the
// adapter independently confirm a profile (however it was built) before start.
func TestEnforceableBy_UnsupportedCapabilityRejected(t *testing.T) {
	t.Parallel()
	// Hand-built profile that bypasses Effective — the adapter must catch it.
	profile := Profile{Grants: []Token{Token(CatFsRead), Token(CatExecBash), "mcp.github"}}
	err := profile.EnforceableBy([]Category{CatFsRead, CatArtifactWrite, CatGitRead})
	if err == nil {
		t.Fatal("expected unenforceable error, got nil")
	}
	if !errors.Is(err, ErrUnenforceable) {
		t.Errorf("error must wrap ErrUnenforceable, got %v", err)
	}
	unsupported, ok := err.(*Unsupported)
	if !ok {
		t.Fatalf("error must be *Unsupported, got %T", err)
	}
	if !containsCategory(unsupported.Missing, CatExecBash) {
		t.Errorf("missing list must contain exec.bash: %v", unsupported.Missing)
	}
	if !containsCategory(unsupported.Missing, "mcp") {
		t.Errorf("missing list must contain mcp: %v", unsupported.Missing)
	}

	// A profile the host fully supports is enforceable.
	okProfile := Profile{Grants: []Token{Token(CatFsRead), Token(CatArtifactWrite)}}
	if err := okProfile.EnforceableBy([]Category{CatFsRead, CatArtifactWrite}); err != nil {
		t.Errorf("enforceable profile rejected: %v", err)
	}
}

// TestSkillToken_InertSeam (ADR 0002 D7): the skill token joins the vocabulary
// but no role grants it, and a profile carrying it is unenforceable against an
// adapter that does not list CatSkill in Supported — the same answer mcp.*
// gets, for the same reason. The category exists so narrowing becomes a config
// change later, not a model redesign.
func TestSkillToken_InertSeam(t *testing.T) {
	t.Parallel()
	// Named and bare tokens collapse to the skill meta-category.
	if CategoryOf("skill") != CatSkill {
		t.Errorf(`CategoryOf("skill") = %q, want %q`, CategoryOf("skill"), CatSkill)
	}
	if CategoryOf("skill.customize-opencode") != CatSkill {
		t.Errorf(`CategoryOf("skill.customize-opencode") = %q, want %q`, CategoryOf("skill.customize-opencode"), CatSkill)
	}
	// "skill." alone is not a named entity (startsWithNamed requires a char
	// after the dot), so it falls through to the Key() path like "mcp." and
	// "secret." — out of the named-entity contract. Not asserted here.

	// No role template grants skill.* — the loop over every role already
	// asserts this via forbiddenAgentCategories, but make the intent explicit.
	for _, role := range []Role{RoleAnalyst, RoleReviewer, RoleImplementer, RoleFixer, RoleSystem} {
		for _, granted := range RoleTemplate(role) {
			if CategoryOf(granted) == CatSkill {
				t.Errorf("role %q grants skill token %q — skill must not be role-granted", role, granted)
			}
		}
	}

	// A profile carrying skill.foo is unenforceable against an adapter that does
	// not list CatSkill in Supported (the opencode adapter does not).
	profile := Profile{Grants: []Token{Token(CatFsRead), "skill.customize-opencode"}}
	err := profile.EnforceableBy([]Category{CatFsRead, CatFsWrite, CatExecBash})
	if err == nil {
		t.Fatal("expected unenforceable error for skill.* against a non-skill-supporting host, got nil")
	}
	if !errors.Is(err, ErrUnenforceable) {
		t.Errorf("error must wrap ErrUnenforceable, got %v", err)
	}
	unsupported, ok := err.(*Unsupported)
	if !ok {
		t.Fatalf("error must be *Unsupported, got %T", err)
	}
	if !containsCategory(unsupported.Missing, CatSkill) {
		t.Errorf("missing list must contain skill: %v", unsupported.Missing)
	}

	// And enforceable against an adapter that DOES list CatSkill — so the seam
	// is a real seam, not a permanent refusal.
	if err := profile.EnforceableBy([]Category{CatFsRead, CatSkill}); err != nil {
		t.Errorf("skill-supporting host must accept skill.*: %v", err)
	}
}

// TestDeriveRole_ConventionFallback: standard stage ids map to the expected
// roles; unknown ids default to the conservative analyst role.
func TestDeriveRole_ConventionFallback(t *testing.T) {
	t.Parallel()
	cases := []struct {
		stage string
		want  Role
	}{
		{"spec", RoleAnalyst},
		{"analyze", RoleAnalyst},
		{"design", RoleAnalyst},
		{"review", RoleReviewer},
		{"pre_review", RoleReviewer},
		{"implement", RoleImplementer},
		{"build", RoleImplementer},
		{"fix", RoleFixer},
		{"patch", RoleFixer},
		{"unknown-stage", RoleAnalyst},
		{"", RoleAnalyst},
	}
	for _, tc := range cases {
		if got := DeriveRole(tc.stage); got != tc.want {
			t.Errorf("DeriveRole(%q) = %q, want %q", tc.stage, got, tc.want)
		}
	}
}

// TestEffective_SourceRecorded: the profile carries its inputs as audit
// evidence, so a reviewer can reconstruct why the invocation saw these caps.
func TestEffective_SourceRecorded(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:        allHostCategories,
		Pack:        []Token{Token(CatFsRead), Token(CatFsWrite)},
		Stage:       []Token{Token(CatFsRead)},
		Role:        RoleImplementer,
		Invocation:  []Token{"net.fetch"},
		HardTimeout: 10 * time.Second,
	})
	if profile.Source.Role != RoleImplementer {
		t.Errorf("source role = %q, want implementer", profile.Source.Role)
	}
	if len(profile.Source.Pack) != 2 {
		t.Errorf("source pack len = %d, want 2", len(profile.Source.Pack))
	}
	if len(profile.Source.Invocation) != 1 || profile.Source.Invocation[0] != "net.fetch" {
		t.Errorf("source invocation = %v, want [net.fetch]", profile.Source.Invocation)
	}
}

// allHostCategories is the opencode adapter's full support set, used in tests
// that are not about host enforcement.
var allHostCategories = []Category{
	CatFsRead, CatFsWrite, CatArtifactWrite, CatExecBash,
	CatGitRead, CatGitWrite, CatNetFetch,
}

func containsToken(tokens []Token, want Token) bool {
	for _, token := range tokens {
		if token == want {
			return true
		}
	}
	return false
}

func containsCategory(categories []Category, want Category) bool {
	for _, category := range categories {
		if category == want {
			return true
		}
	}
	return false
}
