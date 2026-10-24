// Package driver is the shared support of the blueprint's acceptance drivers: it
// generates a fresh application outside the checkout, runs the generated
// application's own tests as child processes, and turns their named results into
// acceptance evidence. It consumes nothing from the generated application.
package driver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/layer"
)

const (
	// ResourcePrefix is the task's prefix for databases, namespaces and registry repositories.
	ResourcePrefix = "t043"
	// ModulePath is the module path the generated application receives.
	ModulePath = "acme.example/inventory-gap"
)

// TemplateDir is templates/go-postgres.
func TemplateDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the template")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// RepoRoot is the Kodama checkout.
func RepoRoot(t testing.TB) string { return filepath.Dir(filepath.Dir(TemplateDir(t))) }

// Generate creates a fresh application (with a deterministic Git commit) in a
// temporary directory outside the checkout and returns its path.
func Generate(t testing.TB) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "inventory")
	cmd := exec.Command("sh", filepath.Join(TemplateDir(t), "generate.sh"), "--module", ModulePath, "--dest", dest, "--git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate.sh: %v\n%s", err, out)
	}
	if rel, err := filepath.Rel(RepoRoot(t), dest); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the generated application must live outside the Kodama checkout: %s", dest)
	}
	return dest
}

func goEnv(name string) string {
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// BaseEnv is a minimal, explicit environment for commands run in a generated app.
func BaseEnv(extra ...string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir(), "GOTOOLCHAIN=local",
		"GOFLAGS=-mod=readonly", "GOCACHE=" + goEnv("GOCACHE"), "GOMODCACHE=" + goEnv("GOMODCACHE"), "GOPROXY=" + goEnv("GOPROXY"), "CGO_ENABLED=1"}
	return append(env, extra...)
}

type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// Tail returns the last n bytes of s.
func Tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// GoTest runs `go test -json` in dir and returns the outcome of every named test
// or subtest ("pass", "fail", "skip") keyed by "pkg.Test/Sub". A failing run
// fails the calling test with the failed names and the output tail.
func GoTest(t testing.TB, dir string, env []string, run string, timeout time.Duration, pkgs ...string) map[string]string {
	t.Helper()
	args := []string{"test", "-json", "-race", "-count=1", "-timeout=" + timeout.String()}
	if run != "" {
		args = append(args, "-run", run)
	}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = BaseEnv(env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	results := map[string]string{}
	var failed []string
	var log strings.Builder // the output of every test, kept for the failure report
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Action == "output" {
			log.WriteString(ev.Output)
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass", "fail", "skip":
			results[strings.TrimPrefix(ev.Package, ModulePath+"/")+"."+ev.Test] = ev.Action
			if ev.Action == "fail" {
				failed = append(failed, ev.Test)
			}
		}
	}
	if runErr != nil {
		t.Fatalf("go %s failed: %v\nfailed tests: %v\nstderr:\n%s\noutput tail:\n%s", strings.Join(args, " "), runErr, failed, errb.String(), Tail(log.String(), 12000))
	}
	return results
}

// RequirePassed asserts that each named test ran and passed: a skip or a missing
// test is not acceptance.
func RequirePassed(t testing.TB, results map[string]string, names ...string) {
	t.Helper()
	for _, n := range names {
		switch results[n] {
		case "pass":
		case "":
			t.Errorf("%s did not run (results: %v)", n, keys(results))
		default:
			t.Errorf("%s: %s", n, results[n])
		}
	}
	for n, r := range results {
		if r == "skip" {
			t.Errorf("%s was skipped: skipped service tests are not acceptance", n)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// RealEnv returns the environment that lets the generated application's
// real-delivery tests reach the provisioned development scope: a kind cluster,
// its registry and PostgreSQL. The scope is `make dev-up DEV_SCOPE=...` of this
// worktree (INVENTORY_DEV_ENV overrides the directory). In the complete layer a
// scope that is not provisioned fails the test: a skip is not acceptance.
func RealEnv(t testing.TB, app string) []string {
	t.Helper()
	layer.SkipService(t, "Kubernetes, registry and container runtime")
	dir := os.Getenv("INVENTORY_DEV_ENV")
	if dir == "" {
		scope := os.Getenv("DEV_SCOPE")
		if scope == "" {
			scope = ResourcePrefix
		}
		dir = filepath.Join(RepoRoot(t), "dev", ".state", "environments", scope)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); err != nil {
		t.Fatalf("the development scope %s is not provisioned (make dev-up DEV_SCOPE=%s): %v", dir, filepath.Base(dir), err)
	}
	env := []string{"INVENTORY_DEV_ENV=" + dir, "INVENTORY_APP_SRC=" + app, "INVENTORY_REAL_PREFIX=" + ResourcePrefix}
	for _, name := range []string{"INVENTORY_CONTAINER_RUNTIME", "INVENTORY_KUBECTL", "INVENTORY_REAL_EVIDENCE_DIR"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	// The generated app's host-network tool container needs an address for the
	// provisioned PostgreSQL that the host can route. The coordinator may
	// provide that proxy URL when the kind network is not host-reachable.
	if v := os.Getenv("KODAMA_TEST_POSTGRES_ADMIN_URL"); v != "" {
		env = append(env, "KODAMA_TEST_POSTGRES_ADMIN_URL="+v)
	}
	return env
}
