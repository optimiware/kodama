package restorequal

import (
	"testing"

	"github.com/optimiware/kodema/test/support/parallel"
)

// restoreGate bounds how many local restore drills run at once. Each drill
// owns a complete source and target stack, so the default follows the
// launcher's stack pool on a host sized for fewer.
var restoreGate = &parallel.Gate{
	Env:         "KODEMA_Q03_CONCURRENCY",
	Default:     4,
	Max:         8,
	HostPoolEnv: "KODEMA_STACK_SLOTS",
}

// acquireRestoreSlot must run before a drill starts its recovery timeline, so
// time spent queued is never measured as RTO.
func acquireRestoreSlot(t *testing.T) {
	t.Helper()
	restoreGate.Enter(t)
}
