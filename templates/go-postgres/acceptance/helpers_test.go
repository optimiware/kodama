// Package acceptance holds the root-module drivers of the Go/PostgreSQL
// blueprint's acceptance tests. Each test generates a fresh application with
// generate.sh outside the Kodama checkout and exercises that generated copy; the
// application itself imports nothing from Kodama. Drivers may consume merged
// factory packages (pipeline validation, runtime contract patterns, environment
// snapshot serialization) to prove the template matches them.
//
// Resource prefix: t043.
package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/templates/go-postgres/acceptance/driver"
	"github.com/optimiware/kodama/test/support/layer"
	pgsupport "github.com/optimiware/kodama/test/support/postgres"
)

const (
	resourcePrefix = driver.ResourcePrefix
	modulePath     = driver.ModulePath
)

// templateDir is templates/go-postgres.
func templateDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the template")
	}
	return filepath.Dir(filepath.Dir(file))
}

func repoRoot(t testing.TB) string { return filepath.Dir(filepath.Dir(templateDir(t))) }

// generate creates a fresh application (with a deterministic Git commit) in a
// temporary directory outside the checkout and returns its path.
func generate(t testing.TB) string { return driver.Generate(t) }

// baseEnv is a minimal, explicit environment for commands run in a generated app.
func baseEnv(extra ...string) []string { return driver.BaseEnv(extra...) }

// postgresEnv returns the environment that makes the application's real-PostgreSQL
// tests run (and fail rather than skip). The service layer is required.
func postgresEnv(t testing.TB) []string {
	t.Helper()
	layer.SkipService(t, "PostgreSQL")
	admin := pgsupport.AdminURL()
	if admin == "" {
		if os.Getenv(pgsupport.RequireEnv) == "1" {
			t.Fatalf("%s is unset but %s=1", pgsupport.AdminURLEnv, pgsupport.RequireEnv)
		}
		t.Skipf("PostgreSQL acceptance skipped: set %s", pgsupport.AdminURLEnv)
	}
	return []string{"INVENTORY_TEST_ADMIN_URL=" + admin, "INVENTORY_REQUIRE_POSTGRES=1", "INVENTORY_TEST_PREFIX=" + resourcePrefix}
}

// goTest runs `go test -json` in dir and returns the outcome of every named
// test or subtest ("pass", "fail", "skip") keyed by "pkg.Test/Sub".
func goTest(t testing.TB, dir string, env []string, run string, pkgs ...string) map[string]string {
	t.Helper()
	return driver.GoTest(t, dir, env, run, 15*time.Minute, pkgs...)
}

func tail(s string, n int) string { return driver.Tail(s, n) }

// requirePassed asserts that each named test ran and passed: a skip or a missing
// test is not acceptance.
func requirePassed(t testing.TB, results map[string]string, names ...string) {
	t.Helper()
	driver.RequirePassed(t, results, names...)
}

func run(t testing.TB, dir string, env []string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = baseEnv(env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustRun(t testing.TB, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, err := run(t, dir, env, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(out, 6000))
	}
	return out
}

func writeFile(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var _ = fmt.Sprintf
