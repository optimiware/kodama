package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// Test_shared_resource_contract runs the run_shared resource lifecycle and
// persistence contract on the PostgreSQL store, explicitly in the `test`
// environment: executiontest.New skips in the unit layer. The in-memory store
// runs the same suite on every unit run (memory/shared_contract_test.go).
func Test_shared_resource_contract(t *testing.T) {
	executiontest.SharedContract(t, func(tb testing.TB) executiontest.SharedSubject {
		_, s := executiontest.NewSharedPG(tb)
		return s
	})
}
