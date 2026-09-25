package publish

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestRegistryResolvesByIdAndRefusesUnknown pins the registry's two
// behaviours a caller depends on: an empty id resolves to the default entry,
// and an unknown id is an error naming the id and the known ones — the
// provider is never silently substituted.
func TestRegistryResolvesByIdAndRefusesUnknown(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(RegistryOptions{})

	resolved, err := registry.Resolve("")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if resolved.ID() != ProviderNoop {
		t.Errorf("default provider = %q, want %q", resolved.ID(), ProviderNoop)
	}

	byID, err := registry.Resolve(ProviderNoop)
	if err != nil {
		t.Fatalf("resolve by id: %v", err)
	}
	if byID != resolved {
		t.Error("resolving the default id returned a different instance than resolving empty")
	}

	_, err = registry.Resolve("gitlab")
	if err == nil {
		t.Fatal("unknown id resolved; want error")
	}
	for _, known := range []string{"noop"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error %q does not name known id %q", err, known)
		}
	}
}

// TestPublisherContractHasNoMerge checks the property the whole isolation
// rests on: the coordinator reaches a provider only through the Publisher
// interface, so an operation absent from its method set is unreachable. A
// method whose name mentions merge would put the operation back in reach.
func TestPublisherContractHasNoMerge(t *testing.T) {
	t.Parallel()
	publisherType := reflect.TypeOf((*Publisher)(nil)).Elem()
	methods := publisherType.NumMethod()
	wantMethods := map[string]bool{
		"ID":       false,
		"Describe": false,
		"Probe":    false,
		"Publish":  false,
	}
	if methods != len(wantMethods) {
		t.Errorf("Publisher has %d methods, want %d; growing the surface needs a second look", methods, len(wantMethods))
	}
	for index := 0; index < methods; index++ {
		name := publisherType.Method(index).Name
		if strings.Contains(strings.ToLower(name), "merge") {
			t.Errorf("Publisher method %q mentions merge; the coordinator must not be able to ask for one", name)
		}
		if _, known := wantMethods[name]; !known {
			t.Errorf("Publisher grew an unexpected method %q", name)
		}
		wantMethods[name] = true
	}
	for name, seen := range wantMethods {
		if !seen {
			t.Errorf("Publisher is missing method %q", name)
		}
	}
}

// TestNoopPublisherRefusesWithCredentialsMissing: the placeholder refuses
// every delivery with the one reason a configuration fix clears, so a wired
// system records a failed publication instead of touching the network.
func TestNoopPublisherRefusesWithCredentialsMissing(t *testing.T) {
	t.Parallel()
	publisher := NewNoopPublisher()
	_, err := publisher.Publish(context.Background(), Delivery{Branch: "agentum/r1"})
	if err == nil {
		t.Fatal("noop Publish returned nil; want a refusal")
	}
	code, message := Classify(err)
	if code != ReasonCredentialsMissing {
		t.Errorf("Classify code = %q, want %q", code, ReasonCredentialsMissing)
	}
	if message == "" {
		t.Error("refusal message is empty")
	}
	if !code.Retryable() {
		t.Errorf("credentials_missing reported not retryable; a fixed configuration clears it")
	}
}

// TestClassifyKeepsRefusalCodeAndDefaultsToProviderError: a typed refusal
// keeps its code, and an untyped error lands on provider_error — the
// coordinator never records a code outside the vocabulary.
func TestClassifyKeepsRefusalCodeAndDefaultsToProviderError(t *testing.T) {
	t.Parallel()
	code, message := Classify(&Refusal{Code: ReasonNonFastForward, Message: "fetch first"})
	if code != ReasonNonFastForward || message != "fetch first" {
		t.Errorf("Classify refusal = (%q, %q), want (non_fast_forward, fetch first)", code, message)
	}
	code, _ = Classify(errors.New("boom"))
	if code != ReasonProviderError {
		t.Errorf("Classify untyped error code = %q, want provider_error", code)
	}
	if code, _ := Classify(nil); code != ReasonProviderError {
		t.Errorf("Classify nil code = %q, want provider_error", code)
	}
}

// TestReasonVocabularyPartitionedByRetry pins which reasons a repeated
// attempt can clear. The publication row's failed/blocked state derives from
// this partition, and the history surface renders the two differently.
func TestReasonVocabularyPartitionedByRetry(t *testing.T) {
	t.Parallel()
	retryable := []ReasonCode{
		ReasonCredentialsMissing,
		ReasonCredentialsRejected,
		ReasonNetworkUnreachable,
		ReasonProviderError,
	}
	blocking := []ReasonCode{
		ReasonRemoteUnknown,
		ReasonBaseBranchUnknown,
		ReasonNonFastForward,
		ReasonPushRejected,
		ReasonDraftUnsupported,
		ReasonPullRequestClosed,
		ReasonChecksNotPassed,
		ReasonCommitMismatch,
		ReasonSecretInDescription,
	}
	for _, code := range retryable {
		if !code.Retryable() {
			t.Errorf("reason %q reported not retryable; want retryable", code)
		}
	}
	for _, code := range blocking {
		if code.Retryable() {
			t.Errorf("reason %q reported retryable; a retry without an external change reproduces it", code)
		}
	}
}
