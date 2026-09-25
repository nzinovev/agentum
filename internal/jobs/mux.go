package jobs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// HandlerFunc adapts a plain function to Handler, the same device
// http.HandlerFunc is: a per-kind method on a component becomes one table
// row without wrapping types.
type HandlerFunc func(ctx context.Context, job sqlc.Job) error

// Handle calls the wrapped function.
func (fn HandlerFunc) Handle(ctx context.Context, job sqlc.Job) error {
	return fn(ctx, job)
}

// Mux is a Handler that routes a job to the handler registered under its
// kind. The worker takes ONE handler; without the mux every new kind would
// have to grow the switch of whichever component got there first, and that
// component's fields — an adapter, a token, a worktree manager — would be
// reachable from code that has no business touching them. The mux keeps the
// handlers peers: the connection between them is a row in this table, which
// the compiler checks as an import boundary.
type Mux struct {
	handlers map[string]Handler
}

// NewMux builds the mux over the given kind → handler table. A kind mapped
// to a nil handler is a wiring bug and is refused here, at construction,
// rather than panicking on the first job of that kind.
func NewMux(handlers map[string]Handler) *Mux {
	for kind, handler := range handlers {
		if handler == nil {
			panic(fmt.Sprintf("jobs: nil handler registered for kind %q", kind))
		}
	}
	return &Mux{handlers: handlers}
}

// Handle routes the job to its kind's handler. An unknown kind is an error
// naming the kind and the registered ones — a job the table cannot answer is
// a lost enqueue, and it must fail loudly instead of running a wrong
// handler.
func (mux *Mux) Handle(ctx context.Context, job sqlc.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handler, found := mux.handlers[job.Kind]
	if !found {
		return fmt.Errorf("jobs: unknown job kind %q (known: %s)", job.Kind, strings.Join(mux.knownKinds(), ", "))
	}
	return handler.Handle(ctx, job)
}

// knownKinds returns the registered kinds sorted, so an error message is
// stable across runs (map iteration is not).
func (mux *Mux) knownKinds() []string {
	kinds := make([]string, 0, len(mux.handlers))
	for kind := range mux.handlers {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}
