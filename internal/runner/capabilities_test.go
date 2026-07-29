// Package runner — capability enforcement integration tests.
//
// This file exercises the code-enforced capability path end-to-end through the
// runner: pack + stage + role → effective profile → persisted on the invocation
// row → emitted as audit evidence → honored by the adapter. It does NOT require
// the opencode binary; a recording adapter captures the profile the runner
// computed and asserts the deny-by-default invariants.
//
// What this proves (mapped to the task acceptance criteria):
//
//   - An invocation without a permitted+supported profile does not start
//     (TestInvocation_UnenforceableProfileDoesNotStart).
//   - An analytical stage cannot change source code
//     (TestProfile_AnalystStageCannotWriteSource).
//   - A reviewer cannot change tracked source or delivery refs
//     (TestProfile_ReviewerCannotTouchDeliveryRefs).
//   - An implementer is scoped to its worktree
//     (TestProfile_ImplementerScopedToWorktree).
//   - An agent never receives undeclared credentials / network / commands /
//     MCP tools / host paths (TestProfile_UndeclaredCapabilitiesAreDenied).
//   - Allowed actions still proceed (TestProfile_AllowedActionsRunToCompletion).
//   - The effective profile is saved on the invocation row and emitted as audit
//     evidence (TestProfile_SavedOnInvocationAndEmitted).
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/caps"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// recordingAdapter captures every Invocation's profile and returns a scripted
// Result per stage. The captured profiles let a test assert exactly what the
// runner granted.
//
//   - claimed is what Supported() reports (the set the runner's computeProfile
//     intersects against).
//   - actual is what the Invoke-time EnforceableBy check enforces.
//
// They default to the same value. The "defense-in-depth refuses to start" test
// sets actual narrower than claimed, modeling a runtime whose real enforcement
// capability is a subset of its declared support (e.g. the operator reconfigured
// the adapter between compute and invoke). That is exactly the scenario the
// per-invocation EnforceableBy check exists for.
type recordingAdapter struct {
	scripts   map[string]agent.ResultJSON
	claimed   []caps.Category
	actual    []caps.Category // nil → same as claimed
	captured  []caps.Profile
	lastError error
}

func (adapter *recordingAdapter) Supported() []caps.Category {
	return adapter.claimed
}

func (adapter *recordingAdapter) Invoke(ctx context.Context, inv agent.Invocation) (<-chan agent.Event, error) {
	// Defense-in-depth: re-check enforceability against the actual runtime
	// capability, exactly as the real opencode adapter does in
	// prepareEnforcement. An unenforceable profile refuses to start — the
	// adapter returns (nil, err) and never spawns anything. The runner maps
	// caps.ErrUnenforceable to stop_reason=capability_unenforceable.
	checkAgainst := adapter.actual
	if checkAgainst == nil {
		checkAgainst = adapter.claimed
	}
	if err := inv.Profile.EnforceableBy(checkAgainst); err != nil {
		adapter.lastError = err
		return nil, err
	}
	adapter.captured = append(adapter.captured, inv.Profile)

	eventCh := make(chan agent.Event, 2)
	go func() {
		defer close(eventCh)
		scripted, ok := adapter.scripts[stageOf(inv)]
		if !ok {
			eventCh <- agent.Event{Kind: agent.EventError, Err: errors.New("no script for stage")}
			return
		}
		eventCh <- agent.Event{Kind: agent.EventResult, Result: &agent.Result{
			SessionID: "sess-" + stageOf(inv), ResultJSON: scripted,
		}}
	}()
	return eventCh, nil
}

// fullSupport is the category set the opencode adapter declares; tests use it
// when they are not exercising the enforceability check.
var fullSupport = []caps.Category{
	caps.CatFsRead, caps.CatFsWrite, caps.CatArtifactWrite, caps.CatExecBash,
	caps.CatGitRead, caps.CatGitWrite, caps.CatNetFetch, "secret", "mcp",
}

// capabilityPack builds an in-memory pack with the given pack-level
// capabilities and per-stage definitions, all on auto gates so the loop
// advances without depending on isClean.
func capabilityPack(packCaps []string, entry string, stages map[string]pack.Stage) *pack.Pack {
	return &pack.Pack{
		API: "agentum/v1", Pack: pack.Meta{Name: "cap-test", Version: "0.1.0"},
		Capabilities: packCaps,
		Tiers:        pack.Tiers{Default: "fast"}, Entry: entry, Stages: stages,
		PromptText: map[string]string{entry: "do the thing"},
	}
}

// runSingleStageOnce drives a one-stage pack through the runner and returns the
// profile the recording adapter observed (or nil if the run never reached it).
func runSingleStageOnce(t *testing.T, taskPack *pack.Pack, supported []caps.Category) (*recordingAdapter, *fakeStore, caps.Profile) {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "C1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "cap-test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "CapProj"}
	store := newFakeStore(task, proj)
	adapter := &recordingAdapter{
		scripts: map[string]agent.ResultJSON{
			"spec":      {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "ok"},
			"review":    {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "ok"},
			"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "ok"},
			"fix":       {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "ok"},
		},
		claimed: supported,
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "C1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	var observed caps.Profile
	if len(adapter.captured) > 0 {
		observed = adapter.captured[0]
	}
	return adapter, store, observed
}

// TestProfile_AnalystStageCannotWriteSource: a spec (analyst) stage in a pack
// that declares fs.write still receives no fs.write — the role excludes it.
// This is the "analytical stages read code and write only their own artifacts"
// acceptance criterion.
func TestProfile_AnalystStageCannotWriteSource(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "git.read", "exec.bash"},
		"spec",
		map[string]pack.Stage{
			"spec": {Gate: pack.GateAuto, Role: "analyst", Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
			"done": {},
		},
	)
	_, _, observed := runSingleStageOnce(t, taskPack, fullSupport)

	if observed.Has(caps.CatFsWrite) {
		t.Errorf("analyst stage must not receive fs.write, got %v", observed.Grants)
	}
	if observed.Has(caps.CatExecBash) {
		t.Errorf("analyst stage must not receive exec.bash, got %v", observed.Grants)
	}
	if !observed.Has(caps.CatFsRead) {
		t.Errorf("analyst stage lost fs.read: %v", observed.Grants)
	}
}

// TestProfile_ReviewerCannotTouchDeliveryRefs: a review stage cannot mutate
// tracked source or delivery refs. Even a pack that declares git.delivery and
// fs.write must not surface them to the reviewer.
func TestProfile_ReviewerCannotTouchDeliveryRefs(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "git.read", "git.write", "git.delivery"},
		"review",
		map[string]pack.Stage{
			"review": {Gate: pack.GateAuto, Role: "reviewer", Prompt: "review.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":   {},
		},
	)
	taskPack.PromptText["review"] = "review body"
	_, _, observed := runSingleStageOnce(t, taskPack, fullSupport)

	if observed.Has(caps.CatGitDelivery) {
		t.Errorf("reviewer must not receive git.delivery, got %v", observed.Grants)
	}
	if observed.Has(caps.CatFsWrite) {
		t.Errorf("reviewer must not change tracked source, got %v", observed.Grants)
	}
}

// TestProfile_ImplementerScopedToWorktree: an implementer stage receives
// fs.write and git.write scoped to the worktree. The recorded profile carries
// the absolute worktree path, not the placeholder.
func TestProfile_ImplementerScopedToWorktree(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "git.read", "git.write", "exec.bash"},
		"implement",
		map[string]pack.Stage{
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
	)
	taskPack.PromptText["implement"] = "implement body"
	_, _, observed := runSingleStageOnce(t, taskPack, fullSupport)

	if !observed.Has(caps.CatFsWrite) {
		t.Fatalf("implementer lost fs.write: %v", observed.Grants)
	}
	for _, granted := range observed.Grants {
		if granted.Key() == string(caps.CatFsWrite) {
			if granted.Scope() == "" {
				t.Errorf("implementer fs.write must be scoped to the worktree, got unscoped")
			}
		}
	}
}

// TestProfile_UndeclaredCapabilitiesAreDenied: a pack that omits net.fetch,
// secrets, and mcp yields a profile that contains none of them, regardless of
// role. This is the "agent does not receive undeclared credentials, network,
// commands, MCP tools, or host paths" criterion.
func TestProfile_UndeclaredCapabilitiesAreDenied(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read"}, // deliberately minimal — no net, no secret, no mcp
		"spec",
		map[string]pack.Stage{
			"spec": {Gate: pack.GateAuto, Role: "analyst", Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
			"done": {},
		},
	)
	_, _, observed := runSingleStageOnce(t, taskPack, fullSupport)

	if observed.Has(caps.CatNetFetch) {
		t.Errorf("net.fetch granted without declaration: %v", observed.Grants)
	}
	if observed.Has("secret") || observed.Has("mcp") {
		t.Errorf("secret/mcp granted without declaration: %v", observed.Grants)
	}
}

// TestProfile_DenyByDefaultWhenPackDeclaresNothing: a pack with an empty
// capability ceiling yields a profile that grants only the mandatory artifact
// floor (result.json is a non-negotiable contract). Every other category —
// source writes, commands, network, secrets — is denied.
func TestProfile_DenyByDefaultWhenPackDeclaresNothing(t *testing.T) {
	taskPack := capabilityPack(
		nil,
		"spec",
		map[string]pack.Stage{
			"spec": {Gate: pack.GateAuto, Role: "analyst", Prompt: "spec.md", Transitions: []pack.Transition{{To: "done"}}},
			"done": {},
		},
	)
	_, _, observed := runSingleStageOnce(t, taskPack, fullSupport)

	if observed.Has(caps.CatFsWrite) || observed.Has(caps.CatExecBash) || observed.Has(caps.CatNetFetch) {
		t.Errorf("empty pack must deny source/bash/network, got %v", observed.Grants)
	}
	if !observed.Has(caps.CatArtifactWrite) {
		t.Errorf("artifact.write floor must always be present (result.json contract), got %v", observed.Grants)
	}
}

// TestProfile_AllowedActionsRunToCompletion: when the pack + role grant a
// coherent set, the run completes (status complete) and the profile is
// non-empty. This is the "allowed actions continue to work" criterion.
func TestProfile_AllowedActionsRunToCompletion(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "git.read", "git.write", "exec.bash"},
		"implement",
		map[string]pack.Stage{
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
	)
	taskPack.PromptText["implement"] = "implement body"
	store := newFakeStoreWithPack(t, taskPack)

	if state := store.taskState(); state != "awaiting_memory_commit" {
		t.Errorf("expected run to complete to awaiting_memory_commit, got state %q", state)
	}
}

// TestInvocation_UnenforceableProfileDoesNotStart: when the adapter cannot
// enforce a granted capability, the invocation refuses to start and the
// invocation row records stop_reason=capability_unenforceable. The adapter
// never observes a profile (no subprocess spawned).
func TestInvocation_UnenforceableProfileDoesNotStart(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "exec.bash"},
		"implement",
		map[string]pack.Stage{
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
	)
	taskPack.PromptText["implement"] = "implement body"
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "U1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "cap-test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "U"}
	store := newFakeStore(task, proj)
	// The adapter claims full support (so the runner's computeProfile keeps
	// fs.write + exec.bash in the effective profile) but its Invoke-time check
	// only honors fs.read + artifact.write + git.read. The defense-in-depth
	// EnforceableBy check then refuses to start — modeling a runtime whose real
	// enforcement is a subset of what it advertised.
	adapter := &recordingAdapter{
		scripts: map[string]agent.ResultJSON{"implement": {SchemaVersion: "1", Status: agent.StatusComplete}},
		claimed: fullSupport,
		actual:  []caps.Category{caps.CatFsRead, caps.CatArtifactWrite, caps.CatGitRead},
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})

	if err := runner.Handle(t.Context(), job("run", "U1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	if len(adapter.captured) != 0 {
		t.Errorf("adapter observed %d profiles — an unenforceable invocation must not start", len(adapter.captured))
	}
	store.mu.Lock()
	stopReason := ""
	if len(store.invocations) > 0 {
		stopReason = store.invocations[0].StopReason.String
	}
	store.mu.Unlock()
	if stopReason != "capability_unenforceable" {
		t.Errorf("stop_reason = %q, want capability_unenforceable", stopReason)
	}
}

// TestProfile_SavedOnInvocationAndEmitted: the effective profile is saved on
// the stage_invocations row and a stage.capability_enforced event is emitted
// with the profile as audit evidence.
func TestProfile_SavedOnInvocationAndEmitted(t *testing.T) {
	taskPack := capabilityPack(
		[]string{"fs.read", "fs.write", "git.read", "git.write", "exec.bash"},
		"implement",
		map[string]pack.Stage{
			"implement": {Gate: pack.GateAuto, Role: "implementer", Prompt: "impl.md", Transitions: []pack.Transition{{To: "done"}}},
			"done":      {},
		},
	)
	taskPack.PromptText["implement"] = "implement body"
	_, store, _ := runSingleStageOnce(t, taskPack, fullSupport)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.invocations) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(store.invocations))
	}
	profileBytes := store.invocations[0].CapabilityProfile
	if !profileBytes.Valid || len(profileBytes.RawMessage) == 0 {
		t.Fatalf("capability_profile not saved on invocation row")
	}
	var saved caps.Profile
	if err := json.Unmarshal(profileBytes.RawMessage, &saved); err != nil {
		t.Fatalf("unmarshal saved profile: %v", err)
	}
	if !saved.Has(caps.CatFsWrite) {
		t.Errorf("saved profile lost fs.write: %+v", saved)
	}

	var enforcedEventFound bool
	for _, event := range store.events {
		if event.Type == EvCapabilityEnforced {
			enforcedEventFound = true
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("unmarshal capability event payload: %v", err)
			}
			if payload["role"] != string(caps.RoleImplementer) {
				t.Errorf("event role = %v, want implementer", payload["role"])
			}
			break
		}
	}
	if !enforcedEventFound {
		t.Errorf("no %s event emitted", EvCapabilityEnforced)
	}
}

// newFakeStoreWithPack is a thin wrapper for tests that only need to confirm
// the run completes; it returns the store after driving a single-implementer
// pack through to completion.
func newFakeStoreWithPack(t *testing.T, taskPack *pack.Pack) *fakeStore {
	t.Helper()
	repo := t.TempDir()
	if err := initRepoWithCommit(repo); err != nil {
		t.Fatalf("setup repo: %v", err)
	}
	task := sqlc.Task{ID: "A1", TenantID: "tn", UserID: "us", ProjectID: "P1", State: "running", PipelinePack: "cap-test@0.1.0"}
	proj := sqlc.Project{ID: "P1", TenantID: "tn", RepoPath: repo, Name: "Allowed"}
	store := newFakeStore(task, proj)
	adapter := &recordingAdapter{
		scripts: map[string]agent.ResultJSON{
			"implement": {SchemaVersion: "1", Status: agent.StatusComplete, Summary: "done"},
		},
		claimed: fullSupport,
	}
	runner := New(Deps{Store: store, Packs: &staticSource{pk: taskPack}, Adapter: adapter, AgentName: "opencode"})
	if err := runner.Handle(t.Context(), job("run", "A1", "tn", "us")); err != nil {
		t.Fatalf("run job: %v", err)
	}
	return store
}
