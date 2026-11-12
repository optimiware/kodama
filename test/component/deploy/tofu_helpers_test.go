//go:build component

// Shared helpers for the OpenTofu qualification checks in this package. They
// locate the pinned OpenTofu binary the same way the chart tests locate helm
// (PATH first, then ~/.local/bin), keep a single provider cache under the short
// run TMPDIR, and copy the deploy tree into a test-owned directory so `tofu
// init` never writes `.terraform` or a lock file into the repository.
package deploy

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// tofuEnvOverride points the suite at a specific OpenTofu binary without
// changing PATH.
const tofuEnvOverride = "KODAMA_TOFU_BINARY"

// tofuPluginCacheOverride shares one pre-populated provider cache across runs
// (and across mutation copies). Otherwise the cache lives in the run's short
// TMPDIR so it is removed with the run directory.
const tofuPluginCacheOverride = "KODAMA_TOFU_PLUGIN_CACHE_DIR"

// tofuBinary locates OpenTofu. A missing binary is a loud skip only when
// KODAMA_REQUIRE_INFRA is unset; with it set the check fails because the
// qualification claim would otherwise be vacuous.
func tofuBinary(t *testing.T) string {
	t.Helper()
	if override := os.Getenv(tofuEnvOverride); override != "" {
		info, err := os.Stat(override)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			if os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
				t.Fatalf("%s=%s is not an executable file", tofuEnvOverride, override)
			}
			t.Skipf("%s=%s is not an executable file; skipping OpenTofu checks (set KODAMA_REQUIRE_INFRA=1 to fail instead)", tofuEnvOverride, override)
		}
		return override
	}
	if path, err := exec.LookPath("tofu"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		// The pinned host install carries both the stable name and the exact
		// versioned name; accept either, preferring the stable one.
		for _, name := range []string{"tofu", "tofu-1.13.9"} {
			candidate := filepath.Join(home, ".local", "bin", name)
			if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	if os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
		t.Fatalf("tofu is required to validate the deployment roots but was not found on PATH or in ~/.local/bin (KODAMA_REQUIRE_INFRA=1)")
	}
	t.Skip("tofu not installed; skipping OpenTofu deployment-root checks (set KODAMA_REQUIRE_INFRA=1 to fail instead)")
	return ""
}

// tofuPluginCacheDir returns a provider cache shared by every root in one run.
// Sharing matters: six `tofu init` calls download each provider once instead of
// once per root. An externally supplied TF_CLI_CONFIG_FILE (for example a
// filesystem provider mirror) is inherited unchanged; this helper only sets the
// plugin cache, never a mirror of its own.
func tofuPluginCacheDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(tofuPluginCacheOverride)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "kodama-tofu-plugin-cache")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create OpenTofu plugin cache %s: %v", dir, err)
	}
	return dir
}

// credentialEnvPrefixes are removed from the child environment so an
// OpenTofu run proves the deployment roots validate without a cloud identity.
// Nothing here is needed by `init -backend=false` or `validate`.
var credentialEnvPrefixes = []string{
	"AWS_",
	"AZURE_",
	"ARM_",
	"CLOUDSDK_",
	"GOOGLE_",
	"SCW_",
	"TF_VAR_",
}

// tofuProcessEnv builds the child environment: the caller's environment minus
// cloud credentials, plus automation and the shared plugin cache. TF_PLUGIN_CACHE_DIR
// must remain writable and, because the test never contacts a backend, no state
// path is configured.
func tofuProcessEnv(cacheDir string) []string {
	var env []string
	for _, entry := range os.Environ() {
		name := entry
		if before, _, ok := strings.Cut(entry, "="); ok {
			name = before
		}
		drop := false
		for _, prefix := range credentialEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			env = append(env, entry)
		}
	}
	return append(env,
		"TF_IN_AUTOMATION=1",
		"CHECKPOINT_DISABLE=1",
		"TF_PLUGIN_CACHE_DIR="+cacheDir,
	)
}

// runTofu runs one OpenTofu command in dir and returns stdout+stderr. It runs
// exactly the binary tofuBinary located (KODAMA_TOFU_BINARY, then PATH, then
// ~/.local/bin) instead of letting exec resolve "tofu" again: a versioned
// install that is not on PATH must still be the one validated. The timeout
// keeps a hung provider download from consuming the package timeout.
func runTofu(t *testing.T, dir string, env []string, timeout time.Duration, args ...string) (string, int) {
	t.Helper()
	binary := tofuBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = dir
	command.Env = env
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			if ctx.Err() != nil {
				t.Fatalf("tofu %s timed out after %s\n%s", strings.Join(args, " "), timeout, output)
			}
			t.Fatalf("run tofu %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		code = exitError.ExitCode()
	}
	return string(output), code
}

// TestRunTofuUsesDiscoveredBinary proves runTofu executes the binary
// tofuBinary found rather than whatever "tofu" PATH resolves to. The child
// environment below has no tofu on PATH at all, so only the explicit
// KODAMA_TOFU_BINARY can succeed; a regression that hard-codes "tofu" fails
// here even on a host where the two happen to be the same file.
func TestRunTofuUsesDiscoveredBinary(t *testing.T) {
	dir := t.TempDir()
	pinned := filepath.Join(dir, "pinned-tofu")
	writeExecutable(t, pinned, "#!/bin/bash\nprintf 'pinned-tofu %s\\n' \"$*\"\n")
	t.Setenv(tofuEnvOverride, pinned)

	emptyPath := filepath.Join(dir, "empty-bin")
	if err := os.MkdirAll(emptyPath, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", emptyPath, err)
	}
	output, code := runTofu(t, dir, []string{"PATH=" + emptyPath}, time.Minute, "version")
	if code != 0 {
		t.Fatalf("runTofu with %s=%s exited %d:\n%s", tofuEnvOverride, pinned, code, output)
	}
	if got, want := strings.TrimSpace(output), "pinned-tofu version"; got != want {
		t.Fatalf("runTofu ran the wrong binary: output = %q, want %q", got, want)
	}
}

// copyDeployTree copies the repository deploy tree into t.TempDir. Running
// `tofu init` in the real tree would write `.terraform` and, for roots without
// a committed lock file, a new `.terraform.lock.hcl`; the copy keeps the
// candidate's changed-path set exactly the files the task actually edits. The
// whole tree is copied, not one root, because deploy/aws|gcp|scw reference
// ../modules/context.
func copyDeployTree(t *testing.T, repoRoot string) string {
	t.Helper()
	source := filepath.Join(repoRoot, "deploy")
	destination := filepath.Join(t.TempDir(), "deploy")
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
	if err != nil {
		t.Fatalf("copy deploy tree: %v", err)
	}
	return destination
}

// terraformBlock matches the top-level `terraform {` block every OpenTofu root
// declares. providerBlock matches a provider configuration. A directory with a
// .tf file carrying either is an OpenTofu root that must be validated.
var (
	terraformBlock = regexp.MustCompile(`(?m)^\s*terraform\s*\{`)
	providerBlock  = regexp.MustCompile(`(?m)^\s*provider\s+"[^"]+"\s*\{`)
)

// isTofuRoot reports whether dir contains a .tf file with a terraform or
// provider block. That is the discovery rule the validation suite uses, so a
// newly added root is found by walking the tree rather than by editing a list.
func isTofuRoot(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tf") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(dir, entry.Name()), err)
		}
		if terraformBlock.Match(body) || providerBlock.Match(body) {
			return true
		}
	}
	return false
}

// discoverTofuRoots walks deploy/aws, deploy/gcp and deploy/scw and returns
// every directory that declares a terraform or provider block, relative to the
// deploy tree with forward slashes. Hidden and `.terraform` directories are
// skipped so an accidental local init cannot add a root.
func discoverTofuRoots(t *testing.T, deployRoot string) []string {
	t.Helper()
	var roots []string
	for _, cloud := range []string{"aws", "gcp", "scw"} {
		root := filepath.Join(deployRoot, cloud)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				return nil
			}
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == ".terraform") {
				return fs.SkipDir
			}
			if !isTofuRoot(t, path) {
				return nil
			}
			relative, err := filepath.Rel(deployRoot, path)
			if err != nil {
				return err
			}
			roots = append(roots, filepath.ToSlash(relative))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return roots
}

// tofuRootSubtestName turns "aws/bootstrap" into a stable subtest name.
func tofuRootSubtestName(root string) string {
	return strings.ReplaceAll(root, "/", "_")
}
