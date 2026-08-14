package runner

import (
	"bytes"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
)

// TestCapDiffPatch_PreservesSmallPatches: a patch under the cap is returned
// verbatim with truncated=false.
func TestCapDiffPatch_PreservesSmallPatches(t *testing.T) {
	t.Parallel()
	patch := []byte("@@ first hunk\n+line\n@@ second hunk\n+line\n")
	body, truncated := capDiffPatch(patch, 1<<20)
	if truncated {
		t.Errorf("small patch marked truncated")
	}
	if !bytes.Equal(body, patch) {
		t.Errorf("small patch body modified:\n got = %q\nwant = %q", body, patch)
	}
}

// TestCapDiffPatch_TruncatesOnHunkBoundaryWithMarker: a patch over the cap is
// cut on a hunk boundary and carries artifacts.DiffTruncationMarker, so the
// final-review payload (which scans for the marker) reports Truncated=true. The
// marker is the durable wire signal — content_size cannot distinguish a
// cap-sized patch from a truncated one.
func TestCapDiffPatch_TruncatesOnHunkBoundaryWithMarker(t *testing.T) {
	t.Parallel()
	// Build a patch with two hunks where the first is well under a tiny cap and
	// the second pushes the total over it.
	hunkOne := []byte("@@ a\n+" + string(bytes.Repeat([]byte("x"), 200)) + "\n")
	hunkTwo := []byte("@@ b\n+" + string(bytes.Repeat([]byte("y"), 200)) + "\n")
	patch := append(append([]byte{}, hunkOne...), hunkTwo...)
	body, truncated := capDiffPatch(patch, 256)
	if !truncated {
		t.Fatal("over-cap patch not marked truncated")
	}
	if !bytes.Contains(body, []byte(artifacts.DiffTruncationMarker)) {
		t.Errorf("truncated patch missing DiffTruncationMarker; the final-review payload would not detect truncation. body ends: %q", body[len(body)-80:])
	}
	// The body must end on a hunk boundary: the retained portion is hunkOne
	// (complete), and the marker follows. hunkTwo must not appear.
	if bytes.Contains(body, hunkTwo) {
		t.Errorf("truncated body contains the second hunk; truncation did not cut on the hunk boundary")
	}
	if !bytes.Contains(body, hunkOne) {
		t.Errorf("truncated body lost the first (fitting) hunk")
	}
}
