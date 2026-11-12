package enginetest

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/testkit"
)

// TestScanRecoversDueWork pins that the due scan expires a running action at
// its persisted deadline and drives the committed stop/cleanup intents.
func TestScanRecoversDueWork(t *testing.T) {
	seeded := runningContainerExecution("exec-scan-due", "ws-scan-due", testNow)
	action := seeded.Actions[0]
	identity := identityFor(seeded.ID, action)
	h := newHarness(t, seeded)
	h.retainWorkspace()
	h.prepareAndStart(identity, action.Container.Definition)

	if err := h.engine.Scan(h.t.Context()); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	expired := h.load()
	if expired.State != kernel.ExecutionFailed {
		t.Fatalf("execution state after due scan = %q, want failed", expired.State)
	}
	if expired.Failure == nil || expired.Failure.Kind != kernel.FailureTimeout {
		t.Fatalf("failure = %+v, want timeout", expired.Failure)
	}
	if mustAction(t, expired, "build").State != kernel.ActionFailed {
		t.Fatalf("action state = %q, want failed", mustAction(t, expired, "build").State)
	}
}

// TestScanRecoversUnfinishedPendingSuccessor pins that the unfinished scan
// activates a successor stranded after a settlement commit.
func TestScanRecoversUnfinishedPendingSuccessor(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	build.State = kernel.ActionSucceeded
	build.Version = 3
	build.Outputs = kernel.Outputs{"candidate": "abc"}
	build.Stop = kernel.ConfirmedStop("build stopped")
	build.Container.WorkloadID = id.WorkloadID("wl-scan-window")
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-scan-window",
	}
	review := requestAction("act-review", "review", 1, time.Hour, definitionSubjectForBuild(), "decision")
	execution := newExecution("exec-scan-window", "ws-scan-window", build, review)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = -1
	h := newHarness(t, execution)
	h.retainWorkspace()

	if err := h.engine.Scan(h.t.Context()); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	resumed := h.load()
	reviewAction := mustAction(t, resumed, "review")
	if reviewAction.State != kernel.ActionRunning {
		t.Fatalf("successor state = %q, want running", reviewAction.State)
	}
	if reviewAction.Request == nil || reviewAction.Request.Subject == nil {
		t.Fatal("scan-activated request has no resolved subject")
	}
	if h.runtime.prepareCount() != 0 || h.runtime.startCount() != 0 {
		t.Fatalf("request activation dispatched a workload: prepare=%d start=%d", h.runtime.prepareCount(), h.runtime.startCount())
	}
}

// TestScanRecoversUnfinishedLaunchIntent pins that the unfinished scan resumes
// a container that was activated but never started before the crash.
func TestScanRecoversUnfinishedLaunchIntent(t *testing.T) {
	seeded := runningContainerExecution("exec-scan-launch", "ws-scan-launch", testNow.Add(time.Hour))
	seeded.Actions[0].Container.Launch.State = kernel.LaunchNotIssued
	h := newHarness(t, seeded)
	h.retainWorkspace()

	if err := h.engine.Scan(h.t.Context()); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if h.fixture.Runtime.StartCount() != 1 {
		t.Fatalf("runtime start effects = %d, want 1", h.fixture.Runtime.StartCount())
	}
	action := mustAction(t, h.load(), "build")
	if action.Container.Launch.State != kernel.LaunchIssued {
		t.Fatalf("launch state = %q, want issued", action.Container.Launch.State)
	}
	if action.Container.WorkloadID != seeded.Actions[0].Container.WorkloadID {
		t.Fatal("scan resumed a different workload identity")
	}
}

// TestScanRecoversPendingCleanup pins that the unfinished scan retries an owed
// cleanup without changing the terminal outcome.
func TestScanRecoversPendingCleanup(t *testing.T) {
	execution, identity := terminalFailedExecutionWithCleanup("exec-scan-cleanup", "ws-scan-cleanup")
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.prepareAndStart(identity, execution.Actions[0].Container.Definition)
	h.confirmRuntimeStop(identity)

	if err := h.engine.Scan(h.t.Context()); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	completed := h.load()
	if completed.State != kernel.ExecutionFailed {
		t.Fatalf("terminal state after cleanup scan = %q, want failed", completed.State)
	}
	if completed.Cleanup.State != kernel.CleanupComplete {
		t.Fatalf("cleanup state = %q, want complete", completed.Cleanup.State)
	}
	if !h.fixture.Workspace.IsReleased(h.workspaceID) {
		t.Fatal("cleanup scan did not release the workspace")
	}
}

// TestScanSurvivesRestart pins that recovery is driven by persisted state, not
// controller memory.
func TestScanSurvivesRestart(t *testing.T) {
	build := containerAction("act-build", "build", 0, time.Hour, "candidate")
	build.State = kernel.ActionSucceeded
	build.Version = 3
	build.Outputs = kernel.Outputs{"candidate": "abc"}
	build.Stop = kernel.ConfirmedStop("build stopped")
	build.Container.WorkloadID = id.WorkloadID("wl-scan-restart")
	build.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     build.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-scan-restart",
	}
	review := requestAction("act-review", "review", 1, time.Hour, definitionSubjectForBuild(), "decision")
	execution := newExecution("exec-scan-restart", "ws-scan-restart", build, review)
	execution.State = kernel.ExecutionRunning
	execution.CurrentAction = -1
	h := newHarness(t, execution)
	h.retainWorkspace()
	h.restart()

	if err := h.engine.Scan(h.t.Context()); err != nil {
		t.Fatalf("Scan() after restart error = %v", err)
	}
	if mustAction(t, h.load(), "review").State != kernel.ActionRunning {
		t.Fatal("restart scan did not resume the stranded successor")
	}
	if !h.fixture.Workspace.IsRetained(h.workspaceID) {
		t.Fatal("restart scan lost workspace retention")
	}
}

// TestScanProcessesMultipleKinds pins that one recovery pass can see both due
// and unfinished work; repeated passes process every stranded execution.
func TestScanProcessesMultipleKinds(t *testing.T) {
	fixture := testkit.NewFixture(testNow)

	due := runningContainerExecution("exec-multi-due", "ws-multi-due", testNow)
	dueAction := due.Actions[0]
	dueIdentity := identityFor(due.ID, dueAction)

	windowBuild := containerAction("act-build", "build", 0, time.Hour, "candidate")
	windowBuild.State = kernel.ActionSucceeded
	windowBuild.Version = 3
	windowBuild.Outputs = kernel.Outputs{"candidate": "abc"}
	windowBuild.Stop = kernel.ConfirmedStop("build stopped")
	windowBuild.Container.WorkloadID = id.WorkloadID("wl-multi-window")
	windowBuild.Container.Launch = kernel.LaunchIntent{
		WorkloadID:     windowBuild.Container.WorkloadID,
		State:          kernel.LaunchIssued,
		IdempotencyKey: "idem-multi-window",
	}
	window := newExecution("exec-multi-window", "ws-multi-window", windowBuild,
		requestAction("act-review", "review", 1, time.Hour, definitionSubjectForBuild(), "decision"))
	window.State = kernel.ExecutionRunning
	window.CurrentAction = -1

	fixture.Store.Seed(due, window)
	if err := fixture.Workspace.Retain(t.Context(), due.WorkspaceID); err != nil {
		t.Fatalf("Retain(due) error = %v", err)
	}
	if err := fixture.Workspace.Retain(t.Context(), window.WorkspaceID); err != nil {
		t.Fatalf("Retain(window) error = %v", err)
	}
	if _, err := fixture.Runtime.Prepare(t.Context(), engine.PrepareRequest{
		Identity:    dueIdentity,
		WorkspaceID: due.WorkspaceID,
		Container:   dueAction.Container.Definition,
	}); err != nil {
		t.Fatalf("Prepare(due) error = %v", err)
	}
	if _, err := fixture.Runtime.Start(t.Context(), engine.StartRequest{
		Identity:    dueIdentity,
		WorkspaceID: due.WorkspaceID,
	}); err != nil {
		t.Fatalf("Start(due) error = %v", err)
	}

	runtime := &recordingRuntime{Runtime: fixture.Runtime}
	reconciler := engine.New(fixture.State, runtime, fixture.Workspace, fixture.Clock)
	for pass := range 2 {
		if err := reconciler.Scan(t.Context()); err != nil {
			t.Fatalf("Scan pass %d error = %v", pass, err)
		}
	}

	dueAfter, err := fixture.State.Load(t.Context(), due.ID)
	if err != nil {
		t.Fatalf("Load(due) error = %v", err)
	}
	if dueAfter.State != kernel.ExecutionFailed {
		t.Fatalf("due execution state = %q, want failed", dueAfter.State)
	}
	windowAfter, err := fixture.State.Load(t.Context(), window.ID)
	if err != nil {
		t.Fatalf("Load(window) error = %v", err)
	}
	if mustAction(t, windowAfter, "review").State != kernel.ActionRunning {
		t.Fatal("unfinished execution was not resumed by the scan")
	}
}
