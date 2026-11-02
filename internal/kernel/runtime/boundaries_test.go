package runtime_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Test_unit_kernel_runtime_dependency_boundary enforces that the kernel runtime
// contract stays pure: standard library plus internal/domain only. It parses
// every non-test source file's imports and fails on any SQL, broker, HTTP,
// Git-hosting, task, identity, agent, shell or composition package.
func Test_unit_kernel_runtime_dependency_boundary(t *testing.T) {
	forbidden := []string{
		"github.com/jackc/",
		"github.com/nats-io/",
		"github.com/nats-",
		"net/http",
		"database/sql",
		"github.com/optimiware/kodama/internal/shell/",
		"github.com/optimiware/kodama/internal/storage/",
		"github.com/optimiware/kodama/internal/tasks/",
		"github.com/optimiware/kodama/internal/gitservice/",
		"github.com/optimiware/kodama/internal/agents/",
		"github.com/optimiware/kodama/internal/gateway/",
		"github.com/optimiware/kodama/internal/mcpprocess/",
		"github.com/optimiware/kodama/internal/mcpproxy/",
		"github.com/optimiware/kodama/internal/app/",
		"github.com/optimiware/kodama/contracts/",
	}
	// Bare package paths without a trailing slash are forbidden as well.
	exact := map[string]bool{}
	for _, f := range forbidden {
		exact[strings.TrimSuffix(f, "/")] = true
	}

	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources found: %v", err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports %s, which the kernel runtime must not depend on", f, path)
				}
			}
			if exact[path] {
				t.Errorf("%s imports %s, which the kernel runtime must not depend on", f, path)
			}
			// Beyond the explicit denylist: the only module package allowed is internal/domain.
			if strings.HasPrefix(path, "github.com/optimiware/kodama/") && path != "github.com/optimiware/kodama/internal/domain" {
				t.Errorf("%s imports %s; the kernel runtime may import only internal/domain from this module", f, path)
			}
			// Everything else must be the standard library (no dot in the first path element).
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") && !strings.HasPrefix(path, "github.com/optimiware/kodama/") {
				t.Errorf("%s imports third-party %s; the kernel runtime is standard library only", f, path)
			}
		}
	}
	if checked < 3 {
		t.Fatalf("expected doc.go, model.go and ports.go; checked %d files", checked)
	}
}
