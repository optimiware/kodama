package installation

// Layer guard. internal/installation is declaration and pure preflight
// logic: it may import only the standard library (never infrastructure
// packages), api/**, internal/id and internal/fault. It must never reach a
// Shell capability, adapter, transport, composition package, kernel or
// userland, and it never executes a process or opens a socket.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackageImportsStayInLayer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("%s: invalid import %s: %v", name, imported.Path.Value, err)
			}
			if !allowedInstallationImport(path) {
				t.Errorf("%s imports forbidden package %q", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no production Go files found in internal/installation")
	}
}

// TestAllowedInstallationImportURLOnly pins the one standard-library exception
// internal/installation carries: the pure URL parser net/url, and nothing else
// that opens a socket, starts a process or negotiates transport security.
func TestAllowedInstallationImportURLOnly(t *testing.T) {
	if !allowedInstallationImport("net/url") {
		t.Error(`installation must allow the pure URL parser "net/url"`)
	}
	for _, path := range []string{
		"net",
		"net/http",
		"net/http/httptest",
		"net/http/cookiejar",
		"net/mail",
		"net/smtp",
		"net/textproto",
		"os/exec",
		"crypto/tls",
		"database/sql",
		"os/user",
		"syscall",
		"plugin",
	} {
		if allowedInstallationImport(path) {
			t.Errorf("installation must reject %q", path)
		}
	}
}

// allowedInstallationImport reports whether the installation layer may import
// path.
func allowedInstallationImport(path string) bool {
	switch {
	case path == "github.com/optimiware/kodama/api" || strings.HasPrefix(path, "github.com/optimiware/kodama/api/"):
		return true
	case path == "github.com/optimiware/kodama/internal/id" || strings.HasPrefix(path, "github.com/optimiware/kodama/internal/id/"):
		return true
	case path == "github.com/optimiware/kodama/internal/fault" || strings.HasPrefix(path, "github.com/optimiware/kodama/internal/fault/"):
		return true
	case strings.HasPrefix(path, "github.com/optimiware/kodama/"):
		return false
	}

	first := path
	if before, _, ok := strings.Cut(path, "/"); ok {
		first = before
	}
	if strings.Contains(first, ".") {
		// External module dependency.
		return false
	}
	if path == "net/url" || strings.HasPrefix(path, "net/url/") {
		// The one standard-library exception: the pure URL parser net/url,
		// a leaf package with no subpackages. Every other net package, the
		// process and transport-security clients below stay forbidden.
		return true
	}
	for _, forbidden := range []string{
		"net",
		"database/sql",
		"os/exec",
		"os/user",
		"crypto/tls",
		"syscall",
		"plugin",
	} {
		if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
			return false
		}
	}
	return true
}
