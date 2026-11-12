//go:build qualification

package production

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/suite"
)

// TestHarnessStageWavesClassifyEveryLiveProbe proves the wave lists and the
// probes agree: every TestStage* function is in exactly one list, every wave 1
// probe enters the wave 1 gate first, and no other probe is ever parallel. A
// new probe therefore cannot join a stage run without a blast-radius decision.
func TestHarnessStageWavesClassifyEveryLiveProbe(t *testing.T) {
	const (
		wave1 = "suite.StageWave1Probes"
		wave2 = "suite.StageWave2ChaosProbes"
		none  = "suite.StageUnscheduledProbes"
	)
	classified := map[string]string{}
	for list, names := range map[string][]string{
		wave1: suite.StageWave1Probes,
		wave2: suite.StageWave2ChaosProbes,
		none:  suite.StageUnscheduledProbes,
	} {
		for _, name := range names {
			if other, ok := classified[name]; ok {
				t.Errorf("%s is listed in %s and %s", name, other, list)
			}
			classified[name] = list
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	found := map[string]bool{}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "TestStage") {
				continue
			}
			name := function.Name.Name
			found[name] = true
			entersGate := firstStatementCalls(function, "stageWave1Parallel")
			switch classified[name] {
			case wave1:
				if !entersGate {
					t.Errorf("%s (%s) is a wave 1 probe; its first statement must be stageWave1Parallel(t)", name, entry.Name())
				}
			case wave2, none:
				if callsFunction(function, "stageWave1Parallel") || callsMethod(function, "Parallel") {
					t.Errorf("%s (%s) is listed in %s and must never run in parallel", name, entry.Name(), classified[name])
				}
			default:
				t.Errorf("%s (%s) is in no wave: add it to %s or %s", name, entry.Name(), wave1, wave2)
			}
		}
	}
	for name, list := range classified {
		if !found[name] {
			t.Errorf("%s names %s, which is not a test of this package", list, name)
		}
	}
}

// firstStatementCalls reports whether the function body starts with a call of
// the named package function.
func firstStatementCalls(function *ast.FuncDecl, name string) bool {
	if function.Body == nil || len(function.Body.List) == 0 {
		return false
	}
	statement, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

// callsFunction reports whether the function body calls the named package
// function anywhere.
func callsFunction(function *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}

// callsMethod reports whether the function body calls the named method on any
// receiver.
func callsMethod(function *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
