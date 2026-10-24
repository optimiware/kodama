package gatewaytest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

// Test_unit_agents_gateway_contract runs the gateway store contract on the
// in-memory adapter over memdb, with no service (SD-13, SD-A11). The agents
// repository contract runs in internal/agents/agentstest. The PostgreSQL store
// runs the same suite as Test_gateway_store_contract in the integration package
// (gatewaytest.PGHarness, service-gated), at the integration checkpoint.
func Test_unit_agents_gateway_contract(t *testing.T) {
	gatewaytest.Contract(t, gatewaytest.MemHarness)
}
