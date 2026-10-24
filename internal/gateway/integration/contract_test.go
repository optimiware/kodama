package integration_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/test/support/layer"
)

// Test_gateway_store_contract runs the gateway.Store contract suite on the
// PostgreSQL store (SD-13, SD-A11); the in-memory store runs the same suite
// without a service in gatewaytest. Each open gives the suite a fresh database
// with real runtime-authority attempts, identity and pinned secret versions. It
// runs at the integration checkpoint, never in the unit gate.
func Test_gateway_store_contract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	gatewaytest.Contract(t, gatewaytest.PGHarness)
}
