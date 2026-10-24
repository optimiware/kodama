package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestUnitExecutionStoreContract runs the execution.Store contract on the
// in-memory store: no database, no broker, no cluster (SD-A11). The PostgreSQL
// store runs the same suite in the `test` environment (postgres/contract_test.go).
func TestUnitExecutionStoreContract(t *testing.T) {
	executiontest.Contract(t, func(tb testing.TB) executiontest.Subject {
		return executiontest.NewMem(tb).Subject()
	})
}
