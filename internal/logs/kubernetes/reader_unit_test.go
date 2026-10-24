//go:build unit

package kubernetes

import (
	"os"
	"path/filepath"
	"testing"
)

func Test_real_cluster_ignores_exported_kubeconfig_in_unit(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "kubectl-called")
	spy := "#!/bin/sh\nprintf called > \"$KODAMA_SPY_MARKER\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(spy), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "kubeconfig")
	data := "current-context: unit\nclusters:\n- name: unit\n  cluster:\n    server: https://127.0.0.1:1\ncontexts:\n- name: unit\n  context:\n    cluster: unit\n    user: unit\nusers:\n- name: unit\n  user: {}\n"
	if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kubeconfigEnv, config)
	t.Setenv("KODAMA_SPY_MARKER", marker)
	t.Setenv("PATH", dir)
	t.Run("real_test_skips", func(t *testing.T) {
		TestKubernetesOwnership(t)
		t.Fatal("real-cluster test must skip under the unit tag")
	})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unit test invoked kubectl")
	}
}
