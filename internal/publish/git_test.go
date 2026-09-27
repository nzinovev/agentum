package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func gitTestCommand(test *testing.T, arguments ...string) string {
	test.Helper()
	command := exec.Command("git", arguments...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Publisher test", "GIT_AUTHOR_EMAIL=publisher@example.invalid", "GIT_COMMITTER_NAME=Publisher test", "GIT_COMMITTER_EMAIL=publisher@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("git %v: %s: %v", arguments, output, err)
	}
	return strings.TrimSpace(string(output))
}

func gitDelivery(test *testing.T) Delivery {
	test.Helper()
	checkout := test.TempDir()
	gitTestCommand(test, "init", "-b", "main", checkout)
	gitTestCommand(test, "-C", checkout, "commit", "--allow-empty", "-m", "base")
	commit := gitTestCommand(test, "-C", checkout, "rev-parse", "HEAD")
	return Delivery{Project: ProjectRef{CheckoutPath: checkout}, ResultCommit: commit, Target: Target{Host: "github.com", Owner: "owner", Repository: "repo", RemoteBranch: "agentum/run-one"}}
}

func TestGitPushArgumentsAndEnvironment(test *testing.T) {
	delivery := gitDelivery(test)
	verify, push := gitArguments(delivery)
	if !reflect.DeepEqual(verify, []string{"-C", delivery.Project.CheckoutPath, "rev-parse", "--verify", delivery.ResultCommit + "^{commit}"}) {
		test.Fatalf("verify=%v", verify)
	}
	if !reflect.DeepEqual(push, []string{"-C", delivery.Project.CheckoutPath, "push", "--porcelain", "https://github.com/owner/repo.git", delivery.ResultCommit + ":refs/heads/agentum/run-one"}) {
		test.Fatalf("push=%v", push)
	}
	environment := gitEnvironment("/temporary", "/temporary/askpass", "credential-marker")
	for _, variable := range environment {
		key, _, _ := strings.Cut(variable, "=")
		switch key {
		case "PATH", "HOME", "GIT_ASKPASS", "GIT_TERMINAL_PROMPT", "GIT_CONFIG_NOSYSTEM", "AGENTUM_PUBLISH_TOKEN", "GIT_CONFIG_COUNT":
		default:
			if !strings.HasPrefix(key, "GIT_CONFIG_KEY_") && !strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
				test.Errorf("ambient variable %q", key)
			}
		}
	}
	if strings.Contains(strings.Join(append(verify, push...), " "), "credential-marker") {
		test.Fatal("credential in argv")
	}
}

func TestGitPushCleansAskpassOnAllOutcomes(test *testing.T) {
	for _, failure := range []bool{false, true} {
		test.Run(map[bool]string{false: "success", true: "failure"}[failure], func(test *testing.T) {
			delivery := gitDelivery(test)
			binaryDirectory := test.TempDir()
			marker := filepath.Join(binaryDirectory, "temporary-path")
			argvLog := filepath.Join(binaryDirectory, "arguments")
			script := "#!/bin/sh\nprintf '%s\\n' \"$HOME\" > '" + marker + "'\nprintf '%s\\n' \"$@\" >> '" + argvLog + "'\n[ \"$(stat -c '%a' \"$HOME\")\" = 700 ] || exit 91\n[ \"$(\"$GIT_ASKPASS\" Username)\" = x-access-token ] || exit 92\n[ \"$(\"$GIT_ASKPASS\" Password)\" = credential-marker ] || exit 93\n[ -z \"$GITHUB_TOKEN\" ] || exit 94\nif [ \"$3\" = rev-parse ]; then printf '%s\\n' '" + delivery.ResultCommit + "'; exit 0; fi\n"
			if failure {
				script += "echo 'non-fast-forward' >&2\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(binaryDirectory, "git"), []byte(script), 0700); err != nil {
				test.Fatal(err)
			}
			test.Setenv("PATH", binaryDirectory+":"+os.Getenv("PATH"))
			test.Setenv("GITHUB_TOKEN", "ambient-marker")
			err := gitPush(test.Context(), delivery, "credential-marker")
			if failure {
				if code, _ := Classify(err); code != ReasonNonFastForward {
					test.Fatalf("err=%v", err)
				}
			} else if err != nil {
				test.Fatal(err)
			}
			raw, readErr := os.ReadFile(marker)
			if readErr != nil {
				test.Fatal(readErr)
			}
			if _, err := os.Stat(strings.TrimSpace(string(raw))); !os.IsNotExist(err) {
				test.Fatalf("temporary directory remains: %v", err)
			}
			arguments, readErr := os.ReadFile(argvLog)
			if readErr != nil {
				test.Fatal(readErr)
			}
			if strings.Contains(string(arguments), "credential-marker") {
				test.Fatal("credential in process arguments")
			}
		})
	}
}

func TestGitPushBareRepositoryUsesPinnedCommitAndRefusesRewind(test *testing.T) {
	delivery := gitDelivery(test)
	bare := filepath.Join(test.TempDir(), "remote.git")
	gitTestCommand(test, "init", "--bare", bare)
	temporary := test.TempDir()
	environment := gitEnvironment(temporary, filepath.Join(temporary, "absent-askpass"), "test-token")
	// The test enables file transport only for its local bare repository.
	for _, variable := range environment {
		if countText, found := strings.CutPrefix(variable, "GIT_CONFIG_COUNT="); found {
			count, err := strconv.Atoi(countText)
			if err != nil {
				test.Fatal(err)
			}
			environment = append(environment, "GIT_CONFIG_COUNT="+strconv.Itoa(count+1), "GIT_CONFIG_KEY_"+countText+"=protocol.file.allow", "GIT_CONFIG_VALUE_"+countText+"=always")
			break
		}
	}
	_, push := gitArguments(delivery)
	push[4] = bare
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "commit", "--allow-empty", "-m", "unchecked tip")
	for range 2 {
		if output, err := runGit(test.Context(), push, environment); err != nil {
			test.Fatalf("push: %s %v", output.stderr, err)
		}
	}
	if actual := gitTestCommand(test, "--git-dir", bare, "rev-parse", "refs/heads/agentum/run-one"); actual != delivery.ResultCommit {
		test.Fatalf("published tip %s instead of pinned %s", actual, delivery.ResultCommit)
	}
	tip := gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "rev-parse", "HEAD")
	push[5] = tip + ":refs/heads/agentum/run-one"
	if output, err := runGit(test.Context(), push, environment); err != nil {
		test.Fatalf("advance: %s %v", output.stderr, err)
	}
	push[5] = delivery.ResultCommit + ":refs/heads/agentum/run-one"
	output, err := runGit(test.Context(), push, environment)
	if err == nil || !strings.Contains(string(output.stdout)+string(output.stderr), "non-fast-forward") {
		test.Fatalf("rewind: %s %v", output.stderr, err)
	}
}

func TestGitRepositoryCannotRedirectCredentials(test *testing.T) {
	for _, configuration := range []string{
		"[url \"https://attacker.invalid/\"]\n insteadOf = https://github.com/\n",
		"[remote \"https://github.com/owner/repo.git\"]\n url = https://attacker.invalid/repo.git\n",
		"[http \"https://github.com\"]\n extraHeader = attacker\n",
		"[credential \"https://github.com\"]\n helper = !echo attacker\n",
		"[include]\n path = /tmp/attacker-config\n",
		"[includeIf \"gitdir:**\"]\n path = /tmp/attacker-config\n",
	} {
		test.Run(strings.Split(configuration, "\n")[0], func(test *testing.T) {
			delivery := gitDelivery(test)
			configFile := filepath.Join(delivery.Project.CheckoutPath, ".git", "config")
			file, err := os.OpenFile(configFile, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := file.WriteString(configuration); err != nil {
				test.Fatal(err)
			}
			if err := file.Close(); err != nil {
				test.Fatal(err)
			}
			if err := checkRepositoryConfig(test.Context(), delivery.Project.CheckoutPath); err == nil {
				test.Fatal("unsafe config accepted")
			}
		})
	}
}

func TestGitDisablesLocalHooksAndHelpers(test *testing.T) {
	delivery := gitDelivery(test)
	marker := filepath.Join(test.TempDir(), "executed")
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "credential.helper", "!touch "+marker)
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "core.hooksPath", test.TempDir())
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "push.followTags", "true")
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "push.pushOption", "local-option")
	environment := gitEnvironment(test.TempDir(), "/unused", "test-token")
	for key, want := range map[string]string{"credential.helper": "", "core.fsmonitor": "false", "http.followRedirects": "false", "protocol.allow": "never", "push.followTags": "false", "push.autoSetupRemote": "false", "push.pushOption": ""} {
		output, err := runGit(context.Background(), []string{"-C", delivery.Project.CheckoutPath, "config", "--get", key}, environment)
		if err != nil || strings.TrimSpace(string(output.stdout)+string(output.stderr)) != want {
			test.Fatalf("%s=%q err=%v", key, output.stdout, err)
		}
	}
	if code, _ := Classify(checkRepositoryConfig(test.Context(), delivery.Project.CheckoutPath)); code != ReasonUnsafeGitConfig {
		test.Fatalf("hooks config code=%s", code)
	}
}

func TestGitConfigAllowsOrdinaryKeysInLinkedWorktree(test *testing.T) {
	delivery := gitDelivery(test)
	for key, value := range map[string]string{
		"notes.rewriteRef": "refs/notes/review", "notes.displayRef": "refs/notes/*",
		"pull.rebase": "true", "submodule.library.url": "https://example.invalid/library.git",
		"maintenance.auto": "false", "filter.lfs.smudge": "git-lfs smudge -- %f",
		"alias.multiline": "status\nhttp.extraheader=not-a-key", "credential.helper": "cache",
	} {
		gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", key, value)
	}
	linked := filepath.Join(test.TempDir(), "linked")
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "worktree", "add", "-b", "linked", linked)
	if err := checkRepositoryConfig(test.Context(), linked); err != nil {
		test.Fatalf("ordinary worktree refused: %v", err)
	}
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "extensions.worktreeConfig", "true")
	if err := checkRepositoryConfig(test.Context(), linked); err != nil {
		test.Fatalf("absent optional config refused: %v", err)
	}
	gitTestCommand(test, "-C", linked, "config", "--worktree", "http.extraHeader", "must-not-be-recorded")
	code, message := SafeError(checkRepositoryConfig(test.Context(), linked))
	if code != ReasonUnsafeGitConfig || !strings.Contains(message, "http.extraheader") || strings.Contains(message, "must-not-be-recorded") {
		test.Fatalf("worktree config: %s %s", code, message)
	}
}

func TestGitConfigRejectsOnlyUnsafeKeys(test *testing.T) {
	for _, key := range []string{
		"include.path", "includeIf.gitdir:/tmp/.path", "url.https://example.invalid/.insteadOf", "http.extraHeader",
		"credential.https://example.invalid.helper", "remote.origin.promisor", "remote.origin.partialCloneFilter",
		"core.sshCommand", "core.hooksPath", "core.fsmonitor",
	} {
		test.Run(key, func(test *testing.T) {
			delivery := gitDelivery(test)
			gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", key, "sensitive-value")
			code, message := SafeError(checkRepositoryConfig(test.Context(), delivery.Project.CheckoutPath))
			if code != ReasonUnsafeGitConfig || strings.Contains(message, "sensitive-value") {
				test.Fatalf("code=%s message=%s", code, message)
			}
			if strings.HasPrefix(key, "core.") && !strings.Contains(message, strings.ToLower(key)) {
				test.Fatalf("missing key: %s", message)
			}
		})
	}
}

func TestGitConfigIOFailuresAreRetryable(test *testing.T) {
	delivery := gitDelivery(test)
	for _, scenario := range []string{"missing-checkout", "malformed-config", "missing-git"} {
		test.Run(scenario, func(test *testing.T) {
			checkout := delivery.Project.CheckoutPath
			switch scenario {
			case "missing-checkout":
				checkout = filepath.Join(test.TempDir(), "absent")
			case "malformed-config":
				checkout = test.TempDir()
				gitTestCommand(test, "init", checkout)
				if err := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[broken"), 0600); err != nil {
					test.Fatal(err)
				}
			case "missing-git":
				test.Setenv("PATH", test.TempDir())
			}
			code, _ := SafeError(checkRepositoryConfig(test.Context(), checkout))
			if code != ReasonProviderError || !code.Retryable() {
				test.Fatalf("code=%s", code)
			}
		})
	}
}

func TestGitVerifySeparatesWarningsFromFailures(test *testing.T) {
	for _, scenario := range []struct {
		name, script string
		want         ReasonCode
	}{
		{"warning", "printf '%s\\n' warning >&2\nprintf '%s\\n' CHECKED\n", ""},
		{"wrong-commit", "printf '%040d\\n' 0\n", ReasonCommitMismatch},
		{"ownership", "echo 'fatal: detected dubious ownership' >&2\nexit 128\n", ReasonProviderError},
		{"io-failure", "echo 'fatal: Input/output error' >&2\nexit 128\n", ReasonProviderError},
		{"missing-git", "", ReasonProviderError},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			delivery := gitDelivery(test)
			binaryDirectory := test.TempDir()
			if scenario.name != "missing-git" {
				script := "#!/bin/sh\nif [ \"$3\" = rev-parse ]; then\n" + strings.ReplaceAll(scenario.script, "CHECKED", delivery.ResultCommit) + "fi\n"
				if err := os.WriteFile(filepath.Join(binaryDirectory, "git"), []byte(script), 0700); err != nil {
					test.Fatal(err)
				}
			}
			test.Setenv("PATH", binaryDirectory)
			err := gitPush(test.Context(), delivery, "test-token")
			if scenario.want == "" {
				if err != nil {
					test.Fatal(err)
				}
				return
			}
			if code, _ := Classify(err); code != scenario.want {
				test.Fatalf("err=%v want=%s", err, scenario.want)
			}
		})
	}
}

func TestUnsafeConfigDiagnosticOmitsScopedCredentials(test *testing.T) {
	delivery := gitDelivery(test)
	gitTestCommand(test, "-C", delivery.Project.CheckoutPath, "config", "http.https://credential-marker@example.invalid.extraHeader", "value-marker")
	code, message := SafeError(checkRepositoryConfig(test.Context(), delivery.Project.CheckoutPath))
	if code != ReasonUnsafeGitConfig || !strings.Contains(message, "http.<scope>.extraheader") || strings.Contains(message, "credential-marker") || strings.Contains(message, "value-marker") {
		test.Fatalf("code=%s message=%s", code, message)
	}
}
