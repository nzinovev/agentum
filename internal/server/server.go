package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
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
	adapter    agent.Adapter
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
//
// New returns an error for configuration the process must not start on
// (ADR 0005 D4, the first refusal point): a malformed models.yaml (anything
// other than "no file"), an unknown execution adapter id, or a model option
// the selected adapter does not declare. Each error names its cause; none of
// them silently falls back to a default.
func New(cfg config.Config, log *slog.Logger, dataStore *store.Store) (*Server, error) {
	// Operator model override. ErrNoConfig is the common case (fall back to
	// the adapter descriptor's tiers); any other load failure is a broken
	// configuration that must stop the process.
	modelsCfg, err := models.Load()
	if err != nil && !errors.Is(err, models.ErrNoConfig) {
		return nil, fmt.Errorf("load models config: %w", err)
	}
	adapter, adapterErr := executionAdapter(cfg, modelsCfg)
	if adapterErr != nil {
		return nil, adapterErr
	}

	queries := sqlc.New(dataStore.DB)

	// The execution model: pack source over a configured root, the resolved
	// execution adapter, per-task worktrees, the artifact revisions store,
	// the evidence manifest service, and the runner that composes them.
	packs := pack.NewDirSource(cfg.PacksDir)
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
		cfg: cfg, log: log, store: dataStore, adapter: adapter,
		artifacts: artifactStore, manifest: manifestService,
		api: apiInst, runner: runnerInst, worker: worker, reconciler: reconciler,
		pool: cfg.WorkerPoolSize,
	}, nil
}

// executionAdapter resolves the configured execution adapter through the
// registry (ADR 0005 D1) and validates the operator's model configuration
// against its descriptor (D4): an unknown adapter id — the registry error
// lists the known ones — and a tier resolving to options the adapter does not
// declare both stop the process at boot. The executor is named only inside
// internal/agent; this function works from ids.
func executionAdapter(cfg config.Config, modelsCfg *models.Config) (agent.Adapter, error) {
	registry := agent.NewRegistry(agent.RegistryOptions{
		DefaultAdapter: agent.AdapterID(cfg.ExecutionAdapter),
		RuntimeBinary:  cfg.RuntimeBinary,
	})
	resolved, err := registry.Resolve("")
	if err != nil {
		return nil, fmt.Errorf("select execution adapter: %w", err)
	}
	descriptor := resolved.Describe()
	if modelsCfg != nil {
		// Sorted, not map order: with two broken tiers the operator would
		// otherwise get a different one named on each boot, and "fix the
		// error, hit the next one" is a worse loop than it looks.
		tiers := make([]string, 0, len(modelsCfg.Tiers))
		for tier := range modelsCfg.Tiers {
			tiers = append(tiers, tier)
		}
		sort.Strings(tiers)
		for _, tier := range tiers {
			selection, resolveErr := models.Resolve(modelsCfg, descriptor.DefaultTiers, tier)
			if resolveErr != nil {
				return nil, fmt.Errorf("validate models config, tier %q: %w", tier, resolveErr)
			}
			if optionErr := selection.Options.SupportedBy(descriptor.ModelOptions); optionErr != nil {
				return nil, fmt.Errorf("validate models config, tier %q: execution adapter %q: %w", tier, descriptor.ID, optionErr)
			}
		}
	}
	return resolved, nil
}

// Handler returns the HTTP handler with the full middleware boundary applied.
// This is the single front door: the UI and every external caller use the same
// handler; nothing internal bypasses authz.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	server.registerRoutes(mux)
	return applyBoundary(mux, server.cfg, server.log)
}

// Run serves HTTP, the job worker, and the periodic reconciler until ctx is
// cancelled, then shuts down gracefully. The reconciler runs its first pass
// before the worker starts, so a crashed worker's stale jobs and any orphaned
// tasks are repaired before any new job is claimed.
func (server *Server) Run(ctx context.Context) error {
	// Warm the runtime probe before the worker starts (ADR 0005 D2): the
	// first task never pays the subprocess, and the boot log records the
	// runtime version — or the failure, which is a probe result, not a boot
	// failure; the run that needs the runtime surfaces it.
	server.warmRuntimeProbe(ctx)

	reconcilerCtx, cancelReconciler := context.WithCancel(ctx)
	defer cancelReconciler()
	go server.reconciler.Start(reconcilerCtx, jobs.DefaultReconcileInterval)

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	for workerIndex := 0; workerIndex < server.pool; workerIndex++ {
		go server.worker.Start(workerCtx)
	}

	httpServer := &http.Server{
		Addr:              server.cfg.HTTPAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	server.log.Info("http server listening", "addr", server.cfg.HTTPAddr, "workers", server.pool)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
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

// warmRuntimeProbe runs the adapter's memoized readiness probe once and logs
// the outcome. The descriptor supplies the id; no executor name is a literal
// here.
func (server *Server) warmRuntimeProbe(ctx context.Context) {
	descriptor := server.adapter.Describe()
	readiness := server.adapter.Probe(ctx)
	if readiness.Ready {
		server.log.Info("execution runtime ready",
			"adapter", string(descriptor.ID), "runtime_version", readiness.RuntimeVersion)
		return
	}
	server.log.Warn("execution runtime not ready",
		"adapter", string(descriptor.ID), "reason", readiness.Reason)
}

// runnerStore adapts *sqlc.Queries to the runner's Store interface (a typed
// subset). sqlc.Queries already satisfies the method set; this thin wrapper
// exists so the dependency is explicit and the runner stays decoupled.
type runnerStore struct{ *sqlc.Queries }
