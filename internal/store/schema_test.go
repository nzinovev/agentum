package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMigrationSeamColumnsAreUUID is the static guard over the tenant/user
// seam: every tenant_id and user_id column must end up uuid. One text-typed
// tenant column breaks every future join and every future FK on the tenant
// (an implicit cast in a JOIN kills the index), and the defect is cheapest to
// catch in the migration file that introduces it. The check reads the
// migration sources only — no database needed — so it runs on every
// `go test ./...`.
//
// A column's type is whatever its LAST mention in migration order says:
// a declaration in CREATE TABLE or ADD COLUMN states it, and ALTER COLUMN …
// TYPE restates it. This lets a later migration legitimately correct an
// earlier shape (task_approvals.tenant_id was text and was cast to uuid)
// while still failing on any new declaration or alteration that leaves a
// seam column non-uuid. Only the Up side of each migration is scanned: a
// Down that restores an older shape is legitimate history, not a new
// declaration.
func TestMigrationSeamColumnsAreUUID(t *testing.T) {
	t.Parallel()

	// The test binary runs with the package directory as its working
	// directory, and migrations live beside it.
	migrationsDir := "migrations"
	migrationFiles, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil || len(migrationFiles) == 0 {
		t.Fatalf("find migrations in %s: %v", migrationsDir, err)
	}

	// The three grammatical shapes a column type appears in. Whitespace and
	// word anchors keep mentions in WHERE clauses, index definitions, and
	// ::casts from looking like declarations.
	declareInCreatePattern := regexp.MustCompile(`(?i)^\s*(tenant_id|user_id)\s+([a-z][a-z0-9_]*)`)
	addColumnPattern := regexp.MustCompile(`(?i)ADD\s+COLUMN\s+(tenant_id|user_id)\s+([a-z][a-z0-9_]*)`)
	alterTypePattern := regexp.MustCompile(`(?i)ALTER\s+COLUMN\s+(tenant_id|user_id)\s+TYPE\s+([a-z][a-z0-9_]*)`)

	// latestTypeByColumn holds the last type each seam column was declared or
	// altered to, across all migrations in order.
	latestTypeByColumn := map[string]string{}
	noteMention := func(match []string) {
		if match != nil {
			latestTypeByColumn[strings.ToLower(match[1])] = strings.ToLower(match[2])
		}
	}

	for _, migrationFile := range migrationFiles {
		raw, readErr := os.ReadFile(migrationFile)
		if readErr != nil {
			t.Fatalf("read %s: %v", migrationFile, readErr)
		}
		// Keep the Up side only; the first Down marker ends it.
		upSide, _, _ := strings.Cut(string(raw), "-- +goose Down")
		for _, line := range strings.Split(upSide, "\n") {
			trimmedLine := strings.TrimSpace(line)
			if strings.HasPrefix(trimmedLine, "--") {
				continue
			}
			noteMention(addColumnPattern.FindStringSubmatch(line))
			noteMention(alterTypePattern.FindStringSubmatch(line))
			noteMention(declareInCreatePattern.FindStringSubmatch(line))
		}
	}

	for column, columnType := range latestTypeByColumn {
		if columnType != "uuid" {
			t.Errorf("seam column %s ends up %q; it must be uuid in every table", column, columnType)
		}
	}
}
