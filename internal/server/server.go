package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nzinovev/agentum/internal/api"
	"github.com/nzinovev/agentum/internal/config"
	"github.com/nzinovev/agentum/internal/store"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

type Server struct {
	cfg   config.Config
	log   *slog.Logger
	store *store.Store
	api   *api.API
}

func New(cfg config.Config, log *slog.Logger, st *store.Store) *Server {
	a := api.New(sqlc.New(st.DB), log)
	return &Server{cfg: cfg, log: log, store: st, api: a}
}

// Handler returns the HTTP handler with the full middleware boundary applied.
// This is the single front door: the UI and every external caller use the same
// handler; nothing internal bypasses authz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return applyBoundary(mux, s.cfg, s.log)
}

// Run serves HTTP until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	s.log.Info("http server listening", "addr", s.cfg.HTTPAddr)
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
