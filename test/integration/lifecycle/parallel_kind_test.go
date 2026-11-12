//go:build integration

package lifecycle

import (
	"testing"

	"github.com/optimiware/kodama/test/support/parallel"
)

// lifecycleKindGate bounds how many private kind clusters this package runs at
// once. The default follows the launcher's kind pool on a host sized for fewer.
var lifecycleKindGate = &parallel.Gate{
	Env:         "KODAMA_LIFECYCLE_KIND_CONCURRENCY",
	Default:     3,
	Max:         6,
	HostPoolEnv: "KODAMA_KIND_SLOTS",
}

// acquireLifecycleKindSlot must be the first statement of every test that
// starts a kind cluster: the slot is released only after the test's cleanups
// have deleted that cluster.
func acquireLifecycleKindSlot(t *testing.T) {
	t.Helper()
	lifecycleKindGate.Enter(t)
}
