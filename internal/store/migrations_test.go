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
		INSERT INTO projects (id, tenant_id, user_id, repo_identity, repo_root_commits, repo_path, name, related_projects)
		VALUES (
		    '0ac58e85-8a51-4971-9b53-bdfec0a80b01',
		    '1bd6f9a6-9c62-4a08-b3d4-ce5fd1b91c02',
		    '2ce7c0b7-ad73-4b19-a4e5-df60e2ca2d03',
		    'git-roots:v1:probe',
		    '{a233383e18550c5974d09c6b36ae62fe1c7e9a1a}',
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

// TestTenantJoinApprovalsToTasksNeedsNoCast pins the tenant seam's type
// uniformity the way a real consumer hits it: the query decisions-inbox style
// screens run joins approvals to tasks on the tenant with no cast. On a schema
// where one side is text and the other uuid, Postgres rejects the bare
// comparison outright — so the query succeeding IS the check.
func TestTenantJoinApprovalsToTasksNeedsNoCast(t *testing.T) {
	handle := dbtest.Store(t)
	ctx := context.Background()

	const tenantID = "3ad5e0b1-64c1-4e30-9f0e-2b1c9d8a7e10"
	const userID = "4be6f1c2-75d2-4f41-8a1f-3c2d0e9b8f21"
	const taskID = "5cf702d3-86e3-4a52-9b20-4d3e1f0c9a32"

	if _, err := handle.Store.DB.ExecContext(ctx, `
		INSERT INTO projects (id, tenant_id, user_id, repo_identity, repo_root_commits, repo_path, name)
		VALUES ('00000000-0000-0000-0000-000000000001', $1, $2,
		        'git-roots:v1:join-probe', '{a233383e18550c5974d09c6b36ae62fe1c7e9a1a}',
		        '/join/probe', 'join probe')`,
		tenantID, userID); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := handle.Store.DB.ExecContext(ctx, `
		INSERT INTO tasks (id, tenant_id, user_id, project_id, pipeline_pack,
		                   title, description, overrides, base_ref, state)
		VALUES ($1, $2, $3, '00000000-0000-0000-0000-000000000001', 'test@1',
		        'join probe', 'probe', '{}', 'HEAD', 'created')`,
		taskID, tenantID, userID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := handle.Store.DB.ExecContext(ctx, `
		INSERT INTO task_approvals (tenant_id, user_id, task_id, name, decision, actor)
		VALUES ($1, $2, $3, 'final_review', 'approved', 'human')`,
		tenantID, userID, taskID); err != nil {
		t.Fatalf("insert approval: %v", err)
	}

	var joinedCount int
	if err := handle.Store.DB.QueryRowContext(ctx,
		`SELECT count(*) FROM task_approvals a JOIN tasks t ON t.tenant_id = a.tenant_id`,
	).Scan(&joinedCount); err != nil {
		t.Fatalf("join approvals to tasks on the tenant: %v", err)
	}
	if joinedCount != 1 {
		t.Fatalf("joined rows = %d, want 1", joinedCount)
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
