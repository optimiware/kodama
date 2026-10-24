package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/gates/gatestest/memory"
)

// TestUnitGatesMCPContract (SD-13, SD-A11): the memory adapter of the gate
// store, authority and execution peer passes the shared gate contract with no
// service. The same suite runs on PostgreSQL in the test environment.
func TestUnitGatesMCPContract(t *testing.T) {
	t.Run("gates", func(t *testing.T) {
		gatescontract.Contract(t, func(t testing.TB) gatescontract.Harness { return memory.NewHarness(t) })
	})
}
