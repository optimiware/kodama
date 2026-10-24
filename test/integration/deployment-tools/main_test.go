package deploymenttools_test

// Resource prefix t065. The package builds the real tools image once, runs the
// real scripts in it under the execution restrictions the platform selects for
// ordinary steps, and points them at a real kind cluster and at the record
// interface fixture (callbacks_test.go).

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
)

var (
	cleanupMu sync.Mutex
	cleanups  []func()
)

func onCleanup(f func()) {
	cleanupMu.Lock()
	cleanups = append(cleanups, f)
	cleanupMu.Unlock()
}

// TestMain removes what this invocation created outside the cluster namespaces:
// its image tags and the node's imported image names. Nothing else is touched.
func TestMain(m *testing.M) {
	code := m.Run()
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := goruntime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func toolsDir() string { return filepath.Join(repoRoot(), "images", "deployment-tools") }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var (
	imageOnce sync.Once
	imageTag  string
	imageErr  error
)

// toolsImage builds the image with the shipped build script (a failed build fails
// the test) and returns its local tag. KODAMA_TOOLS_IMAGE names a prebuilt local
// image and skips the build.
func toolsImage(t *testing.T) string {
	t.Helper()
	imageOnce.Do(func() {
		if pre := os.Getenv("KODAMA_TOOLS_IMAGE"); pre != "" {
			imageTag = pre
			return
		}
		imageTag = "localhost/kodama-deployment-tools:t065-" + randHex(4)
		out := os.TempDir()
		evidence, err := os.MkdirTemp(out, "t065-evidence-")
		if err != nil {
			imageErr = err
			return
		}
		onCleanup(func() { _ = os.RemoveAll(evidence) })
		onCleanup(func() { _, _ = shell("podman", "rmi", imageTag) })
		cmd := exec.Command(filepath.Join(toolsDir(), "build.sh"), "--tag", imageTag, "--out", evidence)
		if b, err := cmd.CombinedOutput(); err != nil {
			imageErr = &buildError{err: err, out: string(b)}
		}
	})
	if imageErr != nil {
		t.Fatalf("building the tools image: %v", imageErr)
	}
	return imageTag
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + strings.TrimSpace(e.out) }
