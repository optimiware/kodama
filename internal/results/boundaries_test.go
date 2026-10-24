package results_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestModuleBoundaries enforces the hexagonal dependency direction: the
// application package (.) depends on no driver, SDK, transport, Git or peer
// adapter, only on consumer-owned ports and the shared contract; the adapters
// are the only code that imports peer modules, and the HTTP adapter imports no
// persistence.
func TestModuleBoundaries(t *testing.T) {
	forbidden := map[string][]string{
		".": {"github.com/", "net/http", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/storage",
			"github.com/optimiware/kodama/internal/results/", "github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/workspace", "github.com/optimiware/kodama/internal/artifacts",
			"github.com/optimiware/kodama/internal/runtimeauth", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/work", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/events"},
		"http": {"github.com/", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/results/postgres", "github.com/optimiware/kodama/internal/results/adapters",
			"github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/workspace", "github.com/optimiware/kodama/internal/artifacts", "github.com/optimiware/kodama/internal/runtimeauth", "github.com/optimiware/kodama/internal/httpapi"},
		"adapters": {"github.com/", "net/http", "os/exec", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/results/http",
			"github.com/optimiware/kodama/internal/execution/postgres", "github.com/optimiware/kodama/internal/workspace/postgres", "github.com/optimiware/kodama/internal/workspace/git", "github.com/optimiware/kodama/internal/artifacts/postgres",
			"github.com/optimiware/kodama/internal/artifacts/s3", "github.com/optimiware/kodama/internal/artifacts/http"},
		"postgres": {"net/http", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/results/http", "github.com/optimiware/kodama/internal/results/adapters",
			"github.com/optimiware/kodama/internal/workspace/git", "github.com/optimiware/kodama/internal/artifacts/"},
	}
	for dir, bad := range forbidden {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, b := range bad {
					if b == "github.com/" && strings.HasPrefix(path, "github.com/optimiware/kodama/") {
						continue // "github.com/" stands for third-party code; this module's packages have their own entries
					}
					if path == b || strings.HasPrefix(path, b) {
						t.Errorf("%s imports %s, which this layer must not depend on", f, path)
					}
				}
			}
		}
	}
	// pgx belongs to the persistence adapter only.
	for _, dir := range []string{".", "http", "adapters"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, _ := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			for _, imp := range parsed.Imports {
				if path, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(path, "github.com/jackc/") {
					t.Errorf("%s imports %s; only postgres/ may", f, path)
				}
			}
		}
	}
	if _, err := os.Stat("README.md"); err != nil {
		t.Errorf("the module must document its wiring: %v", err)
	}
}
