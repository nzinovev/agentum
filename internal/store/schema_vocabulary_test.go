package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/dbtest"
)

// TestSchemaVocabularyHasNoTaskToken is the schema-side vocabulary guard. The
// launch entity is a run at every layer, so after the physical rename no
// relation, column, index, or constraint may carry the old word — the next
// migration that reintroduces it (a new table, a new column named after the
// old vocabulary) fails here, naming the exact object.
//
// The guard reads the RESULTING schema (information_schema + pg_catalog), not
// the migration sources: early migrations legitimately still contain the old
// names as the left side of their renames, so a source-text scan would be red
// forever. Only what the database actually ends up with is checkable.
//
// The reserved token is matched whole, split on underscores (and the dot that
// separates a column from its table): "task_id" and "idx_tasks_state" carry
// it, "multitaskery" would not.
func TestSchemaVocabularyHasNoTaskToken(t *testing.T) {
	handle := dbtest.Store(t)
	ctx := context.Background()

	type schemaObject struct {
		kind string // table | column | index | constraint
		name string
	}

	var offenders []schemaObject
	noteOffender := func(kind, name string) {
		tokens := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '.' })
		for _, token := range tokens {
			if token == "task" || token == "tasks" {
				offenders = append(offenders, schemaObject{kind: kind, name: name})
				return
			}
		}
	}

	rows, err := handle.Store.DB.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'agentum' AND table_name <> 'goose_db_version'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for rows.Next() {
		var tableName string
		if err := rows.Scan(&tableName); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		noteOffender("table", tableName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	rows.Close()

	rows, err = handle.Store.DB.QueryContext(ctx, `
		SELECT table_name || '.' || column_name
		FROM information_schema.columns
		WHERE table_schema = 'agentum'
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	for rows.Next() {
		var columnPath string
		if err := rows.Scan(&columnPath); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		noteOffender("column", columnPath)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	rows.Close()

	rows, err = handle.Store.DB.QueryContext(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = 'agentum'
		ORDER BY indexname`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	for rows.Next() {
		var indexName string
		if err := rows.Scan(&indexName); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		noteOffender("index", indexName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}
	rows.Close()

	rows, err = handle.Store.DB.QueryContext(ctx, `
		SELECT conname FROM pg_constraint
		WHERE connamespace = 'agentum'::regnamespace
		ORDER BY conname`)
	if err != nil {
		t.Fatalf("list constraints: %v", err)
	}
	for rows.Next() {
		var constraintName string
		if err := rows.Scan(&constraintName); err != nil {
			t.Fatalf("scan constraint name: %v", err)
		}
		noteOffender("constraint", constraintName)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate constraints: %v", err)
	}
	rows.Close()

	for _, offender := range offenders {
		t.Errorf("%s %q carries the reserved token; the schema vocabulary word is \"runs\"",
			offender.kind, offender.name)
	}
}
