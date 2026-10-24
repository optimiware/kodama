package chatops_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestModuleBoundaries enforces the hexagonal dependency direction. The domain
// and use-case package (.) imports only the standard library, the shared value
// types, the event contract and the event bus's technology-free types: no
// driver, transport, identity, secrets or peer module. PostgreSQL, the outbound
// HTTP policy and the HTTP transport are adapters that depend inward, never on
// each other.
func TestModuleBoundaries(t *testing.T) {
	forbidden := map[string][]string{
		".": {"github.com/", "net/http", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/storage",
			"github.com/optimiware/kodama/internal/integrations/chatops/", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/work", "github.com/optimiware/kodama/internal/events/",
			"github.com/optimiware/kodama/internal/execution", "github.com/optimiware/kodama/internal/delivery", "github.com/optimiware/kodama/pkg/netpolicy"},
		"webhook": {"github.com/", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/secrets",
			"github.com/optimiware/kodama/internal/events", "github.com/optimiware/kodama/internal/integrations/chatops/postgres", "github.com/optimiware/kodama/internal/integrations/chatops/http"},
		"postgres": {"net/http", "os/exec", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/integrations/chatops/http",
			"github.com/optimiware/kodama/internal/integrations/chatops/webhook", "github.com/optimiware/kodama/pkg/netpolicy", "github.com/nats-io/"},
		"http": {"github.com/jackc/", "database/sql", "os/exec", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/identity/postgres", "github.com/optimiware/kodama/internal/secrets",
			"github.com/optimiware/kodama/internal/events/postgres", "github.com/optimiware/kodama/internal/integrations/chatops/postgres", "github.com/optimiware/kodama/internal/integrations/chatops/webhook", "github.com/optimiware/kodama/pkg/netpolicy"},
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

	// pgx belongs to the persistence adapter only; the outbound dial policy to the
	// webhook adapter only.
	only := map[string]string{"github.com/jackc/": "postgres", "github.com/optimiware/kodama/pkg/netpolicy": "webhook", "net/http/httptrace": "webhook"}
	walk(t, ".", func(path string, src string, imports []string) {
		dir := filepath.Dir(path)
		for _, imp := range imports {
			if dir == "chatopstest" {
				break // the shared test fixture wires every adapter
			}
			for prefix, owner := range only {
				if strings.HasPrefix(imp, prefix) && dir != owner {
					t.Errorf("%s imports %s; only %s/ may", path, imp, owner)
				}
			}
		}
		// No module file reads the environment: retry timing, limits and origins
		// are constructor configuration, and no new environment variable exists.
		for _, call := range []string{"os.Getenv", "os.LookupEnv", "os.Environ"} {
			if strings.Contains(src, call) {
				t.Errorf("%s calls %s: ChatOps has no environment configuration", path, call)
			}
		}
	})

	// The outbound client's security posture is pinned in source: a direct dialer
	// with the public-only Control hook, no proxy, no alternate TLS dialer, normal
	// certificate verification and no redirect following.
	src, err := os.ReadFile(filepath.Join("webhook", "client.go"))
	if err != nil {
		t.Fatal(err)
	}
	client := string(src)
	for _, want := range []string{"Proxy:                  nil", "DialContext:            dialer.DialContext", "http.ErrUseLastResponse", "DisableKeepAlives", "MinVersion: tls.VersionTLS12"} {
		if !strings.Contains(client, want) {
			t.Errorf("webhook/client.go no longer contains %q", want)
		}
	}
	for _, banned := range []string{"ProxyFromEnvironment", "DialTLS", "InsecureSkipVerify", "http.DefaultClient", "http.DefaultTransport", "CheckRedirect: func(req"} {
		if strings.Contains(client, banned) {
			t.Errorf("webhook/client.go contains %q", banned)
		}
	}
	policy, err := os.ReadFile(filepath.Join("webhook", "policy.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(policy), "netpolicy.PublicOnly(network, address, c)") {
		t.Error("the dial-time control no longer consults netpolicy.PublicOnly first")
	}

	if _, err := os.Stat("README.md"); err != nil {
		t.Errorf("the module must document its wiring: %v", err)
	}
}

// walk calls fn for every non-test Go file below dir with its source and imports.
func walk(t *testing.T, dir string, fn func(path, src string, imports []string)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if e.Name() == "testdata" {
				continue
			}
			walk(t, p, fn)
			continue
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, ".gen.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), p, b, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		var imports []string
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			imports = append(imports, path)
		}
		fn(p, string(b), imports)
	}
}
