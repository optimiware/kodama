package drivers

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// approvedDirectDependencies is the exact direct dependency set of the module:
// the YAML codec plus the two approved drivers. Any other direct
// module is a scope violation.
var approvedDirectDependencies = map[string]string{
	"go.yaml.in/yaml/v3":         "v3.0.6",
	"github.com/jackc/pgx/v5":    "v5.12.0",
	"github.com/nats-io/nats.go": "v1.55.0",
}

// forbiddenModuleFragments are dependency families that must not enter the
// composition layer.
var forbiddenModuleFragments = []string{
	"k8s.io/",
	"kubernetes",
	"podman",
	"containers/",
	"docker",
	"github.com/aws/",
	"cloud.google.com/",
	"github.com/azure/",
	"azure-sdk-for-go",
	"github.com/scaleway/",
	"github.com/hashicorp/go-plugin",
}

func TestGoModHasOnlyApprovedDirectDependencies(t *testing.T) {
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	direct, indirect := parseRequires(t, string(data))

	names := make([]string, 0, len(direct))
	for name := range direct {
		names = append(names, name)
	}
	sort.Strings(names)
	approved := make([]string, 0, len(approvedDirectDependencies))
	for name := range approvedDirectDependencies {
		approved = append(approved, name)
	}
	sort.Strings(approved)
	if strings.Join(names, ",") != strings.Join(approved, ",") {
		t.Fatalf("direct dependencies = %v; want exactly %v", names, approved)
	}
	for name, want := range approvedDirectDependencies {
		if got := direct[name]; got != want {
			t.Fatalf("direct dependency %q = %q; want pinned %q", name, got, want)
		}
	}

	for name := range indirect {
		for _, fragment := range forbiddenModuleFragments {
			if strings.Contains(name, fragment) {
				t.Fatalf("forbidden dependency %q appears in go.mod", name)
			}
		}
	}
	for name := range direct {
		for _, fragment := range forbiddenModuleFragments {
			if strings.Contains(name, fragment) {
				t.Fatalf("forbidden direct dependency %q appears in go.mod", name)
			}
		}
	}

	if !strings.Contains(string(data), "go 1.27.3") {
		t.Fatalf("go.mod does not pin the product toolchain:\n%s", data)
	}
}

// parseRequires returns the direct and indirect module versions in go.mod.
func parseRequires(t *testing.T, content string) (map[string]string, map[string]string) {
	t.Helper()
	direct := make(map[string]string)
	indirect := make(map[string]string)
	inBlock := false
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "require ("):
			inBlock = true
			continue
		case inBlock && trimmed == ")":
			inBlock = false
			continue
		case strings.HasPrefix(trimmed, "require "):
			fields := strings.Fields(strings.TrimPrefix(trimmed, "require "))
			recordRequire(t, fields, direct, indirect)
			continue
		case inBlock:
			fields := strings.Fields(trimmed)
			recordRequire(t, fields, direct, indirect)
		}
	}
	return direct, indirect
}

func recordRequire(t *testing.T, fields []string, direct, indirect map[string]string) {
	t.Helper()
	if len(fields) < 2 {
		return
	}
	name := fields[0]
	version := fields[1]
	if strings.HasPrefix(version, "//") {
		return
	}
	isIndirect := false
	for _, field := range fields[2:] {
		if field == "indirect" {
			isIndirect = true
		}
	}
	if isIndirect {
		indirect[name] = version
		return
	}
	direct[name] = version
}

// moduleRoot walks up from the test working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test working directory")
		}
		dir = parent
	}
}
