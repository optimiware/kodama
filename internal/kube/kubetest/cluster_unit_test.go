//go:build unit

package kubetest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// Test_require_cluster_skips_unit: in the unit layer Require skips before it
// inspects anything, even when Kubernetes is marked required and a kubeconfig is
// present; it never reaches the cluster, a container runtime or an image build.
func Test_require_cluster_skips_unit(t *testing.T) {
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte("not a kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KODAMA_TEST_KUBECONFIG", kc)
	t.Setenv("KODAMA_REQUIRE_KUBERNETES", "1")
	t.Setenv("PATH", t.TempDir()) // no kubectl or container runtime is reachable

	t.Run("present", func(t *testing.T) {
		kubetest.Require(t)
		t.Fatal("Require must skip in the unit layer")
	})
	t.Run("missing_and_required", func(t *testing.T) {
		t.Setenv("KODAMA_TEST_KUBECONFIG", filepath.Join(t.TempDir(), "absent"))
		kubetest.Require(t)
		t.Fatal("Require must skip in the unit layer, not fail on a missing cluster")
	})
}

func Test_kubeconfig_scope_contract(t *testing.T) {
	t.Setenv("KODAMA_TEST_KUBECONFIG", "")
	t.Setenv("DEV_SCOPE", "batch")
	t.Setenv("KODAMA_RESOURCE_PREFIX", "t036")
	want := filepath.Join("dev", ".state", "environments", "batch", "kubeconfig")
	got := kubetest.KubeconfigPath()
	if len(got) < len(want) || got[len(got)-len(want):] != want {
		t.Fatalf("scope path %q does not end in %q", got, want)
	}
	t.Setenv("DEV_SCOPE", "")
	want = filepath.Join("environments", "t036", "kubeconfig")
	got = kubetest.KubeconfigPath()
	if len(got) < len(want) || got[len(got)-len(want):] != want {
		t.Fatalf("resource fallback %q does not end in %q", got, want)
	}
	t.Setenv("KODAMA_TEST_KUBECONFIG", "/explicit/kubeconfig")
	if got := kubetest.KubeconfigPath(); got != "/explicit/kubeconfig" {
		t.Fatalf("explicit override lost: %q", got)
	}
}
