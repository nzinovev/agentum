package store_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/store"
)

// expectedTables is every table the full migration set creates in the agentum
// schema. A migration that adds or removes a table updates this list in the
// same change, so the up/down cycle below keeps covering the schema as it
// evolves.
var expectedTables = []string{
	"artifact_revisions",
	"events",
	"jobs",
	"memory_entries",
	"projects",
	"stage_invocations",
	"task_approvals",
	"task_checkpoints",
	"task_manifest_corrections",
	"task_manifests",
	"tasks",
}

// gooseVersionTable is goose's own bookkeeping. A down-to-zero removes its
// rows but not the table, so "every migration rolled back" must mean "every
// table except this one is gone".
const gooseVersionTable = "goose_db_version"

// TestMigrations_UpDownUp exercises, in one pass: the connection and the DSN
// with its search_path, the embedded migrations applied through the production
// entrypoint (store.Open), the Down side of every migration — which nothing
// else runs — and their re-application on a database that has just seen its
// Down.
func TestMigrations_UpDownUp(t *testing.T) {
	ctx := context.Background()
	handle := dbtest.Store(t)

	// The test database is a file copy of a template migrated via store.Open;
	// assert the copy really carries the full schema.
	requireTables(t, handle.Store.DB, expectedTables)

	if err := handle.Store.MigrateDownTo(ctx, 0); err != nil {
		t.Fatalf("migrate down to zero: %v", err)
	}
	requireNoApplicationTables(t, handle.Store.DB)

	reopenedStore, err := store.Open(ctx, handle.URL)
	if err != nil {
		t.Fatalf("re-open through store.Open after a full down: %v", err)
	}
	defer reopenedStore.Close()
	requireTables(t, reopenedStore.DB, expectedTables)
}

// TestDBTest_IsolatesDatabases proves the property every other database test
// stands on: each test's database is its own. Both subtests insert the same
// primary key — in a shared database the second insert would collide and the
// counts would drift, so each seeing exactly its one row is the proof.
func TestDBTest_IsolatesDatabases(t *testing.T) {
	const insertProbeProject = `
		INSERT INTO projects (id, tenant_id, user_id, repo_path, name, related_projects)
		VALUES (
		    '0ac58e85-8a51-4971-9b53-bdfec0a80b01',
		    '1bd6f9a6-9c62-4a08-b3d4-ce5fd1b91c02',
		    '2ce7c0b7-ad73-4b19-a4e5-df60e2ca2d03',
		    '/same/repo/path',
		    'isolation probe',
		    '{}')`

	isolatedWrites := []struct{ name string }{
		{name: "first writer"},
		{name: "second writer"},
	}
	for _, isolatedWrite := range isolatedWrites {
		t.Run(isolatedWrite.name, func(t *testing.T) {
			handle := dbtest.Store(t)

			if _, err := handle.Store.DB.ExecContext(context.Background(), insertProbeProject); err != nil {
				t.Fatalf("insert probe row: %v", err)
			}
			var projectCount int
			if err := handle.Store.DB.QueryRowContext(context.Background(),
				`SELECT count(*) FROM projects`).Scan(&projectCount); err != nil {
				t.Fatalf("count projects: %v", err)
			}
			if projectCount != 1 {
				t.Fatalf("projects visible in this test's database: got %d, want 1", projectCount)
			}
		})
	}
}

// requireTables asserts the agentum schema holds exactly the wanted tables
// (plus goose's version table, which lives there too).
func requireTables(t *testing.T, database *sql.DB, wantedTables []string) {
	t.Helper()
	gotTables := applicationTables(t, database)
	if !slices.Equal(gotTables, wantedTables) {
		t.Fatalf("tables in schema agentum: got %v, want %v", gotTables, wantedTables)
	}
}

// requireNoApplicationTables asserts nothing but goose's version table is left.
func requireNoApplicationTables(t *testing.T, database *sql.DB) {
	t.Helper()
	if remainingTables := applicationTables(t, database); len(remainingTables) > 0 {
		t.Fatalf("tables survived a down-to-zero: %v", remainingTables)
	}
}

// applicationTables lists the schema's tables in name order, minus goose's own.
func applicationTables(t *testing.T, database *sql.DB) []string {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'agentum'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables in schema agentum: %v", err)
	}
	defer rows.Close()

	var tableNames []string
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		if tableName != gooseVersionTable {
			tableNames = append(tableNames, tableName)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables in schema agentum: %v", err)
	}
	return tableNames
}
