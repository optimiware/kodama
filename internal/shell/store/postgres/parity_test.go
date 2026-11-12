package postgres

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/store/storetest"
)

// TestSharedConformanceSuite runs the adapter-neutral store scenarios against
// this adapter through the in-process SQL emulator, which executes the same
// rendered statements the real driver receives. The memory adapter runs the
// same suite, so a rule one adapter enforces cannot silently be missing from
// the other.
func TestSharedConformanceSuite(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		t.Helper()
		adapter := New(newEmuDB("kodama_runtime", testTime()))
		return storetest.Backend{Executor: adapter}
	})
}
