package testkit

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

var _ engine.Runtime = (*Runtime)(nil)

func TestRuntimeCapabilitiesAreCopied(t *testing.T) {
	runtime := NewRuntime()
	runtime.SetCapabilities("podman", "logs")

	capabilities, err := runtime.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !capabilities.Supports("podman") || !capabilities.Supports("logs") {
		t.Fatalf("capabilities = %v, want podman and logs", capabilities.Names)
	}

	capabilities.Names[0] = "tampered"
	again, err := runtime.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("second Capabilities() error = %v", err)
	}
	if again.Names[0] != "podman" {
		t.Fatalf("capabilities aliased caller slice: %v", again.Names)
	}
}

func TestRuntimePrepareIsOncePerWorkload(t *testing.T) {
	runtime := NewRuntime()
	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")

	first := testPrepare(t, runtime, identity, workspaceID)
	second := testPrepare(t, runtime, identity, workspaceID)

	if first.WorkspaceID != workspaceID || second.WorkspaceID != workspaceID {
		t.Fatalf("preparations = %q/%q, want %q", first.WorkspaceID, second.WorkspaceID, workspaceID)
	}
	if runtime.PrepareCount() != 1 {
		t.Fatalf("PrepareCount() = %d, want one materialization", runtime.PrepareCount())
	}

	_, err := runtime.Prepare(context.Background(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: id.WorkspaceID("ws-2"),
	})
	testClass(t, err, fault.InvalidInput)
}

func TestRuntimePrepareFailureCreatesNoResource(t *testing.T) {
	runtime := NewRuntime()
	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")

	runtime.FailNextPrepare(errors.New("materialize failed"))
	if _, err := runtime.Prepare(context.Background(), engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Prepare() error = nil, want injected failure")
	}
	if _, ok := runtime.Workload(identity.WorkloadID); ok {
		t.Fatal("failed preparation created a workload resource")
	}
	if runtime.PrepareCount() != 0 {
		t.Fatalf("PrepareCount() = %d, want 0", runtime.PrepareCount())
	}

	testPrepare(t, runtime, identity, workspaceID)
	if runtime.PrepareCount() != 1 {
		t.Fatalf("PrepareCount() after retry = %d, want 1", runtime.PrepareCount())
	}
}

func TestRuntimeStartRequiresPrepare(t *testing.T) {
	runtime := NewRuntime()
	identity := testIdentity("exec-1", "act-1", "wl-1")

	_, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: id.WorkspaceID("ws-1"),
	})
	if !errors.Is(err, ErrNotPrepared) {
		t.Fatalf("Start() before Prepare error = %v, want ErrNotPrepared", err)
	}
	if runtime.StartCount() != 0 {
		t.Fatalf("StartCount() = %d, want 0", runtime.StartCount())
	}
}

func TestRuntimeStartReplayIsRefused(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)

	_, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed Start() error = %v, want ErrReplay", err)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after replay = %d, want 1", runtime.StartCount())
	}
	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok || workload.StartCount != 1 {
		t.Fatalf("workload after replay = %+v, want one start", workload)
	}
}

func TestRuntimeLostStartResponseKeepsOneEffect(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	runtime.LoseNextStartResponse("start response lost")

	result, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("Start outcome = %q, want unknown", result.Outcome)
	}
	if result.WorkloadID != identity.WorkloadID {
		t.Fatalf("Start workload id = %q, want %q", result.WorkloadID, identity.WorkloadID)
	}

	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok || !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload after lost response = %+v, want one applied effect", workload)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() = %d, want 1", runtime.StartCount())
	}

	if _, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); !errors.Is(err, ErrReplay) {
		t.Fatalf("blind replay after lost response error = %v, want ErrReplay", err)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after blind replay = %d, want 1", runtime.StartCount())
	}

	runtime.QueueObservation(identity.WorkloadID, kernel.RunningObservation())
	observation, err := runtime.Observe(context.Background(), engine.ObserveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Kind != kernel.ObservationRunning {
		t.Fatalf("observation kind = %q, want running", observation.Kind)
	}
}

func TestRuntimeStartPreEffectFailureCreatesNoResource(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	runtime.FailNextStartBeforeEffect(errors.New("connection refused"))

	if _, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Start() error = nil, want injected pre-effect failure")
	}
	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatal("prepared workload record disappeared")
	}
	if workload.Started || workload.StartCount != 0 {
		t.Fatalf("workload after pre-effect failure = %+v, want not started", workload)
	}

	result := testStart(t, runtime, identity, workspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q, want accepted", result.Outcome)
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload after retry = %+v, want one start", workload)
	}
}

func TestRuntimeStartRefusalIsProvenNotStarted(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	runtime.RefuseNextStart("runtime proved the command never executed")

	result, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Start() error = %v, want nil with proven-not-started evidence", err)
	}
	if result.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("Start outcome = %q, want %q", result.Outcome, engine.StartProvenNotStarted)
	}
	if result.WorkloadID != identity.WorkloadID {
		t.Fatalf("Start workload id = %q, want %q", result.WorkloadID, identity.WorkloadID)
	}
	if result.Detail != "runtime proved the command never executed" {
		t.Fatalf("Start detail = %q, want the injected detail", result.Detail)
	}

	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatal("refused start lost the prepared workload record")
	}
	if workload.Started || workload.StartCount != 0 {
		t.Fatalf("workload after refusal = %+v, want no applied effect", workload)
	}
	if runtime.StartCount() != 0 {
		t.Fatalf("StartCount() after refusal = %d, want 0", runtime.StartCount())
	}

	observation, err := runtime.Observe(context.Background(), engine.ObserveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe() after refusal error = %v", err)
	}
	if observation.Kind != kernel.ObservationNeverStarted || !observation.ProofOfNonexecution() {
		t.Fatalf("observation after refusal = %+v, want never-started with proof", observation)
	}

	// The refusal consumed no start attempt: the same prepared identity can
	// still start exactly once.
	retry := testStart(t, runtime, identity, workspaceID)
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("Start after refusal outcome = %q, want accepted", retry.Outcome)
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload after retried start = %+v, want exactly one effect", workload)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after retried start = %d, want 1", runtime.StartCount())
	}

	if _, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); !errors.Is(err, ErrReplay) {
		t.Fatalf("Start after accepted retry error = %v, want ErrReplay", err)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after replay = %d, want 1", runtime.StartCount())
	}
}

func TestRuntimeStartRefusalIsOneShotAndLostResponseStillUnknown(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
	runtime.RefuseNextStart("runtime proved the command never executed")
	runtime.LoseNextStartResponse("start response lost")

	refused := testStart(t, runtime, identity, workspaceID)
	if refused.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("first Start outcome = %q, want %q", refused.Outcome, engine.StartProvenNotStarted)
	}
	if runtime.StartCount() != 0 {
		t.Fatalf("StartCount() after refusal = %d, want 0", runtime.StartCount())
	}

	// The refusal is one-shot: the staged lost-response injection still applies
	// the effect exactly once and reports an unknown outcome.
	lost := testStart(t, runtime, identity, workspaceID)
	if lost.Outcome != engine.StartUnknown {
		t.Fatalf("second Start outcome = %q, want unknown", lost.Outcome)
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if !workload.Started || workload.StartCount != 1 {
		t.Fatalf("workload after lost-response start = %+v, want exactly one effect", workload)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after lost-response start = %d, want 1", runtime.StartCount())
	}

	if _, err := runtime.Start(context.Background(), engine.StartRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); !errors.Is(err, ErrReplay) {
		t.Fatalf("Start after lost response error = %v, want ErrReplay", err)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after replay = %d, want 1", runtime.StartCount())
	}
}

func TestRuntimeConcurrentStartHasOneEffect(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)

	type outcome struct {
		result engine.StartResult
		err    error
	}

	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := runtime.Start(context.Background(), engine.StartRequest{
				Identity:    identity,
				WorkspaceID: workspaceID,
			})
			results <- outcome{result: result, err: err}
		}()
	}
	close(start)

	accepted := 0
	refused := 0
	for range 2 {
		outcome := <-results
		switch {
		case outcome.err == nil && outcome.result.Outcome == engine.StartAccepted:
			accepted++
		case errors.Is(outcome.err, ErrReplay):
			refused++
		default:
			t.Fatalf("unexpected concurrent Start result = %+v, error = %v", outcome.result, outcome.err)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("concurrent Start outcomes = %d accepted, %d refused; want one each", accepted, refused)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() = %d, want 1", runtime.StartCount())
	}
}

func TestRuntimeObserveKeepsEvidenceDistinct(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)

	runtime.QueueObservation(identity.WorkloadID,
		kernel.UnknownObservation(),
		kernel.AbsentObservation(),
		kernel.NeverStarted("runtime proved no object"),
	)

	unknown, err := runtime.Observe(context.Background(), engine.ObserveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Observe(unknown) error = %v", err)
	}
	if unknown.Kind != kernel.ObservationUnknown || unknown.ProofOfNonexecution() || unknown.StopConfirmed() {
		t.Fatalf("unknown observation = %+v", unknown)
	}

	absent, err := runtime.Observe(context.Background(), engine.ObserveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Observe(absent) error = %v", err)
	}
	if absent.Kind != kernel.ObservationAbsent || absent.ProofOfNonexecution() || absent.StopConfirmed() {
		t.Fatalf("absent observation = %+v", absent)
	}

	neverStarted, err := runtime.Observe(context.Background(), engine.ObserveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Observe(never-started) error = %v", err)
	}
	if neverStarted.Kind != kernel.ObservationNeverStarted || !neverStarted.ProofOfNonexecution() {
		t.Fatalf("never-started observation = %+v", neverStarted)
	}

	fallback, err := runtime.Observe(context.Background(), engine.ObserveRequest{
		Identity:    testIdentity("exec-1", "act-1", "wl-missing"),
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe(missing) error = %v", err)
	}
	if fallback.Kind != kernel.ObservationUnknown {
		t.Fatalf("unknown workload observation kind = %q, want unknown", fallback.Kind)
	}
}

func TestRuntimeStopAcknowledgmentIsNotConfirmation(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)

	evidence, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatal("Stop() acknowledgment was treated as confirmation")
	}
	if evidence.State != kernel.StopStateUnconfirmed {
		t.Fatalf("stop state = %q, want unconfirmed", evidence.State)
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.StopRequests != 1 || workload.StopConfirmed {
		t.Fatalf("workload after stop request = %+v, want one pending request", workload)
	}
	if err := runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() = %v, want ErrOwnershipHeld", err)
	}
}

func TestRuntimeConfirmedStopIsIdempotent(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	runtime.ConfirmNextStop("termination observed")

	evidence, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatal("confirmed stop was reported as pending")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if !workload.StopConfirmed {
		t.Fatal("workload stop confirmation was not recorded")
	}
	if err := runtime.OwnershipHeld(workspaceID); err != nil {
		t.Fatalf("OwnershipHeld() after confirmed stop = %v, want nil", err)
	}

	again, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if !again.IsConfirmed() {
		t.Fatal("idempotent Stop() lost the confirmed evidence")
	}
}

func TestRuntimeLostStopResponseKeepsRequest(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	runtime.LoseNextStopResponse("stop response lost")

	evidence, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatal("lost stop response was treated as confirmation")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.StopRequests != 1 || workload.StopConfirmed {
		t.Fatalf("workload after lost stop response = %+v", workload)
	}
	if err := runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() after lost stop response = %v, want ErrOwnershipHeld", err)
	}
}

func TestRuntimeStopPreEffectFailureRecordsNoRequest(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	runtime.FailNextStopBeforeEffect(errors.New("connection refused"))

	if _, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Stop() error = nil, want injected pre-effect failure")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.StopRequests != 0 || workload.StopConfirmed {
		t.Fatalf("workload after failed stop = %+v, want no request", workload)
	}
	if err := runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() = %v, want ErrOwnershipHeld", err)
	}
}

func TestRuntimeRemoveRequiresConfirmedStop(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)

	if _, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); !errors.Is(err, ErrStopUnconfirmed) {
		t.Fatalf("Remove() before confirmed stop error = %v, want ErrStopUnconfirmed", err)
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.Removed {
		t.Fatal("Remove() before confirmed stop marked the workload removed")
	}

	runtime.ConfirmNextStop("termination observed")
	if _, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	removal, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove() after confirmed stop error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("Remove() after confirmed stop reported not removed")
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if !workload.Removed {
		t.Fatal("workload removal was not recorded")
	}

	again, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("second Remove() error = %v", err)
	}
	if !again.Removed {
		t.Fatal("idempotent Remove() reported not removed")
	}
}

func TestRuntimeRemovePreparedNeverStartedSucceeds(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)

	removal, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove() for a prepared but never started workload error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("Remove() for a prepared but never started workload reported not removed")
	}
	workload, ok := runtime.Workload(identity.WorkloadID)
	if !ok {
		t.Fatal("Remove() lost the workload record")
	}
	if !workload.Removed || workload.Started {
		t.Fatalf("workload after Remove() = %+v, want removed and never started", workload)
	}
	if workload.StopRequests != 0 || workload.StopConfirmed {
		t.Fatalf("never-started removal fabricated stop evidence: %+v", workload)
	}
	if len(runtime.Removals()) != 1 {
		t.Fatalf("removal attempts = %d, want 1", len(runtime.Removals()))
	}

	again, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("idempotent Remove() error = %v", err)
	}
	if !again.Removed {
		t.Fatal("idempotent Remove() reported not removed")
	}
	if len(runtime.Removals()) != 1 {
		t.Fatalf("idempotent Remove() recorded a second removal effect: %d", len(runtime.Removals()))
	}
}

func TestRuntimeRemovePreparedNeverStartedAfterObservationProof(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)

	observation, err := runtime.Observe(context.Background(), engine.ObserveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Kind != kernel.ObservationNeverStarted || !observation.ProofOfNonexecution() {
		t.Fatalf("observation = %+v, want never-started with proof of nonexecution", observation)
	}

	removal, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("Remove() after never-started proof error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("Remove() after never-started proof reported not removed")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if !workload.Removed || workload.Started {
		t.Fatalf("workload after Remove() = %+v, want removed and never started", workload)
	}
}

func TestRuntimeRemovePreparedNeverStartedFailureIsAttributedAndRetried(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)

	runtime.FailNextRemove(errors.New("runtime unavailable"))
	if _, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Remove() error = nil, want injected failure")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.Removed {
		t.Fatal("failed Remove() marked the never-started workload removed")
	}
	if len(runtime.Removals()) != 1 {
		t.Fatalf("removal attempts = %d, want 1", len(runtime.Removals()))
	}

	removal, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("retried Remove() error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("retried Remove() reported not removed")
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if !workload.Removed || workload.Started {
		t.Fatalf("workload after retried Remove() = %+v, want removed and never started", workload)
	}
	if len(runtime.Removals()) != 2 {
		t.Fatalf("removal attempts after retry = %d, want 2", len(runtime.Removals()))
	}
}

func TestFixtureRemovedNeverStartedWorkloadReleasesWorkspace(t *testing.T) {
	fixture := NewFixture(testNow)
	identity := testIdentity("exec-1", "act-1", "wl-1")
	workspaceID := id.WorkspaceID("ws-1")

	if err := fixture.Workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain() error = %v", err)
	}
	testPrepare(t, fixture.Runtime, identity, workspaceID)
	if err := fixture.Workspace.Release(context.Background(), workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("Release() before removal error = %v, want ErrOwnershipHeld", err)
	}

	if _, err := fixture.Runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if err := fixture.Workspace.Release(context.Background(), workspaceID); err != nil {
		t.Fatalf("Release() after removing a never-started workload error = %v", err)
	}
	if !fixture.Workspace.IsReleased(workspaceID) {
		t.Fatal("removing a never-started workload did not release the workspace")
	}
}

func TestRuntimeRemoveFailureIsAttributedAndRetried(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	runtime.ConfirmNextStop("termination observed")
	if _, err := runtime.Stop(context.Background(), engine.StopRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}

	runtime.FailNextRemove(errors.New("runtime unavailable"))
	if _, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	}); err == nil {
		t.Fatal("Remove() error = nil, want injected failure")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.Removed {
		t.Fatal("failed Remove() marked the workload removed")
	}
	if len(runtime.Removals()) != 1 {
		t.Fatalf("removal attempts = %d, want 1", len(runtime.Removals()))
	}

	removal, err := runtime.Remove(context.Background(), engine.RemoveRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		t.Fatalf("retried Remove() error = %v", err)
	}
	if !removal.Removed {
		t.Fatal("retried Remove() reported not removed")
	}
	if len(runtime.Removals()) != 2 {
		t.Fatalf("removal attempts after retry = %d, want 2", len(runtime.Removals()))
	}
}

func TestRuntimeCredentialRefreshDoesNotReplay(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)

	refresh, err := runtime.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   identity,
		Credential: "new-token",
	})
	if err != nil {
		t.Fatalf("RefreshCredential() error = %v", err)
	}
	if !refresh.Delivered {
		t.Fatal("RefreshCredential() reported not delivered")
	}
	workload, _ := runtime.Workload(identity.WorkloadID)
	if workload.CredentialRefreshes != 1 {
		t.Fatalf("credential refreshes = %d, want 1", workload.CredentialRefreshes)
	}
	if runtime.StartCount() != 1 || !workload.Started {
		t.Fatalf("credential refresh replayed or restarted the workload: %+v", workload)
	}

	runtime.FailNextCredential(errors.New("delivery failed"))
	refresh, err = runtime.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   identity,
		Credential: "new-token",
	})
	if err == nil {
		t.Fatal("RefreshCredential() error = nil, want injected failure")
	}
	if refresh.Delivered {
		t.Fatal("failed RefreshCredential() reported delivered")
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if workload.CredentialRefreshes != 1 {
		t.Fatalf("credential refreshes after failure = %d, want 1", workload.CredentialRefreshes)
	}

	refresh, err = runtime.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   identity,
		Credential: "new-token",
	})
	if err != nil {
		t.Fatalf("retried RefreshCredential() error = %v", err)
	}
	if !refresh.Delivered {
		t.Fatal("retried RefreshCredential() reported not delivered")
	}
	workload, _ = runtime.Workload(identity.WorkloadID)
	if workload.CredentialRefreshes != 2 {
		t.Fatalf("credential refreshes after retry = %d, want 2", workload.CredentialRefreshes)
	}
	if runtime.StartCount() != 1 {
		t.Fatalf("StartCount() after credential retry = %d, want 1", runtime.StartCount())
	}
}

func TestRuntimeExpireLeaseDoesNotConfirmStop(t *testing.T) {
	runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)

	runtime.ExpireLease(identity.WorkloadID)

	workload, _ := runtime.Workload(identity.WorkloadID)
	if !workload.LeaseExpired {
		t.Fatal("lease expiration was not recorded")
	}
	if workload.StopConfirmed {
		t.Fatal("lease expiration fabricated stop confirmation")
	}
	if err := runtime.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("OwnershipHeld() after lease expiration = %v, want ErrOwnershipHeld", err)
	}
}

func TestRuntimeContextCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("capabilities", func(t *testing.T) {
		runtime := NewRuntime()
		if _, err := runtime.Capabilities(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Capabilities(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("prepare", func(t *testing.T) {
		runtime := NewRuntime()
		identity := testIdentity("exec-1", "act-1", "wl-1")
		if _, err := runtime.Prepare(canceled, engine.PrepareRequest{
			Identity:    identity,
			WorkspaceID: id.WorkspaceID("ws-1"),
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Prepare(canceled) error = %v, want context.Canceled", err)
		}
		if _, ok := runtime.Workload(identity.WorkloadID); ok {
			t.Fatal("canceled Prepare() created a resource")
		}
	})

	t.Run("start", func(t *testing.T) {
		runtime, identity, workspaceID := testRuntimeWithPreparedWorkload(t)
		if _, err := runtime.Start(canceled, engine.StartRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start(canceled) error = %v, want context.Canceled", err)
		}
		workload, _ := runtime.Workload(identity.WorkloadID)
		if workload.Started {
			t.Fatal("canceled Start() applied the effect")
		}
	})

	t.Run("observe", func(t *testing.T) {
		runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
		if _, err := runtime.Observe(canceled, engine.ObserveRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Observe(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("logs", func(t *testing.T) {
		runtime, identity, _ := testRuntimeWithStartedWorkload(t)
		if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte("line")); err != nil {
			t.Fatalf("AppendLog() error = %v", err)
		}
		if _, err := runtime.Logs(canceled, engine.LogsRequest{
			Identity: identity,
			Limit:    1,
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Logs(canceled) error = %v, want context.Canceled", err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
		if _, err := runtime.Stop(canceled, engine.StopRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop(canceled) error = %v, want context.Canceled", err)
		}
		workload, _ := runtime.Workload(identity.WorkloadID)
		if workload.StopRequests != 0 {
			t.Fatal("canceled Stop() recorded a request")
		}
	})

	t.Run("remove", func(t *testing.T) {
		runtime, identity, workspaceID := testRuntimeWithStartedWorkload(t)
		if _, err := runtime.Remove(canceled, engine.RemoveRequest{
			Identity:    identity,
			WorkspaceID: workspaceID,
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Remove(canceled) error = %v, want context.Canceled", err)
		}
		workload, _ := runtime.Workload(identity.WorkloadID)
		if workload.Removed {
			t.Fatal("canceled Remove() applied the effect")
		}
	})

	t.Run("refresh-credential", func(t *testing.T) {
		runtime, identity, _ := testRuntimeWithStartedWorkload(t)
		if _, err := runtime.RefreshCredential(canceled, engine.CredentialRequest{
			Identity:   identity,
			Credential: "token",
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("RefreshCredential(canceled) error = %v, want context.Canceled", err)
		}
		workload, _ := runtime.Workload(identity.WorkloadID)
		if workload.CredentialRefreshes != 0 {
			t.Fatal("canceled RefreshCredential() recorded a delivery")
		}
	})
}

func TestRuntimeInstancesAreIndependent(t *testing.T) {
	first, identity, workspaceID := testRuntimeWithStartedWorkload(t)
	second := NewRuntime()

	if _, ok := second.Workload(identity.WorkloadID); ok {
		t.Fatal("second runtime saw the first runtime's workload")
	}
	if second.StartCount() != 0 || second.PrepareCount() != 0 {
		t.Fatalf("second runtime counters = start %d prepare %d, want zero", second.StartCount(), second.PrepareCount())
	}
	if err := first.OwnershipHeld(workspaceID); !errors.Is(err, ErrOwnershipHeld) {
		t.Fatalf("first runtime OwnershipHeld() = %v, want ErrOwnershipHeld", err)
	}
	if err := second.OwnershipHeld(workspaceID); err != nil {
		t.Fatalf("second runtime OwnershipHeld() = %v, want nil", err)
	}
}
