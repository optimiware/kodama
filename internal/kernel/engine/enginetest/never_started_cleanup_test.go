package enginetest

// Terminal cleanup of an identity the runtime proves never started.
//
// A start that fails before its effect leaves the launch outcome unknown. If
// the execution then becomes terminal, cleanup observes the identity and the
// runtime answers with affirmative never-started proof. That proof exists only
// in the pass that read it, and removal destroys the object that supplied it:
// afterwards the runtime reports absence, which proves nothing. These tests pin
// that the proof is committed before the removal effect, so a failure later in
// the same pass (the completion commit, the workspace release) resumes from the
// durable proof instead of staying pending forever.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// completionFailingState fails cleanup-complete commits while armed. Every other
// commit is delegated, so the proof commit and the removal still happen.
type completionFailingState struct {
	engine.State

	failure error

	mu       sync.Mutex
	failures int
	attempts int
}

func (s *completionFailingState) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if commit.CleanupUpdate != nil && commit.CleanupUpdate.State == kernel.CleanupComplete {
		s.mu.Lock()
		fail := s.failures > 0
		if fail {
			s.failures--
			s.attempts++
		}
		s.mu.Unlock()
		if fail {
			return kernel.Execution{}, s.failure
		}
	}
	return s.State.Commit(ctx, commit)
}

func (s *completionFailingState) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// cancelledUnknownLaunch drives one container action to a start that failed
// before its effect, then cancels the execution. The identity is prepared, its
// launch outcome is unknown and nothing durable says it never started.
func cancelledUnknownLaunch(t *testing.T, executionID, workspaceID string) (*harness, id.WorkloadID) {
	t.Helper()
	build := containerAction("act-build", "build", 0, time.Hour)
	h := newHarness(t, newExecution(executionID, workspaceID, build))
	h.fixture.Runtime.FailNextStartBeforeEffect(errors.New("start transport failure"))
	action := mustAction(t, h.reconcile(), "build")
	if action.Container.Launch.State != kernel.LaunchUnknown {
		t.Fatalf("launch state = %q, want unknown", action.Container.Launch.State)
	}
	h.cancel(flow.CancelRequested)
	cancelled := mustAction(t, h.load(), "build")
	if cancelled.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("cancellation already recorded a never-started proof: %+v", cancelled.Container.Observation)
	}
	return h, action.Container.WorkloadID
}

// assertNeverStartedCleanupComplete checks the settled end state: cleanup is
// complete, the terminal outcome is the cancellation, the identity was removed
// without ever being started or stopped and the workspace is released once.
func assertNeverStartedCleanupComplete(t *testing.T, h *harness, workload id.WorkloadID) {
	t.Helper()
	execution := h.load()
	if execution.State != kernel.ExecutionCancelled || execution.Failure != nil {
		t.Fatalf("terminal outcome = %q failure %+v, want an unchanged cancellation", execution.State, execution.Failure)
	}
	if execution.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup = %+v, want complete", execution.Cleanup)
	}
	action := mustAction(t, execution, "build")
	if action.State != kernel.ActionCancelled {
		t.Fatalf("action state = %q, want cancelled", action.State)
	}
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v, want the persisted never-started proof", action.Container.Observation)
	}
	if action.Stop.IsConfirmed() {
		t.Fatalf("stop = %+v; a never-started identity must not gain stop evidence", action.Stop)
	}
	record, ok := h.fixture.Runtime.Workload(workload)
	if !ok || !record.Removed || record.Started || record.StopRequests != 0 {
		t.Fatalf("workload = %+v, want removed, never started and never stopped", record)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) || h.fixture.Workspace.ReleaseCount() != 1 {
		t.Fatalf("workspace released = %v, releases = %d; want exactly one release",
			h.fixture.Workspace.IsReleased(h.workspaceID), h.fixture.Workspace.ReleaseCount())
	}
}

// TestCleanupCommitsObservedNeverStartedProofBeforeRemoval pins the ordering:
// when terminal cleanup learns from an observation that an identity never
// started, that proof is durable before the removal effect, and the identity
// is then removed without a second observation or any stop.
func TestCleanupCommitsObservedNeverStartedProofBeforeRemoval(t *testing.T) {
	h, workload := cancelledUnknownLaunch(t, "exec-never-order", "ws-never-order")

	probe := newProbeRuntime(h.runtime)
	var atRemove []kernel.Execution
	probe.beforeRemove = func(engine.RemoveRequest) { atRemove = append(atRemove, h.load()) }
	h.engine = engine.New(h.fixture.State, probe, h.fixture.Workspace, h.fixture.Clock)

	h.reconcileUntil("cleanup complete", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})

	if len(atRemove) != 1 {
		t.Fatalf("Remove calls = %d, want exactly one", len(atRemove))
	}
	action := mustAction(t, atRemove[0], "build")
	if !action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("observation at removal = %+v; the never-started proof must be durable before Remove", action.Container.Observation)
	}
	if action.State != kernel.ActionCancelled || atRemove[0].State != kernel.ExecutionCancelled {
		t.Fatalf("states at removal = %q/%q; the proof commit must not change the terminal outcome", atRemove[0].State, action.State)
	}
	if atRemove[0].Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup at removal = %+v, want pending", atRemove[0].Cleanup)
	}
	if observes := probe.count("observe", workload); observes != 1 {
		t.Fatalf("Observe calls = %d, want one: a persisted proof is not observed again", observes)
	}
	if stops := probe.count("stop", workload); stops != 0 {
		t.Fatalf("Stop calls = %d, want none for a never-started identity", stops)
	}
	assertNeverStartedCleanupComplete(t, h, workload)
}

// TestCrashAfterNeverStartedRemovalBeforeCompletionRecovers is the stuck-cleanup
// reproducer: the identity is removed, the completion commit of that pass
// fails and the controller restarts. Recovery reads the durable proof, so it
// neither observes the absent identity nor requests a stop, and completes.
func TestCrashAfterNeverStartedRemovalBeforeCompletionRecovers(t *testing.T) {
	h, workload := cancelledUnknownLaunch(t, "exec-never-crash", "ws-never-crash")

	failing := &completionFailingState{
		State:    h.fixture.State,
		failure:  errors.New("crash at the cleanup-complete commit"),
		failures: 1,
	}
	h.engine = engine.New(failing, h.runtime, h.fixture.Workspace, h.fixture.Clock)
	if _, err := h.engine.Reconcile(t.Context(), h.executionID); !errors.Is(err, failing.failure) {
		t.Fatalf("Reconcile error = %v, want the injected completion failure", err)
	}
	if failing.attemptCount() != 1 {
		t.Fatalf("failed completion commits = %d, want one", failing.attemptCount())
	}
	record, _ := h.fixture.Runtime.Workload(workload)
	if !record.Removed {
		t.Fatalf("workload = %+v; the removal must have happened before the failed commit", record)
	}
	crashed := h.load()
	if crashed.Cleanup.State != kernel.CleanupPending {
		t.Fatalf("cleanup after the crash = %+v, want pending", crashed.Cleanup)
	}
	observes := h.runtime.observeCount()

	h.restart()
	h.reconcileUntil("cleanup complete after restart", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})

	if h.runtime.observeCount() != observes {
		t.Fatalf("Observe calls after restart = %d, want none: absence is not proof", h.runtime.observeCount()-observes)
	}
	if h.runtime.stopCount() != 0 {
		t.Fatalf("Stop calls = %d, want none for a never-started identity", h.runtime.stopCount())
	}
	assertNeverStartedCleanupComplete(t, h, workload)
}

// TestFailedReleaseAfterNeverStartedRemovalStillReleases covers the leak
// variant: the identity is removed and the workspace release of the same pass
// fails. The next pass must retry the release from the durable proof instead of
// treating the absent identity as possibly started and holding the workspace.
func TestFailedReleaseAfterNeverStartedRemovalStillReleases(t *testing.T) {
	h, workload := cancelledUnknownLaunch(t, "exec-never-release", "ws-never-release")

	refuse := true
	h.fixture.Workspace.SetGuard(func(id.WorkspaceID) error {
		if refuse {
			return errors.New("injected release failure")
		}
		return nil
	})
	pending := h.reconcile()
	if pending.Cleanup.State != kernel.CleanupPending || pending.Cleanup.Attempts != 1 {
		t.Fatalf("cleanup after the failed release = %+v, want pending with one attempt", pending.Cleanup)
	}
	record, _ := h.fixture.Runtime.Workload(workload)
	if !record.Removed || h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatalf("workload = %+v, released = %v; want removed with the workspace still held",
			record, h.fixture.Workspace.IsReleased(h.workspaceID))
	}

	refuse = false
	h.reconcileUntil("cleanup complete after the release retry", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	if h.runtime.stopCount() != 0 {
		t.Fatalf("Stop calls = %d, want none for a never-started identity", h.runtime.stopCount())
	}
	assertNeverStartedCleanupComplete(t, h, workload)
}

// TestFailedNeverStartedProofCommitRemovesNothing pins the other side of the
// ordering: when the proof commit itself fails, no removal and no release have
// happened, and a later pass observes the still-present identity again.
func TestFailedNeverStartedProofCommitRemovesNothing(t *testing.T) {
	h, workload := cancelledUnknownLaunch(t, "exec-never-proof", "ws-never-proof")

	injected := errors.New("crash at the proof commit")
	h.fixture.Store.FailNextCommit(injected)
	if _, err := h.engine.Reconcile(t.Context(), h.executionID); !errors.Is(err, injected) {
		t.Fatalf("Reconcile error = %v, want the injected proof-commit failure", err)
	}
	record, _ := h.fixture.Runtime.Workload(workload)
	if record.Removed || h.runtime.removeCount() != 0 || h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatalf("workload = %+v, removes = %d, released = %v; a failed proof commit must precede every effect",
			record, h.runtime.removeCount(), h.fixture.Workspace.IsReleased(h.workspaceID))
	}
	if action := mustAction(t, h.load(), "build"); action.Container.Observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v; the failed commit must not be visible", action.Container.Observation)
	}

	h.restart()
	h.reconcileUntil("cleanup complete after restart", 4, func(candidate kernel.Execution) bool {
		return candidate.Cleanup.State == kernel.CleanupComplete
	})
	assertNeverStartedCleanupComplete(t, h, workload)
}
