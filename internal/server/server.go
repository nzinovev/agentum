package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/api"
	"github.com/nzinovev/agentum/internal/config"
	"github.com/nzinovev/agentum/internal/jobs"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/runner"
	"github.com/nzinovev/agentum/internal/store"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Server wires the full execution model: the HTTP boundary (api), the runner
// (the stage loop), and the job worker that drives it. One process runs one
// worker pool over a shared Postgres-backed queue.
type Server struct {
	cfg    config.Config
	log    *slog.Logger
	store  *store.Store
	api    *api.API
	runner *runner.Runner
	worker *jobs.Worker
	pool   int
}

// New constructs the server and all execution-model dependencies. The worker is
// not started here — Run starts it after recovery so no job runs before stale
// ones are reconciled.
func New(cfg config.Config, log *slog.Logger, dataStore *store.Store) *Server {
	queries := sqlc.New(dataStore.DB)

	// Operator model override (optional; nil → built-in per-agent defaults).
	modelsCfg, _ := models.Load() // ErrNoConfig is expected in the common case

	// The execution model: pack source over a configured root, the opencode
	// adapter, per-task worktrees, and the runner that composes them.
	packs := pack.NewDirSource(cfg.PacksDir)
	adapter := agent.NewOpencodeAdapter(cfg.OpencodeBinary)
	runnerInst := runner.New(runner.Deps{
		Store:     runnerStore{queries},
		Packs:     packs,
		Adapter:   adapter,
		Models:    modelsCfg,
		AgentName: "opencode",
		Log:       log,
	})

	worker := jobs.New(jobs.Deps{
		Store:       jobs.QueueStore{Q: queries},
		Handler:     runnerInst,
		MaxAttempts: cfg.JobMaxAttempts,
		Log:         log,
	})

	apiInst := api.New(queries, log, runnerInst.Cancels())

	return &Server{cfg: cfg, log: log, store: dataStore, api: apiInst, runner: runnerInst, worker: worker, pool: cfg.WorkerPoolSize}
}

// Handler returns the HTTP handler with the full middleware boundary applied.
// This is the single front door: the UI and every external caller use the same
// handler; nothing internal bypasses authz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return applyBoundary(mux, s.cfg, s.log)
}

// Run serves HTTP and the job worker until ctx is cancelled, then shuts down
// gracefully. Recovery runs first so a crashed worker's stale jobs are
// re-queued before any new job is claimed.
func (s *Server) Run(ctx context.Context) error {
	if err := s.worker.Recover(ctx); err != nil {
		// Recovery is best-effort; log and continue rather than refusing boot.
		s.log.Error("worker recovery failed", "error", err)
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	for workerIndex := 0; workerIndex < s.pool; workerIndex++ {
		go s.worker.Start(workerCtx)
	}

	srv := &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	s.log.Info("http server listening", "addr", s.cfg.HTTPAddr, "workers", s.pool)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// runnerStore adapts *sqlc.Queries to the runner's Store interface (a typed
// subset). sqlc.Queries already satisfies the method set; this thin wrapper
// exists so the dependency is explicit and the runner stays decoupled.
type runnerStore struct{ *sqlc.Queries }
