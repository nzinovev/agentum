package publish

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

// CheckoutValidator checks local publication prerequisites before network access.
type CheckoutValidator interface {
	ValidateCheckout(context.Context, string) error
}

// TargetResolver resolves the base branch before the coordinator freezes a target.
// Providers without a remote destination, such as noop, need no resolver.
type TargetResolver interface {
	ResolveTarget(context.Context, Target, string, string) (Target, error)
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var remoteHost = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::[0-9]+)?$`)
var commitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func refuse(code ReasonCode) error {
	code, message := SafeRefusal(code)
	return &Refusal{Code: code, Message: message}
}

// ParseRemote accepts HTTPS and Git's scp-style SSH repository URLs.
// Credentials and URL suffixes are refused so the frozen destination contains no secret.
func ParseRemote(raw string) (Target, error) {
	var host, path string
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)
		if len(parts) != 2 {
			return Target{}, refuse(ReasonRemoteUnknown)
		}
		host, path = parts[0], parts[1]
	} else {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
			return Target{}, refuse(ReasonRemoteUnknown)
		}
		host, path = parsed.Host, strings.TrimPrefix(parsed.Path, "/")
	}
	parts := strings.Split(strings.TrimSuffix(path, ".git"), "/")
	if !remoteHost.MatchString(host) || len(parts) != 2 || !validRepositoryPart(parts[0]) || !validRepositoryPart(parts[1]) {
		return Target{}, refuse(ReasonRemoteUnknown)
	}
	return Target{Host: strings.ToLower(host), Owner: parts[0], Repository: parts[1]}, nil
}

func validRepositoryPart(value string) bool {
	return repositoryPart.MatchString(value) && value != "." && value != ".."
}

// BaseBranchFromRef names the publication target branch a run's base_ref
// points at, for the given remote: `main`, `refs/heads/main`,
// `refs/remotes/<remote>/main`, and `<remote>/main` all name `main`. HEAD, a
// commit SHA, a tag, and another remote's tracking ref name no target branch
// (false). The run-start ancestry check and the publisher's destination
// resolution share this rule, so a base_ref cannot pass one and resolve to a
// different branch in the other. `<remote>/main` is read as the tracking ref,
// as git does unless a local branch carries that literal name.
func BaseBranchFromRef(baseRef, remote string) (string, bool) {
	branch := strings.TrimSpace(baseRef)
	if branch == "HEAD" || commitSHA.MatchString(branch) {
		return "", false
	}
	switch {
	case strings.HasPrefix(branch, "refs/heads/"):
		branch = strings.TrimPrefix(branch, "refs/heads/")
	case strings.HasPrefix(branch, "refs/remotes/"+remote+"/"):
		branch = strings.TrimPrefix(branch, "refs/remotes/"+remote+"/")
	case strings.HasPrefix(branch, "refs/"):
		return "", false
	case strings.HasPrefix(branch, remote+"/"):
		branch = strings.TrimPrefix(branch, remote+"/")
	}
	if branch == "HEAD" || !validBranch(branch) {
		return "", false
	}
	return branch, true
}

func validBranch(branch string) bool {
	if branch == "" || branch == "@" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " ~^:?*[\\\x00\r\n\t\x7f") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return false
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, character := range branch {
		if character < 32 {
			return false
		}
	}
	return true
}

func targetURL(target Target) string {
	return "https://" + target.Host + "/" + target.Owner + "/" + target.Repository + ".git"
}

// ValidateBaseBranch accepts an empty override or a valid branch name.
func ValidateBaseBranch(branch string) error {
	if branch != "" && !validBranch(branch) {
		return refuse(ReasonBaseBranchUnknown)
	}
	return nil
}

// ValidateRemoteName rejects empty names, options, and invalid ref components.
func ValidateRemoteName(remote string) error {
	if !validBranch(remote) {
		return refuse(ReasonRemoteUnknown)
	}
	return nil
}
