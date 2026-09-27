package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func githubFixture(test *testing.T, handler http.HandlerFunc) (*githubPublisher, Delivery) {
	test.Helper()
	server := httptest.NewTLSServer(handler)
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
	return publisher, delivery
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
