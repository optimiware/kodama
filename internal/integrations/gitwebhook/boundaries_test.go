package gitwebhook_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The domain package stays free of transport and infrastructure, and reads Git
// only through the workspace port (never its Git adapter, which could carry
// publisher credentials).
func TestPackageBoundaries(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"net/http", "github.com/jackc/pgx", "github.com/nats-io", "github.com/optimiware/kodama/internal/workspace/git", "github.com/optimiware/kodama/internal/workspace/postgres", "os/exec"}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports %s", e.Name(), path)
				}
			}
		}
	}
}
