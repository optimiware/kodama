// Observe of an absent container must distinguish affirmative never-started
// proof from mere absence using the adapter's own durable facts.
//
// The Kernel cleanup barrier cannot complete for a possibly-started identity
// whose container is absent: Stop can only return a pending barrier, so cleanup
// stays pending and the workspace is never released. A preparation that never
// reached StartContainer is different. The per-workload preparation record is
// required before Start, and the per-workload start-attempt marker is written
// before the first StartContainer request, so their absence is affirmative
// evidence that the command could not have run. Cleanup may then remove the
// identity directly without inventing stop evidence.
//
// These tests start no container and contact no daemon; they use the package's
// deterministic libpod/file/clock fakes.
package podman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// assertNoWorkloadEffect fails if Observe issued any effect beyond inspection.
// An observation may never create, start, stop, remove or release anything.
func assertNoWorkloadEffect(t *testing.T, fx *fixture) {
	t.Helper()
	for _, op := range []string{
		"CreateContainer",
		"StartContainer",
		"StopContainer",
		"RemoveContainer",
		"RemoveVolume",
	} {
		if calls := fx.libpod.callsFor(op); calls != 0 {
			t.Fatalf("Observe issued %d %s call(s), want no effect", calls, op)
		}
	}
}

// assertNeverStartedProof fails unless the observation is the declared
// never-started kind carrying a non-empty proof of nonexecution.
func assertNeverStartedProof(t *testing.T, observation kernel.Observation) {
	t.Helper()
	if observation.Kind != kernel.ObservationNeverStarted {
		t.Fatalf("observation = %+v, want never_started with affirmative proof", observation)
	}
	if !observation.ProofOfNonexecution() {
		t.Fatalf("never_started observation has no proof of nonexecution: %+v", observation)
	}
	if strings.TrimSpace(observation.Proof) == "" {
		t.Fatalf("never_started observation has an empty proof: %+v", observation)
	}
	if !observation.Valid() {
		t.Fatalf("never_started observation is not internally consistent: %+v", observation)
	}
}

// TestObserveAbsentWithoutPreparationProvesNonexecution covers a preparation
// that fails before the adapter writes any preparation record (for example a
// denied activation secret), so no container is ever created and Start would
// refuse. Absence of the container is then not the evidence; the missing
// preparation record is.
func TestObserveAbsentWithoutPreparationProvesNonexecution(t *testing.T) {
	fx := newFixture(t)
	workload := fx.request.Identity.WorkloadID
	if hostFileExists(t, preparationPath(fx.root, workload)) {
		t.Fatal("fixture wrote a preparation record for a never-prepared identity")
	}
	if hostFileExists(t, startAttemptPath(fx.root, workload)) {
		t.Fatal("fixture wrote a start attempt for a never-prepared identity")
	}

	observation := fx.observe(t)
	assertNeverStartedProof(t, observation)
	assertNoWorkloadEffect(t, fx)

	// The proof must be reconstructed from durable facts, not adapter memory.
	reconstructed := newAdapterWithRoot(t, fx)
	replayed, err := reconstructed.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("reconstructed Observe returned error: %v", err)
	}
	assertNeverStartedProof(t, replayed)
}

// TestObserveAbsentAfterPreparationWithoutStartAttemptProvesNonexecution
// covers a crash after a completed preparation but before Start. Prepare never
// starts a container, and the start-attempt marker is written before the first
// StartContainer request, so no marker proves the command never ran even though
// a container may have been created and removed out of band.
func TestObserveAbsentAfterPreparationWithoutStartAttemptProvesNonexecution(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	workload := fx.request.Identity.WorkloadID
	if hostFileExists(t, startAttemptPath(fx.root, workload)) {
		t.Fatal("Prepare recorded a start attempt")
	}

	observation := fx.observe(t)
	assertNeverStartedProof(t, observation)
	assertNoWorkloadEffect(t, fx)

	reconstructed := newAdapterWithRoot(t, fx)
	replayed, err := reconstructed.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("reconstructed Observe returned error: %v", err)
	}
	assertNeverStartedProof(t, replayed)
}

// TestObserveAbsentAfterStartAttemptStaysUnproven pins the other half of the
// distinction. Once a StartContainer request may have been delivered, a
// disappeared container could have run and been removed externally, so the
// observation must stay absent and Stop must stay pending. Cleanup then retains
// the workspace instead of releasing it on a guess.
func TestObserveAbsentAfterStartAttemptStaysUnproven(t *testing.T) {
	t.Run("request may not have been delivered", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.failNext("StartContainer", errFakeUnavailable, false)
		if _, err := fx.startResult(t); err == nil {
			t.Fatal("Start hid a transport error")
		}
		assertStartAttemptRecorded(t, fx)

		// The container was created but never started. Removing it out of band
		// leaves exactly the durable facts of an uncertain start: a marker but no
		// container. The adapter cannot distinguish this from a start that ran
		// and was removed externally.
		if err := fx.libpod.RemoveContainer(context.Background(), fx.containerName); err != nil {
			t.Fatalf("removing the container out of band: %v", err)
		}
		assertAbsentWithoutProof(t, fx)
	})

	t.Run("lost response after the effect", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.failNext("StartContainer", errFakeLost, true)
		if _, err := fx.startResult(t); err == nil {
			t.Fatal("Start hid a lost start response")
		}
		assertStartAttemptRecorded(t, fx)

		if err := fx.libpod.RemoveContainer(context.Background(), fx.containerName); err != nil {
			t.Fatalf("removing the running container out of band: %v", err)
		}
		assertAbsentWithoutProof(t, fx)
	})
}

// assertAbsentWithoutProof fails unless an absent container with a recorded
// start attempt stays merely absent, and the stop barrier stays unconfirmed.
func assertAbsentWithoutProof(t *testing.T, fx *fixture) {
	t.Helper()
	observation := fx.observe(t)
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation = %+v, want absent after a recorded start attempt", observation)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("a recorded start attempt was reported as proof of nonexecution: %+v", observation)
	}
	if !observation.Valid() {
		t.Fatalf("absent observation is not internally consistent: %+v", observation)
	}

	// Mirror the Kernel cleanup barrier: absent is not proof, so Stop is called
	// and can only report a pending barrier. No premature release is possible.
	evidence := fx.stop(t)
	if evidence.IsConfirmed() {
		t.Fatalf("Stop confirmed a stop for an absent, possibly-started identity: %+v", evidence)
	}
	if strings.TrimSpace(evidence.Detail) == "" {
		t.Fatal("pending stop has no detail")
	}
	if names := fx.libpod.stoppedNames(); len(names) != 0 {
		t.Fatalf("Stop issued StopContainer for an absent container: %v", names)
	}
}

// TestObserveAbsentUnreadableStateStaysUnknown proves a storage failure is
// never turned into affirmative proof. If the adapter cannot read its own
// durable facts it cannot claim the command never ran, so the observation must
// stay unknown and carry the error instead of reporting absent or
// never_started.
func TestObserveAbsentUnreadableStateStaysUnknown(t *testing.T) {
	t.Run("preparation state unreadable", func(t *testing.T) {
		fx := newFixture(t)
		fx.files.injectBefore("Lstat", preparationRecordName, errFakeUnavailable)

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe hid an unreadable preparation state")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("unreadable preparation state was reported as proof: %+v", observation)
		}
	})

	t.Run("start attempt state unreadable", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.files.injectBefore("Lstat", startAttemptRecordName, errFakeUnavailable)

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe hid an unreadable start attempt state")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("unreadable start attempt state was reported as proof: %+v", observation)
		}
	})

	// A malformed preparation marker is reachable: the marker is written with a
	// single non-atomic write, so a crash can leave a truncated record. An
	// undecodable record is unreadable state, never a completed preparation, and
	// must not become proof of nonexecution.
	t.Run("preparation marker undecodable", func(t *testing.T) {
		fx := newFixture(t)
		workload := fx.request.Identity.WorkloadID
		if err := os.MkdirAll(fx.workloadRoot(), 0o755); err != nil {
			t.Fatalf("creating the workload root: %v", err)
		}
		if err := os.WriteFile(preparationPath(fx.root, workload), []byte("{not json"), 0o444); err != nil {
			t.Fatalf("writing an undecodable preparation marker: %v", err)
		}

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe accepted an undecodable preparation marker")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("an undecodable preparation marker was reported as proof: %+v", observation)
		}
	})

	// A preparation marker that is a symlink is refused rather than followed: a
	// followed link could import a foreign "record" and turn it into proof.
	t.Run("preparation marker is a symlink", func(t *testing.T) {
		fx := newFixture(t)
		workload := fx.request.Identity.WorkloadID
		if err := os.MkdirAll(fx.workloadRoot(), 0o755); err != nil {
			t.Fatalf("creating the workload root: %v", err)
		}
		target := filepath.Join(t.TempDir(), "record.json")
		if err := os.WriteFile(target, []byte("{}"), 0o444); err != nil {
			t.Fatalf("writing the symlink target: %v", err)
		}
		if err := os.Symlink(target, preparationPath(fx.root, workload)); err != nil {
			t.Fatalf("creating the preparation marker symlink: %v", err)
		}

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe followed a symlinked preparation marker")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("a symlinked preparation marker was reported as proof: %+v", observation)
		}
	})

	// A read that returns bytes and then fails is still unreadable state: a
	// partial read must never be treated as a complete preparation record.
	t.Run("preparation read fails after returning bytes", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.files.injectAfterData("ReadFile", preparationRecordName, errFakeUnavailable)

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe treated a failed preparation read as a complete record")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("a failed preparation read was reported as proof: %+v", observation)
		}
	})

	// An unreadable removal fence is unknown state. Treating it as "no fence"
	// would let Observe fall through to the absent-container classification while
	// a durable removal intent may exist.
	t.Run("removal fence state unreadable", func(t *testing.T) {
		fx := newFixture(t)
		workload := fx.request.Identity.WorkloadID
		fx.files.injectBefore("Lstat", filepath.Join(fenceDirectory, string(workload)+".json"), errFakeUnavailable)

		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe hid an unreadable removal fence")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("an unreadable removal fence was reported as proof: %+v", observation)
		}
	})
}

// TestObservePresentContainerKeepsDaemonEvidence proves the new absent branch
// does not change the evidence for a container that still exists.
func TestObservePresentContainerKeepsDaemonEvidence(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)

		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation = %+v, want running", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("a running container was reported as proof of nonexecution: %+v", observation)
		}
	})

	t.Run("created with a start attempt", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		if err := fx.adapter.writeStartAttempt(context.Background(), fx.request.Identity, fx.request.WorkspaceID); err != nil {
			t.Fatalf("recording the start attempt: %v", err)
		}
		fx.seedCreated(t, false)

		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %+v, want unknown for an attempted created container", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatalf("an attempted created container was reported as proof of nonexecution: %+v", observation)
		}
	})
}

// TestNeverStartedIdentityCleanupCompletesWithoutStop drives the adapter through
// the Kernel cleanup order for an identity whose command never ran:
// observe -> (proof, so no Stop) -> remove -> release. It covers both the
// never-prepared case and the prepared-but-never-started case.
func TestNeverStartedIdentityCleanupCompletesWithoutStop(t *testing.T) {
	t.Run("preparation failed before the adapter prepared", func(t *testing.T) {
		fx := newFixture(t)
		assertCleanupOfNeverStarted(t, fx)
	})

	t.Run("prepared but never started", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		assertCleanupOfNeverStarted(t, fx)
	})
}

// assertCleanupOfNeverStarted mirrors the Kernel terminal-cleanup barrier. A
// proof of nonexecution skips Stop and removes the identity directly; without
// the proof the absent container can only produce a pending stop, which this
// test rules out. The workspace is retained before cleanup and released only
// after removal, exactly as the Kernel orders those effects.
func assertCleanupOfNeverStarted(t *testing.T, fx *fixture) {
	t.Helper()
	workspace := fx.request.WorkspaceID
	if err := fx.adapter.Retain(context.Background(), workspace); err != nil {
		t.Fatalf("Retain before cleanup returned error: %v", err)
	}

	observation := fx.observe(t)
	if !observation.ProofOfNonexecution() {
		// The Kernel would now call Stop. For an absent container that can only
		// stay pending, so cleanup could never complete.
		evidence := fx.stop(t)
		t.Fatalf("cleanup of a never-started identity cannot complete: observation=%+v stop=%+v",
			observation, evidence)
	}
	if calls := fx.libpod.callsFor("StopContainer"); calls != 0 {
		t.Fatalf("cleanup issued %d StopContainer call(s) for a never-started identity", calls)
	}

	removal := fx.remove(t)
	if !removal.Removed {
		t.Fatalf("removal of a never-started identity = %+v, want success", removal)
	}
	if _, ok := fx.libpod.container(fx.containerName); ok {
		t.Fatal("Remove left a container behind for a never-started identity")
	}
	if hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove left the identity's materialization behind")
	}
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("Remove left the start attempt marker behind")
	}
	if _, ok := fx.libpod.volume(fx.volumeName); !ok {
		t.Fatal("Remove released the retained workspace volume")
	}

	// Removal is idempotent, so a crash between Remove and Release is safe.
	if repeated := fx.remove(t); !repeated.Removed {
		t.Fatalf("repeated removal = %+v, want idempotent success", repeated)
	}

	if err := fx.adapter.Release(context.Background(), workspace); err != nil {
		t.Fatalf("Release after never-started cleanup returned error: %v", err)
	}
	if _, ok := fx.libpod.volume(fx.volumeName); ok {
		t.Fatal("Release left the workspace volume behind after never-started cleanup")
	}
	if hostFileExists(t, retentionPath(fx.root, workspace)) {
		t.Fatal("Release left the retention marker behind after never-started cleanup")
	}
}

// observeLockWindow bounds the proof that Observe is blocked on the
// per-workload lock. It is a liveness bound, not a behavioral sleep: the Observe
// goroutine signals that it has started, and a correct implementation cannot
// return until the start barrier is released.
const observeLockWindow = 500 * time.Millisecond

// TestObserveCannotInterleaveWithStartBeforeTheMarker pins the lock assumption
// behind the absent-container proof. "Prepared, no start-attempt marker,
// container absent" is affirmative evidence that the command never ran only
// because Observe takes the same per-workload lock as Start and Start holds it
// across CreateContainer -> durable start-attempt write -> StartContainer. The
// barriers below open the two windows in which an unlocked Observe would see
// those exact facts while Start is in flight and wrongly report never-started;
// the Kernel would then retire the identity and a replacement could run the
// command twice.
func TestObserveCannotInterleaveWithStartBeforeTheMarker(t *testing.T) {
	t.Run("blocked at container creation", func(t *testing.T) {
		runInFlightStartObservation(t, true)
	})
	t.Run("blocked at the start-attempt write", func(t *testing.T) {
		runInFlightStartObservation(t, false)
	})
}

// runInFlightStartObservation starts a real adapter Start, holds it at either
// the pre-effect container creation or the start-attempt write, and observes the
// same identity through the same adapter. Observe must stay blocked until Start
// settles and must never return never-started.
func runInFlightStartObservation(t *testing.T, gateCreate bool) {
	t.Helper()
	fx := newFixture(t)
	fx.prepare(t)

	barrier := "CreateContainer"
	var entered <-chan struct{}
	var release chan struct{}
	if gateCreate {
		gate := fx.libpod.newGate("CreateContainer")
		entered, release = gate.entered, gate.release
	} else {
		barrier = "WriteFile"
		gate := fx.files.newGate("WriteFile")
		entered, release = gate.entered, gate.release
	}

	startDone := make(chan error, 1)
	go func() {
		_, err := fx.adapter.Start(context.Background(), startRequest(fx))
		startDone <- err
	}()
	waitFor(t, entered, "Start at the "+barrier+" barrier")

	begun := make(chan struct{})
	observed := make(chan kernel.Observation, 1)
	go func() {
		close(begun)
		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err != nil {
			observation = kernel.UnknownObservation()
		}
		observed <- observation
	}()
	<-begun

	select {
	case observation := <-observed:
		t.Fatalf("Observe returned %+v while Start held the workload lock at %s", observation, barrier)
	case <-time.After(observeLockWindow):
	}

	close(release)
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start returned error after the %s barrier: %v", barrier, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Start did not return after the %s barrier was released", barrier)
	}

	select {
	case observation := <-observed:
		if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
			t.Fatalf("Observe reported never-started for a start that was in flight: %+v", observation)
		}
		switch observation.Kind {
		case kernel.ObservationRunning, kernel.ObservationUnknown:
		default:
			t.Fatalf("observation = %q, want running or unknown after the start settled", observation.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Observe did not return after the start settled")
	}
}

// TestObserveRejectsForeignWorkspaceContainer pins that the workspace label is
// part of the evidence identity. A container carrying this workload's identity
// labels but another workspace's binding is not this observation's subject, so
// it must never be reported as running or as proof.
func TestObserveRejectsForeignWorkspaceContainer(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	state := fx.seedRunning(t)
	state.Labels[LabelWorkspace] = "workspace-2"
	fx.libpod.setContainer(state)

	observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
	if err == nil {
		t.Fatal("Observe accepted a container bound to another workspace")
	}
	if observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("observation = %+v, want unknown for a foreign workspace container", observation)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("a foreign workspace container was reported as proof: %+v", observation)
	}
}

// TestRemoveCannotInterleaveWithStartBeforeTheMarker pins the same per-workload
// lock on the cleanup side. Remove must not run between CreateContainer and the
// durable start-attempt write: otherwise it could fence and delete the
// materialization of an identity whose start is about to be issued, leaving an
// orphan container or a removal that races a live start. The window is opened by
// holding Start at container creation and asserting Remove stays blocked.
func TestRemoveCannotInterleaveWithStartBeforeTheMarker(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.libpod.newGate("CreateContainer")

	startDone := make(chan error, 1)
	go func() {
		_, err := fx.adapter.Start(context.Background(), startRequest(fx))
		startDone <- err
	}()
	waitFor(t, gate.entered, "Start at container creation")

	removeDone := make(chan engine.Removal, 1)
	go func() {
		removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
		if err != nil {
			removal = engine.Removal{Detail: err.Error()}
		}
		removeDone <- removal
	}()

	early := false
	select {
	case <-removeDone:
		early = true
	case <-time.After(observeLockWindow):
	}
	close(gate.release)

	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start returned error after the create barrier: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the create barrier was released")
	}
	if early {
		t.Fatal("Remove returned while Start held the workload lock before the start attempt")
	}
	select {
	case removal := <-removeDone:
		// The start settled as running, so cleanup must retain the live container.
		if removal.Removed {
			t.Fatalf("Remove reported success for a running container: %+v", removal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove did not return after the start settled")
	}
}

// TestStopCannotInterleaveWithStartBeforeTheMarker pins the per-workload lock on
// Stop. A stop that interleaves between CreateContainer and the durable start
// attempt could confirm whole-container termination and release the workspace
// while Start is about to issue StartContainer, letting a command run after the
// stop barrier was confirmed. The window is opened by holding Start at the
// start-attempt write and asserting Stop stays blocked.
func TestStopCannotInterleaveWithStartBeforeTheMarker(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.files.newGate("WriteFile")

	startDone := make(chan error, 1)
	go func() {
		_, err := fx.adapter.Start(context.Background(), startRequest(fx))
		startDone <- err
	}()
	waitFor(t, gate.entered, "Start at the start-attempt write")

	stopDone := make(chan kernel.StopEvidence, 1)
	go func() {
		evidence, _ := fx.adapter.Stop(context.Background(), stopRequest(fx))
		stopDone <- evidence
	}()

	early := false
	select {
	case <-stopDone:
		early = true
	case <-time.After(observeLockWindow):
	}
	close(gate.release)

	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start returned error after the start-attempt barrier: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the start-attempt barrier was released")
	}
	if early {
		t.Fatal("Stop returned while Start held the workload lock before the start attempt")
	}
	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the start settled")
	}
}
