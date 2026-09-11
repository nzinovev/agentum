package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nzinovev/agentum/internal/config"
	"github.com/nzinovev/agentum/internal/server"
	"github.com/nzinovev/agentum/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	// The process logger is also the default: package-level slog calls (the
	// adapters' probe warnings) must land in the structured stream, not in
	// the stdlib's text fallback on stderr.
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("close store", "error", err)
		}
	}()

	srv, err := server.New(cfg, log, st)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
