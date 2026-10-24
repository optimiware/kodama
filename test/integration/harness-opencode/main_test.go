package harnessopencode_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain fixes the task's resource prefix, makes kubectl findable and removes
// the build directories and local image tags this invocation created.
func TestMain(m *testing.M) {
	if os.Getenv("KODAMA_RESOURCE_PREFIX") == "" {
		os.Setenv("KODAMA_RESOURCE_PREFIX", "t031")
	}
	cleanupDir := shimKubectl()
	code := m.Run()
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
	if cleanupDir != "" {
		_ = os.RemoveAll(cleanupDir)
	}
	os.Exit(code)
}

// shimKubectl puts a `kubectl` on PATH when none is installed, using the pinned
// binary the development environment downloaded next to the kubeconfig
// (dev/.tools/kubectl-<version>). It never installs anything.
func shimKubectl() string {
	if _, err := exec.LookPath("kubectl"); err == nil {
		return ""
	}
	kc := os.Getenv("KODAMA_TEST_KUBECONFIG")
	if kc == "" {
		return ""
	}
	tools := filepath.Join(filepath.Dir(kc), "..", "..", "..", ".tools")
	matches, _ := filepath.Glob(filepath.Join(tools, "kubectl-*"))
	if len(matches) == 0 {
		return ""
	}
	dir, err := os.MkdirTemp("", "kodama-t031-bin-")
	if err != nil {
		return ""
	}
	if err := os.Symlink(matches[0], filepath.Join(dir, "kubectl")); err != nil {
		return ""
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}
