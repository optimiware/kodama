package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestUnitSharedResourceContract runs the run_shared resource lifecycle and
// persistence contract on the in-memory store: no database, no broker, no
// cluster. The PostgreSQL store runs the same suite in the `test` environment
// (postgres/shared_contract_test.go).
func TestUnitSharedResourceContract(t *testing.T) {
	executiontest.SharedContract(t, func(tb testing.TB) executiontest.SharedSubject {
		_, s := executiontest.NewSharedMem(tb)
		return s
	})
}
