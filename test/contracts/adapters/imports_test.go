package adaptercontract

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestRequiredAdapterContractFiles pins the adapter declaration files. A
// missing one is a contract break.
func TestRequiredAdapterContractFiles(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	required := []string{
		"internal/shell/store/adapter_contract.go",
		"internal/shell/runtime/adapter_contract.go",
		"internal/shell/workspace/adapter_contract.go",
		"internal/shell/log/adapter_contract.go",
		"internal/shell/secret/adapter_contract.go",
		"internal/shell/event/adapter_contract.go",
		"internal/shell/token/adapter_contract.go",
		"internal/shell/identity/adapter_contract.go",
		"internal/shell/authorize/adapter_contract.go",
		"internal/shell/pipeline/adapter_contract.go",
		"internal/shell/integration/nats/adapter_contract.go",
		"internal/installation/types.go",
		"internal/installation/ports.go",
		"internal/installation/adapter_contract.go",
	}
	for _, path := range required {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Errorf("required adapter declaration %s is missing: %v", path, err)
		}
	}
}

// TestProductImportGraphAcyclic proves the adapter declaration packages obey
// the layer rules and do not introduce an import cycle. The check is static;
// it does not compile or run a database, runtime, broker or cloud client.
func TestProductImportGraphAcyclic(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	module := readModulePath(t, root)
	graph, imports := collectProductImports(t, root, module)

	for _, violation := range productImportViolations(imports) {
		t.Error(violation)
	}

	if cycle := findCycle(graph); cycle != nil {
		t.Fatalf("product import cycle: %v", cycle)
	}
}

// TestProductImportGraphStoreSeamRules proves the layer rules: a capability
// may consume the neutral internal/shell/store seam, but never a concrete
// store/postgres or store/memory adapter, and transport packages may import
// sibling transport packages.
func TestProductImportGraphStoreSeamRules(t *testing.T) {
	const module = "example.com/fixture"
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module "+module+"\n\ngo 1.27.2\n")
	writeFixtureFile(t, root, "internal/shell/store/executor.go", "package store\n")
	writeFixtureFile(t, root, "internal/shell/store/postgres/postgres.go", "package postgres\n")
	writeFixtureFile(t, root, "internal/shell/store/memory/memory.go", "package memory\n")
	writeFixtureFile(t, root, "internal/shell/secret/secret.go", `package secret

import _ "`+module+`/internal/shell/store"
`)
	writeFixtureFile(t, root, "internal/shell/leaky/leaky.go", `package leaky

import (
	_ "`+module+`/internal/shell/store/postgres"
	_ "`+module+`/internal/shell/store/memory"
)
`)
	writeFixtureFile(t, root, "internal/shell/transport/httpapi/httpapi.go", `package httpapi

import _ "`+module+`/internal/shell/transport/mcp"
`)
	writeFixtureFile(t, root, "internal/shell/transport/mcp/mcp.go", "package mcp\n")

	_, imports := collectProductImports(t, root, module)
	violations := productImportViolations(imports)

	if len(violations) != 2 {
		t.Fatalf("violations = %v, want exactly the two concrete store adapter imports", violations)
	}
	joined := strings.Join(violations, "\n")
	for _, want := range []string{
		"internal/shell/leaky imports adapter/transport/composition package internal/shell/store/postgres",
		"internal/shell/leaky imports adapter/transport/composition package internal/shell/store/memory",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations do not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "internal/shell/secret") {
		t.Errorf("capability import of the neutral store seam was refused:\n%s", joined)
	}
	if strings.Contains(joined, "transport/httpapi") {
		t.Errorf("transport sibling import was refused:\n%s", joined)
	}
}

// TestProductStdlibImportAllowlist proves net/http is refused for capability
// packages except the model gateway directory, whose injected RoundTripper
// needs it. The allowlist applies to test files too.
func TestProductStdlibImportAllowlist(t *testing.T) {
	const module = "example.com/fixture"
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module "+module+"\n\ngo 1.27.2\n")
	writeFixtureFile(t, root, "internal/shell/gateway/gateway.go", `package gateway

import _ "net/http"
`)
	writeFixtureFile(t, root, "internal/shell/gateway/gateway_test.go", `package gateway

import _ "net/http/httptest"
`)
	writeFixtureFile(t, root, "internal/shell/mcpproxy/mcpproxy.go", `package mcpproxy

import _ "net/http"
`)
	writeFixtureFile(t, root, "internal/shell/store/executor.go", "package store\n")

	violations := stdlibImportViolations(collectShellStdlibImports(t, root, module))
	if len(violations) != 1 || !strings.Contains(violations[0], "internal/shell/mcpproxy") {
		t.Fatalf("stdlib violations = %v, want only the mcpproxy net/http import", violations)
	}
}

func TestProductStdlibImportsInRepository(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	module := readModulePath(t, root)
	if violations := stdlibImportViolations(collectShellStdlibImports(t, root, module)); len(violations) > 0 {
		t.Errorf("capability stdlib violations: %v", violations)
	}
}

// TestProductImportGraphIgnoresExternalTestPackages proves a `<package>_test`
// file importing its own product package is not recorded as a product
// self-edge, which would be reported as
// `product import cycle: [internal/shell/identity internal/shell/identity]`.
// The product graph is built only from non-test production files, while real
// production imports are still collected.
func TestProductImportGraphIgnoresExternalTestPackages(t *testing.T) {
	const module = "example.com/fixture"
	root := t.TempDir()
	writeFixtureFile(t, root, "go.mod", "module "+module+"\n\ngo 1.27.2\n")
	writeFixtureFile(t, root, "internal/shell/identity/adapter_contract.go", "package identity\n")
	writeFixtureFile(t, root, "internal/shell/identity/external_test.go", `package identity_test

import (
	"testing"

	_ "`+module+`/internal/shell/identity"
)

func TestSelfImport(t *testing.T) {}
`)
	writeFixtureFile(t, root, "internal/shell/pipeline/adapter_contract.go", `package pipeline

import _ "`+module+`/internal/shell/identity"
`)

	graph, imports := collectProductImports(t, root, module)

	if got := graph["internal/shell/identity"]; len(got) != 0 {
		t.Fatalf("external test package self-import recorded in product graph: %v", got)
	}
	if got := imports["internal/shell/identity"]; len(got) != 0 {
		t.Fatalf("external test package self-import recorded in product imports: %v", got)
	}
	if got := graph["internal/shell/pipeline"]; len(got) != 1 || got[0] != "internal/shell/identity" {
		t.Fatalf("production import of internal/shell/identity not recorded: got %v", got)
	}
	if cycle := findCycle(graph); cycle != nil {
		t.Fatalf("external test package produced product import cycle: %v", cycle)
	}
}

func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", rel, err)
	}
}

func readModulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

func collectProductImports(t *testing.T, root, module string) (map[string][]string, map[string][]string) {
	t.Helper()
	graph := make(map[string][]string)
	imports := make(map[string][]string)
	roots := []string{
		filepath.Join(root, "internal", "shell"),
		filepath.Join(root, "internal", "installation"),
	}
	for _, base := range roots {
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") {
				return nil
			}
			// The product import graph is production-only. Test files,
			// including external `<package>_test` packages that import their
			// own product package, are not product dependencies and must not
			// create product edges (or a self-cycle).
			if strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relDir, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			relDir = filepath.ToSlash(relDir)
			fileSet := token.NewFileSet()
			parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			for _, imported := range parsed.Imports {
				value := strings.Trim(imported.Path.Value, "\"")
				if !strings.HasPrefix(value, module+"/") {
					continue
				}
				target := strings.TrimPrefix(value, module+"/")
				imports[relDir] = append(imports[relDir], target)
				graph[relDir] = append(graph[relDir], target)
			}
			if _, ok := graph[relDir]; !ok {
				graph[relDir] = nil
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	for key := range imports {
		sort.Strings(imports[key])
	}
	return graph, imports
}

func productImportViolations(imports map[string][]string) []string {
	var violations []string
	for importer, imported := range imports {
		for _, target := range imported {
			switch {
			case isInstallation(importer):
				if !isNeutralOrAPI(target) && !isInstallation(target) {
					violations = append(violations, fmt.Sprintf("installation package %s imports forbidden product package %s", importer, target))
				}
			case isCapability(importer):
				if isAdapter(target) || isTransport(target) || isComposition(target) {
					violations = append(violations, fmt.Sprintf("Shell capability %s imports adapter/transport/composition package %s", importer, target))
				}
			case isAdapter(importer):
				if isTransport(target) || isComposition(target) {
					violations = append(violations, fmt.Sprintf("Shell adapter %s imports transport/composition package %s", importer, target))
				}
			case isTransport(importer):
				if isComposition(target) {
					violations = append(violations, fmt.Sprintf("Shell transport %s imports composition package %s", importer, target))
				}
			}
			if strings.HasPrefix(target, "test/") {
				violations = append(violations, fmt.Sprintf("product package %s imports test package %s", importer, target))
			}
		}
	}
	sort.Strings(violations)
	return violations
}

// collectShellStdlibImports records the standard-library imports of every Go
// file under internal/shell, including tests because the gateway allowlist
// applies to its test files too.
func collectShellStdlibImports(t *testing.T, root, module string) map[string][]string {
	t.Helper()
	imports := make(map[string][]string)
	base := filepath.Join(root, "internal", "shell")
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		relDir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		relDir = filepath.ToSlash(relDir)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		for _, imported := range parsed.Imports {
			value := strings.Trim(imported.Path.Value, "\"")
			if strings.HasPrefix(value, module+"/") || isExternalModulePath(value) {
				continue
			}
			imports[relDir] = append(imports[relDir], value)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk %s: %v", base, err)
	}
	for key := range imports {
		sort.Strings(imports[key])
	}
	return imports
}

func isExternalModulePath(path string) bool {
	first := path
	if index := strings.Index(path, "/"); index >= 0 {
		first = path[:index]
	}
	return strings.Contains(first, ".")
}

func stdlibImportViolations(imports map[string][]string) []string {
	var violations []string
	for dir, paths := range imports {
		if !isCapability(dir) {
			continue
		}
		for _, imported := range paths {
			if imported != "net/http" && !strings.HasPrefix(imported, "net/http/") {
				continue
			}
			if dir == "internal/shell/gateway" {
				continue
			}
			violations = append(violations, fmt.Sprintf("capability %s imports standard library %s", dir, imported))
		}
	}
	sort.Strings(violations)
	return violations
}

func isNeutralOrAPI(path string) bool {
	return path == "internal/id" || strings.HasPrefix(path, "internal/id/") ||
		path == "internal/fault" || strings.HasPrefix(path, "internal/fault/") ||
		path == "api" || strings.HasPrefix(path, "api/")
}

func isInstallation(path string) bool {
	return path == "internal/installation" || strings.HasPrefix(path, "internal/installation/")
}

func isStoreSeam(path string) bool {
	return path == "internal/shell/store"
}

func isCapability(path string) bool {
	if !strings.HasPrefix(path, "internal/shell/") {
		return false
	}
	rest := strings.TrimPrefix(path, "internal/shell/")
	switch {
	case rest == "store":
		// The SQL-free commit-unit seam is neutral: capabilities consume it.
		return true
	case strings.HasPrefix(rest, "store/"):
		return false
	case rest == "runtime" || strings.HasPrefix(rest, "runtime/"):
		return false
	case rest == "integration" || strings.HasPrefix(rest, "integration/"):
		return false
	case rest == "transport" || strings.HasPrefix(rest, "transport/"):
		return false
	case rest == "app" || strings.HasPrefix(rest, "app/"):
		return false
	default:
		return true
	}
}

func isAdapter(path string) bool {
	if !strings.HasPrefix(path, "internal/shell/") {
		return false
	}
	rest := strings.TrimPrefix(path, "internal/shell/")
	if isStoreSeam(path) {
		return false
	}
	return strings.HasPrefix(rest, "store/") ||
		rest == "runtime" || strings.HasPrefix(rest, "runtime/") ||
		rest == "integration" || strings.HasPrefix(rest, "integration/")
}

func isTransport(path string) bool {
	return path == "internal/shell/transport" || strings.HasPrefix(path, "internal/shell/transport/")
}

func isComposition(path string) bool {
	return path == "internal/shell/app" || strings.HasPrefix(path, "internal/shell/app/") ||
		path == "cmd" || strings.HasPrefix(path, "cmd/")
}

func findCycle(graph map[string][]string) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var stack []string
	var cycle []string

	var visit func(node string) bool
	visit = func(node string) bool {
		color[node] = gray
		stack = append(stack, node)
		for _, next := range graph[node] {
			if _, ok := graph[next]; !ok {
				continue
			}
			switch color[next] {
			case white:
				if visit(next) {
					return true
				}
			case gray:
				start := 0
				for index, candidate := range stack {
					if candidate == next {
						start = index
						break
					}
				}
				cycle = append(append([]string(nil), stack[start:]...), next)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = black
		return false
	}

	nodes := make([]string, 0, len(graph))
	for node := range graph {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if color[node] == white {
			if visit(node) {
				return cycle
			}
		}
	}
	return nil
}
