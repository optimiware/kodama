package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type layer int

const (
	layerNeutral layer = iota
	layerAPI
	layerKernel
	layerShellCapability
	layerShellAdapter
	layerShellTransport
	layerUserland
	layerUserlandApp
	layerComposition
	layerInstallation
	layerTooling
	layerTest
)

func (l layer) String() string {
	switch l {
	case layerNeutral:
		return "neutral"
	case layerAPI:
		return "api"
	case layerKernel:
		return "kernel"
	case layerShellCapability:
		return "shell capability"
	case layerShellAdapter:
		return "shell adapter"
	case layerShellTransport:
		return "shell transport"
	case layerUserland:
		return "userland"
	case layerUserlandApp:
		return "userland app"
	case layerComposition:
		return "composition"
	case layerInstallation:
		return "installation"
	case layerTooling:
		return "tooling"
	case layerTest:
		return "test"
	default:
		return "unknown"
	}
}

type goSourceFile struct {
	rel     string
	imports []string
}

func checkImports(root string) ([]Finding, error) {
	module, err := readModulePath(root)
	if err != nil {
		return nil, err
	}
	files, findings, err := parseGoSources(root)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		importer := classifyPath(file.rel)
		for _, imported := range file.imports {
			if reason, forbidden := importViolation(module, file.rel, importer, imported); forbidden {
				findings = append(findings, Finding{
					Path:    file.rel,
					Message: reason,
				})
			}
		}
	}

	findings = append(findings, detectKernelCycles(files, module)...)
	return findings, nil
}

func readModulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			module := strings.TrimSpace(after)
			module = strings.Trim(module, "\"'")
			if module != "" {
				return module, nil
			}
		}
	}
	return "", fmt.Errorf("go.mod has no module directive")
}

func parseGoSources(root string) ([]goSourceFile, []Finding, error) {
	var files []goSourceFile
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == ".git" || name == ".work" || name == ".harness" || name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel := relativePath(root, path)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			findings = append(findings, Finding{
				Path:    rel,
				Message: "parse error: " + oneLine(err.Error()),
			})
			return nil
		}
		source := goSourceFile{rel: rel}
		for _, imported := range parsed.Imports {
			value, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				findings = append(findings, Finding{
					Path:    rel,
					Message: "invalid import path: " + oneLine(err.Error()),
				})
				continue
			}
			source.imports = append(source.imports, value)
		}
		files = append(files, source)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return files, findings, nil
}

func classifyPath(rel string) layer {
	rel = filepath.ToSlash(rel)
	switch {
	case rel == "api" || strings.HasPrefix(rel, "api/"):
		return layerAPI
	case rel == "internal/id" || strings.HasPrefix(rel, "internal/id/"):
		return layerNeutral
	case rel == "internal/fault" || strings.HasPrefix(rel, "internal/fault/"):
		return layerNeutral
	case rel == "internal/kernel" || strings.HasPrefix(rel, "internal/kernel/"):
		return layerKernel
	case rel == "internal/shell" || strings.HasPrefix(rel, "internal/shell/"):
		rest := strings.TrimPrefix(rel, "internal/shell/")
		switch {
		case rest == "store":
			// The SQL-free commit-unit seam is neutral: capabilities consume
			// store.Executor without depending on a concrete adapter.
			return layerShellCapability
		case strings.HasPrefix(rest, "store/"):
			// Files directly in internal/shell/store are the seam package;
			// only deeper directories (store/postgres, store/memory) are
			// concrete adapters.
			if !strings.Contains(strings.TrimPrefix(rest, "store/"), "/") &&
				strings.HasSuffix(rest, ".go") {
				return layerShellCapability
			}
			return layerShellAdapter
		case rest == "runtime" || strings.HasPrefix(rest, "runtime/"),
			rest == "integration" || strings.HasPrefix(rest, "integration/"):
			return layerShellAdapter
		case rest == "transport" || strings.HasPrefix(rest, "transport/"):
			return layerShellTransport
		case rest == "app" || strings.HasPrefix(rest, "app/"):
			return layerComposition
		default:
			return layerShellCapability
		}
	case rel == "internal/userland" || strings.HasPrefix(rel, "internal/userland/"):
		rest := strings.TrimPrefix(rel, "internal/userland/")
		if rest == "app" || strings.HasPrefix(rest, "app/") || rest == "git/app" || strings.HasPrefix(rest, "git/app/") ||
			rest == "agent/app" || strings.HasPrefix(rest, "agent/app/") ||
			rest == "services/app" || strings.HasPrefix(rest, "services/app/") {
			// Userland app processes use only public client contracts even
			// when linked into the same binary: no Shell, Kernel or internal
			// package may be imported.
			return layerUserlandApp
		}
		return layerUserland
	case rel == "client" || strings.HasPrefix(rel, "client/"):
		return layerUserland
	case rel == "internal/installation" || strings.HasPrefix(rel, "internal/installation/"):
		return layerInstallation
	case rel == "cmd" || strings.HasPrefix(rel, "cmd/"):
		return layerComposition
	case rel == "tools" || strings.HasPrefix(rel, "tools/"):
		return layerTooling
	case rel == "internal/archtest" || strings.HasPrefix(rel, "internal/archtest/"):
		return layerTooling
	case rel == "test" || strings.HasPrefix(rel, "test/"):
		return layerTest
	case rel == "examples" || strings.HasPrefix(rel, "examples/"):
		// Shipped examples are verified against the real decoders by
		// repository test code, so they follow the test layer rules.
		return layerTest
	default:
		return layerNeutral
	}
}

func importViolation(module, rel string, importer layer, imported string) (string, bool) {
	if productPath, ok := moduleRelative(module, imported); ok {
		if !productImportAllowed(importer, classifyPath(productPath)) {
			return fmt.Sprintf("forbidden import %q from %s package", imported, importer), true
		}
		return "", false
	}
	if isExternalModule(imported) {
		if externalImportApproved(importer, imported) {
			return "", false
		}
		switch importer {
		case layerNeutral, layerAPI, layerKernel, layerShellCapability, layerInstallation:
			return fmt.Sprintf("forbidden external import %q from %s package", imported, importer), true
		}
		// Test consumers may use external test dependencies, and the remaining
		// product layers may use external dependencies.
		return "", false
	}
	if forbiddenStdlibImport(importer, rel, imported) {
		return fmt.Sprintf("forbidden standard-library import %q from %s package", imported, importer), true
	}
	return "", false
}

func moduleRelative(module, imported string) (string, bool) {
	if imported == module {
		return ".", true
	}
	if after, ok := strings.CutPrefix(imported, module+"/"); ok {
		return after, true
	}
	return "", false
}

// approvedExternalImport is one recorded external dependency that a single
// layer may import.
type approvedExternalImport struct {
	module string
	layer  layer
}

// approvedExternalImports is the explicit allowlist for external modules.
// Neutral, API, Kernel and Installation packages remain standard-library only.
var approvedExternalImports = []approvedExternalImport{
	{module: "go.yaml.in/yaml/v3", layer: layerShellCapability},
}

func externalImportApproved(importer layer, imported string) bool {
	for _, approved := range approvedExternalImports {
		if importer == approved.layer && hasImportPrefix(imported, approved.module) {
			return true
		}
	}
	return false
}

func isExternalModule(imported string) bool {
	if imported == "" {
		return false
	}
	first := imported
	if before, _, ok := strings.Cut(imported, "/"); ok {
		first = before
	}
	return strings.Contains(first, ".")
}

func productImportAllowed(importer, imported layer) bool {
	if importer == layerTest {
		// Repository test code is a consumer, not product: it may import any
		// product layer, including concrete adapters it needs to drive.
		return true
	}
	if imported == layerTest {
		// Product code must never depend on repository test code.
		return false
	}
	switch importer {
	case layerNeutral, layerAPI:
		// S01 allows api, internal/id and internal/fault to import only the
		// standard library. They must not import any product package, not even
		// each other.
		return false
	case layerKernel:
		return imported == layerNeutral || imported == layerKernel
	case layerShellCapability:
		return imported == layerNeutral || imported == layerAPI || imported == layerKernel || imported == layerShellCapability
	case layerShellAdapter:
		return imported == layerNeutral || imported == layerAPI || imported == layerKernel || imported == layerShellCapability || imported == layerShellAdapter
	case layerShellTransport:
		return imported == layerNeutral || imported == layerAPI || imported == layerKernel ||
			imported == layerShellCapability || imported == layerShellTransport
	case layerUserland:
		return imported == layerNeutral || imported == layerAPI || imported == layerUserland
	case layerUserlandApp:
		return imported == layerNeutral || imported == layerAPI || imported == layerUserland || imported == layerUserlandApp
	case layerComposition:
		return true
	case layerInstallation:
		return imported == layerNeutral || imported == layerAPI
	case layerTooling:
		return imported == layerTooling
	default:
		return false
	}
}

var infrastructureStdlib = []string{
	"net",
	"database/sql",
	"os/exec",
	"os/user",
	"crypto/tls",
	"syscall",
	"plugin",
}

// approvedStdlibImports is the explicit, narrow exception to the
// infrastructure standard-library ban, keyed by exact directory. The model
// gateway receives an injected http.RoundTripper, so its directory alone may
// import net/http (including net/http/httptest in tests). internal/installation
// is declaration and pure preflight logic, so it alone may import the pure URL
// parser net/url (a leaf package with no subpackages); it still rejects net,
// net/http, net/http/httptest, os/exec, crypto/tls and every other
// infrastructure client. No other capability, neutral, api, kernel or
// installation file may use either exception.
var approvedStdlibImports = map[string][]string{
	"internal/shell/gateway": {"net/http"},
	"internal/installation":  {"net/url"},
}

func stdlibImportApproved(rel, imported string) bool {
	dir := filepath.ToSlash(filepath.Dir(filepath.ToSlash(rel)))
	for _, prefix := range approvedStdlibImports[dir] {
		if hasImportPrefix(imported, prefix) {
			return true
		}
	}
	return false
}

func forbiddenStdlibImport(importer layer, rel, imported string) bool {
	switch importer {
	case layerNeutral, layerAPI, layerKernel, layerShellCapability, layerInstallation:
		for _, prefix := range infrastructureStdlib {
			if hasImportPrefix(imported, prefix) && !stdlibImportApproved(rel, imported) {
				return true
			}
		}
	case layerShellTransport:
		return hasImportPrefix(imported, "database/sql")
	}
	// layerTest is deliberately absent: repository test code may use
	// infrastructure standard-library packages to drive the code under test.
	return false
}

func hasImportPrefix(imported, prefix string) bool {
	return imported == prefix || strings.HasPrefix(imported, prefix+"/")
}

func detectKernelCycles(files []goSourceFile, module string) []Finding {
	edges := make(map[string][]string)
	representative := make(map[string]string)
	kernelDirs := make(map[string]bool)
	for _, file := range files {
		if classifyPath(file.rel) != layerKernel {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(file.rel))
		kernelDirs[dir] = true
		if representative[dir] == "" {
			representative[dir] = file.rel
		}
		for _, imported := range file.imports {
			productPath, ok := moduleRelative(module, imported)
			if !ok {
				continue
			}
			if classifyPath(productPath) != layerKernel {
				continue
			}
			importedDir := filepath.ToSlash(productPath)
			if importedDir == dir {
				continue
			}
			edges[dir] = append(edges[dir], importedDir)
		}
	}
	for dir := range edges {
		sort.Strings(edges[dir])
	}

	color := make(map[string]int, len(kernelDirs))
	var stack []string
	seenCycles := make(map[string]bool)
	var findings []Finding
	var visit func(string)
	visit = func(dir string) {
		color[dir] = 1
		stack = append(stack, dir)
		for _, next := range edges[dir] {
			if !kernelDirs[next] {
				continue
			}
			switch color[next] {
			case 0:
				visit(next)
			case 1:
				start := -1
				for i, item := range stack {
					if item == next {
						start = i
						break
					}
				}
				if start < 0 {
					continue
				}
				cycle := append(append([]string(nil), stack[start:]...), next)
				keyParts := append([]string(nil), cycle[:len(cycle)-1]...)
				sort.Strings(keyParts)
				key := strings.Join(keyParts, ",")
				if seenCycles[key] {
					continue
				}
				seenCycles[key] = true
				path := representative[cycle[0]]
				if path == "" {
					path = cycle[0]
				}
				findings = append(findings, Finding{
					Path:    path,
					Message: "import cycle: " + strings.Join(cycle, " -> "),
				})
			}
		}
		stack = stack[:len(stack)-1]
		color[dir] = 2
	}
	dirs := make([]string, 0, len(kernelDirs))
	for dir := range kernelDirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if color[dir] == 0 {
			visit(dir)
		}
	}
	return findings
}
