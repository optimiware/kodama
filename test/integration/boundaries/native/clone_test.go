//go:build integration

// Package native_test drives real Git 2.47.3 clients against the composed Git
// app: real smart-HTTP, real `git http-backend`, product-installed bare-repo
// hooks, the private Unix-socket callback channel, the real actor-handle
// registry and the contract fixture.
//
// The contract server and admission verifier are labelled fixtures of Shell
// facts, not the full Shell composition.
package native_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestCloneAndFetchOfAuthorizedRepository(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	seedDirectory, _ := seedMain(t, harness)

	human := cloneFor(t, harness, harness.HumanToken, "human-clone")
	execution := cloneFor(t, harness, nativefix.ExecutionToken, "execution-clone")

	second := harness.Commit(t, seedDirectory, "second")
	if stderr, code := harness.Push(t, seedDirectory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("push second: %s", stderr)
	}
	if stderr, code := harness.Fetch(t, human, harness.HumanToken); code != 0 {
		t.Fatalf("fetch: %s", stderr)
	}
	stdout, stderr, code := harness.GitIn(t, human, "rev-parse", "origin/main")
	if code != 0 {
		t.Fatalf("rev-parse origin/main: %s", stderr)
	}
	if strings.TrimSpace(stdout) != second {
		t.Fatalf("fetched commit = %q; want %q", strings.TrimSpace(stdout), second)
	}
	if stderr, code := harness.Fetch(t, execution, nativefix.ExecutionToken); code != 0 {
		t.Fatalf("execution fetch: %s", stderr)
	}
	if stdout, _, code := harness.GitIn(t, execution, "rev-parse", "origin/main"); code != 0 || strings.TrimSpace(stdout) != second {
		t.Fatalf("execution clone origin/main = %q; want %q", strings.TrimSpace(stdout), second)
	}
}

func TestUnauthorizedActorIsRefusedUniformly(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	seedMain(t, harness)

	if stderr, code := harness.Clone(t, "not-a-real-token", nativefix.Repository, filepath.Join(harness.Root, "denied-existing")); code == 0 {
		t.Fatalf("clone with an unknown credential succeeded: %s", stderr)
	}
	if stderr, code := harness.Clone(t, "not-a-real-token", "missing/repo", filepath.Join(harness.Root, "denied-missing")); code == 0 {
		t.Fatalf("clone of a missing repository with an unknown credential succeeded: %s", stderr)
	}
	existing := httpStatus(t, harness, "not-a-real-token", "/shop/web/info/refs?service=git-upload-pack")
	missing := httpStatus(t, harness, "not-a-real-token", "/missing/repo/info/refs?service=git-upload-pack")
	if existing != http.StatusUnauthorized || missing != http.StatusUnauthorized {
		t.Fatalf("unauthorized statuses = (%d, %d); want both 401", existing, missing)
	}
	// A valid actor is told nothing about an unconfigured repository beyond a
	// uniform hidden refusal.
	if status := httpStatus(t, harness, nativefix.ExecutionToken, "/missing/repo/info/refs?service=git-upload-pack"); status != http.StatusNotFound {
		t.Fatalf("unknown repository status = %d; want 404", status)
	}
}
