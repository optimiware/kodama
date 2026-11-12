package publiccontract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// declarationRoots are the public and Userland declaration directories.
var declarationRoots = []string{
	"api",
	"client",
	"internal/userland",
}

// publicDeclarationFile reports whether a path is a public or Userland
// declaration file.
func publicDeclarationFile(rel string) bool {
	rel = filepath.ToSlash(rel)
	name := filepath.Base(rel)
	if strings.HasPrefix(rel, "api/") {
		return strings.HasSuffix(name, ".go")
	}
	if rel == "client/types.go" || rel == "client/ports.go" {
		return true
	}
	if strings.HasPrefix(rel, "internal/userland/") {
		return name == "types.go" || name == "ports.go" ||
			name == "adapter_contract.go" || strings.HasPrefix(name, "contract_") && strings.HasSuffix(name, ".go")
	}
	return false
}

type sourceFile struct {
	rel     string
	imports []string
	ast     *ast.File
}

// scanDeclarationSources parses every declaration source. It skips testdata
// and non-Go files so only product declarations are inspected.
func scanDeclarationSources(t *testing.T, root string) []sourceFile {
	t.Helper()
	var files []sourceFile
	for _, dir := range declarationRoots {
		base := filepath.Join(root, filepath.FromSlash(dir))
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || entry.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !publicDeclarationFile(rel) {
				return nil
			}
			fileSet := token.NewFileSet()
			parsed, err := parser.ParseFile(fileSet, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}
			source := sourceFile{rel: rel, ast: parsed}
			for _, imported := range parsed.Imports {
				value, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				source.imports = append(source.imports, value)
			}
			files = append(files, source)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return files
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

func firstImportSegment(path string) string {
	if index := strings.IndexByte(path, '/'); index >= 0 {
		return path[:index]
	}
	return path
}

func isStandardLibrary(path string) bool {
	return !strings.Contains(firstImportSegment(path), ".")
}

func isModuleImport(module, path string) bool {
	return path == module || strings.HasPrefix(path, module+"/")
}

// TestPublicAndUserlandDependencyRules pins the S01 import direction for the
// public/Userland contract: api imports only the standard library; client and
// Userland never import Kernel or Shell internals.
func TestPublicAndUserlandDependencyRules(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	module := readModulePath(t, root)
	files := scanDeclarationSources(t, root)
	if len(files) == 0 {
		t.Fatal("no declaration sources found")
	}

	infrastructure := []string{"net", "database/sql", "os/exec", "os/user", "crypto/tls", "syscall", "plugin"}
	seenAPI := 0
	seenUserland := 0
	for _, file := range files {
		isAPI := strings.HasPrefix(file.rel, "api/")
		if isAPI {
			seenAPI++
		} else {
			seenUserland++
		}
		for _, imported := range file.imports {
			if isAPI {
				if isModuleImport(module, imported) {
					t.Errorf("%s imports product package %q; api imports only the standard library", file.rel, imported)
					continue
				}
				if !isStandardLibrary(imported) {
					t.Errorf("%s imports external module %q; api imports only the standard library", file.rel, imported)
					continue
				}
				for _, prefix := range infrastructure {
					if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
						t.Errorf("%s imports infrastructure package %q; api is a schema package", file.rel, imported)
					}
				}
				continue
			}

			// client and internal/userland may use the standard library, public
			// schema and neutral values, and their own adapters. They never
			// import Kernel, Shell or repository test code.
			if !isModuleImport(module, imported) {
				if !isStandardLibrary(imported) {
					t.Errorf("%s imports external module %q; public declarations are stdlib only", file.rel, imported)
				}
				continue
			}
			switch {
			case imported == module+"/api", imported == module+"/internal/id", imported == module+"/internal/fault":
			case imported == module+"/client", strings.HasPrefix(imported, module+"/client/"):
			case strings.HasPrefix(imported, module+"/internal/userland/"):
			case imported == module+"/internal/kernel" || strings.HasPrefix(imported, module+"/internal/kernel/"):
				t.Errorf("%s imports Kernel package %q; Userland never imports Kernel", file.rel, imported)
			case imported == module+"/internal/shell" || strings.HasPrefix(imported, module+"/internal/shell/"):
				t.Errorf("%s imports Shell package %q; Userland never imports Shell", file.rel, imported)
			case strings.HasPrefix(imported, module+"/test/"), strings.HasPrefix(imported, module+"/cmd/"):
				t.Errorf("%s imports %q; product declarations never depend on tests or composition", file.rel, imported)
			default:
				t.Errorf("%s imports unapproved product package %q", file.rel, imported)
			}
		}
	}
	if seenAPI == 0 {
		t.Error("no api declaration sources found")
	}
	if seenUserland == 0 {
		t.Error("no client/Userland declaration sources found")
	}
}

// TestDeclarationsExposeNoUnsupportedStubs pins that the declarations are
// declarations: no panic-based stub, no process exit, no main entrypoint and no
// init side effect that could run an unimplemented capability.
func TestDeclarationsExposeNoUnsupportedStubs(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	files := scanDeclarationSources(t, root)
	if len(files) == 0 {
		t.Fatal("no declaration sources found")
	}
	for _, file := range files {
		ast.Inspect(file.ast, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				if name, ok := typed.Fun.(*ast.Ident); ok && name.Name == "panic" {
					t.Errorf("%s calls panic(); declarations must not expose runnable unsupported stubs", file.rel)
				}
				if selector, ok := typed.Fun.(*ast.SelectorExpr); ok {
					if ident, ok := selector.X.(*ast.Ident); ok {
						if ident.Name == "os" && selector.Sel.Name == "Exit" {
							t.Errorf("%s calls os.Exit", file.rel)
						}
						if ident.Name == "log" && (selector.Sel.Name == "Fatal" || selector.Sel.Name == "Fatalf") {
							t.Errorf("%s calls log.Fatal", file.rel)
						}
					}
				}
			case *ast.FuncDecl:
				if typed.Name.Name == "main" && typed.Recv == nil {
					t.Errorf("%s declares func main; entrypoints belong to composition", file.rel)
				}
				if typed.Name.Name == "init" && typed.Recv == nil {
					t.Errorf("%s declares func init(); declarations have no init side effects", file.rel)
				}
			}
			return true
		})
	}
}

// TestNoArtifactEntity pins S09: artifact references are ordinary strings and
// no artifact entity, identity, table or route exists anywhere in the public or
// Userland contract.
func TestNoArtifactEntity(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	files := scanDeclarationSources(t, root)
	if len(files) == 0 {
		t.Fatal("no declaration sources found")
	}
	for _, file := range files {
		ast.Inspect(file.ast, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.TypeSpec:
				if strings.Contains(strings.ToLower(typed.Name.Name), "artifact") {
					t.Errorf("%s declares artifact type %q; references are ordinary strings", file.rel, typed.Name.Name)
				}
			case *ast.Field:
				for _, name := range typed.Names {
					if strings.Contains(strings.ToLower(name.Name), "artifact") {
						t.Errorf("%s declares artifact field %q", file.rel, name.Name)
					}
				}
			}
			return true
		})
	}
	// No artifact package or directory may exist under the owned roots.
	for _, dir := range declarationRoots {
		base := filepath.Join(root, filepath.FromSlash(dir))
		err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() && strings.Contains(strings.ToLower(entry.Name()), "artifact") {
				t.Errorf("artifact package directory %q exists", path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}
