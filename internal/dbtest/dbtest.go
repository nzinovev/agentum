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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/nzinovev/agentum/internal/store"
	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// databaseURLEnvVar names the DSN the harness connects to. It must point at a
// database whose user may create and drop databases (the compose Postgres and
// CI service both do).
const databaseURLEnvVar = "AGENTUM_TEST_DATABASE_URL"

// databaseNamePrefix marks every database this harness creates, so a later run
// can recognize what an interrupted one left behind.
const databaseNamePrefix = "agentum_test_"

// staleAfter is how old a leftover database must be before a later run drops
// it. It only has to exceed the lifetime of a live test database — seconds —
// while staying far away from the age of one a sibling `go test` process is
// using right now: `go test ./...` runs package binaries in parallel, and
// dropping their databases would fail tests that did nothing wrong.
const staleAfter = time.Hour

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
	// cannot be created or dropped from inside itself. It is opened together
	// with the template and stays open for the life of the process.
	adminConnection *sql.DB
	// templateName is empty until the one-time setup below has completed.
	templateName string

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

	// The cleanup is registered the moment the database exists, so a failure
	// anywhere below still drops it.
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
//
// The template then outlives every test in the binary. Dropping it once no test
// database is left would rebuild it — a full migration run — for the next test
// and every test after that, which is the cost the template exists to avoid;
// and a test binary has no exit hook to drop it in. So it is left behind, and
// swept by a later run instead.
func ensureTemplate(ctx context.Context, baseURL string) (string, error) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	if templateName != "" {
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

	dropStaleDatabases(ctx, admin)

	name := templateDatabaseName()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name))); err != nil {
		_ = admin.Close()
		return "", fmt.Errorf("create template database %s: %w", name, err)
	}
	dropTemplate := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name))

	templateURL, err := withDatabase(baseURL, name)
	if err != nil {
		_, _ = admin.ExecContext(ctx, dropTemplate)
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
	return name, nil
}

// dropStaleDatabases removes what an interrupted run left behind: Ctrl-C during
// `go test` runs no cleanup, so that run's databases outlive it, and every run
// leaves its template. Age is the only filter safe against a sibling `go test`
// process working in the same cluster right now.
//
// It is best-effort and deliberately silent: clearing someone else's leftovers
// must never be the reason a test run fails.
func dropStaleDatabases(ctx context.Context, admin *sql.DB) {
	rows, err := admin.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE starts_with(datname, $1)`, databaseNamePrefix)
	if err != nil {
		return
	}
	defer rows.Close()

	var staleNames []string
	for rows.Next() {
		var databaseName string
		if err := rows.Scan(&databaseName); err != nil {
			return
		}
		created, ok := creationTime(databaseName)
		if ok && time.Since(created) > staleAfter {
			staleNames = append(staleNames, databaseName)
		}
	}
	if rows.Err() != nil {
		return
	}

	for _, staleName := range staleNames {
		_, _ = admin.ExecContext(ctx,
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(staleName)))
	}
}

// execAdmin runs a cluster-level statement (CREATE/DROP DATABASE) on the admin
// connection. The lock is held only long enough to read the pool — the pool
// itself is safe to share, and holding the lock across the statement would
// serialize parallel tests on database creation.
func execAdmin(ctx context.Context, query string) error {
	stateMutex.Lock()
	admin := adminConnection
	stateMutex.Unlock()
	if admin == nil {
		return errors.New("database admin connection is not open")
	}
	_, err := admin.ExecContext(ctx, query)
	return err
}

// templateDatabaseName and nextDatabaseName build names that are unique across
// processes and carry the time they were created, in Unix seconds: Postgres
// does not record when a database was created, and the sweep above needs an age
// to tell an abandoned database from one that is in use.
func templateDatabaseName() string {
	return fmt.Sprintf("%s%d_%d_template", databaseNamePrefix, time.Now().Unix(), os.Getpid())
}

func nextDatabaseName() string {
	return fmt.Sprintf("%s%d_%d_%d", databaseNamePrefix,
		time.Now().Unix(), os.Getpid(), databaseCounter.Add(1))
}

// creationTime reads back the timestamp the two names above encode. A name that
// does not parse is reported as unknown and therefore never dropped: whatever
// it is, this harness did not make it.
func creationTime(databaseName string) (time.Time, bool) {
	fields := strings.Split(strings.TrimPrefix(databaseName, databaseNamePrefix), "_")
	if len(fields) != 3 {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
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

// quoteIdent quotes a database name for interpolation into DDL, doubling an
// embedded quote the way Postgres expects. Names here come from a fixed
// alphabet of letters, digits, and underscores, so this guards the construction
// rather than escaping anything a caller supplied.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
