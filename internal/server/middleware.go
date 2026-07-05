package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/nzinovev/agentum/internal/authz"
	"github.com/nzinovev/agentum/internal/config"
)

// applyBoundary wraps the router in the single front-door middleware chain.
// The slots are stubs today; SSO/RBAC/audit/rate-limit and MCP capability
// enforcement slot into these exact positions later, with no handler-side
// changes.
func applyBoundary(h http.Handler, cfg config.Config, log *slog.Logger) http.Handler {
	return chain(
		h,
		recoverer(log),
		requestLog(log),
		tenantResolver(cfg),
		authzGate(),
	)
}

// chain composes middlewares so the first listed runs outermost.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "recover", rec, "stack", string(debug.Stack()))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func requestLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "dur", time.Since(start))
		})
	}
}

// tenantResolver resolves the caller to a Principal. Today there is exactly one
// caller — the local owner (seam). Later: OIDC/SSO validation produces the real
// Principal at this same slot.
func tenantResolver(cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := authz.Principal{
				TenantID: cfg.TenantID,
				UserID:   cfg.TenantOwnerUserID,
			}
			ctx := authz.WithPrincipal(r.Context(), p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authzGate routes every identity/permission decision through authz.Can — the
// single function nothing internal may bypass.
func authzGate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := authz.PrincipalFrom(r.Context())
			if !ok {
				http.Error(w, "unresolved principal", http.StatusUnauthorized)
				return
			}
			if d := authz.Can(r.Context(), p, "access", r.URL.Path); !d.Allowed {
				http.Error(w, d.Reason, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(authz.WithPrincipal(r.Context(), p)))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
