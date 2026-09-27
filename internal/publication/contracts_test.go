package publication

import (
	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/publish"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestDescriptionVerdictContract detects additions to the agent vocabulary
// without importing executor code into the publication implementation.
func TestDescriptionVerdictContract(test *testing.T) {
	if descriptionVerdictSchema != agent.VerdictSchemaVersion {
		test.Fatal("verdict schema drift")
	}
	source, err := parser.ParseFile(token.NewFileSet(), "../agent/verdict.go", nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	canonical := make(map[string]bool)
	ast.Inspect(source, func(node ast.Node) bool {
		declaration, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		declaredType, ok := declaration.Type.(*ast.Ident)
		if !ok || declaredType.Name != "Verdict" {
			return true
		}
		for _, value := range declaration.Values {
			literal, ok := value.(*ast.BasicLit)
			if !ok {
				test.Fatal("verdict constants no longer use string literals")
			}
			decoded, err := strconv.Unquote(literal.Value)
			if err != nil {
				test.Fatal(err)
			}
			canonical[decoded] = true
		}
		return true
	})
	if len(canonical) == 0 || len(canonical) != len(descriptionVerdicts) {
		test.Fatalf("verdict vocabulary drift: canonical=%v publication=%v", canonical, descriptionVerdicts)
	}
	for _, value := range descriptionVerdicts {
		if !canonical[value] {
			test.Fatalf("unknown verdict %q", value)
		}
		wire := []byte(`{"schema_version":"` + agent.VerdictSchemaVersion + `","verdict":"` + value + `","findings":[{"id":"finding","severity":"major","detail":"detail"}]}`)
		if _, err := agent.ParseVerdictJSON(wire); err != nil {
			test.Fatal(err)
		}
		if descriptionVerdict(wire) != value {
			test.Fatalf("verdict rejected: %q", value)
		}
	}
}

func TestDescriptionHashContract(test *testing.T) {
	for _, text := range []string{"Stored description\n", "Проверено: ✅\n[REDACTED:github-pat]"} {
		description := publish.DescriptionRef{Text: text, RevisionID: "revision", ContentHash: artifacts.Hash([]byte(text))}
		if err := description.Validate(); err != nil {
			test.Fatalf("artifact hash rejected by provider: %v", err)
		}
		description.Text += "changed"
		if code, _ := publish.Classify(description.Validate()); code != publish.ReasonDescriptionInvalid || code.Retryable() {
			test.Fatalf("tampered description: %s", code)
		}
	}
}

func TestMissingDescriptionIsTerminal(test *testing.T) {
	for _, description := range []publish.DescriptionRef{{}, {Text: "body", ContentHash: artifacts.Hash([]byte("body"))}, {Text: "body", RevisionID: "revision"}, {RevisionID: "revision", ContentHash: artifacts.Hash(nil)}} {
		code, _ := publish.Classify(description.Validate())
		if code != publish.ReasonDescriptionInvalid || code.Retryable() {
			test.Fatalf("description=%+v code=%s", description, code)
		}
	}
}
