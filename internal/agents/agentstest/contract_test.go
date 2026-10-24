package agentstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
)

// Test_unit_agents_gateway_contract runs the agents repository contract on the
// in-memory adapter with no service (SD-13, SD-A11). The PostgreSQL store runs
// the same suite in internal/agents/integration; the gateway store's twin is
// in internal/gateway/gatewaytest.
func Test_unit_agents_gateway_contract(t *testing.T) {
	agentstest.Contract(t, func(testing.TB) agents.Repository { return &agentstest.MemRepo{} }, agentstest.MemScopes)
}
