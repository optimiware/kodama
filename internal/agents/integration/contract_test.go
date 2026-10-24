package integration_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/test/support/layer"
)

// Test_agents_repository_contract runs the agents.Repository contract suite on
// the PostgreSQL store (SD-13, SD-A11); the in-memory adapter runs it without a
// service in agentstest. Every open gives the suite a fresh database whose
// fixture projects are the scopes the suite addresses.
func Test_agents_repository_contract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	scopes := agentstest.Scopes{A: executiontest.ScopeA, A2: executiontest.ScopeA2, B: executiontest.ScopeB}
	agentstest.Contract(t, func(tb testing.TB) agents.Repository {
		return newEnv(tb.(*testing.T)).AgentRepo
	}, scopes)
}
