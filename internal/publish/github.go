package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	ProviderGitHub     ProviderID = "github"
	DefaultAPIBase                = "https://api.github.com"
	publicationTimeout            = 2 * time.Minute
)

type githubOperation int

const (
	opRepository githubOperation = iota
	opBranch
	opListPullRequests
	opCreatePullRequest
	opUpdatePullRequest
)

type operationSpec struct{ method, suffix string }

var githubOperations = map[githubOperation]operationSpec{
	opRepository:        {http.MethodGet, ""},
	opBranch:            {http.MethodGet, "/branches/%s"},
	opListPullRequests:  {http.MethodGet, "/pulls"},
	opCreatePullRequest: {http.MethodPost, "/pulls"},
	opUpdatePullRequest: {http.MethodPatch, "/pulls/%s"},
}

type githubPublisher struct {
	credential    Credential
	probeTarget   Target
	apiBase       string
	client        *http.Client
	push          func(context.Context, Delivery, string) error
	checkCheckout func(context.Context, string) error
	// compareAncestry answers whether ancestor is reachable from descendant
	// in the run's checkout. Seam-injected so tests script the git outcome.
	compareAncestry func(ctx context.Context, checkout, ancestor, descendant string) (verified bool, unverifiable bool, err error)
}

func newGitHubPublisher(options RegistryOptions) *githubPublisher {
	apiBase := options.APIBase
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	return &githubPublisher{
		credential: options.Credential, probeTarget: options.ProbeTarget, apiBase: strings.TrimRight(apiBase, "/"),
		client:          &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		push:            gitPush,
		checkCheckout:   checkRepositoryConfig,
		compareAncestry: checkoutAncestry,
	}
}

func (publisher *githubPublisher) ValidateCheckout(ctx context.Context, checkout string) error {
	return publisher.checkCheckout(ctx, checkout)
}

func (publisher *githubPublisher) ID() ProviderID { return ProviderGitHub }
func (publisher *githubPublisher) Describe() Descriptor {
	return Descriptor{ID: publisher.ID(), ProviderVersion: "v1", APIBase: publisher.apiBase,
		Capabilities: []string{CapabilityDraftPullRequests, CapabilityUpdatePullRequests}}
}

// Probe reads the configured repository without testing write permissions.
// A process-wide registry has no target; project readiness supplies ProbeTarget.
func (publisher *githubPublisher) Probe(ctx context.Context) (ProbeResult, error) {
	token, err := publisher.token(ctx)
	if err != nil {
		return ProbeResult{Reason: string(ReasonCredentialsMissing)}, err
	}
	if publisher.probeTarget.Host == "" {
		return ProbeResult{Reason: "repository target required for a publication readiness probe"}, nil
	}
	if err := publisher.validateTarget(publisher.probeTarget); err != nil {
		return ProbeResult{Reason: string(ReasonRemoteUnknown)}, err
	}
	status, response, err := publisher.request(ctx, opRepository, publisher.probeTarget, "", nil, nil, token)
	if err == nil && status != http.StatusOK {
		err = responseRefusal(status, response)
	}
	if err != nil {
		code, _ := Classify(err)
		return ProbeResult{Reason: string(code)}, err
	}
	return ProbeResult{Ready: true}, nil
}

// ValidateAPIBase refuses credential-bearing or non-HTTPS API configuration.
func ValidateAPIBase(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !remoteHost.MatchString(parsed.Host) || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return refuse(ReasonRemoteUnknown)
	}
	for _, component := range strings.Split(parsed.Path, "/") {
		if component == "." || component == ".." {
			return refuse(ReasonRemoteUnknown)
		}
	}
	return nil
}

func (publisher *githubPublisher) validateTarget(target Target) error {
	parsed, err := url.Parse(publisher.apiBase)
	if err != nil || parsed.User != nil {
		return refuse(ReasonRemoteUnknown)
	}
	expectedHost := parsed.Host
	if strings.EqualFold(expectedHost, "api.github.com") {
		expectedHost = "github.com"
	}
	if !strings.EqualFold(target.Host, expectedHost) || !validRepositoryPart(target.Owner) || !validRepositoryPart(target.Repository) {
		return refuse(ReasonRemoteUnknown)
	}
	return nil
}

func (publisher *githubPublisher) token(ctx context.Context) (string, error) {
	if publisher.credential == nil {
		return "", refuse(ReasonCredentialsMissing)
	}
	token, err := publisher.credential.Secret(ctx)
	if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", refuse(ReasonCredentialsMissing)
	}
	return token, nil
}

func (publisher *githubPublisher) ResolveTarget(ctx context.Context, target Target, baseRef, override string) (Target, error) {
	if err := publisher.validateTarget(target); err != nil {
		return Target{}, err
	}
	token, err := publisher.token(ctx)
	if err != nil {
		return Target{}, err
	}
	target.Provider = publisher.ID()
	if override != "" {
		if !validBranch(override) {
			return Target{}, refuse(ReasonBaseBranchUnknown)
		}
		target.BaseBranch = override
		return target, nil
	}
	candidate := strings.TrimPrefix(baseRef, "refs/heads/")
	if !strings.HasPrefix(candidate, "refs/") && !commitSHA.MatchString(candidate) && validBranch(candidate) {
		status, _, requestErr := publisher.request(ctx, opBranch, target, candidate, nil, nil, token)
		if requestErr != nil {
			return Target{}, requestErr
		}
		if status == http.StatusOK {
			target.BaseBranch = candidate
			return target, nil
		}
		if status == http.StatusNotFound {
			// A base_ref that names a branch is the target the run verified
			// its base against at start. The default branch may well contain
			// base_commit too, so falling back to it would pass the ancestry
			// check and open the pull request against a branch nobody chose.
			return Target{}, refuse(ReasonBaseBranchUnknown)
		}
		return Target{}, responseRefusal(status, nil)
	}
	status, response, err := publisher.request(ctx, opRepository, target, "", nil, nil, token)
	if err != nil {
		return Target{}, err
	}
	if status != http.StatusOK {
		return Target{}, responseRefusal(status, response)
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if json.Unmarshal(response, &repository) != nil || !validBranch(repository.DefaultBranch) {
		return Target{}, refuse(ReasonBaseBranchUnknown)
	}
	target.BaseBranch = repository.DefaultBranch
	return target, nil
}

type githubPullRequest struct {
	Number   int     `json:"number"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Draft    bool    `json:"draft"`
	Head     struct {
		Ref   string `json:"ref"`
		Label string `json:"label"`
	} `json:"head"`
}

func (publisher *githubPublisher) Publish(ctx context.Context, delivery Delivery) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, publicationTimeout)
	defer cancel()
	result := Result{}
	if !delivery.Checks.MandatoryPassed {
		return result, refuse(ReasonChecksNotPassed)
	}
	if !commitSHA.MatchString(delivery.ResultCommit) || delivery.Checks.Commit != delivery.ResultCommit {
		return result, refuse(ReasonCommitMismatch)
	}
	if err := publisher.validateTarget(delivery.Target); err != nil {
		return result, err
	}
	if !validBranch(delivery.Target.BaseBranch) || !validBranch(delivery.Target.RemoteBranch) || delivery.Target.BaseBranch == delivery.Target.RemoteBranch {
		return result, refuse(ReasonBaseBranchUnknown)
	}
	if err := delivery.Description.Validate(); err != nil {
		return result, err
	}
	if err := publisher.ValidateCheckout(ctx, delivery.Project.CheckoutPath); err != nil {
		return result, err
	}
	token, err := publisher.token(ctx)
	if err != nil {
		return result, err
	}
	// Re-verify the base against the provider's CURRENT branch head before
	// anything leaves the host: the run pinned its base at start, and the
	// target branch may have moved since. A pull request whose result does
	// not contain the branch head would carry foreign commits or reverse the
	// branch's newer work — a diagnostic refusal, never a published PR.
	if err := publisher.verifyBaseCurrent(ctx, delivery, token); err != nil {
		return result, err
	}
	if err := publisher.push(ctx, delivery, token); err != nil {
		return result, err
	}
	result.BranchPushed = true
	existing, found, err := publisher.findPullRequest(ctx, delivery, token)
	if err != nil {
		return result, err
	}
	if found {
		return publisher.updatePullRequest(ctx, delivery, existing, result, token)
	}
	if delivery.PullRequest != 0 {
		return result, refuse(ReasonPullRequestNotFound)
	}
	payload := map[string]any{"title": delivery.Request.Title, "body": delivery.Description.Text, "head": delivery.Target.RemoteBranch, "base": delivery.Target.BaseBranch, "draft": true}
	status, response, err := publisher.request(ctx, opCreatePullRequest, delivery.Target, "", nil, payload, token)
	if err != nil {
		return result, err
	}
	if status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(string(response)), "already exists") {
		existing, found, err = publisher.findPullRequest(ctx, delivery, token)
		if err != nil {
			return result, err
		}
		if found {
			return publisher.updatePullRequest(ctx, delivery, existing, result, token)
		}
	}
	if status != http.StatusCreated {
		return result, responseRefusal(status, response)
	}
	var created githubPullRequest
	if json.Unmarshal(response, &created) != nil || created.Number <= 0 {
		return result, refuse(ReasonProviderError)
	}
	result = pullRequestResult(delivery.Target, created, result)
	if created.State != "open" {
		return result, refuse(ReasonPullRequestClosed)
	}
	if !created.Draft {
		result.DraftRejected = true
		return result, refuse(ReasonDraftUnsupported)
	}
	return result, nil
}

// verifyBaseCurrent reads the target base branch's current head from the
// provider and verifies the run's base_commit is part of that branch's
// history. That is the condition under which the pull request carries only
// the run's own commits: everything the result holds beyond the branch head
// then lies in base_commit..result_commit. A base built on commits the branch
// does not have — a developer branch's unpushed work — is refused, while a
// branch that merely moved forward since the run started is not: the pull
// request shows the run's commits against the merge base as usual. The run
// and its result are untouched on refusal; the attempt is recorded and the
// diagnostic names the action.
func (publisher *githubPublisher) verifyBaseCurrent(ctx context.Context, delivery Delivery, token string) error {
	if !commitSHA.MatchString(delivery.BaseCommit) {
		return refuse(ReasonBaseUnverifiable)
	}
	status, response, requestErr := publisher.request(ctx, opBranch, delivery.Target, delivery.Target.BaseBranch, nil, nil, token)
	if requestErr != nil {
		return requestErr
	}
	if status == http.StatusNotFound {
		return refuse(ReasonBaseBranchUnknown)
	}
	if status != http.StatusOK {
		return responseRefusal(status, response)
	}
	var branch struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if json.Unmarshal(response, &branch) != nil || !commitSHA.MatchString(branch.Commit.SHA) {
		return refuse(ReasonBaseBranchUnknown)
	}
	verified, unverifiable, ancestryErr := publisher.compareAncestry(ctx, delivery.Project.CheckoutPath, delivery.BaseCommit, branch.Commit.SHA)
	if ancestryErr != nil || unverifiable {
		return refuse(ReasonBaseUnverifiable)
	}
	if !verified {
		return refuse(ReasonBaseDiverged)
	}
	return nil
}

// checkoutAncestry answers, from the run's pinned checkout, whether ancestor
// is reachable from descendant. unverifiable=true marks the case where the
// comparison could not RUN (the ancestor object was never fetched locally) —
// a fetch and a retry clear it, unlike a disproven ancestry.
func checkoutAncestry(ctx context.Context, checkout, ancestor, descendant string) (verified bool, unverifiable bool, err error) {
	command := exec.CommandContext(ctx, "git", "-C", checkout, "merge-base", "--is-ancestor", ancestor, descendant)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
	output, runErr := command.CombinedOutput()
	if runErr == nil {
		return true, false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
		// A clean "no": both objects resolved, ancestry disproven.
		return false, false, nil
	}
	// Any other failure — most commonly the branch head being an object this
	// checkout never fetched. Not a divergence verdict; not a silent pass.
	return false, true, fmt.Errorf("git merge-base --is-ancestor %s %s: %v (%s)",
		ancestor, descendant, runErr, strings.TrimSpace(string(output)))
}

func (publisher *githubPublisher) findPullRequest(ctx context.Context, delivery Delivery, token string) (githubPullRequest, bool, error) {
	// Include closed requests so a crash before persisting their number cannot create a replacement.
	for page := 1; page <= 100; page++ {
		query := url.Values{"head": {delivery.Target.Owner + ":" + delivery.Target.RemoteBranch}, "state": {"all"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		status, response, err := publisher.request(ctx, opListPullRequests, delivery.Target, "", query, nil, token)
		if err != nil {
			return githubPullRequest{}, false, err
		}
		if status != http.StatusOK {
			return githubPullRequest{}, false, responseRefusal(status, response)
		}
		var requests []githubPullRequest
		if json.Unmarshal(response, &requests) != nil {
			return githubPullRequest{}, false, refuse(ReasonProviderError)
		}
		for _, request := range requests {
			if request.Number <= 0 || request.Head.Ref != delivery.Target.RemoteBranch || !strings.EqualFold(request.Head.Label, delivery.Target.Owner+":"+delivery.Target.RemoteBranch) {
				continue
			}
			if delivery.PullRequest == 0 || request.Number == delivery.PullRequest {
				return request, true, nil
			}
		}
		if len(requests) < 100 {
			return githubPullRequest{}, false, nil
		}
	}
	return githubPullRequest{}, false, refuse(ReasonProviderError)
}

func (publisher *githubPublisher) updatePullRequest(ctx context.Context, delivery Delivery, existing githubPullRequest, result Result, token string) (Result, error) {
	result = pullRequestResult(delivery.Target, existing, result)
	if existing.State != "open" || existing.MergedAt != nil {
		return result, refuse(ReasonPullRequestClosed)
	}
	if delivery.DraftRejected && !existing.Draft {
		result.DraftRejected = true
		return result, refuse(ReasonDraftUnsupported)
	}
	payload := map[string]string{"title": delivery.Request.Title, "body": delivery.Description.Text}
	status, response, err := publisher.request(ctx, opUpdatePullRequest, delivery.Target, strconv.Itoa(existing.Number), nil, payload, token)
	if err != nil {
		return result, err
	}
	if status == http.StatusNotFound {
		return result, refuse(ReasonPullRequestNotFound)
	}
	if status != http.StatusOK {
		return result, responseRefusal(status, response)
	}
	var updated githubPullRequest
	if json.Unmarshal(response, &updated) != nil || updated.Number != existing.Number {
		return result, refuse(ReasonProviderError)
	}
	result = pullRequestResult(delivery.Target, updated, result)
	if updated.State != "open" || updated.MergedAt != nil {
		return result, refuse(ReasonPullRequestClosed)
	}
	return result, nil
}

func pullRequestResult(target Target, request githubPullRequest, result Result) Result {
	result.PullRequest = request.Number
	// Construct the page from the trusted target; provider response URLs can contain credentials.
	result.PullRequestURL = "https://" + target.Host + "/" + target.Owner + "/" + target.Repository + "/pull/" + strconv.Itoa(request.Number)
	result.PullRequestState = request.State
	if request.MergedAt != nil {
		result.PullRequestState = "merged"
	}
	return result
}

func (publisher *githubPublisher) request(ctx context.Context, operation githubOperation, target Target, argument string, query url.Values, payload any, token string) (int, []byte, error) {
	spec, exists := githubOperations[operation]
	if !exists {
		return 0, nil, refuse(ReasonProviderError)
	}
	suffix := spec.suffix
	if strings.Contains(suffix, "%s") {
		suffix = fmt.Sprintf(suffix, url.PathEscape(argument))
	}
	endpoint := publisher.apiBase + "/repos/" + url.PathEscape(target.Owner) + "/" + url.PathEscape(target.Repository) + suffix
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return 0, nil, refuse(ReasonProviderError)
		}
	}
	request, err := http.NewRequestWithContext(ctx, spec.method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, refuse(ReasonProviderError)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := publisher.client.Do(request)
	if err != nil {
		return 0, nil, refuse(ReasonNetworkUnreachable)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return 0, nil, refuse(ReasonProviderError)
	}
	if (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests) &&
		(response.StatusCode == http.StatusTooManyRequests || response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0" || strings.Contains(strings.ToLower(string(data)), "rate limit")) {
		return response.StatusCode, nil, refuse(ReasonProviderRateLimited)
	}
	return response.StatusCode, data, nil
}

func responseRefusal(status int, body []byte) error {
	message := strings.ToLower(string(body))
	switch {
	case status == http.StatusNotFound:
		return refuse(ReasonRemoteUnknown)
	case (status == 422 || status == 403) && strings.Contains(message, "draft") && (strings.Contains(message, "not supported") || strings.Contains(message, "not available") || strings.Contains(message, "not enabled")):
		return refuse(ReasonDraftUnsupported)
	case status == 401 || status == 403:
		return refuse(ReasonCredentialsRejected)
	default:
		return refuse(ReasonProviderError)
	}
}
