package postgres_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// Test_store_contract runs the execution.Store contract on the PostgreSQL
// store, explicitly in the `test` environment: executiontest.New skips in the
// unit layer. The in-memory store runs the same suite on every unit run
// (memory/contract_test.go).
func Test_store_contract(t *testing.T) {
	executiontest.Contract(t, func(tb testing.TB) executiontest.Subject {
		return executiontest.New(tb).Subject()
	})
}
