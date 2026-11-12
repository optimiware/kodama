package enginetest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEngineProductionImportsStayInsideKernel pins K-INDEPENDENT: the engine
// production package imports only the standard library and acyclic Kernel
// packages. Test files may use the testkit, but production code must not.
func TestEngineProductionImportsStayInsideKernel(t *testing.T) {
	allowedProduct := map[string]bool{
		"github.com/optimiware/kodama/internal/id":                true,
		"github.com/optimiware/kodama/internal/fault":             true,
		"github.com/optimiware/kodama/internal/kernel":            true,
		"github.com/optimiware/kodama/internal/kernel/definition": true,
		"github.com/optimiware/kodama/internal/kernel/flow":       true,
		"github.com/optimiware/kodama/internal/kernel/script":     true,
	}
	forbiddenStdlib := map[string]bool{
		"os/exec":      true,
		"net":          true,
		"net/http":     true,
		"database/sql": true,
	}

	entries, err := os.ReadDir("..")
	if err != nil {
		t.Fatalf("ReadDir(..) error = %v", err)
	}
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		found++
		path := filepath.Join("..", name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s error = %v", path, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s error = %v", path, err)
			}
			switch {
			case strings.HasPrefix(importPath, "github.com/optimiware/kodama/"):
				if !allowedProduct[importPath] {
					t.Errorf("%s imports forbidden product package %q", path, importPath)
				}
			case strings.Contains(importPath, "."):
				t.Errorf("%s imports external dependency %q", path, importPath)
			default:
				if forbiddenStdlib[importPath] {
					t.Errorf("%s imports infrastructure package %q", path, importPath)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no production engine source files were inspected")
	}
}
