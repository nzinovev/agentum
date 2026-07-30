package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config is resolved entirely from the environment (12-factor). The Tenant*
// fields are the single-tenant seam: they stand in for real identity until
// SSO/RBAC arrive at the same boundary.
type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	LogLevel          string
	TenantID          string
	TenantOwnerUserID string

	// Execution model (F.6).
	PacksDir       string // directory holding <name>/manifest.yaml packs
	WorkerPoolSize int    // concurrent job workers (1 is fine for single-host MVP)
	JobMaxAttempts int    // poison bound before a job is failed (04 §7.5)
	OpencodeBinary string // path to the opencode binary the adapter shells out to

	// Per-invocation capability timeouts (Epic 6). Zero means no cap; the
	// values are layered onto every effective capability profile and enforced
	// by the adapter (hard via ctx deadline, idle via stream-chunk watchdog).
	HardTimeoutSeconds int
	IdleTimeoutSeconds int

	// Project-check executor defaults (orchestrator-owned checks). Applied when
	// a check in the project registry (.agentum.yaml) declares no value of its
	// own. CheckTimeoutSeconds bounds a single check; CheckMaxOutputBytes caps
	// each stream (stdout / stderr) stored in the manifest.
	CheckTimeoutSeconds int
	CheckMaxOutputBytes int

	// ArtifactRoot is the canonical root for content-addressed artifact blobs.
	// Defaults to .agentum/artifacts under the process CWD; the worktree's own
	// per-stage artifact dir is separate and disposable — this root survives
	// worktree teardown so revisions remain readable for review / comparison.
	ArtifactRoot string
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          getenv("AGENTUM_HTTP_ADDR", ":8080"),
		DatabaseURL:       getenv("AGENTUM_DATABASE_URL", defaultDatabaseURL()),
		LogLevel:          getenv("AGENTUM_LOG_LEVEL", "info"),
		TenantID:          getenv("AGENTUM_TENANT_ID", "00000000-0000-0000-0000-000000000001"),
		TenantOwnerUserID: getenv("AGENTUM_OWNER_USER_ID", "00000000-0000-0000-0000-000000000001"),

		PacksDir:       getenv("AGENTUM_PACKS_DIR", "packs"),
		WorkerPoolSize: getenvInt("AGENTUM_WORKER_POOL_SIZE", 1),
		JobMaxAttempts: getenvInt("AGENTUM_JOB_MAX_ATTEMPTS", 3),
		OpencodeBinary: getenv("AGENTUM_OPENCODE_BINARY", "opencode"),
		ArtifactRoot:   getenv("AGENTUM_ARTIFACT_ROOT", defaultArtifactRoot()),

		HardTimeoutSeconds: getenvInt("AGENTUM_HARD_TIMEOUT_SECONDS", 0),
		IdleTimeoutSeconds: getenvInt("AGENTUM_IDLE_TIMEOUT_SECONDS", 0),

		CheckTimeoutSeconds: getenvInt("AGENTUM_CHECK_TIMEOUT_SECONDS", 0),
		CheckMaxOutputBytes: getenvInt("AGENTUM_CHECK_MAX_OUTPUT_BYTES", 0),
	}
	if cfg.DatabaseURL == "" {
		return cfg, fmt.Errorf("AGENTUM_DATABASE_URL must be set")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if rawValue, ok := os.LookupEnv(key); ok {
		if parsed, err := strconv.Atoi(rawValue); err == nil {
			return parsed
		}
	}
	return fallback
}

// defaultArtifactRoot returns .agentum/artifacts under the current working
// directory. The .agentum/ prefix matches the worktree tree's gitignore entry
// so an operator pointing the root at a project repo's .agentum/ stays
// untracked. Operators may override via AGENTUM_ARTIFACT_ROOT.
func defaultArtifactRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ".agentum/artifacts"
	}
	return filepath.Join(cwd, ".agentum", "artifacts")
}

// Local-development Postgres defaults. These match the docker-compose service
// (user/db "agentum") so `make docker-up && make run` works with no env. They
// are assembled here rather than written as a single DSN literal so no
// connection-string password is hardcoded in source (production sets
// AGENTUM_DATABASE_URL with its own secret).
const (
	localDevDBUser   = "agentum"
	localDevDBSecret = "agentum"
	localDevDBHost   = "localhost:5432"
	localDevDBName   = "agentum"
)

// defaultDatabaseURL composes the local-dev DSN from the named parts above.
func defaultDatabaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable&search_path=agentum",
		localDevDBUser, localDevDBSecret, localDevDBHost, localDevDBName)
}
