package publish

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func gitPush(ctx context.Context, delivery Delivery, token string) error {
	if !commitSHA.MatchString(delivery.ResultCommit) {
		return refuse(ReasonCommitMismatch)
	}
	if !validBranch(delivery.Target.RemoteBranch) {
		return refuse(ReasonRemoteUnknown)
	}
	temporary, err := os.MkdirTemp("", "agentum-publisher-")
	if err != nil {
		return refuse(ReasonProviderError)
	}
	defer os.RemoveAll(temporary)
	askpass := filepath.Join(temporary, "askpass")
	script := "#!/bin/sh\ncase \"$1\" in\n *Username*) printf '%s\\n' 'x-access-token' ;;\n *) printf '%s\\n' \"$AGENTUM_PUBLISH_TOKEN\" ;;\nesac\n"
	if err := os.WriteFile(askpass, []byte(script), 0700); err != nil {
		return refuse(ReasonProviderError)
	}
	environment := gitEnvironment(temporary, askpass, token)
	verifyArgs, pushArgs := gitArguments(delivery)
	verified, err := runGit(ctx, verifyArgs, environment)
	if err != nil {
		return refuse(ReasonProviderError)
	}
	if strings.TrimSpace(string(verified.stdout)) != delivery.ResultCommit {
		return refuse(ReasonCommitMismatch)
	}
	output, err := runGit(ctx, pushArgs, environment)
	if err == nil {
		return nil
	}
	message := strings.ToLower(string(output.stdout) + "\n" + string(output.stderr))
	switch {
	case strings.Contains(message, "non-fast-forward"), strings.Contains(message, "fetch first"):
		return refuse(ReasonNonFastForward)
	case strings.Contains(message, "authentication failed"), strings.Contains(message, "invalid username or token"), strings.Contains(message, "could not read username"), strings.Contains(message, "error: 401"):
		return refuse(ReasonCredentialsRejected)
	case ctx.Err() != nil, strings.Contains(message, "could not resolve host"), strings.Contains(message, "failed to connect"), strings.Contains(message, "connection timed out"):
		return refuse(ReasonNetworkUnreachable)
	default:
		return refuse(ReasonPushRejected)
	}
}

func gitArguments(delivery Delivery) ([]string, []string) {
	return []string{"-C", delivery.Project.CheckoutPath, "rev-parse", "--verify", delivery.ResultCommit + "^{commit}"},
		[]string{"-C", delivery.Project.CheckoutPath, "push", "--porcelain", targetURL(delivery.Target), delivery.ResultCommit + ":refs/heads/" + delivery.Target.RemoteBranch}
}

func gitEnvironment(temporary, askpass, token string) []string {
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temporary, "GIT_ASKPASS=" + askpass, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "AGENTUM_PUBLISH_TOKEN=" + token}
	// Command-scoped settings override repository helpers and hooks, which HOME does not disable.
	settings := [][2]string{
		{"credential.helper", ""}, {"credential.username", "x-access-token"},
		{"core.hooksPath", temporary}, {"core.fsmonitor", "false"},
		{"http.extraHeader", ""}, {"http.proxy", ""}, {"http.followRedirects", "false"},
		{"http.sslVerify", "true"}, {"protocol.allow", "never"}, {"protocol.https.allow", "always"},
		{"push.gpgSign", "false"}, {"push.recurseSubmodules", "no"},
		{"push.followTags", "false"}, {"push.pushOption", ""}, {"push.autoSetupRemote", "false"}, {"push.negotiate", "false"},
	}
	environment = append(environment, "GIT_CONFIG_COUNT="+strconv.Itoa(len(settings)))
	for index, setting := range settings {
		environment = append(environment, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", index, setting[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, setting[1]))
	}
	return environment
}

type cappedOutput struct {
	bytes.Buffer
	truncated bool
}

func (output *cappedOutput) Write(data []byte) (int, error) {
	length := len(data)
	if length > 64*1024-output.Len() {
		output.truncated = true
	}
	if remaining := 64*1024 - output.Len(); remaining > 0 {
		_, _ = output.Buffer.Write(data[:min(remaining, length)])
	}
	return length, nil
}

type gitOutput struct {
	stdout, stderr []byte
	truncated      bool
}

func runGit(ctx context.Context, arguments, environment []string) (gitOutput, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Env = environment
	command.WaitDelay = time.Second
	var stdout, stderr cappedOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return gitOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes(), truncated: stdout.truncated || stderr.truncated}, err
}

// checkRepositoryConfig reads keys through git without following includes.
// Values are excluded because they can contain credentials and embedded newlines.
func checkRepositoryConfig(ctx context.Context, checkout string) error {
	temporary, err := os.MkdirTemp("", "agentum-config-")
	if err != nil {
		return refuse(ReasonProviderError)
	}
	defer os.RemoveAll(temporary)
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temporary, "GIT_CONFIG_NOSYSTEM=1"}
	// The effective list also covers config.worktree, which may be absent.
	// HOME is empty and system configuration is disabled for both reads.
	for _, localOnly := range []bool{true, false} {
		arguments := []string{"-C", checkout, "config", "--list", "--name-only", "--null", "--no-includes"}
		if localOnly {
			arguments = append(arguments, "--local")
		}
		output, err := runGit(ctx, arguments, environment)
		if err != nil || output.truncated {
			return refuse(ReasonProviderError)
		}
		for _, key := range strings.Split(string(output.stdout), "\x00") {
			if diagnostic := unsafeConfigKey(key); diagnostic != "" {
				code, message := SafeRefusal(ReasonUnsafeGitConfig)
				return &Refusal{Code: code, Message: message + ": " + diagnostic, configKey: diagnostic}
			}
		}
	}
	return nil
}

func unsafeConfigKey(key string) string {
	key = strings.ToLower(key)
	switch key {
	case "core.sshcommand", "core.hookspath", "core.fsmonitor", "extensions.partialclone":
		return key
	}
	section, remainder, found := strings.Cut(key, ".")
	if !found {
		return ""
	}
	suffix := remainder[strings.LastIndex(remainder, ".")+1:]
	// Subsections can be URLs containing credentials. Report their key pattern only.
	switch section {
	case "include":
		return "include." + safeConfigSuffix(suffix)
	case "includeif":
		return "includeif.<condition>." + safeConfigSuffix(suffix)
	case "url":
		return "url.<base>." + safeConfigSuffix(suffix)
	case "http":
		if strings.Contains(remainder, ".") {
			return "http.<scope>." + safeConfigSuffix(suffix)
		}
		return "http." + safeConfigSuffix(suffix)
	case "credential":
		if strings.Contains(remainder, ".") {
			return "credential.<scope>." + safeConfigSuffix(suffix)
		}
	case "remote":
		if suffix == "promisor" || suffix == "partialclonefilter" {
			return "remote.<name>." + suffix
		}
		// A URL-shaped remote name can replace the explicit URL passed to git push.
		if strings.Contains(remainder, ":") {
			return "remote.<url>." + safeConfigSuffix(suffix)
		}
	}
	return ""
}

func safeConfigSuffix(suffix string) string {
	if len(suffix) == 0 || len(suffix) > 128 {
		return "<key>"
	}
	for _, character := range suffix {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return "<key>"
		}
	}
	return suffix
}
