package publication

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenImportsPerDir names the packages each side of the coordinator /
// runner pair must not reach, keyed by the directory whose files are checked.
// The coordinator and the runner are peers behind the queue's kind table, and
// the blindness is mutual: publication importing runner would reach the stage
// loop and the adapter; runner importing publication would give the component
// that drives stages a path into the delivery coordinator. Neither direction
// is a compile error on its own — only a parse of both packages' files keeps
// the boundary checked. The coordinator also stays off the worktree manager
// (it names branches, it does not manage them) and off the executor package.
var forbiddenImportsPerDir = map[string]map[string]string{
	".": {
		"github.com/nzinovev/agentum/internal/runner":   "reaches the stage loop",
		"github.com/nzinovev/agentum/internal/worktree": "owns branch state",
		"github.com/nzinovev/agentum/internal/agent":    "invokes executors",
	},
	"../runner": {
		"github.com/nzinovev/agentum/internal/publication": "reaches the delivery coordinator",
	},
}

// TestCoordinatorAndRunnerStayMutuallyBlind parses every non-test file in
// each checked directory and fails on a forbidden import, naming the file
// and what that import would reach. The parser walks the syntax the compiler
// sees, so an import that squeaks past review cannot squeak past this.
func TestCoordinatorAndRunnerStayMutuallyBlind(t *testing.T) {
	t.Parallel()
	for dir, forbidden := range forbiddenImportsPerDir {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		fileSet := token.NewFileSet()
		checked := 0
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fileSet, filepath.Join(dir, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", entry.Name(), err)
			}
			checked++
			for _, imported := range file.Imports {
				path := strings.Trim(imported.Path.Value, `"`)
				reason, forbiddenImport := forbidden[path]
				if !forbiddenImport {
					continue
				}
				t.Errorf("%s: imports %q (%s); the coordinator and the runner are peers behind the queue's kind table",
					filepath.Join(dir, entry.Name()), path, reason)
			}
		}
		if checked == 0 {
			t.Fatalf("%s: no Go files parsed — did the package move?", dir)
		}
	}
}
