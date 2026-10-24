package artifacts_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestModuleBoundaries enforces the hexagonal dependency direction: the domain
// and application code import no driver, SDK or transport; each adapter imports
// only what it implements a port for.
func TestModuleBoundaries(t *testing.T) {
	forbidden := map[string][]string{
		".":        {"github.com/jackc/", "github.com/aws/", "net/http", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/artifacts/"},
		"postgres": {"github.com/aws/", "net/http", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/artifacts/s3", "github.com/optimiware/kodama/internal/artifacts/oci", "github.com/optimiware/kodama/internal/artifacts/http", "github.com/optimiware/kodama/contracts/"},
		"s3":       {"github.com/jackc/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/artifacts/postgres", "github.com/optimiware/kodama/internal/artifacts/oci", "github.com/optimiware/kodama/internal/artifacts/http"},
		"oci":      {"github.com/jackc/", "github.com/aws/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/artifacts/postgres", "github.com/optimiware/kodama/internal/artifacts/s3", "github.com/optimiware/kodama/internal/artifacts/http"},
		"http":     {"github.com/jackc/", "github.com/aws/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/artifacts/postgres", "github.com/optimiware/kodama/internal/artifacts/s3", "github.com/optimiware/kodama/internal/artifacts/oci"},
	}
	for dir, bad := range forbidden {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, ".gen.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, b := range bad {
					if path == b || strings.HasPrefix(path, b) {
						t.Errorf("%s imports %s, which this layer must not depend on", f, path)
					}
				}
			}
		}
	}
	// The module touches only its own paths: no other module's package is imported.
	for dir := range forbidden {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, _ := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, other := range []string{"work", "secrets", "events", "entrypoint", "cli", "app"} {
					if strings.HasPrefix(path, "github.com/optimiware/kodama/internal/"+other) {
						t.Errorf("%s imports another module (%s)", f, path)
					}
				}
			}
		}
	}
	if _, err := os.Stat("README.md"); err != nil {
		t.Errorf("the module must document its wiring: %v", err)
	}
}
