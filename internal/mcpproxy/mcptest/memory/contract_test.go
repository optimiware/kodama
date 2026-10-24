package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/mcpcontract"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/memory"
)

// TestUnitGatesMCPContract (SD-13, SD-A11): the memory adapter of the MCP proxy
// store, runtime authority and secrets passes the shared store contract with no
// service. The same suite runs on PostgreSQL in the test environment.
func TestUnitGatesMCPContract(t *testing.T) {
	t.Run("mcpproxy", func(t *testing.T) {
		mcpcontract.Contract(t, func(t testing.TB) mcpcontract.Harness { return memory.NewHarness(t) })
	})
}
