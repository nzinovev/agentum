package artifacts

import "testing"

// TestDiffTruncationMarker_StableWireContract: the marker is a stable wire
// string shared between the runner (producer, capDiffPatch) and the final-review
// payload (consumer, truncation detection over stored bytes). Changing it is a
// wire-format break: already-stored truncated patches would stop being detected
// as truncated. This test exists so a rename trips CI.
func TestDiffTruncationMarker_StableWireContract(t *testing.T) {
	t.Parallel()
	want := "\n--- diff truncated by Agentum at the size cap; see diff.stat and read named files directly ---\n"
	if DiffTruncationMarker != want {
		t.Errorf("DiffTruncationMarker changed:\n got = %q\nwant = %q", DiffTruncationMarker, want)
	}
}
