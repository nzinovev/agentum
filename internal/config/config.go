package config

import (
	"fmt"
	"os"
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
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:          getenv("AGENTUM_HTTP_ADDR", ":8080"),
		DatabaseURL:       getenv("AGENTUM_DATABASE_URL", "postgres://agentum:agentum@localhost:5432/agentum?sslmode=disable&search_path=agentum"),
		LogLevel:          getenv("AGENTUM_LOG_LEVEL", "info"),
		TenantID:          getenv("AGENTUM_TENANT_ID", "00000000-0000-0000-0000-000000000001"),
		TenantOwnerUserID: getenv("AGENTUM_OWNER_USER_ID", "00000000-0000-0000-0000-000000000001"),

		PacksDir:       getenv("AGENTUM_PACKS_DIR", "packs"),
		WorkerPoolSize: getenvInt("AGENTUM_WORKER_POOL_SIZE", 1),
		JobMaxAttempts: getenvInt("AGENTUM_JOB_MAX_ATTEMPTS", 3),
		OpencodeBinary: getenv("AGENTUM_OPENCODE_BINARY", "opencode"),
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
