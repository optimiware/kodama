// Package delivery_test is the joined deployment qualification of task T-067:
// admission -> persisted snapshot -> runtime context -> delivery record ->
// the REAL tools image's apply/lookup/delete scripts running under the ordinary
// step restrictions against a REAL kind cluster -> cleanup by the REAL expiry
// scheduler and a bound cleanup run, all over REAL PostgreSQL (non-owner role,
// forced row-level security) and the real runtime handlers.
//
// Resource prefix t067. The package builds the real tools image once, pushes it
// to the scope registry (the kubelet pulls it from there, and the real OCI
// verifier registers it by digest), and mounts the task's kind cluster. Nothing
// skips: a missing cluster, container provider or registry fails the test.
//
// The doubles are the ones the fixtures document: the workload runtime is
// execution's Manual runtime (the tests start the image themselves with the
// ordinary-step restrictions and report the attempt's observations and result
// manifest), and Git is observed through the fixture. The step entrypoint and
// the Podman runtime adapter are not in this path; T-038 qualifies the whole
// agent/delivery tracer.
package delivery_test

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"

	"github.com/optimiware/kodama/test/integration/delivery/stack"
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

// TestMain removes what this invocation created outside the cluster namespaces
// (its local image tags). Nothing else is touched.
func TestMain(m *testing.M) {
	code := m.Run()
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
	stack.Close()
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := goruntime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func toolsDir() string { return filepath.Join(repoRoot(), "images", "deployment-tools") }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
