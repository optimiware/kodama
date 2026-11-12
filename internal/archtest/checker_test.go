package archtest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// checkerPath is built once in TestMain. The checker contract is:
//
//	go run ./tools/check -root <dir> <check>...
//
// with checks imports and docs.
// Exit 0 means clean, 1 means findings on stdout, and 2 means usage or IO error.
var checkerPath string

func TestMain(m *testing.M) {
	root := repoRoot()
	dir, err := os.MkdirTemp("", "kodama-archtest-checker-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create checker temp dir: %v\n", err)
		os.Exit(1)
	}
	checkerPath = filepath.Join(dir, "check")

	cmd := exec.Command("go", "build", "-o", checkerPath, "./tools/check")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build ./tools/check: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("archtest: cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

type checkResult struct {
	code   int
	stdout string
	stderr string
}

func runCheck(t *testing.T, root string, checks ...string) checkResult {
	t.Helper()
	args := append([]string{"-root", root}, checks...)
	cmd := exec.Command(checkerPath, args...)
	cmd.Dir = repoRoot()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run checker %v: %v\nstderr:\n%s", checks, err, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	return checkResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func assertClean(t *testing.T, res checkResult) {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("checker exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	if strings.TrimSpace(res.stdout) != "" {
		t.Fatalf("clean check printed findings:\n%s", res.stdout)
	}
}

func assertFindings(t *testing.T, res checkResult, wants ...string) {
	t.Helper()
	if res.code != 1 {
		t.Fatalf("checker exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	trimmed := strings.TrimRight(res.stdout, "\n")
	if strings.TrimSpace(trimmed) == "" {
		t.Fatalf("checker exited 1 without findings on stdout; stderr:\n%s", res.stderr)
	}
	for line := range strings.SplitSeq(trimmed, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		idx := strings.Index(line, ": ")
		if idx <= 0 {
			t.Errorf("finding %q does not match \"<path>: <message>\"", line)
			continue
		}
		prefix := line[:idx]
		if filepath.IsAbs(prefix) {
			t.Errorf("finding path %q is absolute; want a root-relative path", prefix)
		}
	}
	lower := strings.ToLower(res.stdout)
	for _, want := range wants {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("findings do not mention %q:\n%s", want, res.stdout)
		}
	}
}

func assertUsageError(t *testing.T, res checkResult) {
	t.Helper()
	if res.code != 2 {
		t.Fatalf("checker exit = %d, want 2 (usage/IO error)\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(), "internal", "archtest", "testdata", filepath.FromSlash(name))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return path
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dst := t.TempDir()
	if err := copyTree(fixturePath(t, name), dst); err != nil {
		t.Fatalf("copy fixture %s: %v", name, err)
	}
	return dst
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Usage contract
// ---------------------------------------------------------------------------

func TestCheckerUsageErrors(t *testing.T) {
	t.Run("unknown check", func(t *testing.T) {
		assertUsageError(t, runCheck(t, repoRoot(), "not-a-check"))
	})
	t.Run("missing root", func(t *testing.T) {
		assertUsageError(t, runCheck(t, filepath.Join(t.TempDir(), "does-not-exist"), "imports"))
	})
	t.Run("no checks", func(t *testing.T) {
		assertUsageError(t, runCheck(t, repoRoot()))
	})
}

// ---------------------------------------------------------------------------
// imports: S01 layer rules
// ---------------------------------------------------------------------------

func TestImportsCleanFixture(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "imports/clean"), "imports"))
}

func TestImportsNeutralTypesRejectProductImports(t *testing.T) {
	root := fixturePath(t, "imports/neutral-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "api/dto.go", "internal/id/id.go", "internal/fault/fault.go")
}

func TestImportsNeutralAndAPIAreStandardLibraryOnly(t *testing.T) {
	root := fixturePath(t, "imports/neutral-api-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "api/dto.go", "internal/id/id.go")
}

func TestImportsKernelRejectsShellUserlandAndInfrastructure(t *testing.T) {
	root := fixturePath(t, "imports/kernel-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/kernel/flow/flow.go")
}

func TestImportsKernelDependencyCycle(t *testing.T) {
	root := fixturePath(t, "imports/kernel-cycle")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/kernel/", "cycle")
}

func TestImportsShellCapabilityRejectsStoreAndUserland(t *testing.T) {
	root := fixturePath(t, "imports/shell-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/shell/request/request.go", "internal/shell/tenancy/tenancy.go")
}

func TestImportsShellCapabilityAllowsRecordedExternalDependency(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "imports/shell-approved-external"), "imports"))
}

func TestImportsRecordedExternalDependencyRejectedOutsideCapabilityLayers(t *testing.T) {
	res := runCheck(t, fixturePath(t, "imports/allowlist-scope"), "imports")
	assertFindings(t, res, "api/dto.go", "internal/id/id.go", "internal/kernel/flow/flow.go",
		`forbidden external import "go.yaml.in/yaml/v3"`)
	if got := len(strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")); got != 3 {
		t.Errorf("finding count = %d, want 3:\n%s", got, res.stdout)
	}
}

func TestImportsShellCapabilityRejectsUnapprovedExternalDependency(t *testing.T) {
	res := runCheck(t, fixturePath(t, "imports/shell-unapproved-external"), "imports")
	assertFindings(t, res, "internal/shell/pipeline/other.go",
		`forbidden external import "github.com/example/external" from shell capability package`)
}

func TestImportsShellTransportRejectsSQL(t *testing.T) {
	root := fixturePath(t, "imports/shell-transport-sql")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/shell/transport/httpapi/httpapi.go")
}

func TestImportsCapabilityAcceptsNeutralStoreSeam(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "imports/capability-imports-clean"), "imports"))
}

func TestImportsCapabilityRejectsConcreteStoreAdapters(t *testing.T) {
	root := fixturePath(t, "imports/capability-imports-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res,
		"internal/shell/secret/secret.go",
		"internal/shell/tenancy/tenancy.go",
		"internal/shell/store/postgres",
		"internal/shell/store/memory",
	)
}

func TestImportsGatewayStdlibAllowlist(t *testing.T) {
	root := fixturePath(t, "imports/capability-imports-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/shell/mcpproxy/mcpproxy.go", `"net/http"`)
	if strings.Contains(res.stdout, "internal/shell/gateway/gateway.go") {
		t.Errorf("gateway allowlisted net/http import was reported:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "internal/shell/gateway/gateway_test.go") {
		t.Errorf("gateway allowlisted test net/http import was reported:\n%s", res.stdout)
	}
}

func TestImportsNeutralAPIAndKernelRejectStoreSeam(t *testing.T) {
	root := fixturePath(t, "imports/capability-imports-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "api/dto.go", "internal/id/id.go", "internal/kernel/flow/flow.go")
}

func TestImportsKernelTestFileStillCannotImportShell(t *testing.T) {
	root := fixturePath(t, "imports/capability-imports-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/kernel/flow/flow_test.go")
}

func TestImportsTransportMayImportSiblingTransport(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "imports/capability-imports-clean"), "imports"))
}

// TestInstallationURLImportExceptionIsExact pins the single standard-library
// exception granted to internal/installation: the pure URL parser net/url is
// the only net package it may import, and no other layer inherits the
// exception. The exception is keyed by exact directory, so it covers both
// production and _test.go files of internal/installation but not its
// subdirectories.
func TestInstallationURLImportExceptionIsExact(t *testing.T) {
	root := fixturePath(t, "imports/installation-url-exception")
	res := runCheck(t, root, "imports")
	assertFindings(t, res,
		"internal/installation/net.go",
		"internal/installation/netip.go",
		"internal/installation/httpclient.go",
		"internal/installation/publishtest_test.go",
		"internal/installation/sub/url.go",
		"internal/installation/exec.go",
		"internal/installation/tls.go",
		"internal/kernel/flow/url.go",
		"internal/id/url.go",
		"internal/installation-evil/url.go",
		"internal/shell/secret/url.go",
		`forbidden standard-library import "net/netip"`,
	)
	if strings.Contains(res.stdout, "internal/installation/url.go") {
		t.Errorf("installation net/url import was reported:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "internal/installation/urlparse_test.go") {
		t.Errorf("installation test net/url import was reported:\n%s", res.stdout)
	}
	if got := len(strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")); got != 11 {
		t.Errorf("finding count = %d, want 11:\n%s", got, res.stdout)
	}
}

func TestImportsUserlandRejectsKernelAndShellInternals(t *testing.T) {
	root := fixturePath(t, "imports/userland-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/userland/cli/cli.go", "client/client.go")
}

func TestUserlandAppLayerImportsOnlyPublicContracts(t *testing.T) {
	for _, path := range []string{"internal/userland/git/app/main.go", "internal/userland/agent/app/run.go", "internal/userland/services/app/run.go"} {
		if got := classifyPath(path); got != layerUserlandApp {
			t.Errorf("classifyPath(%q) = %s, want userland app", path, got)
		}
	}
	if got := classifyPath("cmd/kodama/main.go"); got != layerComposition {
		t.Errorf("cmd must stay composition, got %s", got)
	}
	for _, imported := range []layer{layerNeutral, layerAPI, layerUserland, layerUserlandApp} {
		if !productImportAllowed(layerUserlandApp, imported) {
			t.Errorf("userland app must be able to import %s", imported)
		}
	}
	for _, imported := range []layer{layerKernel, layerShellCapability, layerShellAdapter, layerShellTransport, layerComposition, layerInstallation, layerTooling} {
		if productImportAllowed(layerUserlandApp, imported) {
			t.Errorf("userland app must not import %s", imported)
		}
	}
}

func TestClassifyPathTestLayer(t *testing.T) {
	paths := []string{
		"test/contracts/kernel/consumer_test.go",
		"test/component/store/postgres_test.go",
		"test/integration/flow_test.go",
		"test/e2e/cli_test.go",
		"test/qualification/system_test.go",
		"test/support/factory/factory.go",
	}
	for _, path := range paths {
		if got := classifyPath(path); got != layerTest {
			t.Errorf("classifyPath(%q) = %s, want test", path, got)
		}
	}
}

func TestTestLayerImportPolicy(t *testing.T) {
	productLayers := []layer{
		layerNeutral, layerAPI, layerKernel, layerShellCapability, layerShellAdapter,
		layerShellTransport, layerUserland, layerUserlandApp, layerComposition, layerInstallation, layerTooling,
	}
	for _, imported := range productLayers {
		if !productImportAllowed(layerTest, imported) {
			t.Errorf("test package must be able to import %s layer", imported)
		}
		if productImportAllowed(imported, layerTest) {
			t.Errorf("%s package must not import test layer", imported)
		}
	}
	for _, stdlib := range infrastructureStdlib {
		if forbiddenStdlibImport(layerTest, "test/contracts/adapters/imports_test.go", stdlib) {
			t.Errorf("test package must be able to import standard library %q", stdlib)
		}
	}
}

func TestClassifyPathStoreSeam(t *testing.T) {
	cases := map[string]layer{
		"internal/shell/store":               layerShellCapability,
		"internal/shell/store/executor.go":   layerShellCapability,
		"internal/shell/store/postgres":      layerShellAdapter,
		"internal/shell/store/postgres/x.go": layerShellAdapter,
		"internal/shell/store/memory":        layerShellAdapter,
		"internal/shell/store/memory/x.go":   layerShellAdapter,
	}
	for path, want := range cases {
		if got := classifyPath(path); got != want {
			t.Errorf("classifyPath(%q) = %s, want %s", path, got, want)
		}
	}
}

func TestClassifyPathKeepsTestFilesInTheirProductLayer(t *testing.T) {
	cases := map[string]layer{
		"internal/kernel/flow/flow_test.go":       layerKernel,
		"internal/shell/secret/secret_test.go":    layerShellCapability,
		"internal/shell/gateway/gateway_test.go":  layerShellCapability,
		"test/contracts/adapters/imports_test.go": layerTest,
	}
	for path, want := range cases {
		if got := classifyPath(path); got != want {
			t.Errorf("classifyPath(%q) = %s, want %s", path, got, want)
		}
	}
}

func TestImportsTestConsumersMayImportProductLayers(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "imports/test-consumer"), "imports"))
}

func TestImportsProductLayersRejectTestImports(t *testing.T) {
	root := fixturePath(t, "imports/test-import-forbidden")
	res := runCheck(t, root, "imports")
	assertFindings(t, res, "internal/kernel/flow/flow.go", `forbidden import "kodama/test/support/factory"`, "kernel package")
}

func TestImportsAcceptsRealRepository(t *testing.T) {
	assertClean(t, runCheck(t, repoRoot(), "imports"))
}

// ---------------------------------------------------------------------------
// docs: Markdown links, anchors and JSON
// ---------------------------------------------------------------------------

func TestDocsValidFixture(t *testing.T) {
	assertClean(t, runCheck(t, fixturePath(t, "docs/valid"), "docs"))
}

func TestDocsRejectsBrokenFileLink(t *testing.T) {
	root := copyFixture(t, "docs/valid")
	appendFile(t, filepath.Join(root, "docs", "README.md"), "\n[missing](missing.md)\n")
	assertFindings(t, runCheck(t, root, "docs"), "missing.md")
}

func TestDocsRejectsBrokenAnchor(t *testing.T) {
	root := copyFixture(t, "docs/valid")
	appendFile(t, filepath.Join(root, "docs", "README.md"), "\n[bad](guide.md#missing-anchor)\n")
	assertFindings(t, runCheck(t, root, "docs"), "missing-anchor")
}

func TestDocsRejectsInvalidJSON(t *testing.T) {
	root := copyFixture(t, "docs/valid")
	writeFile(t, filepath.Join(root, "docs", "bad.json"), "{\n")
	assertFindings(t, runCheck(t, root, "docs"), "bad.json")
}

func TestDocsAcceptsRealRepository(t *testing.T) {
	assertClean(t, runCheck(t, repoRoot(), "docs"))
}
