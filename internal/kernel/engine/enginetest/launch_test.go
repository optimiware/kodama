package enginetest

// Launch-attempt regressions. The launch marker must be durable before
// the start effect, so every identity whose start may have happened is visible
// to terminal cleanup even when the controller crashes, its context is
// cancelled or the ambiguous-outcome commit fails. These tests drive the
// engine's own Start; they do not seed the post-start state.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// TestIssuedMarkerCommitFailureDoesNotStart pins persist-intent-before-effect:
// if the launch-attempt marker cannot be committed, Start is never called and
// the identity stays not-issued. A later pass can still start it.
func TestIssuedMarkerCommitFailureDoesNotStart(t *testing.T) {
	seeded := runningContainerExecution("exec-marker-fail", "ws-marker-fail", testNow.Add(time.Hour))
	seeded.Actions[0].Container.Launch.State = kernel.LaunchNotIssued
	h := newHarness(t, seeded)
	h.retainWorkspace()

	h.fixture.Store.FailNextCommit(errors.New("injected launch-marker commit failure"))
	_, err := h.engine.Reconcile(h.t.Context(), h.executionID)
	if err == nil {
		t.Fatal("Reconcile succeeded despite a failed launch-marker commit")
	}
	if h.runtime.startCount() != 0 || h.fixture.Runtime.StartCount() != 0 {
		t.Fatalf("Start was issued without a durable marker: calls=%d effects=%d",
			h.runtime.startCount(), h.fixture.Runtime.StartCount())
	}
	afterFailure := mustAction(t, h.load(), "build")
	if afterFailure.Container.Launch.State != kernel.LaunchNotIssued {
		t.Fatalf("launch state after a failed marker commit = %q, want not_issued", afterFailure.Container.Launch.State)
	}

	// The same persisted identity can still be started once the marker commits.
	started := h.reconcileUntil("workload started", 8, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	action := mustAction(t, started, "build")
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state after a successful retry = %q, want issued", action.Container.Launch.State)
	}
	if action.Container.WorkloadID != seeded.Actions[0].Container.WorkloadID {
		t.Fatalf("retry used workload %q, want persisted %q",
			action.Container.WorkloadID, seeded.Actions[0].Container.WorkloadID)
	}
}

// TestStartEffectWithFailedOutcomeCommitLeavesStoppableIdentity pins that an
// applied start whose ambiguous-outcome commit fails still leaves a durable,
// stoppable identity. Cancel and deadline expiry must both request Stop for it
// and keep cleanup pending until termination is confirmed.
func TestStartEffectWithFailedOutcomeCommitLeavesStoppableIdentity(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		h, identity := startWithFailedOutcomeCommit(t, "exec-effect-cancel", "ws-effect-cancel")
		assertStoppableIdentity(t, h, identity)

		h.cancel(flow.CancelRequested)
		pending := h.reconcileUntil("cancelled cleanup with a stop request", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionCancelled && h.runtime.stopCount() > 0
		})
		assertCleanupWaitsForStop(t, h, pending, identity)
		assertCleanupCompletesAfterStop(t, h, identity)
	})

	t.Run("deadline expiry", func(t *testing.T) {
		h, identity := startWithFailedOutcomeCommit(t, "exec-effect-expire", "ws-effect-expire")
		assertStoppableIdentity(t, h, identity)

		h.fixture.Clock.Set(testNow.Add(time.Hour))
		pending := h.reconcileUntil("expired cleanup with a stop request", 8, func(candidate kernel.Execution) bool {
			return candidate.State == kernel.ExecutionFailed && h.runtime.stopCount() > 0
		})
		if pending.Failure == nil || pending.Failure.Kind != kernel.FailureTimeout {
			t.Fatalf("failure = %+v, want timeout", pending.Failure)
		}
		assertCleanupWaitsForStop(t, h, pending, identity)
		assertCleanupCompletesAfterStop(t, h, identity)
	})
}

// TestCancelledStartContextLeavesStoppableIdentity pins the same invariant when
// the controller context is cancelled while Start is in flight: the effect was
// applied, the issued marker is durable and cleanup can still stop the identity.
func TestCancelledStartContextLeavesStoppableIdentity(t *testing.T) {
	h := newHarness(t, newExecution("exec-start-context", "ws-start-context",
		containerAction("act-build", "build", 0, time.Hour)))

	barrier := testkit.NewBarrier()
	h.fixture.Runtime.BlockNextStartAfterEffect(barrier)
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitBarrier(t, barrier, ctx)
	// The start effect is applied and the issued marker is durable before the
	// context ends.
	identity := identityFor(h.executionID, mustAction(t, h.load(), "build"))
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile error = %v, want context.Canceled", err)
	}
	assertStoppableIdentity(t, h, identity)

	h.cancel(flow.CancelRequested)
	pending := h.reconcileUntil("cancelled cleanup with a stop request", 8, func(candidate kernel.Execution) bool {
		return candidate.State == kernel.ExecutionCancelled && h.runtime.stopCount() > 0
	})
	assertCleanupWaitsForStop(t, h, pending, identity)
	assertCleanupCompletesAfterStop(t, h, identity)
}

// TestCancelBeforeIssuedMarkerSkipsStartAndCleansUp pins the other side of the
// Cancel/Start race: when cancellation wins before the launch marker commits,
// no start is issued and cleanup must not livelock on an unprepared identity.
func TestCancelBeforeIssuedMarkerSkipsStartAndCleansUp(t *testing.T) {
	seeded := runningContainerExecution("exec-cancel-before-marker", "ws-cancel-before-marker", testNow.Add(time.Hour))
	seeded.Actions[0].State = kernel.ActionCancelled
	seeded.Actions[0].Container.Launch.State = kernel.LaunchNotIssued
	seeded.State = kernel.ExecutionCancelled
	seeded.CurrentAction = -1
	seeded.Cleanup = kernel.Cleanup{
		State:       kernel.CleanupPending,
		WorkloadID:  seeded.Actions[0].Container.WorkloadID,
		WorkspaceID: seeded.WorkspaceID,
	}
	h := newHarness(t, seeded)
	h.retainWorkspace()

	completed := h.reconcileUntil("cleanup complete without a start", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if h.runtime.startCount() != 0 || h.fixture.Runtime.StartCount() != 0 {
		t.Fatalf("cancelled-before-marker execution issued a start: calls=%d effects=%d",
			h.runtime.startCount(), h.fixture.Runtime.StartCount())
	}
	if h.runtime.prepareCount() != 0 {
		t.Fatalf("cancelled-before-marker execution prepared a workload: %d", h.runtime.prepareCount())
	}
	if h.runtime.stopCount() != 0 {
		t.Fatalf("cancelled-before-marker execution requested %d stops, want none", h.runtime.stopCount())
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup livelocked instead of releasing an unstarted workspace")
	}
	if completed.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", completed.State)
	}
}

// TestCancelBetweenIssuedMarkerAndStartLeavesStoppableIdentity pins that a
// Cancel committed after activation but before the start effect either prevents
// Start or leaves a recorded, stoppable identity. The deterministic ordering
// here blocks Start before its effect, commits Cancel, then releases it.
func TestCancelBetweenIssuedMarkerAndStartLeavesStoppableIdentity(t *testing.T) {
	h := newHarness(t, newExecution("exec-cancel-start-race", "ws-cancel-start-race",
		containerAction("act-build", "build", 0, time.Hour)))

	barrier := testkit.NewBarrier()
	h.fixture.Runtime.BlockNextStartBeforeEffect(barrier)
	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitBarrier(t, barrier, ctx)

	// Activation and the issued marker are durable while Start is still blocked.
	blocked := mustAction(t, h.load(), "build")
	if blocked.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state while Start is blocked = %q, want issued", blocked.Container.Launch.State)
	}
	identity := identityFor(h.executionID, blocked)
	h.cancel(flow.CancelRequested)
	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}

	// The recorded identity is stoppable even though cancellation won the race.
	if h.runtime.startCount() != 1 {
		t.Fatalf("Start calls = %d, want the single already-committed attempt", h.runtime.startCount())
	}
	assertStoppableIdentity(t, h, identity)
	pending := h.load()
	if pending.State != kernel.ExecutionCancelled {
		t.Fatalf("terminal state = %q, want cancelled", pending.State)
	}
	assertCleanupWaitsForStop(t, h, pending, identity)
	assertCleanupCompletesAfterStop(t, h, identity)
}

// TestCrashAfterIssuedMarkerBeforeStartRelaunchesOnlyAfterProof pins the window
// between the durable issued marker and the start effect: the restart observes
// the same identity, gets affirmative never-started proof and only then
// allocates a fresh identity.
func TestCrashAfterIssuedMarkerBeforeStartRelaunchesOnlyAfterProof(t *testing.T) {
	seeded := runningContainerExecution("exec-issued-before-start", "ws-issued-before-start", testNow.Add(time.Hour))
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	// Reconstruct a controller that prepared the identity and committed the
	// issued marker, then crashed before calling Start.
	if _, err := h.fixture.Runtime.Prepare(h.t.Context(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: h.workspaceID,
		Container:   action.Container.Definition,
	}); err != nil {
		t.Fatalf("Prepare(%s) error = %v", identity.WorkloadID, err)
	}

	started := h.reconcileUntil("fresh workload started", 10, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	starts := h.runtime.startRequests()
	if len(starts) != 1 {
		t.Fatalf("Start calls = %d, want one fresh attempt", len(starts))
	}
	if starts[0].Identity.WorkloadID == identity.WorkloadID {
		t.Fatalf("start reused the unstarted identity %q without proof", identity.WorkloadID)
	}
	oldWorkload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || oldWorkload.Started {
		t.Fatalf("unstarted identity %q was started: %+v", identity.WorkloadID, oldWorkload)
	}
	if !oldWorkload.Removed {
		t.Fatalf("unstarted identity %q was not removed before the replacement started", identity.WorkloadID)
	}
	if oldWorkload.StopRequests != 0 {
		t.Fatalf("unstarted identity %q received %d stops, want none", identity.WorkloadID, oldWorkload.StopRequests)
	}
	relaunched := mustAction(t, started, "build")
	if relaunched.Container.WorkloadID != starts[0].Identity.WorkloadID {
		t.Fatalf("persisted workload = %q, want relaunched %q",
			relaunched.Container.WorkloadID, starts[0].Identity.WorkloadID)
	}
	if relaunched.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("relaunch state = %q, want issued", relaunched.Container.Launch.State)
	}
	assertWorkloadStarted(t, h.fixture.Runtime, starts[0].Identity.WorkloadID)
}

// TestEngineDrivenStartSurvivesRestartWithoutReplay supplements the seeded
// after-start/before-response case: the engine itself starts the workload, then
// a fresh controller observes the persisted identity instead of replaying it.
func TestEngineDrivenStartSurvivesRestartWithoutReplay(t *testing.T) {
	h := newHarness(t, newExecution("exec-engine-start-restart", "ws-engine-start-restart",
		containerAction("act-build", "build", 0, time.Hour)))

	started := h.reconcileUntil("workload started", 8, func(candidate kernel.Execution) bool {
		return h.fixture.Runtime.StartCount() == 1
	})
	action := mustAction(t, started, "build")
	identity := identityFor(h.executionID, action)
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state after engine start = %q, want issued", action.Container.Launch.State)
	}
	if action.Container.WorkloadID != identity.WorkloadID {
		t.Fatalf("started workload = %q, want %q", action.Container.WorkloadID, identity.WorkloadID)
	}

	h.restart()
	observed := h.reconcile()
	if h.fixture.Runtime.StartCount() != 1 || h.runtime.startCount() != 1 {
		t.Fatalf("restart replayed Start: effects=%d calls=%d",
			h.fixture.Runtime.StartCount(), h.runtime.startCount())
	}
	if h.runtime.observeCount() == 0 {
		t.Fatal("restart did not observe the persisted identity")
	}
	recovered := mustAction(t, observed, "build")
	if recovered.Container.Observation.Kind != kernel.ObservationRunning {
		t.Fatalf("observation after restart = %+v, want running", recovered.Container.Observation)
	}
	if recovered.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("restart changed launch state to %q", recovered.Container.Launch.State)
	}
}

// ---------------------------------------------------------------------------
// Launch regression helpers
// ---------------------------------------------------------------------------

// startWithFailedOutcomeCommit drives the engine's own start until the effect is
// applied, injects a failure into the ambiguous-outcome commit and returns the
// harness plus the persisted identity.
func startWithFailedOutcomeCommit(t *testing.T, executionID, workspaceID string) (*harness, engine.EffectIdentity) {
	t.Helper()
	h := newHarness(t, newExecution(executionID, workspaceID, containerAction("act-build", "build", 0, time.Hour)))

	barrier := testkit.NewBarrier()
	h.fixture.Runtime.BlockNextStartAfterEffect(barrier)
	h.fixture.Runtime.LoseNextStartResponse("start response lost after the effect")
	ctx, cancel := context.WithTimeout(h.t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.engine.Reconcile(ctx, h.executionID)
		done <- err
	}()
	waitBarrier(t, barrier, ctx)
	identity := identityFor(h.executionID, mustAction(t, h.load(), "build"))

	// The effect is applied and the issued marker is durable. The next commit is
	// the ambiguous outcome, so its failure leaves the issued marker in place.
	h.fixture.Store.FailNextCommit(errors.New("injected launch-outcome commit failure"))
	barrier.Release()
	if err := <-done; err != nil {
		t.Fatalf("Reconcile error = %v", err)
	}
	return h, identity
}

func assertStoppableIdentity(t *testing.T, h *harness, identity engine.EffectIdentity) {
	t.Helper()
	action := mustAction(t, h.load(), "build")
	if action.Container.Launch.State == kernel.LaunchNotIssued {
		t.Fatal("started workload is durably marked not-issued")
	}
	if action.Container.WorkloadID != identity.WorkloadID {
		t.Fatalf("persisted workload = %q, want %q", action.Container.WorkloadID, identity.WorkloadID)
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Started {
		t.Fatalf("start effect was not applied to %q: %+v", identity.WorkloadID, workload)
	}
}

func assertCleanupWaitsForStop(t *testing.T, h *harness, execution kernel.Execution, identity engine.EffectIdentity) {
	t.Helper()
	action := mustAction(t, execution, "build")
	if action.Stop.IsConfirmed() {
		t.Fatal("cleanup promoted an unconfirmed stop")
	}
	if execution.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup state = %q, want pending until stop is confirmed", execution.Cleanup.State)
	}
	if execution.Cleanup.WorkloadID != identity.WorkloadID {
		t.Fatalf("cleanup workload = %q, want attributed identity %q",
			execution.Cleanup.WorkloadID, identity.WorkloadID)
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || workload.StopRequests == 0 {
		t.Fatalf("cleanup did not request Stop for %q: %+v", identity.WorkloadID, workload)
	}
	if h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup released the workspace before stop confirmation")
	}
	if h.runtime.removeCount() != 0 {
		t.Fatal("cleanup removed resources before stop confirmation")
	}
}

func assertCleanupCompletesAfterStop(t *testing.T, h *harness, identity engine.EffectIdentity) {
	t.Helper()
	h.confirmRuntimeStop(identity)
	completed := h.reconcileUntil("cleanup complete", 8, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if !completed.State.Terminal() {
		t.Fatalf("cleanup completion changed the terminal outcome to %q", completed.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("confirmed stop did not release the workspace")
	}
	workload, ok := h.fixture.Runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Removed {
		t.Fatalf("confirmed stop did not remove workload %q: %+v", identity.WorkloadID, workload)
	}
}
