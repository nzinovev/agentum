package jobs

import (
	"context"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// TestMuxRoutesByKindAndRefusesUnknown pins the mux's two behaviours: a job
// reaches exactly the handler registered under its kind, and an unknown kind
// is an error naming the kind and the registered ones instead of running a
// wrong handler.
func TestMuxRoutesByKindAndRefusesUnknown(t *testing.T) {
	t.Parallel()
	calls := map[string]int{}
	mux := NewMux(map[string]Handler{
		"run":     HandlerFunc(func(context.Context, sqlc.Job) error { calls["run"]++; return nil }),
		"publish": HandlerFunc(func(context.Context, sqlc.Job) error { calls["publish"]++; return nil }),
	})

	for _, kind := range []string{"run", "publish"} {
		if err := mux.Handle(t.Context(), sqlc.Job{Kind: kind}); err != nil {
			t.Fatalf("handle %s: %v", kind, err)
		}
		if calls[kind] != 1 {
			t.Errorf("kind %s reached its handler %d times, want 1", kind, calls[kind])
		}
	}

	err := mux.Handle(t.Context(), sqlc.Job{Kind: "run-archive"})
	if err == nil {
		t.Fatal("unknown kind handled; want error")
	}
	if !strings.Contains(err.Error(), "run-archive") {
		t.Errorf("error %q does not name the unknown kind", err)
	}
	for _, kind := range []string{"publish", "run"} {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error %q does not name known kind %q", err, kind)
		}
	}
}

// TestMuxPropagatesHandlerOutcome: the mux adds no policy of its own — a
// handler's error is the job's error, and nil is nil.
func TestMuxPropagatesHandlerOutcome(t *testing.T) {
	t.Parallel()
	mux := NewMux(map[string]Handler{
		"explode": HandlerFunc(func(context.Context, sqlc.Job) error {
			return context.DeadlineExceeded
		}),
	})
	if err := mux.Handle(t.Context(), sqlc.Job{Kind: "explode"}); err != context.DeadlineExceeded {
		t.Errorf("handler error came back as %v; want it unchanged", err)
	}
}
