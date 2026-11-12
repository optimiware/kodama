package enginetest

// No-op stop-write guard regressions. S02 §5 makes
// every reconciler decision a compare-and-commit of *changed* state, and
// K-STOP/K-CLEANUP keep the stop barrier attributable and bounded. A workload
// inside its termination grace period acknowledges Stop on every scan pass
// (LoopInterval defaults to 20 ms). Rewriting that identical unconfirmed
// barrier and cleanup record on every pass churns the execution and action row
// versions, so a real user task.reply loses its compare-and-commit with
// version_conflict and the store is written for no durable change. The engine
// must therefore commit the first acknowledgment (it records attribution) and
// commit again only when the persisted evidence actually changes.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// attestationRuntime returns one fixed Stop attestation for every call, so a
// test can replay the same acknowledgment across scan passes or change the
// reported detail without staging one-shot fakes.
type attestationRuntime struct {
	*recordingRuntime

	mu       sync.Mutex
	evidence kernel.StopEvidence
	err      error
	calls    int
}

func (r *attestationRuntime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.evidence, r.err
}

func (r *attestationRuntime) set(evidence kernel.StopEvidence, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evidence = evidence
	r.err = err
}

func (r *attestationRuntime) stopCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// stopAttestationHarness seeds a terminal failed execution with a started
// workload, applies shape to the persisted record before admission, and wires a
// runtime whose Stop always returns evidence.
func stopAttestationHarness(t *testing.T, executionID, workspaceID string, shape func(*kernel.Execution, engine.EffectIdentity), evidence kernel.StopEvidence) (*harness, *attestationRuntime, engine.EffectIdentity) {
	t.Helper()
	execution, identity := terminalFailedExecutionWithCleanup(executionID, workspaceID)
	if shape != nil {
		shape(&execution, identity)
	}
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	runtime := &attestationRuntime{recordingRuntime: h.runtime, evidence: evidence}
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)
	return h, runtime, identity
}

// unconfirmedStopHarness reconstructs the unconfirmed-stop shape: cleanup is
// pending and not yet attributed, no stop evidence is persisted, and the
// workload acknowledges every Stop without termination evidence.
func unconfirmedStopHarness(t *testing.T, executionID, workspaceID string, evidence kernel.StopEvidence) (*harness, *attestationRuntime, engine.EffectIdentity) {
	t.Helper()
	return stopAttestationHarness(t, executionID, workspaceID, func(execution *kernel.Execution, _ engine.EffectIdentity) {
		execution.Actions[0].Stop = kernel.StopEvidence{}
		execution.Cleanup = kernel.Cleanup{
			State:       kernel.CleanupPending,
			WorkspaceID: execution.WorkspaceID,
		}
	}, evidence)
}

// TestUnconfirmedStopAcknowledgmentCommitsOnce pins the no-op guard: an
// unconfirmed Stop whose evidence and cleanup progress are unchanged from the
// persisted values issues the Stop observation again but writes nothing, so the
// execution version stops churning.
func TestUnconfirmedStopAcknowledgmentCommitsOnce(t *testing.T) {
	const pendingDetail = "stop request acknowledged; termination is not confirmed"
	h, runtime, identity := unconfirmedStopHarness(t, "exec-stop-noop", "ws-stop-noop", kernel.PendingStop(pendingDetail))

	passes := 8
	for pass := range passes {
		execution := h.reconcile()

		if got := h.fixture.Store.CommitCount(); got != 1 {
			t.Fatalf("commits after pass %d = %d, want exactly the first acknowledgment: an unchanged unconfirmed Stop must not be rewritten",
				pass, got)
		}
		if execution.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state after pass %d = %q, want pending", pass, execution.Cleanup.State)
		}
		if execution.Cleanup.Attempts != 0 {
			t.Fatalf("cleanup attempts after pass %d = %d, want 0: an acknowledgment is not a failed attempt",
				pass, execution.Cleanup.Attempts)
		}
		if execution.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload after pass %d = %q, want %q", pass, execution.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if execution.Cleanup.LastError != pendingDetail {
			t.Fatalf("cleanup last error after pass %d = %q, want the acknowledgment detail", pass, execution.Cleanup.LastError)
		}
		action := mustAction(t, execution, "build")
		if action.Stop.State != kernel.StopStateUnconfirmed || action.Stop.Detail != pendingDetail {
			t.Fatalf("stop evidence after pass %d = %+v, want the unconfirmed acknowledgment", pass, action.Stop)
		}
		if h.fixture.Workspace.IsReleased(h.workspaceID) {
			t.Fatalf("unconfirmed Stop released the workspace after pass %d", pass)
		}
	}

	// The Stop observation still progresses on every pass: only the redundant
	// write is skipped.
	if got := runtime.stopCalls(); got != passes {
		t.Fatalf("Stop calls = %d, want one per pass (%d)", got, passes)
	}
	if got := h.runtime.observeCount(); got < passes {
		t.Fatalf("Observe calls = %d, want at least one per pass (%d)", got, passes)
	}

	// The persisted execution version does not churn after the first
	// acknowledgment.
	settled := h.load().Version
	h.reconcile()
	h.reconcile()
	if got := h.load().Version; got != settled {
		t.Fatalf("execution version = %d after no-op passes, want the settled %d", got, settled)
	}
}

// TestUnconfirmedStopChangedDetailCommitsAgain pins that a new acknowledgment
// detail is a changed fact: exact stop evidence drives the guard, not merely
// the unconfirmed state.
func TestUnconfirmedStopChangedDetailCommitsAgain(t *testing.T) {
	const firstDetail = "stop unconfirmed: 0 workload pods observed"
	const nextDetail = "stop unconfirmed: pod terminating"
	h, runtime, _ := unconfirmedStopHarness(t, "exec-stop-detail", "ws-stop-detail", kernel.PendingStop(firstDetail))

	h.reconcile()
	settled := h.fixture.Store.CommitCount()
	if settled != 1 {
		t.Fatalf("commits after the first acknowledgment = %d, want 1", settled)
	}

	h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != settled {
		t.Fatalf("identical acknowledgment commits = %d, want the settled %d", got, settled)
	}

	runtime.set(kernel.PendingStop(nextDetail), nil)
	changed := h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != settled+1 {
		t.Fatalf("commits after the changed detail = %d, want %d", got, settled+1)
	}
	if changed.Cleanup.LastError != nextDetail {
		t.Fatalf("cleanup last error = %q, want %q", changed.Cleanup.LastError, nextDetail)
	}
	action := mustAction(t, changed, "build")
	if action.Stop.Detail != nextDetail {
		t.Fatalf("stop detail = %q, want %q", action.Stop.Detail, nextDetail)
	}

	// The new value settles: the following identical pass is a no-op again.
	h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != settled+1 {
		t.Fatalf("commits after the settled new detail = %d, want %d", got, settled+1)
	}
}

// TestStopAttributionRepairCommits pins that workload identity is part of the
// guard: a record whose barrier detail and counters already match but whose
// cleanup attribution names another identity is still repaired.
func TestStopAttributionRepairCommits(t *testing.T) {
	const pendingDetail = "stop request acknowledged; termination is not confirmed"
	stale := id.WorkloadID("wl-stale-attribution")
	h, _, identity := stopAttestationHarness(t, "exec-stop-identity", "ws-stop-identity",
		func(execution *kernel.Execution, _ engine.EffectIdentity) {
			execution.Actions[0].Stop = kernel.PendingStop(pendingDetail)
			execution.Cleanup = kernel.Cleanup{
				State:       kernel.CleanupPending,
				WorkloadID:  stale,
				WorkspaceID: execution.WorkspaceID,
				LastError:   pendingDetail,
			}
		}, kernel.PendingStop(pendingDetail))

	repaired := h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != 1 {
		t.Fatalf("commits while repairing attribution = %d, want 1", got)
	}
	if repaired.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("cleanup workload = %q, want the attributed identity %q", repaired.Cleanup.WorkloadID, identity.WorkloadID)
	}

	// Only the identity changed; the repaired record settles again.
	h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != 1 {
		t.Fatalf("commits after attribution settled = %d, want 1", got)
	}
}

// TestUnconfirmedStopConfirmationCommits pins that a state transition is never
// suppressed by the guard: confirmation commits and cleanup proceeds.
func TestUnconfirmedStopConfirmationCommits(t *testing.T) {
	const pendingDetail = "stop request acknowledged; termination is not confirmed"
	h, runtime, _ := unconfirmedStopHarness(t, "exec-stop-confirm", "ws-stop-confirm", kernel.PendingStop(pendingDetail))

	h.reconcile()
	settled := h.fixture.Store.CommitCount()

	// The fake still owns the removal guard, so confirm the workload there as
	// well; the engine must then observe the confirming acknowledgment through
	// its own Stop call.
	h.confirmRuntimeStop(identityFor(h.executionID, h.load().Actions[0]))
	runtime.set(kernel.ConfirmedStop("test: termination observed"), nil)
	completed := h.reconcileUntil("cleanup complete after the stop confirms", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if got := h.fixture.Store.CommitCount(); got <= settled {
		t.Fatalf("commits after confirmation = %d, want more than the settled %d", got, settled)
	}
	action := mustAction(t, completed, "build")
	if !action.Stop.IsConfirmed() || action.Stop.Detail != "test: termination observed" {
		t.Fatalf("stop evidence = %+v, want the confirmed termination", action.Stop)
	}
	if got := h.runtime.removeCount(); got != 1 {
		t.Fatalf("Remove calls = %d, want exactly one after confirmation", got)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop did not release the workspace")
	}
}

// TestRestartOverPersistedStopEvidenceDoesNotRewrite pins that the guard is
// derived from persisted evidence, not controller memory: a fresh controller
// observing the same acknowledgment writes nothing.
func TestRestartOverPersistedStopEvidenceDoesNotRewrite(t *testing.T) {
	const pendingDetail = "stop request acknowledged; termination is not confirmed"
	h, runtime, _ := unconfirmedStopHarness(t, "exec-stop-restart", "ws-stop-restart", kernel.PendingStop(pendingDetail))

	h.reconcile()
	settled := h.fixture.Store.CommitCount()
	before := h.load()

	h.restart()
	h.engine = engine.New(h.fixture.State, runtime, h.fixture.Workspace, h.fixture.Clock)

	for pass := range 5 {
		h.reconcile()
		if got := h.fixture.Store.CommitCount(); got != settled {
			t.Fatalf("commits after restart pass %d = %d, want the settled %d", pass, got, settled)
		}
	}
	after := h.load()
	if after.Version != before.Version {
		t.Fatalf("execution version after restart = %d, want the persisted %d", after.Version, before.Version)
	}
	if got := runtime.stopCalls(); got != 6 {
		t.Fatalf("Stop calls = %d, want 6 (one per pass)", got)
	}
}

// TestRepeatedStopFailureAttemptsStillCommit pins the attempt dimension: a
// failed Stop spends an attempt, and an identical failure message on the next
// pass is still a changed record because the attempt count changed.
func TestRepeatedStopFailureAttemptsStillCommit(t *testing.T) {
	h, runtime, identity := unconfirmedStopHarness(t, "exec-stop-failure", "ws-stop-failure", kernel.StopEvidence{})
	stopErr := errors.New("injected stop failure")
	runtime.set(kernel.StopEvidence{}, stopErr)

	for attempt := 1; attempt <= 3; attempt++ {
		execution := h.reconcile()
		if execution.Cleanup.Attempts != attempt {
			t.Fatalf("cleanup attempts = %d, want %d", execution.Cleanup.Attempts, attempt)
		}
		if execution.Cleanup.LastError != stopErr.Error() {
			t.Fatalf("cleanup last error = %q, want %q", execution.Cleanup.LastError, stopErr.Error())
		}
		if execution.Cleanup.WorkloadID != identity.WorkloadID {
			t.Fatalf("cleanup workload = %q, want %q", execution.Cleanup.WorkloadID, identity.WorkloadID)
		}
		if got := h.fixture.Store.CommitCount(); got != attempt {
			t.Fatalf("commits after attempt %d = %d, want one per failed attempt", attempt, got)
		}
	}
}

// TestCleanupExhaustionDiagnosticCommitsOnce pins that the exhaustion
// diagnostic is still committed, exactly once, and that later passes stay
// read-only.
func TestCleanupExhaustionDiagnosticCommitsOnce(t *testing.T) {
	h, _ := cleanupBoundHarness(t, "exec-stop-exhaust", "ws-stop-exhaust")
	h.engine.SetCleanupPolicy(engine.CleanupPolicy{MaxAttempts: 2})
	h.runtime.setRemoveError(errors.New("injected removal failure"))

	for range 2 {
		h.reconcile()
	}
	if got := h.fixture.Store.CommitCount(); got != 2 {
		t.Fatalf("commits after two failed removals = %d, want 2", got)
	}

	h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != 3 {
		t.Fatalf("commits after the exhaustion transition = %d, want 3", got)
	}

	for range 4 {
		h.reconcile()
	}
	if got := h.fixture.Store.CommitCount(); got != 3 {
		t.Fatalf("commits after exhausted passes = %d, want the diagnostic committed once", got)
	}
	exhausted := h.load()
	if !strings.Contains(exhausted.Cleanup.LastError, "cleanup retry exhausted") {
		t.Fatalf("cleanup last error = %q, want the exhaustion diagnostic", exhausted.Cleanup.LastError)
	}
	if exhausted.Cleanup.Attempts != 2 {
		t.Fatalf("cleanup attempts = %d, want the preserved 2", exhausted.Cleanup.Attempts)
	}
	if got := h.runtime.removeCount(); got != 2 {
		t.Fatalf("Remove calls = %d, want exactly the two failed attempts", got)
	}
}

// TestExitedObservationUnconfirmedStopCommitsOnce covers the other branch: an
// exited observation already carrying an unconfirmed stop barrier is recorded
// once and not rewritten while it is unchanged.
func TestExitedObservationUnconfirmedStopCommitsOnce(t *testing.T) {
	const detail = "exited observation: termination not confirmed"
	h, runtime, identity := unconfirmedStopHarness(t, "exec-stop-exited", "ws-stop-exited", kernel.StopEvidence{})

	passes := 4
	for range passes {
		h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.ExitedObservation(0, kernel.PendingStop(detail)))
	}
	for pass := range passes {
		execution := h.reconcile()
		if got := h.fixture.Store.CommitCount(); got != 1 {
			t.Fatalf("commits after exited pass %d = %d, want exactly one", pass, got)
		}
		if execution.Cleanup.State != kernel.CleanupPending {
			t.Fatalf("cleanup state after exited pass %d = %q, want pending", pass, execution.Cleanup.State)
		}
		action := mustAction(t, execution, "build")
		if action.Stop.State != kernel.StopStateUnconfirmed || action.Stop.Detail != detail {
			t.Fatalf("stop evidence after exited pass %d = %+v, want the unconfirmed barrier", pass, action.Stop)
		}
	}
	if got := runtime.stopCalls(); got != 0 {
		t.Fatalf("Stop calls = %d, want none: an exited observation carries its own barrier", got)
	}

	// A changed barrier detail commits again.
	h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.ExitedObservation(0, kernel.PendingStop("exited observation: pod terminating")))
	changed := h.reconcile()
	if got := h.fixture.Store.CommitCount(); got != 2 {
		t.Fatalf("commits after the changed exited detail = %d, want 2", got)
	}
	action := mustAction(t, changed, "build")
	if action.Stop.Detail != "exited observation: pod terminating" {
		t.Fatalf("stop detail = %q, want the changed value", action.Stop.Detail)
	}

	// A confirmed barrier is a state transition and completes cleanup.
	h.confirmRuntimeStop(identityFor(h.executionID, h.load().Actions[0]))
	h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("exited and terminated")))
	completed := h.reconcileUntil("cleanup complete from the exited observation", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	action = mustAction(t, completed, "build")
	if !action.Stop.IsConfirmed() {
		t.Fatalf("stop evidence = %+v, want confirmed", action.Stop)
	}
	if got := h.runtime.removeCount(); got != 1 {
		t.Fatalf("Remove calls = %d, want exactly one", got)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed exited observation did not release the workspace")
	}
}
