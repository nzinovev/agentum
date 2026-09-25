package publish

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// forbiddenImports names the packages a provider implementation must not
// reach. Without the store it reads no database; without the worktree
// manager it owns no branch; without the agent package it invokes no
// executor; without the runner it reaches no stage loop; without the
// manifest it writes no evidence; without the publication coordinator there
// is no path back into the caller. The boundary is what makes a provider
// unable to rewrite a checkpoint commit or delete a delivery branch, and a
// test is what makes the boundary survive the next convenient import.
var forbiddenImports = map[string]string{
	"github.com/nzinovev/agentum/internal/store":       "reads the database",
	"github.com/nzinovev/agentum/internal/worktree":    "owns branch state",
	"github.com/nzinovev/agentum/internal/agent":       "invokes executors",
	"github.com/nzinovev/agentum/internal/runner":      "reaches the stage loop",
	"github.com/nzinovev/agentum/internal/manifest":    "writes evidence",
	"github.com/nzinovev/agentum/internal/publication": "reaches the coordinator",
}

// TestPackageImportsStayInsideTheBoundary parses every non-test file of this
// package and fails on a forbidden import, naming the file and what that
// import would reach. The parser walks the syntax the compiler sees, so an
// import that squeaks past review cannot squeak past this.
func TestPackageImportsStayInsideTheBoundary(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fileSet := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, entry.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		checked++
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			reason, forbidden := forbiddenImports[path]
			if !forbidden {
				continue
			}
			t.Errorf("%s: imports %q (%s); the provider contract stays inside internal/publish",
				entry.Name(), path, reason)
		}
	}
	if checked == 0 {
		t.Fatal("no Go files parsed — did the package move?")
	}
}
