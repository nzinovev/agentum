package publication

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nzinovev/agentum/internal/publish"
)

func readPublicationRemote(ctx context.Context, checkout, remote string) (string, error) {
	if publish.ValidateRemoteName(remote) != nil {
		return "", &publish.Refusal{Code: publish.ReasonRemoteUnknown}
	}
	temporary, err := os.MkdirTemp("", "agentum-remote-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	command := exec.CommandContext(ctx, "git", "-C", checkout, "config", "--local", "--no-includes", "--get", "remote."+remote+".url")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temporary, "GIT_CONFIG_NOSYSTEM=1"}
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return "", &publish.Refusal{Code: publish.ReasonRemoteUnknown}
		}
		return "", &publish.Refusal{Code: publish.ReasonProviderError}
	}
	return strings.TrimSpace(string(output)), nil
}
