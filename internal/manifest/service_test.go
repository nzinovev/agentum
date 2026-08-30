package manifest

import (
	"encoding/json"
	"testing"
)

// TestMergeIntoLocked_DecodesAndMerges protects the AddEvidence write path: the
// bytes read under the row lock must be decoded and the patch deep-merged into
// them before the SQL replacement. A regression that re-introduced a
// pre-transaction merge base would still pass this, but a regression that
// skipped the decode (merging onto an empty body) would not — and that is the
// silent corruption worth guarding, since it would replace the row with the
// patch alone.
func TestMergeIntoLocked_DecodesAndMerges(t *testing.T) {
	t.Parallel()
	locked := Body{Schema: "1", Input: &InputEvidence{TaskID: "T1", Revision: "v1"}}
	lockedBytes, err := json.Marshal(locked)
	if err != nil {
		t.Fatalf("marshal locked: %v", err)
	}
	patch := Body{Invocations: []InvocationEvidence{testInvocation("inv-1", "spec", 0)}}

	merged, err := mergeIntoLocked(lockedBytes, patch)
	if err != nil {
		t.Fatalf("mergeIntoLocked: %v", err)
	}
	decoded, err := decodeBody(merged)
	if err != nil {
		t.Fatalf("decode merged: %v", err)
	}
	if decoded.Input == nil || decoded.Input.Revision != "v1" {
		t.Errorf("existing section lost: %+v", decoded.Input)
	}
	if len(decoded.Invocations) != 1 || decoded.Invocations[0].Stage != "spec" {
		t.Errorf("patch not merged: %+v", decoded.Invocations)
	}
}

// TestMergeIntoLocked_UndecodableBodyIsAnError guards against the failure mode
// where a corrupt body row would be silently treated as empty and the patch
// written in place of the real evidence. The merge must fail loudly so the row
// is not clobbered with a partial patch.
func TestMergeIntoLocked_UndecodableBodyIsAnError(t *testing.T) {
	t.Parallel()
	_, err := mergeIntoLocked([]byte("{not json"), Body{Invocations: []InvocationEvidence{testInvocation("inv-1", "s", 0)}})
	if err == nil {
		t.Fatal("mergeIntoLocked on undecodable body returned nil error; an empty merge base would clobber the row")
	}
}

// TestMergeIntoLocked_EmptyLockedBodyStartsFresh covers the create path: a
// fresh manifest row may carry an empty body, which must decode to the schema
// baseline and accept the patch rather than erroring.
func TestMergeIntoLocked_EmptyLockedBodyStartsFresh(t *testing.T) {
	t.Parallel()
	patch := Body{Input: &InputEvidence{TaskID: "T1", Revision: "v1"}}
	merged, err := mergeIntoLocked(nil, patch)
	if err != nil {
		t.Fatalf("mergeIntoLocked on empty body: %v", err)
	}
	decoded, err := decodeBody(merged)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Schema != schemaVersion {
		t.Errorf("Schema = %q, want %q", decoded.Schema, schemaVersion)
	}
	if decoded.Input == nil || decoded.Input.TaskID != "T1" {
		t.Errorf("patch not applied: %+v", decoded.Input)
	}
}

// TestCorrectionBase_PreferLatestOverSealed is the D2 invariant test: a new
// correction must chain onto the latest existing correction, not the sealed
// body. If it merged onto the sealed body, the second correction would drop
// the first's changes — the exact failure a correction chain exists to
// prevent, and invisible because both correction rows still exist.
func TestCorrectionBase_PreferLatestOverSealed(t *testing.T) {
	t.Parallel()
	sealed := Body{Input: &InputEvidence{TaskID: "T1", Revision: "sealed"}}
	latest := Body{Input: &InputEvidence{TaskID: "T1", Revision: "correction-1"}}

	base := correctionBase(sealed, &latest)
	if base.Input == nil || base.Input.Revision != "correction-1" {
		t.Errorf("base = %+v, want the latest correction's body", base.Input)
	}
}

// TestCorrectionBase_FallsBackToSealedWhenNoLatest covers the first correction:
// when no correction exists yet, the sealed body is the base.
func TestCorrectionBase_FallsBackToSealedWhenNoLatest(t *testing.T) {
	t.Parallel()
	sealed := Body{Input: &InputEvidence{TaskID: "T1", Revision: "sealed"}}

	base := correctionBase(sealed, nil)
	if base.Input == nil || base.Input.Revision != "sealed" {
		t.Errorf("base = %+v, want the sealed body", base.Input)
	}
}

// TestCorrectionChain_PreservesEarlierCorrections is the end-to-end merge claim
// behind D2: applying two patches through the chain (latest as base, then
// merge) preserves the first patch's contribution. This is what Get sees when
// it takes the last correction as the authoritative body.
func TestCorrectionChain_PreservesEarlierCorrections(t *testing.T) {
	t.Parallel()
	sealed := Body{Missing: []string{"memory"}}
	first := mergeBodies(sealed, Body{Input: &InputEvidence{TaskID: "T1", Revision: "v1"}})
	second := mergeBodies(first, Body{Invocations: []InvocationEvidence{testInvocation("inv-2", "spec", 1)}})

	if second.Input == nil || second.Input.Revision != "v1" {
		t.Errorf("first correction's input lost in chain: %+v", second.Input)
	}
	if len(second.Invocations) != 1 || second.Invocations[0].InvocationID != "inv-2" {
		t.Errorf("second correction's invocations lost in chain: %+v", second.Invocations)
	}
	if len(second.Missing) != 1 || second.Missing[0] != "memory" {
		t.Errorf("sealed body's missing section lost in chain: %+v", second.Missing)
	}
}
