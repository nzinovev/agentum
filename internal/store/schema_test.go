package store_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The static guard over the tenant/user seam: every tenant_id and user_id
// column must end up uuid. One text-typed tenant column breaks every future
// join and every future FK on the tenant (an implicit cast in a JOIN kills
// the index), and the defect is cheapest to catch in the migration file that
// introduces it. The check reads the migration sources only — no database
// needed — so it runs on every `go test ./...`.

var (
	// A column declaration inside CREATE TABLE: the seam column name starts
	// the line, followed by its type.
	declareInCreatePattern = regexp.MustCompile(`(?i)^\s*(tenant_id|user_id)\s+([a-z][a-z0-9_]*)`)
	// CREATE TABLE [IF NOT EXISTS] <name> — switches the current table.
	createTablePattern = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z0-9_.]+)`)
	// ALTER TABLE [ONLY] <name> — switches the current table for the
	// statements that follow.
	alterTablePattern = regexp.MustCompile(`(?i)ALTER\s+TABLE\s+(?:ONLY\s+)?([a-z0-9_.]+)`)
	// ADD COLUMN <seam> <type> — a declaration mid-statement.
	addColumnPattern = regexp.MustCompile(`(?i)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(tenant_id|user_id)\s+([a-z][a-z0-9_]*)`)
	// ALTER COLUMN <seam> TYPE <type> — restates an existing column's type.
	alterTypePattern = regexp.MustCompile(`(?i)ALTER\s+COLUMN\s+(tenant_id|user_id)\s+TYPE\s+([a-z][a-z0-9_]*)`)
)

// seamColumnTypes returns, for every "<table>.<column>" seam declaration, the
// type the column ends up with across the given Up-side migration bodies (in
// application order), plus the index of the body that declared that type
// last. The caller maps the index to a file name for the failure message.
//
// A column's type is whatever its LAST declaration says: a line in CREATE
// TABLE or an ADD COLUMN states it, and ALTER COLUMN … TYPE restates it. This
// lets a later migration legitimately correct an earlier shape
// (task_approvals.tenant_id was text and was cast to uuid) while still
// failing on any declaration or alteration that leaves a seam column
// non-uuid. Mentions in UNIQUE (...), PRIMARY KEY (...), WHERE clauses, and
// ::casts never put a type word after the bare column name, so they cannot
// look like declarations. Per-table keying is the point: with one shared key,
// every new table's uuid declaration would mask a text column left in an
// older one, and the guard would degrade into "the last migration is fine".
func seamColumnTypes(upSides []string) (columnTypes map[string]string, lastDeclaredIn map[string]int) {
	columnTypes = map[string]string{}
	lastDeclaredIn = map[string]int{}
	for bodyIndex, body := range upSides {
		// The table a seam declaration belongs to. CREATE TABLE and ALTER
		// TABLE switch it; a bare column line only appears inside one of
		// them, so a declaration outside any table statement is a parse bug,
		// not a state to attribute.
		currentTable := ""
		for _, line := range strings.Split(body, "\n") {
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "--") {
				continue
			}
			if match := createTablePattern.FindStringSubmatch(line); match != nil {
				currentTable = strings.Trim(match[1], `"`)
			}
			if match := alterTablePattern.FindStringSubmatch(line); match != nil {
				currentTable = strings.Trim(match[1], `"`)
			}
			noteDeclaration := func(match []string) {
				if match == nil || currentTable == "" {
					return
				}
				key := currentTable + "." + strings.ToLower(match[1])
				columnTypes[key] = strings.ToLower(match[2])
				lastDeclaredIn[key] = bodyIndex
			}
			noteDeclaration(addColumnPattern.FindStringSubmatch(line))
			noteDeclaration(alterTypePattern.FindStringSubmatch(line))
			noteDeclaration(declareInCreatePattern.FindStringSubmatch(line))
		}
	}
	return columnTypes, lastDeclaredIn
}

// TestSeamColumnTypes pins the parser's contract on inline SQL, so a false
// pass or a false fail is diagnosable without touching real migrations.
func TestSeamColumnTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		upSides []string
		want    map[string]string
	}{
		{
			// A uuid declaration in a LATER table must not mask a text column
			// left in an earlier one — the failure this per-table keying
			// exists to catch.
			name: "text column is not masked by a later uuid table",
			upSides: []string{
				"CREATE TABLE probe_bad (\n    tenant_id text NOT NULL\n);\nCREATE TABLE probe_good (\n    tenant_id uuid NOT NULL\n);",
			},
			want: map[string]string{"probe_bad.tenant_id": "text", "probe_good.tenant_id": "uuid"},
		},
		{
			name: "alter type corrects an earlier declaration in the same table",
			upSides: []string{
				"CREATE TABLE probe (\n    tenant_id text NOT NULL\n);",
				"ALTER TABLE probe ALTER COLUMN tenant_id TYPE uuid USING tenant_id::uuid;",
			},
			want: map[string]string{"probe.tenant_id": "uuid"},
		},
		{
			name: "non-declaration mentions do not count",
			upSides: []string{
				"CREATE TABLE probe (\n    id uuid PRIMARY KEY,\n    tenant_id uuid NOT NULL,\n    UNIQUE (tenant_id, repo_path),\n    PRIMARY KEY (tenant_id, user_id)\n);\n" +
					"CREATE UNIQUE INDEX idx ON probe(tenant_id, user_id);\n" +
					"UPDATE probe SET x = 1 WHERE tenant_id = 'y' AND user_id::text = 'z';",
			},
			// user_id is only MENTIONED (PRIMARY KEY, index, WHERE, cast),
			// never declared — so it must not appear in the map at all,
			// which is exactly what keeps mentions from masking or
			// inventing declarations.
			want: map[string]string{"probe.tenant_id": "uuid"},
		},
		{
			name: "add column is a declaration",
			upSides: []string{
				"ALTER TABLE probe ADD COLUMN user_id uuid NOT NULL;",
			},
			want: map[string]string{"probe.user_id": "uuid"},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, _ := seamColumnTypes(testCase.upSides)
			if !maps.Equal(got, testCase.want) {
				t.Fatalf("seamColumnTypes = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestMigrationSeamColumnsAreUUID runs the guard over the real migrations.
// Only the Up side of each migration is scanned: a Down that restores an
// older (pre-uuid) shape is legitimate history, not a new declaration.
func TestMigrationSeamColumnsAreUUID(t *testing.T) {
	t.Parallel()

	// The test binary runs with the package directory as its working
	// directory, and migrations live beside it.
	migrationsDir := "migrations"
	migrationFiles, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil || len(migrationFiles) == 0 {
		t.Fatalf("find migrations in %s: %v", migrationsDir, err)
	}

	var upSides []string
	for _, migrationFile := range migrationFiles {
		raw, readErr := os.ReadFile(migrationFile)
		if readErr != nil {
			t.Fatalf("read %s: %v", migrationFile, readErr)
		}
		upSide, _, _ := strings.Cut(string(raw), "-- +goose Down")
		upSides = append(upSides, upSide)
	}

	columnTypes, lastDeclaredIn := seamColumnTypes(upSides)
	for column, columnType := range maps.All(columnTypes) {
		if columnType != "uuid" {
			t.Errorf("%s ends up %q (declared last in %s); it must be uuid in every table",
				column, columnType, filepath.Base(migrationFiles[lastDeclaredIn[column]]))
		}
	}
}
