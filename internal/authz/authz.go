// Package authz is the single enforcement point for identity and permission
// decisions in Agentum. Every caller — HTTP handlers, background workers, any
// future CLI — must traverse authz.Can; nothing internal bypasses it.
//
// Today it returns true for the single local owner. SSO and RBAC slot in here
// with no caller-side changes: the schema is already multi-tenant, the
// Principal grows fields, and Can grows rules.
package authz

import (
	"context"
	"fmt"
)

// Principal is the resolved caller. Today there is exactly one: the local owner
// injected by the server's tenantResolver middleware.
type Principal struct {
	TenantID string
	UserID   string
	// Roles []string // arrives with RBAC; absent now on purpose
}

type ctxKey struct{}

// WithPrincipal stores the resolved Principal in the context. The HTTP boundary
// calls this; downstream callers retrieve it via PrincipalFrom.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom retrieves the Principal from the context. The boolean is false
// when no Principal was injected (a programming error — every inbound path must
// resolve one).
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Decision is the result of an authorization check.
type Decision struct {
	Allowed bool
	Reason  string
}

func Allow() Decision        { return Decision{Allowed: true, Reason: "owner"} }
func Deny(r string) Decision { return Decision{false, r} }

// Action vocabulary. These are the `action` arguments callers pass to Can.
// Centralized here so the permission surface has one source of truth — a typo
// in a handler cannot silently invent a new permission. A caller that needs a
// permission not listed here adds the constant here FIRST; never pass a string
// literal to Can, or the vocabulary stops being enumerable and RBAC has nothing
// to attach rules to.
const (
	// ActionAccess is the route-level gate every inbound request passes in the
	// server middleware, ahead of any handler-specific action. Its resource is
	// the request path, not a row id.
	ActionAccess = "access"

	// ActionTaskCreate and ActionTaskList are tenant-scoped: they carry no
	// resource id, because the task does not exist yet (create) or the resource
	// is the whole collection (list).
	ActionTaskCreate = "task:create"
	ActionTaskList   = "task:list"

	// ActionTaskRead is the right to read a task and its artifacts / manifest.
	// Every read-side handler (tasks, artifacts, manifest, diff) checks it.
	ActionTaskRead = "task:read"

	// ActionTaskStart moves a fresh task into the running pipeline.
	ActionTaskStart = "task:start"

	// ActionTaskAdvance and its approve/reject/cancel siblings are the
	// human-gate verbs. Each is its own action rather than one collapsed
	// "task:write": approving a result, rejecting it, and aborting a run are
	// different rights, and RBAC will have to grant them separately.
	ActionTaskAdvance = "task:advance"
	ActionTaskApprove = "task:approve"
	ActionTaskReject  = "task:reject"
	ActionTaskCancel  = "task:cancel"

	// ActionTaskCleanup deletes the delivery artifacts (the task branch) of an
	// already-terminal task — destructive, so it is not folded into cancel.
	ActionTaskCleanup = "task:cleanup"

	// ActionProjectCreate and its read/list siblings mirror the task actions.
	// A project is the repository a task runs against.
	ActionProjectCreate = "project:create"
	ActionProjectRead   = "project:read"
	ActionProjectList   = "project:list"

	// ActionEventStream is the right to tail the event stream: tenant-global
	// when the resource is empty, task-scoped otherwise.
	ActionEventStream = "event:stream"
)

// Can is THE permission function. action/resource are coarse today and refine
// per-route as handlers land.
func Can(ctx context.Context, p Principal, action string, resource string) Decision {
	_ = ctx
	_ = action
	_ = resource
	if p.UserID == "" || p.TenantID == "" {
		return Deny("unresolved principal")
	}
	return Allow()
}

func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	return fmt.Errorf("denied: %s", d.Reason)
}
