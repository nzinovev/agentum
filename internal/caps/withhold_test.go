package caps

import (
	"encoding/json"
	"testing"
)

// TestEffective_WithheldSourceWriteDropsWritesAndBash: an implementer template
// with Withheld: SourceWriteCategories yields only fs.read, git.read, and
// artifact.write — the pre-approval profile a plan stage's gate enforces.
// exec.bash is withheld alongside fs.write/git.write on purpose (ADR 0003 D3):
// withholding only fs.write would be theatre since bash allows shell redirects.
func TestEffective_WithheldSourceWriteDropsWritesAndBash(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:     allHostCategories,
		Pack:     []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitRead), Token(CatGitWrite), Token(CatExecBash), Token(CatArtifactWrite)},
		Stage:    []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitRead), Token(CatGitWrite), Token(CatExecBash), Token(CatArtifactWrite)},
		Role:     RoleImplementer,
		Withheld: SourceWriteCategories,
	})
	if profile.Has(CatFsWrite) {
		t.Errorf("fs.write survived withholding: %v", profile.Grants)
	}
	if profile.Has(CatGitWrite) {
		t.Errorf("git.write survived withholding: %v", profile.Grants)
	}
	if profile.Has(CatExecBash) {
		t.Errorf("exec.bash survived withholding: %v", profile.Grants)
	}
	if !profile.Has(CatFsRead) || !profile.Has(CatGitRead) {
		t.Errorf("read grants dropped under withholding: %v", profile.Grants)
	}
}

// TestEffective_WithheldArtifactWriteSurvivesFloor: the runner injects an
// artifact.write floor AFTER Effective, so withholding does not need to spare
// it. But artifact.write must still survive a SourceWriteCategories withholding
// (it is not in the set) — every stage must be able to write result.json.
func TestEffective_WithheldArtifactWriteSurvivesFloor(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:     allHostCategories,
		Pack:     []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatArtifactWrite)},
		Stage:    []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatArtifactWrite)},
		Role:     RoleImplementer,
		Withheld: SourceWriteCategories,
	})
	if !profile.Has(CatArtifactWrite) {
		t.Errorf("artifact.write must survive SourceWriteCategories withholding: %v", profile.Grants)
	}
}

// TestEffective_WithheldReasonRoundTripsJSON: the reason and the withheld list
// must survive a JSON round-trip through the stored Source, so an audit reader
// sees both.
func TestEffective_WithheldReasonRoundTripsJSON(t *testing.T) {
	t.Parallel()
	profile := Effective(Input{
		Host:           allHostCategories,
		Pack:           []Token{Token(CatFsRead), Token(CatFsWrite)},
		Stage:          []Token{Token(CatFsRead), Token(CatFsWrite)},
		Role:           RoleImplementer,
		Withheld:       SourceWriteCategories,
		WithheldReason: "plan approval not recorded",
	})
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Profile
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Source.WithheldReason != "plan approval not recorded" {
		t.Errorf("withheld reason round-trip = %q", decoded.Source.WithheldReason)
	}
	if len(decoded.Source.Withheld) != len(SourceWriteCategories) {
		t.Errorf("withheld list round-trip len = %d, want %d", len(decoded.Source.Withheld), len(SourceWriteCategories))
	}
}

// TestEffective_WithheldCategoryNotGrantedIsNoOp: withholding a category the
// profile never granted must not affect other grants or panic.
func TestEffective_WithheldCategoryNotGrantedIsNoOp(t *testing.T) {
	t.Parallel()
	baseline := Effective(Input{
		Host:  allHostCategories,
		Pack:  []Token{Token(CatFsRead)},
		Stage: []Token{Token(CatFsRead)},
		Role:  RoleAnalyst,
	})
	withheld := Effective(Input{
		Host:     allHostCategories,
		Pack:     []Token{Token(CatFsRead)},
		Stage:    []Token{Token(CatFsRead)},
		Role:     RoleAnalyst,
		Withheld: SourceWriteCategories, // analyst never had these — no-op
	})
	if len(withheld.Grants) != len(baseline.Grants) {
		t.Errorf("withholding not-granted categories changed grants: baseline=%v withheld=%v", baseline.Grants, withheld.Grants)
	}
}

// TestEffective_EmptyWithheldMatchesPreADR0003: an empty Withheld reproduces
// the pre-ADR-0003 profile byte-for-byte (the golden). This is the property
// that keeps every existing fixture and test unchanged.
func TestEffective_EmptyWithheldMatchesPreADR0003(t *testing.T) {
	t.Parallel()
	// An implementer profile with full grants, no withholding.
	profile := Effective(Input{
		Host:  allHostCategories,
		Pack:  []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitRead), Token(CatGitWrite), Token(CatExecBash)},
		Stage: []Token{Token(CatFsRead), Token(CatFsWrite), Token(CatGitRead), Token(CatGitWrite), Token(CatExecBash)},
		Role:  RoleImplementer,
	})
	// Must carry all source-write categories when nothing is withheld.
	for _, category := range SourceWriteCategories {
		if !profile.Has(category) {
			t.Errorf("pre-ADR-0003 profile lost %s: %v", category, profile.Grants)
		}
	}
	if len(profile.Source.Withheld) != 0 || profile.Source.WithheldReason != "" {
		t.Errorf("empty Withheld must produce empty Source.Withheld/Reason: %+v", profile.Source)
	}
}
