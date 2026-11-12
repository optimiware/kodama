//go:build integration

package native_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestPerTenantStorageIsolation(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	harness.InitBareRepository(t, nativefix.Repository)
	otherPath := harness.InitBareRepositoryFor(t, nativefix.OtherTenant, "secret")

	// A symlinked repository inside the tenant area must not reach another
	// tenant's directory.
	link := filepath.Join(harness.RepositoryPath("shop"), "linked")
	if err := os.Symlink(otherPath, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"authorized", "/shop/web/info/refs?service=git-upload-pack", http.StatusOK},
		{"git suffix", "/shop/web.git/info/refs?service=git-upload-pack", http.StatusOK},
		{"parent traversal", "/../tenant-other/secret/info/refs?service=git-upload-pack", http.StatusBadRequest},
		{"encoded slash", "/shop%2fweb/info/refs?service=git-upload-pack", http.StatusBadRequest},
		{"other tenant path", "/tenant-other/secret/info/refs?service=git-upload-pack", http.StatusNotFound},
		{"double git suffix", "/shop/web.git.git/info/refs?service=git-upload-pack", http.StatusNotFound},
		{"symlinked repository", "/shop/linked/info/refs?service=git-upload-pack", http.StatusBadRequest},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if status := httpStatus(t, harness, nativefix.ExecutionToken, testCase.path); status != testCase.want {
				t.Fatalf("status = %d; want %d", status, testCase.want)
			}
		})
	}

	if stderr, code := harness.Clone(t, nativefix.ExecutionToken, "tenant-other/secret", filepath.Join(harness.Root, "other-clone")); code == 0 {
		t.Fatalf("another tenant's repository was reachable: %s", stderr)
	}
	if stderr, code := harness.Clone(t, nativefix.ExecutionToken, "shop/linked", filepath.Join(harness.Root, "linked-clone")); code == 0 {
		t.Fatalf("a symlinked cross-tenant repository was reachable: %s", stderr)
	}
}
