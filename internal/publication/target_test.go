package publication

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadPublicationRemoteUsesPinnedCheckoutAndNamedRemote(test *testing.T) {
	checkout := test.TempDir()
	for _, arguments := range [][]string{
		{"init", checkout},
		{"-C", checkout, "config", "remote.origin.url", "git@github.com:owner/origin.git"},
		{"-C", checkout, "config", "remote.delivery.url", "https://github.com/owner/delivery.git"},
	} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			test.Fatalf("git: %s %v", output, err)
		}
	}
	ambient := test.TempDir()
	if err := os.WriteFile(filepath.Join(ambient, ".gitconfig"), []byte("[remote \"missing\"]\n url = https://github.com/ambient/repo.git\n"), 0600); err != nil {
		test.Fatal(err)
	}
	test.Setenv("HOME", ambient)
	for _, scenario := range []struct{ remote, want string }{
		{"origin", "git@github.com:owner/origin.git"},
		{"delivery", "https://github.com/owner/delivery.git"},
	} {
		remote, err := readPublicationRemote(test.Context(), checkout, scenario.remote)
		if err != nil || remote != scenario.want {
			test.Fatalf("%s: %q %v", scenario.remote, remote, err)
		}
	}
	if _, err := readPublicationRemote(test.Context(), checkout, "missing"); err == nil {
		test.Fatal("ambient remote was used")
	}
}
