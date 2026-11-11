package contract_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/platform/config"
)

// The investigator reads LLM output, so it must have no path to approval or
// to the rollback mutation. Its composition root may connect only to the
// read-only Kubernetes MCP and the observability MCP; it never builds the
// orchestrator workflow that owns approval and execution.
//
// The forbidden names are spelled out here, not taken from production code,
// so renaming a production identifier cannot quietly weaken the test.
func TestInvestigatorHasNoPathToRollbackOrApproval(t *testing.T) {
	forbidden := map[string]string{
		"KubernetesRollbackCatalog":   "rollback MCP tool catalog",
		"ToolExecuteApprovedRollback": "rollback mutation tool",
		"ToolPrepareRollback":         "rollback preparation tool",
		"NewRollbackExecutor":         "rollback executor adapter",
		"NewServices":                 "orchestrator workflow (approval and execution)",
		"NewInvestigationCoordinator": "orchestrator investigation coordinator",
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "cmd", "investigator", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no investigator composition root found")
	}
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if reason, bad := forbidden[selector.Sel.Name]; bad {
				t.Errorf("%s references %s (%s)", fset.Position(selector.Pos()), selector.Sel.Name, reason)
			}
			return true
		})
	}
}

// TestInvestigatorConfigHasNoRollbackOrApprovalSettings checks the
// investigator's configuration surface: it cannot be pointed at the rollback
// MCP or given approval or auth settings, because it has no field for them.
func TestInvestigatorConfigHasNoRollbackOrApprovalSettings(t *testing.T) {
	fields := reflect.VisibleFields(reflect.TypeFor[config.InvestigatorConfig]())
	if len(fields) == 0 {
		t.Fatal("investigator config has no fields")
	}
	for _, field := range fields {
		for _, forbidden := range []string{"rollback", "approv", "auth"} {
			if strings.Contains(strings.ToLower(field.Name), forbidden) {
				t.Errorf("InvestigatorConfig.%s: the investigator must not hold %s settings", field.Name, forbidden)
			}
		}
	}
}
