package integration_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/mcpcontract"
)

// TestMCPStoreContract (SD-13, SD-A11): the PostgreSQL store, the real runtime
// authority and the real secrets module pass the same contract the memory
// adapter passes in the unit layer. It skips in the unit layer.
func TestMCPStoreContract(t *testing.T) {
	mcpcontract.Contract(t, func(t testing.TB) mcpcontract.Harness {
		return mcptest.NewPostgresHarness(mcptest.New(t, mcptest.Config{}))
	})
}
