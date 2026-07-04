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

// Decision is the result of an authorization check.
type Decision struct {
	Allowed bool
	Reason  string
}

func Allow() Decision        { return Decision{Allowed: true, Reason: "owner"} }
func Deny(r string) Decision { return Decision{Allowed: false, Reason: r} }

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
