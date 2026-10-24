package delivery_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestModuleBoundaries enforces the hexagonal dependency direction. The domain
// and application package (.) imports only the standard library, the shared
// value types and the runtime contract: no driver, transport, Kubernetes client,
// deployment engine or peer module. Peer modules appear only in adapters, and
// pgx only in postgres. No package may import a Kubernetes client or run a
// command: external actions remain ordinary scripts outside this module.
//
// One narrow exception is documented and pinned here: postgres/keyed.go joins
// the human-authorized deployment creation to the HTTP foundation's keyed
// (idempotent) create transaction, so it alone may import internal/httpapi and
// internal/httpapi/postgres. No other persistence file, and no other layer
// except http (which imports only the transport foundation), may.
func TestModuleBoundaries(t *testing.T) {
	// exempt lists the single imports a file may use despite its layer's
	// prohibition. The path is relative to the package directory.
	exempt := map[string][]string{
		"postgres/keyed.go": {"github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/httpapi/postgres"},
	}
	forbidden := map[string][]string{
		".": {"github.com/", "k8s.io/", "net/http", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/storage",
			"github.com/optimiware/kodama/internal/delivery/", "github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/workspace", "github.com/optimiware/kodama/internal/artifacts",
			"github.com/optimiware/kodama/internal/runtimeauth", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/work", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/events",
			"github.com/optimiware/kodama/internal/results", "github.com/optimiware/kodama/internal/pipeline"},
		"http": {"github.com/jackc/", "k8s.io/", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/delivery/postgres",
			"github.com/optimiware/kodama/internal/delivery/adapters", "github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/workspace", "github.com/optimiware/kodama/internal/artifacts",
			"github.com/optimiware/kodama/internal/runtimeauth", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/secrets"},
		"adapters": {"github.com/jackc/", "k8s.io/", "net/http", "os/exec", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi",
			"github.com/optimiware/kodama/internal/delivery/http", "github.com/optimiware/kodama/internal/execution/postgres", "github.com/optimiware/kodama/internal/secrets/postgres",
			"github.com/optimiware/kodama/internal/artifacts/postgres", "github.com/optimiware/kodama/internal/artifacts/s3", "github.com/optimiware/kodama/internal/identity/postgres"},
		"postgres": {"k8s.io/", "net/http", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/delivery/http", "github.com/optimiware/kodama/internal/delivery/adapters",
			"github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/artifacts", "github.com/optimiware/kodama/internal/identity"},
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
				if slices.Contains(exempt[filepath.ToSlash(f)], path) {
					continue
				}
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
	// The keyed-adapter exception is exactly one file importing exactly those
	// packages: a stale or widened entry fails here rather than silently allowing
	// the import elsewhere.
	for file, imports := range exempt {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("exempt file %s: %v", file, err)
		}
		have := map[string]bool{}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			have[path] = true
		}
		for _, want := range imports {
			if !have[want] {
				t.Errorf("%s no longer imports %s: remove the stale exemption", file, want)
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
	// No file of the module touches a cluster: neither a Kubernetes client nor a
	// process runner exists anywhere in it, tests excepted.
	var scan func(dir string)
	scan = func(dir string) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				scan(p)
				continue
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if strings.HasPrefix(path, "k8s.io/") || path == "os/exec" || strings.Contains(path, "client-go") {
					t.Errorf("%s imports %s: delivery applies nothing itself", p, path)
				}
			}
		}
	}
	scan(".")
	if _, err := os.Stat("README.md"); err != nil {
		t.Errorf("the module must document its wiring: %v", err)
	}
}
