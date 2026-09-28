package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
)

func githubFixture(test *testing.T, handler http.HandlerFunc) (*githubPublisher, Delivery) {
	return githubFixtureWithBranch(test, handler, strings.Repeat("a", 40))
}

// githubFixtureWithBranch is githubFixture with the base-branch head the
// publisher's pre-push lookup reports; the default is the delivery's own
// result commit, which short-circuits the ancestry comparison.
func githubFixtureWithBranch(test *testing.T, handler http.HandlerFunc, branchHead string) (*githubPublisher, Delivery) {
	test.Helper()
	server := httptest.NewTLSServer(routeBaseBranchAnswers(handler, branchHead))
	test.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		test.Fatal(err)
	}
	publisher := newGitHubPublisher(RegistryOptions{Credential: StaticCredential("test-credential"), APIBase: server.URL})
	publisher.client = server.Client()
	publisher.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	publisher.checkCheckout = func(context.Context, string) error { return nil }
	publisher.push = func(context.Context, Delivery, string) error { return nil }
	delivery := Delivery{
		Run:        RunRef{ID: "run-one"},
		Target:     Target{Provider: ProviderGitHub, Host: parsed.Host, Owner: "owner", Repository: "repo", RemoteBranch: "agentum/run-one", BaseBranch: "main"},
		BaseCommit: strings.Repeat("b", 40), ResultCommit: strings.Repeat("a", 40), Request: RequestRef{Title: "Request title", Description: "Request description"},
		Checks: ChecksSeal{MandatoryPassed: true, Commit: strings.Repeat("a", 40)},
	}
	body, err := RenderDescription(delivery)
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(body)
	delivery.Description = DescriptionRef{Text: string(body), RevisionID: "stored-description", ContentHash: hex.EncodeToString(digest[:])}
	return publisher, delivery
}

// routeBaseBranchAnswers wraps a test handler so the base-branch head lookup
// the publisher performs before pushing answers with branchHead, without
// every scenario handler growing a branch route. Only the delivery's own
// base branch (main) is intercepted: ResolveTarget probes candidate branches
// through the same operation, and those must reach the scenario handler. A
// branchHead equal to the delivery's result commit short-circuits the
// ancestry comparison.
func routeBaseBranchAnswers(handler http.HandlerFunc, branchHead string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.EscapedPath() == "/repos/owner/repo/branches/main" {
			_, _ = w.Write([]byte(`{"commit":{"sha":"` + branchHead + `"}}`))
			return
		}
		handler(w, r)
	}
}

func writePullRequest(w http.ResponseWriter, state string, draft bool) {
	_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": state, "draft": draft, "head": map[string]string{"ref": "agentum/run-one", "label": "owner:agentum/run-one"}})
}

func TestGitHubPublishRecoveryAndUpdates(test *testing.T) {
	for _, scenario := range []string{"new", "existing", "recorded", "creation-race", "crash-before-record"} {
		test.Run(scenario, func(test *testing.T) {
			exists := scenario == "existing" || scenario == "recorded"
			creates, updates := 0, 0
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-credential" {
					test.Error("missing credential")
				}
				switch r.Method {
				case http.MethodGet:
					if r.URL.Query().Get("head") != "owner:agentum/run-one" || r.URL.Query().Get("state") != "all" {
						test.Errorf("query = %s", r.URL.RawQuery)
					}
					if !exists {
						_, _ = w.Write([]byte("[]"))
						return
					}
					_, _ = w.Write([]byte(`[{"number":42,"state":"open","draft":false,"head":{"ref":"agentum/run-one","label":"owner:agentum/run-one"}}]`))
				case http.MethodPost:
					creates++
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						test.Error(err)
					}
					if payload["draft"] != true || payload["base"] != "main" || payload["head"] != "agentum/run-one" {
						test.Errorf("creation payload = %v", payload)
					}
					body, _ := payload["body"].(string)
					for _, expected := range []string{"Request title", "Request description", "declares no checks", "complete:", "human performs the merge", deliveryRange()} {
						if !strings.Contains(body, expected) {
							test.Errorf("body missing %q", expected)
						}
					}
					exists = true
					if scenario == "creation-race" {
						w.WriteHeader(422)
						_, _ = w.Write([]byte(`{"message":"A pull request already exists"}`))
						return
					}
					w.WriteHeader(http.StatusCreated)
					writePullRequest(w, "open", true)
				case http.MethodPatch:
					updates++
					if r.URL.Path != "/repos/owner/repo/pulls/42" {
						test.Errorf("update path = %s", r.URL.Path)
					}
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						test.Error(err)
					}
					if len(payload) != 2 || payload["title"] != "Request title" || payload["body"] == nil {
						test.Errorf("update changes more than title/body: %v", payload)
					}
					writePullRequest(w, "open", false)
				default:
					test.Errorf("unexpected operation %s", r.Method)
					w.WriteHeader(500)
				}
			})
			if scenario == "recorded" {
				delivery.PullRequest = 42
			}
			result, err := publisher.Publish(test.Context(), delivery)
			if err != nil || result.PullRequest != 42 || !result.BranchPushed {
				test.Fatalf("result=%+v err=%v", result, err)
			}
			if strings.Contains(result.PullRequestURL, "test-credential") {
				test.Fatal("credential in URL")
			}
			if scenario == "crash-before-record" {
				// The retry receives no number, exactly as after an unrecorded remote success.
				result, err = publisher.Publish(test.Context(), delivery)
				if err != nil || result.PullRequest != 42 || creates != 1 || updates != 1 {
					test.Fatalf("retry=%+v err=%v creates=%d updates=%d", result, err, creates, updates)
				}
			}
			if creates > 1 {
				test.Fatal("duplicate creation")
			}
		})
	}
}

func deliveryRange() string { return strings.Repeat("b", 40) + ".." + strings.Repeat("a", 40) }

func TestGitHubRefusals(test *testing.T) {
	scenarios := []struct {
		name     string
		status   int
		response string
		want     ReasonCode
	}{
		{"credentials", 401, `{"message":"test-credential"}`, ReasonCredentialsRejected},
		{"server", 503, `{"message":"test-credential"}`, ReasonProviderError},
		{"draft", 422, `{"message":"Draft pull requests are not supported"}`, ReasonDraftUnsupported},
		{"closed", 200, `[{"number":42,"state":"closed","head":{"ref":"agentum/run-one","label":"owner:agentum/run-one"}}]`, ReasonPullRequestClosed},
		{"merged", 200, `[{"number":42,"state":"closed","merged_at":"2026-01-01","head":{"ref":"agentum/run-one","label":"owner:agentum/run-one"}}]`, ReasonPullRequestClosed},
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			writes := 0
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				if scenario.name == "draft" && r.Method == http.MethodGet {
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.response))
			})
			result, err := publisher.Publish(test.Context(), delivery)
			code, _ := Classify(err)
			if code != scenario.want || !result.BranchPushed {
				test.Fatalf("result=%+v err=%v want=%s", result, err, scenario.want)
			}
			if strings.Contains(err.Error(), "test-credential") {
				test.Fatal("unsafe error")
			}
			if scenario.want == ReasonPullRequestClosed && (writes != 0 || result.PullRequestState == "" || result.PullRequest != 42) {
				test.Fatalf("closed observation lost: %+v writes=%d", result, writes)
			}
		})
	}
}

func TestGitHubNetworkAndRedirectRefusals(test *testing.T) {
	publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/credential-sink", http.StatusTemporaryRedirect)
	})
	_, err := publisher.Publish(test.Context(), delivery)
	if code, _ := Classify(err); code != ReasonProviderError {
		test.Fatalf("redirect err=%v", err)
	}
	publisher.client.Transport = failingTransport{}
	_, err = publisher.Publish(test.Context(), delivery)
	if code, _ := Classify(err); code != ReasonNetworkUnreachable {
		test.Fatalf("network err=%v", err)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unreachable")
}

func TestGitHubTargetResolution(test *testing.T) {
	for _, scenario := range []struct {
		name, ref, override, want string
		branchStatus              int
	}{
		{"override", "feature", "release", "release", 404},
		{"branch", "feature", "", "feature", 200},
		{"slash", "release/1.2", "", "release/1.2", 200},
		{"qualified", "refs/heads/feature", "", "feature", 200},
		{"default", "missing", "", "main", 404},
		{"commit", strings.Repeat("a", 40), "", "main", 404},
		{"tag", "refs/tags/v1", "", "main", 404},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			calls := 0
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if scenario.name == "slash" && r.URL.EscapedPath() != "/repos/owner/repo/branches/release%2F1.2" {
					test.Errorf("branch URL = %s", r.URL.EscapedPath())
				}
				if strings.Contains(r.URL.Path, "/branches/") {
					w.WriteHeader(scenario.branchStatus)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			})
			target, err := publisher.ResolveTarget(test.Context(), delivery.Target, scenario.ref, scenario.override)
			if err != nil || target.BaseBranch != scenario.want {
				test.Fatalf("target=%+v err=%v", target, err)
			}
			if scenario.override != "" && calls != 0 {
				test.Fatal("override queried provider")
			}
		})
	}
}

func TestGitHubClosedOperationTable(test *testing.T) {
	if len(githubOperations) != 5 {
		test.Fatal("operation surface changed")
	}
	for operation, spec := range githubOperations {
		if spec.method == http.MethodPut || strings.Contains(spec.suffix, "merge") {
			test.Fatalf("merge operation %d: %+v", operation, spec)
		}
		if spec.method != http.MethodGet && operation != opCreatePullRequest && operation != opUpdatePullRequest {
			test.Fatalf("unexpected write operation %d", operation)
		}
	}
}

func TestGitHubRejectsUnboundDeliveryBeforePush(test *testing.T) {
	for _, scenario := range []string{"checks", "commit", "host", "branch"} {
		test.Run(scenario, func(test *testing.T) {
			publisher, delivery := githubFixture(test, func(http.ResponseWriter, *http.Request) { test.Error("HTTP called for invalid delivery") })
			publisher.push = func(context.Context, Delivery, string) error {
				test.Error("push called for invalid delivery")
				return nil
			}
			switch scenario {
			case "checks":
				delivery.Checks.MandatoryPassed = false
			case "commit":
				delivery.ResultCommit = "HEAD"
			case "host":
				delivery.Target.Host = "attacker.invalid"
			case "branch":
				delivery.Target.RemoteBranch = "main"
			}
			if _, err := publisher.Publish(test.Context(), delivery); err == nil {
				test.Fatal("invalid delivery accepted")
			}
		})
	}
}

func TestGitHubFindsRecordedRequestAcrossPages(test *testing.T) {
	gets, updates := 0, 0
	publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			updates++
			writePullRequest(w, "open", false)
			return
		}
		if r.Method != http.MethodGet {
			test.Errorf("unexpected creation")
			w.WriteHeader(500)
			return
		}
		gets++
		if r.URL.Query().Get("page") == "1" {
			requests := make([]map[string]any, 100)
			for index := range requests {
				requests[index] = map[string]any{"number": 100 + index, "state": "closed", "head": map[string]string{"ref": "agentum/run-one", "label": "owner:agentum/run-one"}}
			}
			_ = json.NewEncoder(w).Encode(requests)
			return
		}
		_, _ = w.Write([]byte(`[{"number":42,"state":"open","head":{"ref":"agentum/run-one","label":"owner:agentum/run-one"}}]`))
	})
	delivery.PullRequest = 42
	result, err := publisher.Publish(test.Context(), delivery)
	if err != nil || result.PullRequest != 42 || gets != 2 || updates != 1 {
		test.Fatalf("result=%+v err=%v gets=%d updates=%d", result, err, gets, updates)
	}
}

func TestGitHubTargetFailureAndEmptyDefault(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		status   int
		response string
		want     ReasonCode
	}{
		{"unknown-default", 200, `{"default_branch":""}`, ReasonBaseBranchUnknown},
		{"unauthorized", 401, `{}`, ReasonCredentialsRejected},
		{"server-error", 503, `{}`, ReasonProviderError},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.response))
			})
			_, err := publisher.ResolveTarget(test.Context(), delivery.Target, "refs/tags/v1", "")
			if code, _ := Classify(err); code != scenario.want {
				test.Fatalf("err=%v want=%s", err, scenario.want)
			}
		})
	}
}

func TestGitHubProbeReadsRepositoryOnly(test *testing.T) {
	calls := 0
	publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo" {
			test.Errorf("probe wrote or read wrong target: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	})
	if probe, err := publisher.Probe(test.Context()); err != nil || probe.Ready || calls != 0 {
		test.Fatalf("unscoped probe=%+v err=%v calls=%d", probe, err, calls)
	}
	publisher.probeTarget = delivery.Target
	if probe, err := publisher.Probe(test.Context()); err != nil || !probe.Ready || calls != 1 {
		test.Fatalf("scoped probe=%+v err=%v calls=%d", probe, err, calls)
	}
}

func TestGitHubClassifiesHTTPRefusalsWithoutRetry(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		want    ReasonCode
	}{
		{"repository-not-found", 404, nil, `{"message":"Not Found"}`, ReasonRemoteUnknown},
		{"credentials", 403, nil, `{"message":"Resource not accessible by integration"}`, ReasonCredentialsRejected},
		{"primary-limit", 403, map[string]string{"X-RateLimit-Remaining": "0"}, `{"message":"quota"}`, ReasonProviderRateLimited},
		{"retry-after", 403, map[string]string{"Retry-After": "60"}, `{"message":"slow down"}`, ReasonProviderRateLimited},
		{"secondary-limit", 403, nil, `{"message":"You have exceeded a secondary rate limit"}`, ReasonProviderRateLimited},
		{"too-many", 429, nil, `{}`, ReasonProviderRateLimited},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			calls := 0
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				calls++
				for key, value := range scenario.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			})
			_, err := publisher.ResolveTarget(test.Context(), delivery.Target, strings.Repeat("a", 40), "")
			if code, _ := Classify(err); code != scenario.want {
				test.Fatalf("resolve err=%v want=%s", err, scenario.want)
			}
			if calls != 1 {
				test.Fatalf("resolution retried: %d calls", calls)
			}
			calls = 0
			_, err = publisher.Publish(test.Context(), delivery)
			if code, _ := Classify(err); code != scenario.want {
				test.Fatalf("publish err=%v want=%s", err, scenario.want)
			}
			if calls != 1 {
				test.Fatalf("publication retried: %d calls", calls)
			}
		})
	}
}

func TestGitHubFindsCanonicalOwnerWithoutChangingBranchCase(test *testing.T) {
	for _, scenario := range []struct {
		name, ref, label string
		found            bool
	}{
		{"canonical-owner", "agentum/run-one", "OwNeR:agentum/run-one", true},
		{"different-branch-case", "agentum/RUN-one", "OwNeR:agentum/RUN-one", false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode([]any{map[string]any{"number": 42, "state": "open", "head": map[string]string{"ref": scenario.ref, "label": scenario.label}}})
			})
			_, found, err := publisher.findPullRequest(test.Context(), delivery, "test-token")
			if err != nil || found != scenario.found {
				test.Fatalf("found=%t err=%v", found, err)
			}
		})
	}
}

func TestGitHubMissingRecordedRequestDoesNotClaimClosed(test *testing.T) {
	calls := 0
	publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			test.Errorf("unexpected write %s", r.Method)
		}
		_, _ = w.Write([]byte(`[]`))
	})
	delivery.PullRequest = 42
	_, err := publisher.Publish(test.Context(), delivery)
	if code, _ := Classify(err); code != ReasonPullRequestNotFound || code.Retryable() {
		test.Fatalf("err=%v", err)
	}
	if calls != 1 {
		test.Fatalf("calls=%d", calls)
	}
}

func TestGitHubDraftRejectionPersistsUntilDraftObserved(test *testing.T) {
	exists, draft := false, false
	creates, updates := 0, 0
	publisher, delivery := githubFixture(test, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if !exists {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"number": 42, "state": "open", "draft": draft, "head": map[string]string{"ref": "agentum/run-one", "label": "owner:agentum/run-one"}}})
		case http.MethodPost:
			creates++
			exists = true
			w.WriteHeader(http.StatusCreated)
			writePullRequest(w, "open", false)
		case http.MethodPatch:
			updates++
			writePullRequest(w, "open", draft)
		}
	})
	result, err := publisher.Publish(test.Context(), delivery)
	if code, _ := Classify(err); code != ReasonDraftUnsupported || !result.DraftRejected || result.PullRequest != 42 {
		test.Fatalf("creation result=%+v err=%v", result, err)
	}
	delivery.PullRequest = result.PullRequest
	delivery.DraftRejected = result.DraftRejected
	for range 2 {
		result, err = publisher.Publish(test.Context(), delivery)
		if code, _ := Classify(err); code != ReasonDraftUnsupported || !result.DraftRejected {
			test.Fatalf("retry result=%+v err=%v", result, err)
		}
	}
	if creates != 1 || updates != 0 {
		test.Fatalf("unexpected writes creates=%d updates=%d", creates, updates)
	}
	draft = true
	result, err = publisher.Publish(test.Context(), delivery)
	if err != nil || result.DraftRejected {
		test.Fatalf("confirmed draft result=%+v err=%v", result, err)
	}
	// After confirmation, a human can mark the PR ready without publisher restoring draft.
	delivery.DraftRejected = false
	draft = false
	if _, err = publisher.Publish(test.Context(), delivery); err != nil {
		test.Fatalf("human draft removal: %v", err)
	}
	if creates != 1 || updates != 2 {
		test.Fatalf("writes creates=%d updates=%d", creates, updates)
	}
}

// TestGitHubBaseVerification pins the pre-push base check: the pull request
// never leaves the host when the target branch's current head is not part of
// the result's history. A diverged branch refuses blocked (a person decides),
// an unverifiable comparison refuses retryable (fetch and retry), a missing
// branch keeps its own reason, and a contained head publishes.
func TestGitHubBaseVerification(test *testing.T) {
	for _, scenario := range []struct {
		name string
		// branchHead the provider reports; empty means 404.
		branchHead string
		// ancestry: verified / unverifiable flags returned by the scripted
		// comparator; skip runs no comparator and relies on the short-circuit.
		runComparator bool
		verified      bool
		unverifiable  bool
		wantCode      ReasonCode
		wantPushed    bool
	}{
		{name: "branch head equals result", branchHead: strings.Repeat("a", 40), wantCode: "", wantPushed: true},
		{name: "branch head contained in result", branchHead: strings.Repeat("c", 40), runComparator: true, verified: true, wantCode: "", wantPushed: true},
		{name: "branch diverged from result", branchHead: strings.Repeat("c", 40), runComparator: true, wantCode: ReasonBaseDiverged},
		{name: "comparison unverifiable locally", branchHead: strings.Repeat("c", 40), runComparator: true, unverifiable: true, wantCode: ReasonBaseUnverifiable},
		{name: "base branch missing at provider", branchHead: "", wantCode: ReasonBaseBranchUnknown},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			pushes := 0
			// Minimal PR flow for the scenarios that pass the base check.
			publisher, delivery := githubFixtureWithBranch(test, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_, _ = w.Write([]byte("[]"))
				case http.MethodPost:
					w.WriteHeader(http.StatusCreated)
					writePullRequest(w, "open", true)
				}
			}, scenario.branchHead)
			if scenario.branchHead == "" {
				// githubFixtureWithBranch always answers 200; replace the
				// whole client routing with a 404 for the branch lookup.
				publisher.client = &http.Client{Transport: notFoundBranches{}}
			}
			publisher.push = func(context.Context, Delivery, string) error { pushes++; return nil }
			if scenario.runComparator {
				verified, unverifiable := scenario.verified, scenario.unverifiable
				publisher.compareAncestry = func(context.Context, string, string, string) (bool, bool, error) {
					return verified, unverifiable, nil
				}
			}
			_, err := publisher.Publish(test.Context(), delivery)
			if scenario.wantCode == "" {
				if err != nil || pushes != 1 {
					test.Fatalf("err=%v pushes=%d", err, pushes)
				}
				return
			}
			if code, _ := Classify(err); code != scenario.wantCode {
				test.Fatalf("code=%s want=%s err=%v", code, scenario.wantCode, err)
			}
			if pushes != 0 {
				test.Fatal("a refused base verification still pushed")
			}
		})
	}
}

// notFoundBranches answers every request with 404, standing in for a
// provider whose base branch is gone.
type notFoundBranches struct{}

func (transport notFoundBranches) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found",
		Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

// TestCheckoutAncestryRealGit drives the real git comparator against a
// throwaway repository: contained commits verify, divergent ones do not, and
// an object the checkout never saw reports unverifiable rather than guessing.
func TestCheckoutAncestryRealGit(test *testing.T) {
	repo := test.TempDir()
	if err := initBareRepoWithCommit(test, repo); err != nil {
		test.Fatal(err)
	}
	base := gitHead(test, repo)
	// A child commit on top of base.
	if err := gitRun(test, repo, "commit", "--allow-empty", "-m", "child"); err != nil {
		test.Fatal(err)
	}
	child := gitHead(test, repo)
	// A sibling line: reset back and commit a different child.
	if err := gitRun(test, repo, "reset", "--hard", base); err != nil {
		test.Fatal(err)
	}
	if err := gitRun(test, repo, "commit", "--allow-empty", "-m", "sibling"); err != nil {
		test.Fatal(err)
	}
	sibling := gitHead(test, repo)

	verified, unverifiable, err := checkoutAncestry(test.Context(), repo, base, child)
	if err != nil || unverifiable || !verified {
		test.Fatalf("base→child: verified=%v unverifiable=%v err=%v", verified, unverifiable, err)
	}
	verified, unverifiable, err = checkoutAncestry(test.Context(), repo, sibling, child)
	if err != nil || unverifiable || verified {
		test.Fatalf("sibling→child must be a clean no: verified=%v unverifiable=%v err=%v", verified, unverifiable, err)
	}
	unknown := strings.Repeat("7", 40)
	_, unverifiable, err = checkoutAncestry(test.Context(), repo, unknown, child)
	if err == nil || !unverifiable {
		test.Fatalf("unknown object must report unverifiable: unverifiable=%v err=%v", unverifiable, err)
	}
}

// initBareRepoWithCommit creates a git repository with one commit and the
// test identity configured, reusing the repo directory as the checkout.
func initBareRepoWithCommit(test *testing.T, dir string) error {
	test.Helper()
	if err := gitRun(test, dir, "init", "--quiet"); err != nil {
		return err
	}
	if err := gitRun(test, dir, "config", "user.email", "t@example.com"); err != nil {
		return err
	}
	if err := gitRun(test, dir, "config", "user.name", "t"); err != nil {
		return err
	}
	return gitRun(test, dir, "commit", "--allow-empty", "-m", "base")
}

func gitRun(test *testing.T, dir string, args ...string) error {
	test.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func gitHead(test *testing.T, dir string) string {
	test.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		test.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
