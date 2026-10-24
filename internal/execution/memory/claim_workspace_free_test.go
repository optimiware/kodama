package memory_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestExecutionClaimWorkspaceFree: a work run's access: none step is claimed
// with no workspace while source-consuming steps keep the run's workspace,
// through the shared adapter contract on the memory store.
func TestExecutionClaimWorkspaceFree(t *testing.T) {
	executiontest.ClaimWorkspaceFree(t, func(tb testing.TB) executiontest.Subject {
		return executiontest.NewMem(tb).Subject()
	})
}
