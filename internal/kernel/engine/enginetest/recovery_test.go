package enginetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// TestCrashBeforeIntentLeavesNoEffect pins that a cancelled context stops the
// reconciler before it persists an intent or performs an effect.
func TestCrashBeforeIntentLeavesNoEffect(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-crash-before", "ws-crash-before", build))

	ctx, cancel := context.WithCancel(h.t.Context())
	cancel()
	_, err := h.engine.Reconcile(ctx, h.executionID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile(cancelled ctx) error = %v, want context.Canceled", err)
	}
	if h.fixture.Store.CommitCount() != 0 {
		t.Fatalf("cancelled reconcile committed %d times, want 0", h.fixture.Store.CommitCount())
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("cancelled reconcile performed effects: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	if loaded := h.load(); loaded.State != kernel.ExecutionQueued {
		t.Fatalf("execution state = %q, want queued", loaded.State)
	}
}

// TestCrashAfterIntentBeforeStartResumesWithPersistedIdentity pins the
// not_issued crash window: a new controller materializes and starts the same
// persisted workload identity and idempotency key.
func TestCrashAfterIntentBeforeStartResumesWithPersistedIdentity(t *testing.T) {
	seeded := runningContainerExecution("exec-crash-intent", "ws-crash-intent", testNow.Add(time.Hour))
	seeded.Actions[0].Container.Launch.State = kernel.LaunchNotIssued
	workloadID := seeded.Actions[0].Container.WorkloadID
	idempotencyKey := seeded.Actions[0].Container.Launch.IdempotencyKey
	h := newHarness(t, seeded)
	h.retainWorkspace()

	started := h.reconcileUntil("workload started", 8, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	action := mustAction(t, started, "build")
	if action.Container.WorkloadID != workloadID {
		t.Fatalf("resumed workload id = %q, want persisted %q", action.Container.WorkloadID, workloadID)
	}
	if action.Container.Launch.IdempotencyKey != idempotencyKey {
		t.Fatalf("resumed idempotency key = %q, want persisted %q", action.Container.Launch.IdempotencyKey, idempotencyKey)
	}
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state = %q, want issued", action.Container.Launch.State)
	}
	if h.runtime.prepareCount() != 1 {
		t.Fatalf("Prepare calls = %d, want 1", h.runtime.prepareCount())
	}
	prepare := h.runtime.lastPrepare()
	if prepare.Identity.WorkloadID != workloadID || prepare.Identity.IdempotencyKey != idempotencyKey {
		t.Fatalf("prepare identity = %+v, want persisted identity %q/%q", prepare.Identity, workloadID, idempotencyKey)
	}
	if starts := h.runtime.startRequests(); len(starts) != 1 || starts[0].Identity.WorkloadID != workloadID {
		t.Fatalf("start requests = %+v, want one for the persisted identity", starts)
	}
}

// TestCrashAfterStartBeforeResponseObservesInsteadOfReplaying pins the
// issued crash window: the controller observes the same identity and never
// launches a replacement.
func TestCrashAfterStartBeforeResponseObservesInsteadOfReplaying(t *testing.T) {
	seeded := runningContainerExecution("exec-crash-start", "ws-crash-start", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)

	observed := h.reconcile()
	if h.runtime.startCount() != 0 {
		t.Fatalf("recovery replayed Start %d times", h.runtime.startCount())
	}
	if h.runtime.observeCount() == 0 {
		t.Fatal("recovery did not observe the persisted identity")
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("runtime start effects = %d, want the single pre-crash start", h.fixture.Runtime.StartCount())
	}
	recovered := mustAction(t, observed, "build")
	if recovered.Container.Observation.Kind != kernel.ObservationRunning {
		t.Fatalf("observation = %+v, want running", recovered.Container.Observation)
	}

	h.confirmRuntimeStop(identity)
	h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.ExitedObservation(0, kernel.ConfirmedStop("exited")))
	settled := h.reconcileUntil("build succeeded", 8, func(candidate kernel.Execution) bool {
		current, ok := actionByName(candidate, "build")
		return ok && current.State == kernel.ActionSucceeded
	})
	if mustAction(t, settled, "build").Container.WorkloadID != identity.WorkloadID {
		t.Fatal("settlement lost the persisted workload identity")
	}
}

// TestCrashAfterSettlementBeforeNextActivationResumes pins the window after a
// successful settlement commit but before the successor activation: a restart
// recovers the pending successor from persisted state.
func TestCrashAfterSettlementBeforeNextActivationResumes(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	build.State = kernel.ActionSucceeded
	build.Version = 3
	build.Outputs = kernel.Outputs{"candidate": "abc"}
	build.Stop = kernel.ConfirmedStop("build stopped")
	build.Container.WorkloadID = id.WorkloadID("wl-settled")
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-settled",
	}
	review := requestAction("act-review", "review", 1, time.Hour, definitionSubjectForBuild(), "decision")
	execution := newExecution("exec-window", "ws-window", build, review)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = -1

	h := newHarness(t, execution)
	h.retainWorkspace()
	h.restart()

	resumed := h.reconcileUntil("review running", 8, func(candidate kernel.Execution) bool {
		action, ok := actionByName(candidate, "review")
		return ok && action.State == kernel.ActionRunning
	})
	assertSingleActiveAction(t, resumed)
	if mustAction(t, resumed, "build").State != kernel.ActionSucceeded {
		t.Fatal("resume changed the settled build action")
	}
	reviewAction := mustAction(t, resumed, "review")
	if reviewAction.Request == nil || reviewAction.Request.Subject == nil {
		t.Fatal("resumed request has no resolved subject")
	}
	if reviewAction.Request.Subject.Fields()["candidate"].Literal != "abc" {
		t.Fatalf("resumed subject = %+v, want candidate=abc", reviewAction.Request.Subject.Fields())
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("resumed request dispatched a workload: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("resume lost workspace retention")
	}
}

// TestLostStartResponseStaysUnknownAndIsNotReplayed pins S02 §5: a lost start
// response keeps the launch visibly unknown, observes the original identity and
// never blindly relaunches.
func TestLostStartResponseStaysUnknownAndIsNotReplayed(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-lost-start", "ws-lost-start", build))
	h.fixture.Runtime.LoseNextStartResponse("start response lost after the effect")

	_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		class, ok := fault.ClassOf(err)
		if !ok || (class != fault.UncertainExternalEffect && class != fault.DependencyUnavailable) {
			t.Fatalf("Reconcile error = %v (class %q, %v), want uncertain_external_effect/dependency_unavailable", err, class, ok)
		}
	}
	unknown := h.load()
	action := mustAction(t, unknown, "build")
	if action.Container.Launch.State != kernel.LaunchUnknown {
		t.Fatalf("launch state = %q, want unknown", action.Container.Launch.State)
	}
	if action.Container.WorkloadID == "" {
		t.Fatal("lost start response lost the stable workload identity")
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("runtime start effects = %d, want 1", h.fixture.Runtime.StartCount())
	}

	// Repeated reconciliation observes but never replays the start.
	for range 5 {
		_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
		if err != nil {
			class, ok := fault.ClassOf(err)
			if !ok || (class != fault.UncertainExternalEffect && class != fault.DependencyUnavailable) {
				t.Fatalf("Reconcile error = %v (class %q, %v), want uncertainty", err, class, ok)
			}
		}
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("unknown launch was replayed: runtime starts = %d", h.fixture.Runtime.StartCount())
	}
	if h.runtime.startCount() != 1 {
		t.Fatalf("Start calls = %d, want the single lost-response attempt", h.runtime.startCount())
	}
	if h.runtime.observeCount() == 0 {
		t.Fatal("unknown launch was not observed")
	}

	// Absence is not proof of nonexecution or of stop.
	h.fixture.Runtime.QueueObservation(action.Container.WorkloadID, kernel.AbsentObservation())
	_, err = h.engine.Reconcile(h.t.Context(), h.executionID)
	if err != nil {
		class, ok := fault.ClassOf(err)
		if !ok || (class != fault.UncertainExternalEffect && class != fault.DependencyUnavailable) {
			t.Fatalf("Reconcile after absence error = %v (class %q, %v), want uncertainty", err, class, ok)
		}
	}
	afterAbsent := mustAction(t, h.load(), "build")
	if afterAbsent.Container.Observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation = %+v, want absent", afterAbsent.Container.Observation)
	}
	if afterAbsent.Container.Launch.State != kernel.LaunchUnknown {
		t.Fatalf("absence collapsed launch uncertainty into %q", afterAbsent.Container.Launch.State)
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("absence triggered a blind replay: runtime starts = %d", h.fixture.Runtime.StartCount())
	}
}

// TestStartProvenNotStartedRelaunchesWithFreshIdentity pins that only
// affirmative nonexecution proof permits a relaunch, and that the relaunch uses
// a fresh workload identity.
func TestStartProvenNotStartedRelaunchesWithFreshIdentity(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-refused-start", "ws-refused-start", build))
	h.fixture.Runtime.RefuseNextStart("runtime proved the command never ran")

	started := h.reconcileUntil("relaunched workload started", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) < 2 {
		t.Fatalf("Start calls = %d, want a refused attempt followed by a relaunch", len(starts))
	}
	first := starts[0].Identity.WorkloadID
	second := starts[1].Identity.WorkloadID
	if first == second {
		t.Fatalf("relaunch reused workload identity %q", first)
	}
	action := mustAction(t, started, "build")
	if action.Container.WorkloadID != second {
		t.Fatalf("persisted workload id = %q, want relaunched %q", action.Container.WorkloadID, second)
	}
	firstWorkload, ok := h.fixture.Runtime.Workload(first)
	if !ok {
		t.Fatalf("runtime has no record for the refused identity %q", first)
	}
	if firstWorkload.Started {
		t.Fatalf("refused identity %q was marked started", first)
	}
	if !firstWorkload.Removed {
		t.Fatalf("refused identity %q was not removed before the replacement started", first)
	}
	if firstWorkload.StopRequests != 0 {
		t.Fatalf("refused identity %q received %d stops, want none", first, firstWorkload.StopRequests)
	}
	assertWorkloadStarted(t, h.fixture.Runtime, second)
}

// TestGenericStartErrorDoesNotReplayAndRelaunchesOnlyAfterProof pins that an
// unclassified Start error persists unknown, is not replayed, and only a later
// affirmative never-started observation permits a fresh launch.
func TestGenericStartErrorDoesNotReplayAndRelaunchesOnlyAfterProof(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution("exec-generic-start", "ws-generic-start", build))
	h.fixture.Runtime.FailNextStartBeforeEffect(errors.New("injected start transport failure"))

	_, _ = h.engine.Reconcile(h.t.Context(), h.executionID)
	afterError := h.load()
	action := mustAction(t, afterError, "build")
	if action.Container.Launch.State != kernel.LaunchUnknown {
		t.Fatalf("launch state after a generic Start error = %q, want unknown", action.Container.Launch.State)
	}
	if h.fixture.Runtime.StartCount() != 0 {
		t.Fatalf("failed Start applied an effect: %d", h.fixture.Runtime.StartCount())
	}
	oldWorkload := action.Container.WorkloadID
	if oldWorkload == "" {
		t.Fatal("generic Start error lost the stable workload identity")
	}

	started := h.reconcileUntil("relaunched workload started", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) < 2 {
		t.Fatalf("Start calls = %d, want the failed attempt plus a relaunch", len(starts))
	}
	if starts[1].Identity.WorkloadID == oldWorkload {
		t.Fatalf("relaunch reused the unproven identity %q", oldWorkload)
	}
	if mustAction(t, started, "build").Container.WorkloadID == oldWorkload {
		t.Fatal("relaunch did not persist a fresh workload identity")
	}
	if workload, ok := h.fixture.Runtime.Workload(oldWorkload); ok && workload.Started {
		t.Fatalf("unproven identity %q was marked started", oldWorkload)
	}
}

// TestStopIntentIsCommittedBeforeStopEffect pins persist-intent-before-effect:
// the stop intent is durable while the Stop effect is still in flight.
func TestStopIntentIsCommittedBeforeStopEffect(t *testing.T) {
	seeded := runningContainerExecution("exec-stop-intent", "ws-stop-intent", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)

	barrier := testkit.NewBarrier()
	h.fixture.Runtime.BlockNextStopAfterEffect(barrier)
	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		if err := h.engine.Cancel(ctx, h.executionID, flow.CancelRequested); err != nil {
			done <- err
			return
		}
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()

	waitBarrier(t, barrier, ctx)
	stops := h.fixture.Store.PendingStops()
	if len(stops) == 0 {
		t.Fatal("Stop effect ran before the stop intent was committed")
	}
	if stops[0].WorkloadID != identity.WorkloadID || stops[0].WorkspaceID != h.workspaceID {
		t.Fatalf("pending stop = %+v, want workload %q workspace %q", stops[0], identity.WorkloadID, h.workspaceID)
	}
	if state := h.load(); state.State != kernel.ExecutionCancelled {
		t.Fatalf("execution state while stop is in flight = %q, want cancelled", state.State)
	}
	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("Cancel/Reconcile error = %v", err)
	}
}

// TestLostStopResponseLeavesPendingCleanup pins that a lost stop response keeps
// cleanup attributable and blocks workspace release until termination is
// confirmed.
func TestLostStopResponseLeavesPendingCleanup(t *testing.T) {
	seeded := runningContainerExecution("exec-lost-stop", "ws-lost-stop", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)
	h.fixture.Runtime.LoseNextStopResponse("stop response lost after the request")

	h.cancel(flow.CancelRequested)
	pending := h.reconcileUntil("pending cleanup", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionCancelled && candidate.Cleanup.State == kernel.CleanupPending
	})
	pendingAction := mustAction(t, pending, "build")
	if pendingAction.Stop.IsConfirmed() {
		t.Fatal("lost stop response was promoted to confirmation")
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("lost stop response released the workspace")
	}
	if h.runtime.removeCount() != 0 {
		t.Fatal("lost stop response removed resources before confirmation")
	}

	h.confirmRuntimeStop(identity)
	completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop did not release the workspace")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed {
		t.Fatalf("cleanup did not remove workload %q: %+v", identity.WorkloadID, workload)
	}
}

// TestAbsentWorkloadIsNotProofOfStop pins that absence never releases the
// workspace or completes cleanup.
func TestAbsentWorkloadIsNotProofOfStop(t *testing.T) {
	seeded := runningContainerExecution("exec-absent", "ws-absent", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)

	h.cancel(flow.CancelRequested)
	h.fixture.Runtime.QueueObservation(identity.WorkloadID, kernel.AbsentObservation())
	pending := h.reconcileUntil("absent stop evidence", 8, func(candidate kernel.Execution) bool {
		current, ok := actionByName(candidate, "build")
		return ok && current.Stop.State == kernel.StopStateUnconfirmed
	})
	if h.runtime.observeCount() == 0 {
		t.Fatal("pending stop was retried without observing the identity")
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("absence released the workspace")
	}
	if h.runtime.removeCount() != 0 {
		t.Fatal("absence removed resources")
	}
	if pending.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
	}

	h.confirmRuntimeStop(identity)
	h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop after absence did not release the workspace")
	}
}

// TestExpiredLeaseDoesNotReleaseWorkspace pins that an expired controller lease
// is not termination evidence.
func TestExpiredLeaseDoesNotReleaseWorkspace(t *testing.T) {
	seeded := runningContainerExecution("exec-lease", "ws-lease", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)

	h.cancel(flow.CancelRequested)
	h.fixture.Runtime.ExpireLease(identity.WorkloadID)
	pending := h.reconcileUntil("pending cleanup", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupPending
	})
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("expired lease released the workspace")
	}
	if pending.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending", pending.Cleanup.State)
	}
	if h.runtime.removeCount() != 0 {
		t.Fatal("expired lease removed resources")
	}

	h.confirmRuntimeStop(identity)
	h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop after lease expiry did not release the workspace")
	}
}

// TestLostWorkspaceFailsVisiblyWithoutRecreation pins S02 §2/§5: a missing
// retained workspace fails the execution, cancels pending work and is never
// recreated or replayed.
func TestLostWorkspaceFailsVisiblyWithoutRecreation(t *testing.T) {
	seeded := runningContainerExecution("exec-lost-workspace", "ws-lost-workspace", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	seeded.Actions = append(seeded.Actions, containerAction("act-next", "next", 1, time.Hour))
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)
	retainsBefore := h.fixture.Workspace.RetainCount()

	h.fixture.Workspace.Lose(h.workspaceID)
	failed := h.reconcileUntil("workspace loss failure", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionFailed
	})
	if failed.Failure == nil || failed.Failure.Kind != kernel.FailureWorkspaceLost {
		t.Fatalf("failure = %+v, want workspace_lost", failed.Failure)
	}
	if mustAction(t, failed, "build").State != kernel.ActionFailed {
		t.Fatalf("build state = %q, want failed", mustAction(t, failed, "build").State)
	}
	if mustAction(t, failed, "next").State != kernel.ActionCancelled {
		t.Fatalf("pending successor state = %q, want cancelled", mustAction(t, failed, "next").State)
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("workspace loss dispatched work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
	if retains := h.fixture.Workspace.RetainCount(); retains != retainsBefore {
		t.Fatalf("workspace loss recreated retention: %d -> %d", retainsBefore, retains)
	}
	status, err := h.fixture.Workspace.Status(h.t.Context(), h.workspaceID)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Available {
		t.Fatal("lost workspace became available again")
	}

	h.confirmRuntimeStop(identity)
	h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop after workspace loss did not release ownership")
	}
	if retains := h.fixture.Workspace.RetainCount(); retains != retainsBefore {
		t.Fatalf("cleanup recreated the lost workspace: %d -> %d", retainsBefore, retains)
	}
}

// TestCleanupFailureIsAttributedAndRetried pins bounded cleanup progress:
// a failed removal stays visible with an attempt count and last error, the
// terminal outcome is immutable and a later pass retries.
func TestCleanupFailureIsAttributedAndRetried(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-cleanup-failure", "ws-cleanup-failure")
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)
	h.runtime.setRemoveError(errors.New("injected removal failure"))

	// A failed effect may be reported to the caller, but the attempt and last
	// error must still be persisted before returning.
	for range 8 {
		_, _ = h.engine.Reconcile(h.t.Context(), h.executionID)
		attempted := h.load()
		if attempted.Cleanup.State == kernel.CleanupPending && attempted.Cleanup.Attempts > 0 {
			break
		}
	}
	pending := h.load()
	if pending.Cleanup.State != kernel.CleanupPending || pending.Cleanup.Attempts == 0 {
		t.Fatalf("failed cleanup was not attributed: %+v", pending.Cleanup)
	}
	if pending.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state changed to %q", pending.State)
	}
	if pending.Cleanup.LastError == "" {
		t.Fatal("failed cleanup did not record the last actionable error")
	}
	if pending.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("cleanup workload = %q, want %q", pending.Cleanup.WorkloadID, identity.WorkloadID)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("failed cleanup released the workspace")
	}
	if mustAction(t, pending, "build").State != kernel.ActionFailed {
		t.Fatal("cleanup progress changed the failed action state")
	}

	h.runtime.setRemoveError(nil)
	completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state after cleanup = %q, want failed", completed.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("successful cleanup did not release the workspace")
	}
}

// TestRepeatedCleanupIsIdempotent pins that a completed cleanup is stable under
// another reconciliation pass.
func TestRepeatedCleanupIsIdempotent(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-repeat-cleanup", "ws-repeat-cleanup")
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)

	completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	removals := h.runtime.removeCount()
	stable := h.reconcile()
	if stable.State != kernel.ExecutionFailed || stable.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("repeated cleanup changed the terminal record: state=%q cleanup=%q", stable.State, stable.Cleanup.State)
	}
	if h.runtime.removeCount() < removals {
		t.Fatalf("Remove calls regressed from %d to %d", removals, h.runtime.removeCount())
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("repeated cleanup lost workspace release")
	}
}

// TestCancelQueuedExecutionCleansUp pins cancellation before activation: every
// action is cancelled, authority is revoked and the workspace is released.
func TestCancelQueuedExecutionCleansUp(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour)
	review := requestAction("act-review", "review", 1, time.Hour, literalSubject(map[string]string{"operation": "review"}), "decision")
	h := newHarness(t, newExecution("exec-cancel-queued", "ws-cancel-queued", build, review))
	h.retainWorkspace()

	h.cancel(flow.CancelRequested)
	cancelled := h.reconcileUntil("queued cancellation cleanup", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionCancelled && candidate.Cleanup.State == kernel.CleanupComplete
	})
	assertNoPendingActions(t, cancelled)
	for _, action := range cancelled.Actions {
		if action.State != kernel.ActionCancelled {
			t.Fatalf("action %q state = %q, want cancelled", action.Name, action.State)
		}
	}
	if !h.fixture.Store.CredentialInvalidated(h.executionID) {
		t.Fatal("queued cancellation did not revoke execution authority")
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("queued cancellation did not release the workspace")
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("queued cancellation dispatched work: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
}

// TestZeroActionCancel pins the zero-action execution combination: cancellation
// settles with no action row and still cleans up.
func TestZeroActionCancel(t *testing.T) {
	h := newHarness(t, newExecution("exec-zero", "ws-zero"))
	h.retainWorkspace()

	h.cancel(flow.CancelRequested)
	cancelled := h.reconcileUntil("zero-action cleanup", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionCancelled && candidate.Cleanup.State == kernel.CleanupComplete
	})
	if len(cancelled.Actions) != 0 {
		t.Fatalf("zero-action execution gained %d actions", len(cancelled.Actions))
	}
	if cancelled.CurrentAction != -1 {
		t.Fatalf("CurrentAction = %d, want -1", cancelled.CurrentAction)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("zero-action cancellation did not release the workspace")
	}
}

// TestCleanupStatusIsSeparateFromTerminalOutcome pins that pending cleanup is
// visible independently of the immutable terminal execution outcome.
func TestCleanupStatusIsSeparateFromTerminalOutcome(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-separate-cleanup", "ws-separate-cleanup")
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)

	seeded := h.load()
	if seeded.State != kernel.ExecutionFailed || seeded.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("seeded state = %q cleanup=%q, want failed/pending", seeded.State, seeded.Cleanup.State)
	}
	completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal outcome changed to %q while cleanup progressed", completed.State)
	}
	if completed.Failure == nil || completed.Failure.Kind != kernel.FailureRuntime {
		t.Fatalf("terminal failure was lost: %+v", completed.Failure)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func waitBarrier(t *testing.T, barrier *testkit.Barrier, ctx context.Context) {
	t.Helper()
	select {
	case <-barrier.Reached():
	case <-ctx.Done():
		t.Fatalf("effect barrier was not reached: %v", ctx.Err())
	}
}

func definitionSubjectForBuild() definition.Subject {
	return definition.NewSubject(map[string]definition.SubjectValue{
		"candidate": actionReference("build", "candidate"),
	})
}

func terminalFailedExecutionWithCleanup(executionID, workspaceID string) (kernel.Execution, engine.EffectIdentity) {
	build := containerAction("act-build", "build", 0, time.Hour)
	build.State = kernel.ActionFailed
	build.Version = 3
	build.Stop = kernel.ConfirmedStop("workload stopped")
	build.Container.WorkloadID = id.WorkloadID("wl-" + executionID)
	build.Container.WorkspaceID = id.WorkspaceID(workspaceID)
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-" + executionID,
	}
	execution := newExecution(executionID, workspaceID, build)
	execution.State = kernel.ExecutionFailed
	execution.CurrentAction = -1
	execution.Failure = &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "workload_exited"}
	execution.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  build.Container.WorkloadID,
		WorkspaceID: id.WorkspaceID(workspaceID),
	}
	return execution, identityFor(execution.ID, build)
}
