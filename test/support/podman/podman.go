// Package podman locates the Podman engine and the test resources real
// Podman runtime tests share.
//
//	KODAMA_REQUIRE_PODMAN=1  fail instead of skip when Podman is unusable
//	KODAMA_TEST_PODMAN_DIR   a directory the Podman machine shares
//	                         (default: the user cache directory on macOS,
//	                         the test's temporary directory elsewhere)
package podman

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/layer"
)

// Image is the pinned image real Podman tests run (the base image pinned by
// the Kubernetes suites).
const Image = "docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

// Require skips the test unless the Podman engine answers (fails instead with
// KODAMA_REQUIRE_PODMAN=1) and pulls Image once.
func Require(t testing.TB) {
	t.Helper()
	layer.SkipService(t, "Podman")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "podman", "info", "--format", "{{.Host.Arch}}").CombinedOutput(); err != nil {
		if os.Getenv("KODAMA_REQUIRE_PODMAN") == "1" {
			t.Fatalf("podman is required: %v: %s", err, out)
		}
		t.Skipf("podman is unavailable: %v", err)
	}
	if out, err := exec.CommandContext(ctx, "podman", "image", "exists", Image).CombinedOutput(); err != nil {
		if out, err = exec.CommandContext(ctx, "podman", "pull", "--quiet", Image).CombinedOutput(); err != nil {
			t.Fatalf("pulling %s: %v: %s", Image, err, out)
		}
	}
}

// Arch is the Podman machine's Go architecture.
func Arch(t testing.TB) string {
	t.Helper()
	out, err := exec.Command("podman", "info", "--format", "{{.Host.Arch}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// SharedDir returns a fresh directory the Podman machine can bind-mount,
// removed after the test.
func SharedDir(t testing.TB) string {
	t.Helper()
	root := os.Getenv("KODAMA_TEST_PODMAN_DIR")
	if root == "" && runtime.GOOS == "darwin" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(cache, "kodama-test")
	}
	if root == "" {
		return t.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "podman-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
