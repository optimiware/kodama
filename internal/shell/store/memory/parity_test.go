package memory

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/store/storetest"
)

// TestSharedConformanceSuite runs the adapter-neutral store scenarios against
// the memory adapter. The PostgreSQL adapter runs the same suite, so a rule one
// adapter enforces cannot silently be missing from the other.
func TestSharedConformanceSuite(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Backend {
		t.Helper()
		adapter := New()
		return storetest.Backend{Executor: adapter}
	})
}
