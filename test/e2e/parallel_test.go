//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
	"github.com/optimiware/kodama/test/support/parallel"
)

// e2eGate bounds how many live scenarios run at once against the installation
// under test. KODAMA_E2E_CONCURRENCY=1 runs them one at a time.
var e2eGate = &parallel.Gate{Env: "KODAMA_E2E_CONCURRENCY", Default: 6, Max: 32}

// e2eParallel must be the first statement of a live scenario that tolerates
// concurrent siblings in the same project. A scenario that asserts on
// project-wide state (TestE2E_Parity reads the tail of the project's event
// stream) omits it and runs alone, before any parallel scenario starts.
func e2eParallel(t *testing.T) {
	t.Helper()
	e2eGate.Enter(t)
}

// protectedTargetRef is the ref the protected-Git scenarios create and advance.
const protectedTargetRef = "refs/heads/main"

// The protected-Git scenarios spread over the installation's ordinary
// repositories by slot. An installation with one repository maps every slot to
// it and the scenarios take turns on its protected ref.
const (
	gitSlotProtectedTask = iota
	gitSlotDelivery
	gitSlotUserlandGit
	gitSlotHumanPush
)

var protectedRefs parallel.KeyedLock

// requireProtectedRepository is the gate of a scenario that advances a
// protected ref: it loads the installation, selects the scenario's repository
// and owns that repository's protected ref until the test ends.
func requireProtectedRepository(t *testing.T, proof string, slot int) (*live, harness.GitRepository) {
	t.Helper()
	gate := requireACInstallation(t)
	requireACGit(t, gate, proof)
	repository, _ := gate.smokeGitInputs(t, slot)
	protectedRefs.Hold(t, repository.ResourceID+"\x00"+protectedTargetRef)
	// The wait for the ref can outlast a persona token, so the scenario reads
	// its credentials only now that it owns the ref.
	return requireACInstallation(t), repository
}

// smokeRequireNoExecutionWithActions requires that no listed execution
// vanished and that no execution admitted since carries a forbidden action
// name. Concurrent scenarios admit their own executions in the same project,
// so the unchanged-set check alone would refuse a healthy parallel run.
func (instance *live) smokeRequireNoExecutionWithActions(
	ctx context.Context,
	t *testing.T,
	token harness.Secret,
	before, after []string,
	forbiddenActions ...string,
) {
	t.Helper()
	added, err := smoke.ExecutionSetDelta(before, after)
	if err != nil {
		t.Fatalf("execution set regressed: %v", err)
	}
	for _, executionID := range added {
		view := instance.smokeInspect(ctx, t, token, executionID)
		if err := smoke.CheckForbiddenActionNames(view, forbiddenActions...); err != nil {
			t.Fatalf("%v", err)
		}
	}
}
