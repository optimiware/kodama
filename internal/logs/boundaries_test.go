package logs_test

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
// and use cases import no driver, SDK, transport or peer module; each adapter
// imports only what it implements a port for; nothing here depends on the
// entrypoint, the application root or the runtime wire contract.
func TestModuleBoundaries(t *testing.T) {
	driver := []string{"github.com/jackc/", "github.com/aws/", "net/http", "k8s.io/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi"}
	peers := []string{"github.com/optimiware/kodama/internal/artifacts", "github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/work", "github.com/optimiware/kodama/internal/workspace"}
	root := []string{"github.com/optimiware/kodama/internal/app", "github.com/optimiware/kodama/internal/entrypoint", "github.com/optimiware/kodama/internal/cli", "github.com/optimiware/kodama/contracts/"}
	logsAdapters := []string{"github.com/optimiware/kodama/internal/logs/adapters", "github.com/optimiware/kodama/internal/logs/postgres", "github.com/optimiware/kodama/internal/logs/kubernetes", "github.com/optimiware/kodama/internal/logs/http", "github.com/optimiware/kodama/internal/logs/logstest"}

	forbidden := map[string][]string{
		".":          concat(driver, peers, root, logsAdapters),
		"contract":   concat(driver, peers, root, logsAdapters, []string{"github.com/optimiware/kodama/internal/logs"}),
		"adapters":   concat([]string{"github.com/jackc/", "net/http", "k8s.io/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi"}, root, []string{"github.com/optimiware/kodama/internal/logs/postgres", "github.com/optimiware/kodama/internal/logs/kubernetes", "github.com/optimiware/kodama/internal/logs/http", "github.com/optimiware/kodama/internal/logs/logstest"}),
		"postgres":   concat([]string{"github.com/aws/", "net/http", "k8s.io/", "github.com/optimiware/kodama/internal/httpapi"}, peers, root, []string{"github.com/optimiware/kodama/internal/logs/adapters", "github.com/optimiware/kodama/internal/logs/kubernetes", "github.com/optimiware/kodama/internal/logs/http", "github.com/optimiware/kodama/internal/logs/logstest"}),
		"kubernetes": concat([]string{"github.com/jackc/", "github.com/aws/", "k8s.io/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi"}, peers, root, []string{"github.com/optimiware/kodama/internal/logs/adapters", "github.com/optimiware/kodama/internal/logs/postgres", "github.com/optimiware/kodama/internal/logs/http", "github.com/optimiware/kodama/internal/logs/logstest"}),
		"http":       concat([]string{"github.com/jackc/", "github.com/aws/", "k8s.io/", "github.com/optimiware/kodama/internal/storage"}, []string{"github.com/optimiware/kodama/internal/artifacts", "github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/work", "github.com/optimiware/kodama/internal/workspace"}, root, []string{"github.com/optimiware/kodama/internal/logs/adapters", "github.com/optimiware/kodama/internal/logs/postgres", "github.com/optimiware/kodama/internal/logs/kubernetes", "github.com/optimiware/kodama/internal/logs/logstest"}),
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
					if path == b || strings.HasPrefix(path, b+"/") || (strings.HasSuffix(b, "/") && strings.HasPrefix(path, b)) {
						t.Errorf("%s imports %s, which this layer must not depend on", f, path)
					}
				}
			}
		}
	}
	// The generated wire types come from the OpenAPI fragment, which this module owns.
	if _, err := os.Stat("../../api/logs.yaml"); err != nil {
		t.Errorf("the route's OpenAPI fragment is missing: %v", err)
	}
	if _, err := os.Stat("README.md"); err != nil {
		t.Errorf("the module must document its wiring: %v", err)
	}
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
