package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// schemaName is the single Postgres schema all Agentum objects live in, created
// before the first migration runs. The connection's search_path (set via the
// DSN) must resolve to this schema so unqualified table names land here.
const schemaName = "agentum"

// Store is the Postgres access point. State lives in Postgres; artifacts live
// on local FS behind an object-storage interface, wired with the artifact work.
type Store struct {
	DB *sql.DB
}

// Open connects to Postgres (pgx under database/sql), configures the pool, and
// applies migrations before returning.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	s := &Store{DB: db}
	if err := s.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// ensureSchema creates the application schema if it does not yet exist. It must
// run before goose: goose needs somewhere to write its version table, and that
// table belongs in the same schema as everything else.
func (s *Store) ensureSchema(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schemaName))
	if err != nil {
		return fmt.Errorf("ensure schema %s: %w", schemaName, err)
	}
	return nil
}

// Migrate applies the embedded goose migrations. The app auto-migrates on boot,
// so a separate goose CLI step is optional.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(embed.FS{})
	if err := goose.UpContext(ctx, s.DB, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

// Ping verifies connectivity (used by the readiness endpoint).
func (s *Store) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.DB.PingContext(pingCtx)
}
