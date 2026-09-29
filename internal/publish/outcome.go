package publish

import (
	"errors"
	"fmt"
)

// ReasonCode is why a publication attempt did not complete. The vocabulary is
// closed because the run-history category "publication error" assigns each
// reason its own text and its own next action; a free-form string could not
// carry that. Durable records use SafeRefusal to replace provider prose.
type ReasonCode string

const (
	// ReasonDescriptionInvalid refuses an absent or corrupted stored description.
	ReasonDescriptionInvalid ReasonCode = "description_invalid"
	// ReasonUnsafeGitConfig names a local setting that can redirect authenticated execution.
	ReasonUnsafeGitConfig ReasonCode = "unsafe_git_config"
	// ReasonProviderRateLimited records a temporary provider quota refusal.
	ReasonProviderRateLimited ReasonCode = "provider_rate_limited"
	// ReasonPullRequestNotFound preserves a recorded PR identity that the provider did not return.
	ReasonPullRequestNotFound ReasonCode = "pull_request_not_found"
	// ReasonLeaseBudgetExhausted stops network work before the publication lease expires.
	ReasonLeaseBudgetExhausted ReasonCode = "lease_budget_exhausted"
	// ReasonCredentialsMissing: no credential reached the provider. Retrying
	// after the configuration is fixed clears it.
	ReasonCredentialsMissing ReasonCode = "credentials_missing"
	// ReasonCredentialsRejected: the provider refused the credential.
	// Retrying with a valid token clears it.
	ReasonCredentialsRejected ReasonCode = "credentials_rejected"
	// ReasonNetworkUnreachable: the provider host could not be reached.
	ReasonNetworkUnreachable ReasonCode = "network_unreachable"
	// ReasonRemoteUnknown: the checkout names no usable remote, so the
	// destination cannot be derived.
	ReasonRemoteUnknown ReasonCode = "remote_unknown"
	// ReasonBaseBranchUnknown: no base branch could be derived for the pull
	// request.
	ReasonBaseBranchUnknown ReasonCode = "base_branch_unknown"
	// ReasonNonFastForward: the remote branch moved ahead of the local
	// history, so the push was not a fast-forward. Someone changed the
	// branch outside Agentum; a person decides what happens next.
	ReasonNonFastForward ReasonCode = "non_fast_forward"
	// ReasonPushRejected: the provider refused the push for a reason other
	// than a non-fast-forward (a protected branch, a refused permission).
	ReasonPushRejected ReasonCode = "push_rejected"
	// ReasonDraftUnsupported: the repository's plan does not allow draft
	// pull requests. A plain pull request is NOT opened instead — the task
	// names a draft, and an unavailable option is an error, not a downgrade.
	ReasonDraftUnsupported ReasonCode = "draft_unsupported"
	// ReasonPullRequestClosed: the pull request for this delivery was closed
	// or merged at the provider. Neither is reopened nor replaced.
	ReasonPullRequestClosed ReasonCode = "pull_request_closed"
	// ReasonChecksNotPassed: the delivery did not pass the mandatory project
	// checks, so publication is refused before any byte leaves the host.
	ReasonChecksNotPassed ReasonCode = "checks_not_passed"
	// ReasonCommitMismatch: the commit the checks verified is not the commit
	// pinned as the run's result, or no result commit is pinned at all.
	ReasonCommitMismatch ReasonCode = "commit_mismatch"
	// ReasonBaseDiverged: the run's base_commit is not part of the
	// publication target branch's history, so the pull request would carry
	// commits the branch does not have besides the run's own. A person
	// decides: publish the base's commits first, or start a new run from the
	// target branch. Retrying the same result reproduces the divergence.
	ReasonBaseDiverged ReasonCode = "base_diverged"
	// ReasonBaseUnverifiable: the target branch's head is known, but the
	// local checkout cannot compare it against the run's base_commit (the
	// head object was never fetched). A fetch in the checkout and a retry
	// clear it, so the attempt is re-runnable, not reconfigured.
	ReasonBaseUnverifiable ReasonCode = "base_unverifiable"
	// ReasonSecretInDescription: the rendered pull request body tripped the
	// secret scanner under the reject policy.
	ReasonSecretInDescription ReasonCode = "secret_in_description"
	// ReasonProviderUnknown: the provider id recorded on the publication row
	// resolves to no registry entry. A retry with the configuration unfixed
	// reproduces it — and the frozen row keeps the id — so the attempt is
	// blocked, and the action is a corrected configuration plus a new
	// publication, not a retry.
	ReasonProviderUnknown ReasonCode = "provider_unknown"
	// ReasonProviderError: a failure this vocabulary does not name, most often
	// an unnamed provider answer or a transient failure on Agentum's own side
	// (a manifest read that did not complete). Retryable: the attempt is
	// re-run, not reconfigured.
	ReasonProviderError ReasonCode = "provider_error"
)

// allReasonCodes is the vocabulary's roster, in declaration order. Retryable
// classifies through a switch, so a code added without a case lands in that
// switch's default and is silently treated as blocking — the
// misclassification a closed vocabulary exists to prevent. The partition test
// walks this slice and names the code no set claims, which turns "someone
// forgot the case" into a failing test instead of a wrong next action shown
// to a person.
var allReasonCodes = []ReasonCode{
	ReasonDescriptionInvalid,
	ReasonUnsafeGitConfig,
	ReasonProviderRateLimited,
	ReasonPullRequestNotFound,
	ReasonLeaseBudgetExhausted,
	ReasonCredentialsMissing,
	ReasonCredentialsRejected,
	ReasonNetworkUnreachable,
	ReasonRemoteUnknown,
	ReasonBaseBranchUnknown,
	ReasonNonFastForward,
	ReasonPushRejected,
	ReasonDraftUnsupported,
	ReasonPullRequestClosed,
	ReasonChecksNotPassed,
	ReasonCommitMismatch,
	ReasonBaseDiverged,
	ReasonBaseUnverifiable,
	ReasonSecretInDescription,
	ReasonProviderUnknown,
	ReasonProviderError,
}

// Retryable reports whether a repeated attempt can clear this reason. A
// missing or rejected credential, an unreachable network, a provider limit,
// an exhausted lease, and an unnamed failure can be re-tested by an explicit retry.
// Every other reason names a fact about the delivery or the remote that a
// retry without an external change reproduces, so the publication is blocked
// and the reason names the action required.
func (code ReasonCode) Retryable() bool {
	switch code {
	case ReasonDescriptionInvalid:
		return false
	case ReasonProviderRateLimited,
		ReasonLeaseBudgetExhausted,
		ReasonCredentialsMissing,
		ReasonCredentialsRejected,
		ReasonNetworkUnreachable,
		ReasonBaseUnverifiable,
		ReasonProviderError:
		return true
	default:
		return false
	}
}

// Refusal is a publication attempt that did not complete, carrying its reason
// from the closed vocabulary plus the provider's message. The message is
// available during the invocation; durable records use SafeError or SafeRefusal.
type Refusal struct {
	configKey string
	Code      ReasonCode
	Message   string
}

// Error renders the refusal with its code first, so a log line names the
// stable identifier before the provider's prose.
func (refusal *Refusal) Error() string {
	return fmt.Sprintf("publish: refused (%s): %s", refusal.Code, refusal.Message)
}

// Result is what a completed publication did. Pushing the branch and creating
// the pull request are separate outcomes, so both marks exist independently:
// an attempt can push the branch and still fail the pull request, and the
// row keeps the half that succeeded.
type Result struct {
	// DraftRejected records an explicit provider failure to create a draft.
	DraftRejected bool
	// BranchPushed is true when the result commit reached the remote branch.
	BranchPushed bool
	// PullRequest is the number of the created or updated draft pull
	// request. Zero when the attempt stopped before the pull request.
	PullRequest int
	// PullRequestURL is the provider's page for that pull request.
	PullRequestURL string
	// PullRequestState is the observed state of the pull request
	// ("open" on success). A closed or merged observation accompanies a Refusal
	// so the coordinator can retain that state.
	PullRequestState string
}

// Classify maps a provider error onto the reason vocabulary. A *Refusal keeps
// its own code; anything else is an unnamed provider failure. The coordinator
// normalizes the code through SafeRefusal before recording the outcome.
func Classify(err error) (ReasonCode, string) {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Code, refusal.Message
	}
	if err == nil {
		return ReasonProviderError, ""
	}
	return ReasonProviderError, err.Error()
}

// SafeRefusal returns a closed reason code and a credential-free diagnostic.
// Provider prose can contain arbitrary secrets, including unrecognizable tokens.
func SafeRefusal(code ReasonCode) (ReasonCode, string) {
	messages := map[ReasonCode]string{
		ReasonDescriptionInvalid:   "the stored pull request description is missing or fails its integrity check; repair the description before retrying",
		ReasonUnsafeGitConfig:      "a local git configuration key is unsafe for publication",
		ReasonProviderRateLimited:  "the provider rate limit was reached; wait for the limit to reset before retrying publication",
		ReasonPullRequestNotFound:  "the recorded pull request was not found; verify its repository and head branch before retrying",
		ReasonLeaseBudgetExhausted: "the publication lease has insufficient time remaining for a network attempt",
		ReasonCredentialsMissing:   "publication credentials are missing",
		ReasonCredentialsRejected:  "the provider rejected publication credentials",
		ReasonNetworkUnreachable:   "the publication provider could not be reached",
		ReasonRemoteUnknown:        "the publication repository is missing, inaccessible, or has no usable remote",
		ReasonBaseBranchUnknown:    "the pull request base branch could not be determined",
		ReasonNonFastForward:       "the remote branch has diverged",
		ReasonPushRejected:         "the provider rejected the branch push",
		ReasonDraftUnsupported:     "the repository does not support draft pull requests",
		ReasonPullRequestClosed:    "the pull request is closed or merged",
		ReasonChecksNotPassed:      "mandatory check evidence is missing or failed",
		ReasonCommitMismatch:       "the result commit is missing or differs from the checked commit",
		ReasonBaseDiverged:         "the run's base is not part of the publication target branch's history; the pull request would carry foreign commits",
		ReasonBaseUnverifiable:     "the publication target branch's head could not be compared locally; fetch the target branch and retry",
		ReasonSecretInDescription:  "the pull request description contains credential material",
		ReasonProviderUnknown:      "the configured publication provider is unknown",
		ReasonProviderError:        "the publication attempt could not be completed",
	}
	if message, exists := messages[code]; exists {
		return code, message
	}
	return ReasonProviderError, messages[ReasonProviderError]
}

// SafeError retains only structured diagnostics produced by this package.
// Provider prose and scoped config names may contain credentials.
func SafeError(err error) (ReasonCode, string) {
	code, _ := Classify(err)
	code, message := SafeRefusal(code)
	var refusal *Refusal
	if code == ReasonUnsafeGitConfig && errors.As(err, &refusal) && refusal.configKey != "" {
		message += ": " + refusal.configKey
	}
	return code, message
}
