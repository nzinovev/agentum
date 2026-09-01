// Package dbtest is the Postgres-backed test harness: one database per test,
// copied from a template that was migrated through store.Open — the same road
// the application walks on boot, not a second copy of migration logic.
//
// The DSN comes from AGENTUM_TEST_DATABASE_URL (`make test-db` sets it). When
// the variable is unset the harness skips on a developer machine — a
// convenience, since Postgres may not be running — but fails under CI (the CI
// env var): there a skipped database test would be a lost check nobody notices.
package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/nzinovev/agentum/internal/store"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// databaseURLEnvVar names the DSN the harness connects to. It must point at a
// database whose user may create and drop databases (the compose Postgres and
// CI service both do).
const databaseURLEnvVar = "AGENTUM_TEST_DATABASE_URL"

// Handle is one test's database: the open store, sqlc queries over it, and the
// URL, so a test can tear the schema down and re-open through the production
// path (the migration-cycle test does exactly that).
type Handle struct {
	Store   *store.Store
	Queries *sqlc.Queries
	URL     string
}

var (
	stateMutex sync.Mutex
	// adminConnection talks to the database named in the DSN itself: a database
	// cannot be created or dropped from inside itself. It is opened when the
	// template is first needed and closed when the last test database is gone.
	adminConnection *sql.DB
	templateName    string
	templateReady   bool
	// openTestDatabases counts Store handles not yet cleaned up; the last one
	// out drops the template and closes the admin connection, so a normal test
	// run leaves nothing behind.
	openTestDatabases int

	databaseCounter atomic.Int64
)

// Store returns a database for this test alone: created as a file copy of the
// migrated template (cheaper than re-running migrations), dropped when the
// test ends. Tests therefore cannot see each other's rows and need no cleanup
// ordering between themselves.
func Store(t *testing.T) *Handle {
	t.Helper()

	baseURL := requireDatabaseURL(t)
	ctx := context.Background()

	template, err := ensureTemplate(ctx, baseURL)
	if err != nil {
		t.Fatalf("prepare database template: %v", err)
	}

	databaseName := nextDatabaseName()
	if err := execAdmin(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s",
		quoteIdent(databaseName), quoteIdent(template))); err != nil {
		t.Fatalf("create test database %s: %v", databaseName, err)
	}
	retainTemplate()

	// The cleanup is registered the moment the database exists, so a failure
	// anywhere below still drops it and releases the template.
	var testStore *store.Store
	t.Cleanup(func() {
		if testStore != nil {
			_ = testStore.Close()
		}
		// WITH (FORCE): a test that leaked a connection must not wedge the
		// drop for every test that follows.
		if err := execAdmin(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)",
			quoteIdent(databaseName))); err != nil {
			t.Errorf("drop test database %s: %v", databaseName, err)
		}
		if err := releaseTemplate(ctx); err != nil {
			t.Errorf("release database template: %v", err)
		}
	})

	databaseURL, err := withDatabase(baseURL, databaseName)
	if err != nil {
		t.Fatalf("build test database url: %v", err)
	}

	// store.Open runs goose up on the copy; at the template's version that is
	// a no-op, which is also the proof that goose bookkeeping survived the
	// file copy.
	testStore, err = store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database %s: %v", databaseName, err)
	}

	return &Handle{Store: testStore, Queries: sqlc.New(testStore.DB), URL: databaseURL}
}

// requireDatabaseURL returns the DSN or stops the test: skip where a developer
// may not have Postgres running, fatal in CI where the same skip would read as
// a green run without the check.
func requireDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv(databaseURLEnvVar)
	if databaseURL != "" {
		return databaseURL
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("%s is not set: CI must run database tests, not skip them (see make test-db)",
			databaseURLEnvVar)
	}
	t.Skipf("%s is not set; run `make test-db` to run Postgres-backed tests", databaseURLEnvVar)
	return ""
}

// ensureTemplate creates, at most once per process, the database every test is
// copied from: empty, then migrated through store.Open, then closed — copying
// with TEMPLATE requires that nothing stay connected to the source.
func ensureTemplate(ctx context.Context, baseURL string) (string, error) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	if templateReady {
		return templateName, nil
	}

	connectionConfig, err := pgx.ParseConfig(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", databaseURLEnvVar, err)
	}
	admin := stdlib.OpenDB(*connectionConfig)
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		return "", fmt.Errorf("connect via %s: %w", databaseURLEnvVar, err)
	}

	name := fmt.Sprintf("agentum_test_template_%d", os.Getpid())
	// A crashed earlier run may have left this name behind and PIDs recycle;
	// claim the name instead of failing on the leftover.
	dropTemplate := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name))
	if _, err := admin.ExecContext(ctx, dropTemplate); err != nil {
		_ = admin.Close()
		return "", fmt.Errorf("reclaim template database %s: %w", name, err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))); err != nil {
		_ = admin.Close()
		return "", fmt.Errorf("create template database %s: %w", name, err)
	}

	templateURL, err := withDatabase(baseURL, name)
	if err != nil {
		_ = admin.Close()
		return "", err
	}
	templateStore, err := store.Open(ctx, templateURL)
	if err != nil {
		_, _ = admin.ExecContext(ctx, dropTemplate)
		_ = admin.Close()
		return "", fmt.Errorf("migrate template database %s: %w", name, err)
	}
	if err := templateStore.Close(); err != nil {
		_, _ = admin.ExecContext(ctx, dropTemplate)
		_ = admin.Close()
		return "", fmt.Errorf("close template database %s: %w", name, err)
	}

	adminConnection = admin
	templateName = name
	templateReady = true
	return name, nil
}

// retainTemplate accounts for one more live test database.
func retainTemplate() {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	openTestDatabases++
}

// releaseTemplate accounts for one fewer live test database and, for the last
// one, drops the template and closes the admin connection.
func releaseTemplate(ctx context.Context) error {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	openTestDatabases--
	if openTestDatabases > 0 || !templateReady {
		return nil
	}
	droppedName := templateName
	_, dropErr := adminConnection.ExecContext(ctx,
		fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(droppedName)))
	closeErr := adminConnection.Close()
	adminConnection = nil
	templateName = ""
	templateReady = false
	if dropErr != nil {
		return fmt.Errorf("drop template database %s: %w", droppedName, dropErr)
	}
	return closeErr
}

// execAdmin runs a cluster-level statement (CREATE/DROP DATABASE) on the admin
// connection.
func execAdmin(ctx context.Context, query string) error {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	if adminConnection == nil {
		return errors.New("database admin connection is not open")
	}
	_, err := adminConnection.ExecContext(ctx, query)
	return err
}

// nextDatabaseName builds a unique per-test database name: per-process template
// names collide only if PIDs recycle after a crash, and the counter keeps
// tests within one process apart.
func nextDatabaseName() string {
	return fmt.Sprintf("agentum_test_%d_%d", os.Getpid(), databaseCounter.Add(1))
}

// withDatabase rewrites only the database name, preserving the parameters the
// caller wrote (sslmode, search_path) exactly as they are. It requires a URL
// form DSN: the name is the path component, and a keyword/value DSN has no
// place to rewrite.
func withDatabase(baseURL string, databaseName string) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", databaseURLEnvVar, err)
	}
	if parsedURL.Scheme == "" {
		return "", fmt.Errorf("%s must be a postgres:// URL: %q", databaseURLEnvVar, baseURL)
	}
	parsedURL.Path = "/" + databaseName
	return parsedURL.String(), nil
}

// quoteIdent quotes a database name for interpolation into DDL. Names here come
// from a fixed alphabet of letters, digits, and underscores, where Go's %q
// matches Postgres identifier quoting.
func quoteIdent(name string) string {
	return fmt.Sprintf("%q", name)
}
