//go:build integration

package native_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// TestGitServeBinaryRefusesTheRemovedPolicyInputs pins the removed policy-file
// fallback at the real entrypoint: --policy is an unknown flag (exit 2) and the
// removed KODAMA_GIT_POLICY variable is ignored, so the command reaches its
// normal required-flag refusal instead of reading a policy file.
func TestGitServeBinaryRefusesTheRemovedPolicyInputs(t *testing.T) {
	environment := []string{
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
	}

	stdout, stderr, code := provisionCLI(t, environment, "git", "serve", "--policy", filepath.Join(t.TempDir(), "git.conf"))
	if code != 2 {
		t.Fatalf("git serve --policy exited %d; want 2 (stdout %q stderr %q)", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("git serve --policy wrote stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "flag provided but not defined") || !strings.Contains(stderr, "policy") {
		t.Fatalf("stderr = %q; want the unknown-flag usage error naming policy", stderr)
	}

	environment = append(environment, "KODAMA_GIT_POLICY="+filepath.Join(t.TempDir(), "git.conf"))
	_, stderr, code = provisionCLI(t, environment, "git", "serve")
	if code != 2 {
		t.Fatalf("git serve with only KODAMA_GIT_POLICY exited %d; want the required-flag refusal (2)", code)
	}
	if !strings.Contains(stderr, "git serve requires --tenant, --storage-root and --journal-dir") {
		t.Fatalf("stderr = %q; want the required-flag refusal", stderr)
	}
	if strings.Contains(stderr, "policy") {
		t.Fatalf("stderr = %q; the removed KODAMA_GIT_POLICY variable must not be part of the contract", stderr)
	}
}

// TestCommittedPolicyMissingDependencyIs503 pins the served smart-HTTP
// dependency refusal: a missing admin repository and a missing trusted ref both
// return 503 with the generic status body, no storage path and no policy
// content.
func TestCommittedPolicyMissingDependencyIs503(t *testing.T) {
	faults := []struct {
		name   string
		mutate func(t *testing.T, harness *nativefix.Harness)
	}{
		{
			name: "admin repository removed",
			mutate: func(t *testing.T, harness *nativefix.Harness) {
				if err := os.RemoveAll(harness.RepositoryPath(nativefix.AdminRepository)); err != nil {
					t.Fatalf("remove admin repository: %v", err)
				}
			},
		},
		{
			name: "trusted ref removed",
			mutate: func(t *testing.T, harness *nativefix.Harness) {
				adminPath := harness.RepositoryPath(nativefix.AdminRepository)
				if _, stderr, code := harness.Git(t, "--git-dir="+adminPath, "update-ref", "-d", "refs/heads/main"); code != 0 {
					t.Fatalf("delete trusted ref: %s", stderr)
				}
			},
		},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			harness := newHarness(t, nativefix.Options{})
			seedWebWithHooks(t, harness)
			fault.mutate(t, harness)

			request, err := http.NewRequest(http.MethodGet,
				harness.BaseURL+"/shop/web/info/refs?service=git-upload-pack", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			request.SetBasicAuth("kodama", harness.HumanToken)
			response, err := nativefix.NewHTTPClient().Do(request)
			if err != nil {
				t.Fatalf("GET info/refs: %v", err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read refusal body: %v", err)
			}
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d; want 503 (body %q)", response.StatusCode, body)
			}
			if string(body) != http.StatusText(http.StatusServiceUnavailable)+"\n" {
				t.Fatalf("body = %q; want the generic status text", body)
			}
			if strings.Contains(string(body), harness.StorageRoot) {
				t.Fatalf("body %q leaks the storage root", body)
			}
		})
	}
}

// TestCommittedPolicyHasNoWorkingTreeSource pins that there is no working-tree
// fallback: a git.conf in the working tree is not read when the committed
// policy is unavailable.
func TestCommittedPolicyHasNoWorkingTreeSource(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	base := seedWebWithHooks(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-working-tree-policy")
	candidate := harness.Commit(t, human, "working-tree-policy-refused")

	// The removed bootstrap location: a permissive file beside the storage
	// root. The app must never consult it.
	strayPolicy := filepath.Join(harness.Root, "git.conf")
	if err := os.WriteFile(strayPolicy, []byte("repo shop/web\n    RW = @all\n"), 0o600); err != nil {
		t.Fatalf("write stray policy: %v", err)
	}
	if err := os.RemoveAll(harness.RepositoryPath(nativefix.AdminRepository)); err != nil {
		t.Fatalf("remove admin repository: %v", err)
	}

	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != base {
		t.Fatalf("refused push moved main to %q; want %q (candidate %q)", observed, base, candidate)
	}
}
