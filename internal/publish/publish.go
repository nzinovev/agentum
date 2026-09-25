// Package publish is the provider contract for publishing a finished run:
// pushing the delivery branch and opening a draft pull request. The package
// holds the contract, the registry, and the provider implementations. It does
// not read the database, own worktrees, invoke agents, or write manifests —
// the coordinator in internal/publication assembles the input from durable
// rows and calls a Publisher through this contract, so a provider cannot
// reach the sources it publishes from.
//
// The contract has no merge. A pull request is merged by a person at the
// provider; the four methods below are the only operations the coordinator
// can ask for, and an operation absent from them is unreachable.
package publish

import (
	"context"
	"time"
)

// ProviderID is the stable identity of a publication provider. It is declared
// exactly once, next to the implementation it names, and the provider is
// named ONLY inside this package: callers select a provider by id through the
// registry and never construct a concrete type by name.
type ProviderID string

// Capability names a provider feature the coordinator may rely on. Declared
// as constants so a provider's descriptor and the code that reads it share
// one spelling.
const (
	// CapabilityDraftPullRequests: the provider can open a pull request in
	// draft state. A repository whose plan lacks the feature refuses the
	// creation, and the refusal is a terminal outcome of the attempt, not a
	// reason to open a non-draft pull request instead.
	CapabilityDraftPullRequests = "draft_pull_requests"
	// CapabilityUpdatePullRequests: the provider can change the title and
	// body of an existing pull request, which is what a repeated publication
	// does instead of creating a second pull request.
	CapabilityUpdatePullRequests = "update_pull_requests"
)

// Descriptor is a provider's self-description: everything knowable about it
// without running it. It comes from the provider itself (Describe) rather
// than sitting beside it in the registry table, so there is one record of the
// provider's identity, not two that can drift.
type Descriptor struct {
	// ID is the provider's stable identity.
	ID ProviderID
	// ProviderVersion versions THIS implementation of the provider — bumped
	// when its observable behaviour changes (git argv, HTTP operations,
	// environment handling). It is not the provider host's version.
	ProviderVersion string
	// APIBase is the host the provider talks to. Host and path only: never a
	// URL carrying the credential.
	APIBase string
	// Capabilities is the closed set of features the provider declares.
	Capabilities []string
}

// ProbeResult is the provider's answer to "are you reachable". Reachability
// is a fact about the provider, not a promise that a publication will
// succeed; write access is only observable by writing.
type ProbeResult struct {
	// Ready is true when the provider answered the probe.
	Ready bool
	// Reason names what failed when Ready is false.
	Reason string
}

// Credential is the provider's access secret. It is handed to the provider
// at construction and never travels with a Delivery: the value that appears
// in evidence is a host and a path, and the value that opens the network is
// reachable only through this interface. Declared as an interface so a
// secret store can replace the static token without touching the contract.
type Credential interface {
	// Secret returns the access token. Only a provider calls this.
	Secret(ctx context.Context) (string, error)
}

// StaticCredential is a Credential holding a literal token, sourced from the
// process environment today.
type StaticCredential string

// Secret returns the literal token.
func (token StaticCredential) Secret(context.Context) (string, error) {
	return string(token), nil
}

// Publisher is the provider contract. Publish receives the delivery as a
// value of scalars and hashes; it holds no database handle and no pointer
// into mutable state, so a provider cannot re-read a source and get a
// different answer than the coordinator assembled.
type Publisher interface {
	// ID returns the provider's stable identity.
	ID() ProviderID
	// Describe returns the provider's self-description.
	Describe() Descriptor
	// Probe asks the provider whether it is reachable. Readiness surfaces
	// call this; publication itself never does.
	Probe(ctx context.Context) (ProbeResult, error)
	// Publish pushes the delivery's result commit to the remote branch and
	// creates or updates the draft pull request. The returned error is nil, a
	// *Refusal with a reason from the closed vocabulary, or an unexpected
	// failure the coordinator records as provider_error.
	Publish(ctx context.Context, delivery Delivery) (Result, error)
}

// Delivery is the complete input to one publication attempt. Every field is a
// scalar or a hash: artifact BODIES are deliberately absent (their names,
// revision ids, and content hashes ride along instead), because the pull
// request body is rendered by the orchestrator from exactly these fields and
// a provider that never sees a body can never leak one. The credential is
// absent for the same reason — it reaches the provider at construction, not
// per call.
type Delivery struct {
	// Run identifies the run being published.
	Run RunRef
	// Project identifies the repository and the pinned local checkout the
	// push runs in.
	Project ProjectRef
	// Target names where the delivery goes: the provider, the repository,
	// and the two branches involved. Frozen in the publication row at first
	// derivation; a later move of the remote does not change it.
	Target Target
	// Branch is the local delivery branch (agentum/<run-id>). It names the
	// lineage in evidence; the push itself uses ResultCommit, so a branch
	// that moved after the gate cannot smuggle an unchecked tip out.
	Branch string
	// BaseCommit is the full SHA the run started from.
	BaseCommit string
	// ResultCommit is the full SHA pinned at the final gate — the commit the
	// checks verified and the only commit this delivery publishes.
	ResultCommit string
	// Request is the run's task request: title, description, and the
	// revision hash of the request. The description passed the prose scanner
	// at run creation.
	Request RequestRef
	// Checks is the sealed outcome of the mandatory project checks against
	// ResultCommit. Statuses and durations only — never check output.
	Checks ChecksSeal
	// Review is the reviewer's verdict and the number of completed fix
	// cycles, as far as the durable rows record them.
	Review ReviewRef
	// Plan names the approved plan artifact by revision, not by body.
	Plan PlanRef
	// Evidence summarizes the manifest: sealed or not, complete or not,
	// which sections are missing.
	Evidence EvidenceRef
}

// RunRef identifies the run behind a delivery.
type RunRef struct {
	ID            string
	TenantID      string
	CreatorUserID string
	// InputRevision is the canonical hash of the run request, from the
	// manifest's input section.
	InputRevision string
}

// ProjectRef identifies the repository and the working copy a push runs in.
type ProjectRef struct {
	ID string
	// RepoIdentity is the repository's fingerprint — stable across clones,
	// unlike the path.
	RepoIdentity string
	// CheckoutPath is the run's pinned local checkout. The two git commands
	// of a push run here.
	CheckoutPath string
}

// Target names the publication destination.
type Target struct {
	Provider     ProviderID
	Host         string
	Owner        string
	Repository   string
	BaseBranch   string
	RemoteBranch string
}

// RequestRef is the run's task request.
type RequestRef struct {
	Title       string
	Description string
	Revision    string
}

// ChecksSeal is the mandatory-check outcome bound to the published commit.
type ChecksSeal struct {
	// Ran is false when the project declares no checks. The delivery is
	// publishable in that state; the pull request body says so in a line of
	// its own instead of presenting an empty set as a passed gate.
	Ran              bool
	MandatoryPassed  bool
	Commit           string
	SetVersion       string
	RegistryRevision string
	// Checks carries one summary per check: name, obligatoriness, status,
	// duration. No stdout, no stderr.
	Checks []CheckSummary
}

// CheckSummary is one check's outcome in a ChecksSeal.
type CheckSummary struct {
	Name       string
	Required   bool
	Status     string
	DurationMs int64
}

// ReviewRef is the review outcome a delivery reports.
type ReviewRef struct {
	// Verdict is the reviewer's last verdict, empty when no verdict artifact
	// was recorded.
	Verdict string
	// FixCycles is the number of review ⇄ fix cycles the run completed.
	FixCycles int
}

// PlanRef names the approved plan artifact by revision.
type PlanRef struct {
	Name       string
	RevisionID string
	// ContentHash is the plan body's hash. The body itself stays in the
	// revisions store.
	ContentHash string
	ApprovedBy  string
	ApprovedAt  time.Time
}

// EvidenceRef summarizes the manifest state at publication time.
type EvidenceRef struct {
	Sealed   bool
	Complete bool
	Missing  []string
}
