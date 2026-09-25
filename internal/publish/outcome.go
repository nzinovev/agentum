package publish

import (
	"errors"
	"fmt"
)

// ReasonCode is why a publication attempt did not complete. The vocabulary is
// closed because the run-history category "publication error" assigns each
// reason its own text and its own next action; a free-form string could not
// carry that. The provider's own message is recorded beside the code, never
// instead of it.
type ReasonCode string

const (
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
	ReasonSecretInDescription,
	ReasonProviderUnknown,
	ReasonProviderError,
}

// Retryable reports whether a repeated attempt can clear this reason. A
// missing or rejected credential, an unreachable network, and an unnamed
// provider failure are states outside the delivery that a retry re-tests.
// Every other reason names a fact about the delivery or the remote that a
// retry without an external change reproduces, so the publication is blocked
// and the reason names the action required.
func (code ReasonCode) Retryable() bool {
	switch code {
	case ReasonCredentialsMissing,
		ReasonCredentialsRejected,
		ReasonNetworkUnreachable,
		ReasonProviderError:
		return true
	default:
		return false
	}
}

// Refusal is a publication attempt that did not complete, carrying its reason
// from the closed vocabulary plus the provider's message. The message is
// stored beside the code for diagnosis; only the code is a stable identifier.
type Refusal struct {
	Code    ReasonCode
	Message string
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
	// BranchPushed is true when the result commit reached the remote branch.
	BranchPushed bool
	// PullRequest is the number of the created or updated draft pull
	// request. Zero when the attempt stopped before the pull request.
	PullRequest int
	// PullRequestURL is the provider's page for that pull request.
	PullRequestURL string
	// PullRequestState is the observed state of the pull request
	// ("open" on success; a closed or merged observation is a Refusal, not a
	// Result).
	PullRequestState string
}

// Classify maps a provider error onto the reason vocabulary. A *Refusal keeps
// its own code; anything else is an unnamed provider failure. The coordinator
// records the returned code and message on the publication row.
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
