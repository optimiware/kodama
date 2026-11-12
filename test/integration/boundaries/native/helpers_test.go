//go:build integration

package native_test

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func newHarness(t *testing.T, options nativefix.Options) *nativefix.Harness {
	t.Helper()
	if options.BinaryPath == "" {
		options.BinaryPath = kodamaBinary
	}
	if options.Logger == nil && os.Getenv("KODAMA_TEST_DEBUG") != "" {
		options.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	harness, err := nativefix.NewHarness(t, options)
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	return harness
}

// seedMain creates shop/web with one commit on refs/heads/main.
func seedMain(t *testing.T, harness *nativefix.Harness) (string, string) {
	t.Helper()
	harness.InitBareRepository(t, nativefix.Repository)
	harness.InstallProductHooks(t, nativefix.Repository)
	directory := filepath.Join(harness.Root, "seed-clone")
	if stderr, code := harness.Clone(t, nativefix.ExecutionToken, nativefix.Repository, directory); code != 0 {
		t.Fatalf("seed clone: %s", stderr)
	}
	commit := harness.Commit(t, directory, "initial")
	if stderr, code := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("seed push: %s", stderr)
	}
	return directory, commit
}

func cloneFor(t *testing.T, harness *nativefix.Harness, token, name string) string {
	t.Helper()
	directory := filepath.Join(harness.Root, name)
	if stderr, code := harness.Clone(t, token, nativefix.Repository, directory); code != 0 {
		t.Fatalf("clone %s: %s", name, stderr)
	}
	return directory
}

func requireSuccess(t *testing.T, stderr string, code int) {
	t.Helper()
	if code != 0 {
		t.Fatalf("command failed with %d: %s", code, stderr)
	}
}

func requireFailure(t *testing.T, stderr string, code int) {
	t.Helper()
	if code == 0 {
		t.Fatalf("command unexpectedly succeeded: %s", stderr)
	}
}

func requireRejectionMessage(t *testing.T, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, nativefixRejectionMessage) {
		t.Fatalf("stderr = %q; want %q", stderr, nativefixRejectionMessage)
	}
}

// nativefixRejectionMessage mirrors the product's single sanitized diagnostic.
const nativefixRejectionMessage = "kodama git: update rejected"

func httpStatus(t *testing.T, harness *nativefix.Harness, token, path string) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, harness.BaseURL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest(%q): %v", path, err)
	}
	if token != "" {
		request.SetBasicAuth("kodama", token)
	}
	response, err := nativefix.NewHTTPClient().Do(request)
	if err != nil {
		t.Fatalf("GET %q: %v", path, err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// probeScript writes a test-only pre-receive wrapper that runs the real hook
// binary and then records the ref, journal and environment while git is still
// in the pre-receive phase (receive-pack has not applied anything yet).
func probeScript(harness *nativefix.Harness, probePath string) string {
	journal := shellQuote(harness.JournalDir)
	return "#!/bin/sh\n" +
		"set -u\n" +
		shellQuote(harness.BinaryPath) + " git hook pre-receive\n" +
		"code=$?\n" +
		"{\n" +
		"  printf 'exit=%s\\n' \"$code\"\n" +
		"  printf 'ref='; git rev-parse --verify -q refs/heads/main 2>/dev/null || true; printf '\\n'\n" +
		"  printf 'journal-start\\n'\n" +
		"  for file in " + journal + "/*.json; do [ -f \"$file\" ] && cat \"$file\"; printf '\\n'; done\n" +
		"  printf 'journal-end\\n'\n" +
		"  printf 'env-start\\n'\n" +
		"  env\n" +
		"  printf 'env-end\\n'\n" +
		"} > " + shellQuote(probePath) + "\n" +
		"exit $code\n"
}

// barrierScript writes a test-only pre-receive wrapper that waits for the test
// to kill the hook channel before it forwards the real decision.
func barrierScript(harness *nativefix.Harness, barrierPath, ackPath string) string {
	return "#!/bin/sh\n" +
		"printf 'ready\\n' > " + shellQuote(barrierPath) + "\n" +
		"read ack < " + shellQuote(ackPath) + "\n" +
		"exec " + shellQuote(harness.BinaryPath) + " git hook pre-receive\n"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// gatedVerification blocks the service verification until released or the
// callback deadline fires, so the hook timeout is observable without sleeping.
type gatedVerification struct {
	inner   git.VerificationClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedVerification(inner git.VerificationClient) *gatedVerification {
	return &gatedVerification{
		inner:   inner,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (verification *gatedVerification) Verify(ctx context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	verification.once.Do(func() { close(verification.entered) })
	select {
	case <-verification.release:
	case <-ctx.Done():
		return api.VerifyResponse{}, ctx.Err()
	}
	return verification.inner.Verify(ctx, request)
}

func (verification *gatedVerification) Report(ctx context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	return verification.inner.Report(ctx, request)
}

func waitFor(t *testing.T, channel <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
