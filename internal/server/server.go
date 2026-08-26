package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/api"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/checks"
	"github.com/nzinovev/agentum/internal/config"
	"github.com/nzinovev/agentum/internal/jobs"
	"github.com/nzinovev/agentum/internal/manifest"
	"github.com/nzinovev/agentum/internal/models"
	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/runner"
	"github.com/nzinovev/agentum/internal/store"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// Server wires the full execution model: the HTTP boundary (api), the runner
// (the stage loop), the job worker that drives it, and the periodic reconciler
// that repairs stale leases and orphaned tasks between restarts. One process
// runs one worker pool over a shared Postgres-backed queue.
type Server struct {
	cfg        config.Config
	log        *slog.Logger
	store      *store.Store
	artifacts  *artifacts.SQLStore
	manifest   *manifest.Service
	api        *api.API
	runner     *runner.Runner
	worker     *jobs.Worker
	reconciler *jobs.Reconciler
	pool       int
}

// New constructs the server and all execution-model dependencies. The worker
// and reconciler are not started here — Run starts them after recovery so no
// job runs before stale ones are reconciled.
func New(cfg config.Config, log *slog.Logger, dataStore *store.Store) *Server {
	queries := sqlc.New(dataStore.DB)

	// Operator model override (optional; nil → built-in per-agent defaults).
	modelsCfg, _ := models.Load() // ErrNoConfig is expected in the common case

	// The execution model: pack source over a configured root, the opencode
	// adapter, per-task worktrees, the artifact revisions store, the evidence
	// manifest service, and the runner that composes them.
	packs := pack.NewDirSource(cfg.PacksDir)
	adapter := agent.NewOpencodeAdapter(cfg.OpencodeBinary)
	artifactStore := artifacts.NewSQLStore(artifacts.SQLStoreDeps{
		DB:         dataStore.DB,
		Queries:    queries,
		Blobs:      artifacts.NewBlobStore(cfg.ArtifactRoot),
		ScanPolicy: artifacts.ScanPolicy(cfg.ArtifactScanPolicy),
		Log:        log,
	})
	manifestService := manifest.New(manifest.Deps{
		DB:      dataStore.DB,
		Queries: queries,
		Log:     log,
	})
	checkExecutor := checks.NewExecutor(checks.ExecutorDeps{
		DefaultTimeout:   time.Duration(cfg.CheckTimeoutSeconds) * time.Second,
		DefaultMaxOutput: cfg.CheckMaxOutputBytes,
		Log:              log,
	})
	runnerInst := runner.New(runner.Deps{
		Store:       runnerStore{queries},
		Packs:       packs,
		Adapter:     adapter,
		Models:      modelsCfg,
		Artifacts:   artifactStore,
		Manifest:    manifestService,
		CheckExec:   checkExecutor,
		HardTimeout: time.Duration(cfg.HardTimeoutSeconds) * time.Second,
		IdleTimeout: time.Duration(cfg.IdleTimeoutSeconds) * time.Second,
		Log:         log,
	})

	worker := jobs.New(jobs.Deps{
		Store:       jobs.QueueStore{Q: queries},
		Handler:     runnerInst,
		MaxAttempts: cfg.JobMaxAttempts,
		Log:         log,
	})

	// The reconciler repairs stale job leases AND orphaned running tasks (a
	// crash between the FSM transition and EnqueueJob). *sqlc.Queries satisfies
	// TaskStore directly; QueueStore adapts the queue side. The tenant seam is
	// the single-tenant id from config until SSO/RBAC arrive.
	reconciler := jobs.NewReconciler(jobs.ReconcilerDeps{
		TenantID:    cfg.TenantID,
		Queue:       jobs.QueueStore{Q: queries},
		Tasks:       queries,
		MaxAttempts: cfg.JobMaxAttempts,
		Log:         log,
	})

	apiInst := api.New(dataStore.DB, queries, log, runnerInst.Cancels(),
		api.WithArtifactStore(artifactStore), api.WithManifestService(manifestService),
		api.WithPackSource(packs))

	return &Server{
		cfg: cfg, log: log, store: dataStore,
		artifacts: artifactStore, manifest: manifestService,
		api: apiInst, runner: runnerInst, worker: worker, reconciler: reconciler,
		pool: cfg.WorkerPoolSize,
	}
}

// Handler returns the HTTP handler with the full middleware boundary applied.
// This is the single front door: the UI and every external caller use the same
// handler; nothing internal bypasses authz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return applyBoundary(mux, s.cfg, s.log)
}

// Run serves HTTP, the job worker, and the periodic reconciler until ctx is
// cancelled, then shuts down gracefully. The reconciler runs its first pass
// before the worker starts, so a crashed worker's stale jobs and any orphaned
// tasks are repaired before any new job is claimed.
func (s *Server) Run(ctx context.Context) error {
	reconcilerCtx, cancelReconciler := context.WithCancel(ctx)
	defer cancelReconciler()
	go s.reconciler.Start(reconcilerCtx, jobs.DefaultReconcileInterval)

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
