// Regression coverage for the durable start-attempt marker.
//
// A libpod client cannot know whether a StartContainer request is still in
// flight, so the adapter must record the attempt durably before the effect.
// These tests pin the consequences: an ambiguous start is never reported as
// never-started, is never replayed, and is never removed while it may still
// run — including across an adapter reconstruction.
package podman

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// TestAmbiguousStartIsNeverNeverStarted covers a transport error before and
// after the effect: the container exists, the attempt marker exists, and the
// identity must stay unknown, retained and non-replayable until Stop confirms.
func TestAmbiguousStartIsNeverNeverStarted(t *testing.T) {
	t.Run("transport error before the effect", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.failNext("StartContainer", errFakeUnavailable, false)

		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start hid a transport error")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		assertStartAttemptRecorded(t, fx)

		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %q, want unknown after an ambiguous start", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("an ambiguous start was reported as proof of nonexecution")
		}
		if removal := fx.remove(t); removal.Removed {
			t.Fatal("Remove succeeded while a start may still be delivered")
		}
		if !hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove deleted the materialization while a start may still be delivered")
		}

		startsBefore := fx.libpod.callsFor("StartContainer")
		retry, retryErr := fx.startResult(t)
		if retryErr == nil {
			t.Fatal("second Start hid the recorded start attempt")
		}
		if retry.Outcome != engine.StartUnknown {
			t.Fatalf("second Start outcome = %q, want unknown", retry.Outcome)
		}
		if calls := fx.libpod.callsFor("StartContainer"); calls != startsBefore {
			t.Fatalf("second Start re-issued StartContainer: %d -> %d", startsBefore, calls)
		}

		if evidence := fx.stop(t); !evidence.IsConfirmed() {
			t.Fatal("Stop on a marked created container did not confirm whole-container termination")
		}
		if removal := fx.remove(t); !removal.Removed {
			t.Fatalf("removal after confirmed stop = %+v, want success", removal)
		}
		if _, ok := fx.libpod.container(fx.containerName); ok {
			t.Fatal("Remove left the stopped container behind")
		}
		if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
			t.Fatal("Remove left the start attempt marker behind with the materialization")
		}
	})

	t.Run("lost response after the effect", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.failNext("StartContainer", errFakeLost, true)

		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start hid a lost start response")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		assertStartAttemptRecorded(t, fx)

		// The start effect landed, so the container is affirmatively running:
		// either way, never-started must be impossible.
		observation := fx.observe(t)
		if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
			t.Fatalf("a delivered start was reported as never-started: %+v", observation)
		}
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation = %q, want running after the delivered start", observation.Kind)
		}
		if removal := fx.remove(t); removal.Removed {
			t.Fatal("Remove succeeded while the container runs")
		}

		startsBefore := fx.libpod.callsFor("StartContainer")
		retry, retryErr := fx.startResult(t)
		if retryErr != nil {
			t.Fatalf("second Start returned error: %v", retryErr)
		}
		if retry.Outcome != engine.StartAccepted {
			t.Fatalf("second Start outcome = %q, want accepted reuse", retry.Outcome)
		}
		if calls := fx.libpod.callsFor("StartContainer"); calls != startsBefore {
			t.Fatalf("second Start re-issued StartContainer: %d -> %d", startsBefore, calls)
		}

		if evidence := fx.stop(t); !evidence.IsConfirmed() {
			t.Fatal("Stop did not confirm whole-container termination")
		}
		if removal := fx.remove(t); !removal.Removed {
			t.Fatalf("removal after confirmed stop = %+v, want success", removal)
		}
	})

	t.Run("cancelled after the request was sent", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		gate := fx.libpod.newGate("StartContainer")

		ctx, cancel := context.WithCancel(context.Background())
		type outcome struct {
			result engine.StartResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := fx.adapter.Start(ctx, startRequest(fx))
			done <- outcome{result: result, err: err}
		}()
		waitFor(t, gate.entered, "StartContainer at the barrier")
		cancel()

		select {
		case got := <-done:
			if got.err == nil {
				t.Fatal("cancelled Start returned no error")
			}
			if got.result.Outcome != engine.StartUnknown {
				t.Fatalf("outcome = %q, want unknown", got.result.Outcome)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after cancellation")
		}
		fx.libpod.clearGate("StartContainer")
		assertStartAttemptRecorded(t, fx)

		if observation := fx.observe(t); observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %q, want unknown after a cancelled start request", observation.Kind)
		}
		if removal := fx.remove(t); removal.Removed {
			t.Fatal("Remove succeeded after a cancelled start request")
		}

		startsBefore := fx.libpod.callsFor("StartContainer")
		if _, err := fx.startResult(t); err == nil {
			t.Fatal("second Start hid the recorded start attempt")
		}
		if calls := fx.libpod.callsFor("StartContainer"); calls != startsBefore {
			t.Fatalf("second Start re-issued StartContainer: %d -> %d", startsBefore, calls)
		}

		if evidence := fx.stop(t); !evidence.IsConfirmed() {
			t.Fatal("Stop on a marked created container did not confirm whole-container termination")
		}
		if removal := fx.remove(t); !removal.Removed {
			t.Fatalf("removal after confirmed stop = %+v, want success", removal)
		}
	})
}

// TestObserveInterleavedWithStartNeverSaysNeverStarted observes the identity
// from a reconstructed adapter while the original start is blocked at the
// StartContainer barrier. The durable marker, not the libpod flags, is what
// makes the interleaved observation unknown.
func TestObserveInterleavedWithStartNeverSaysNeverStarted(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.libpod.newGate("StartContainer")

	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	started := make(chan startOutcome, 1)
	go func() {
		result, err := fx.adapter.Start(context.Background(), startRequest(fx))
		started <- startOutcome{result: result, err: err}
	}()
	waitFor(t, gate.entered, "StartContainer at the barrier")

	reconstructed := newAdapterWithRoot(t, fx)
	observation, err := reconstructed.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("interleaved Observe returned error: %v", err)
	}
	if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
		t.Fatalf("interleaved Observe reported never-started while a start was in flight: %+v", observation)
	}
	if observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("interleaved observation = %q, want unknown", observation.Kind)
	}

	close(gate.release)
	select {
	case got := <-started:
		if got.err != nil {
			t.Fatalf("Start returned error: %v", got.err)
		}
		if got.result.Outcome != engine.StartAccepted {
			t.Fatalf("start outcome = %q, want accepted", got.result.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
}

// TestObserveOnSameAdapterSerializesWithStart proves the in-process lock: an
// observation taken while a start is in flight returns only after the start
// settles and therefore reports running, never never-started.
func TestObserveOnSameAdapterSerializesWithStart(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	gate := fx.libpod.newGate("StartContainer")

	started := make(chan error, 1)
	go func() {
		_, err := fx.adapter.Start(context.Background(), startRequest(fx))
		started <- err
	}()
	waitFor(t, gate.entered, "StartContainer at the barrier")

	observations := make(chan kernel.Observation, 1)
	go func() {
		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err != nil {
			observations <- kernel.UnknownObservation()
			return
		}
		observations <- observation
	}()

	close(gate.release)
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
	select {
	case observation := <-observations:
		if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
			t.Fatalf("Observe reported never-started during a start: %+v", observation)
		}
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation = %q, want running after the settled start", observation.Kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Observe did not return")
	}
}

// TestStartAttemptMarkerSurvivesReconstruction pins durability: after an
// ambiguous start the marker is on disk, a reconstructed adapter observes
// unknown and never replays the start.
func TestStartAttemptMarkerSurvivesReconstruction(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.libpod.failNext("StartContainer", errFakeUnavailable, false)
	if _, err := fx.startResult(t); err == nil {
		t.Fatal("Start hid a transport error")
	}
	assertStartAttemptRecorded(t, fx)

	reconstructed := newAdapterWithRoot(t, fx)
	observation, err := reconstructed.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("Observe after reconstruction returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("observation after reconstruction = %q, want unknown", observation.Kind)
	}

	startsBefore := fx.libpod.callsFor("StartContainer")
	if _, err := reconstructed.Start(context.Background(), startRequest(fx)); err == nil {
		t.Fatal("reconstructed Start hid the recorded start attempt")
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != startsBefore {
		t.Fatalf("reconstructed Start re-issued StartContainer: %d -> %d", startsBefore, calls)
	}
}

// TestCreatedContainerWithoutMarkerIsNeverStarted keeps the only safe
// never-started case: a created container that was never attempted.
func TestCreatedContainerWithoutMarkerIsNeverStarted(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, false)
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("test fixture left a start attempt marker behind")
	}

	observation := fx.observe(t)
	if observation.Kind != kernel.ObservationNeverStarted {
		t.Fatalf("observation = %q, want never_started without a start attempt", observation.Kind)
	}
	if !observation.ProofOfNonexecution() || observation.Proof == "" {
		t.Fatalf("never-started observation has no affirmative proof: %+v", observation)
	}
	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("never-attempted container was not removable: %+v", removal)
	}
}

// TestStartDoesNotIssueStartContainerWithoutMarker proves the marker is
// persisted before the effect: a failed marker write sends no start request.
func TestStartDoesNotIssueStartContainerWithoutMarker(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.files.injectBefore("WriteFile", startAttemptRecordName, errFakeUnavailable)

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid an unrecordable start attempt")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("Start issued %d StartContainer calls without a durable marker", calls)
	}
}

// TestStartReusePathRefusesToStartWithoutMarker pins the same invariant on the
// startExisting reuse path. An existing created container with no recorded
// start attempt may only be started after the durable marker is written: the
// "no marker => no StartContainer request was issued" proof depends on refusing
// the effect when that write fails.
func TestStartReusePathRefusesToStartWithoutMarker(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, false)
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("test fixture left a start attempt marker behind")
	}
	fx.files.injectBefore("WriteFile", startAttemptRecordName, errFakeUnavailable)

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid an unrecordable start attempt on the reuse path")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("reuse path issued %d StartContainer call(s) without a durable marker", calls)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("reuse path created %d replacement container(s)", calls)
	}
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("a start attempt marker exists despite the injected write failure")
	}
}

// TestStartReusePathRefusesAmbiguousStartedCreatedContainer pins the third reuse
// guard. A created container that reports an accepted start is contradictory
// evidence; without a durable attempt marker the adapter cannot prove the
// command did not run, so it must stay unknown instead of issuing StartContainer
// again.
func TestStartReusePathRefusesAmbiguousStartedCreatedContainer(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	state := fx.seedCreated(t, false)
	state.Started = true
	fx.libpod.setContainer(state)
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("test fixture left a start attempt marker behind")
	}

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start accepted a created container with an ambiguous accepted start")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("reuse path re-issued StartContainer %d time(s) for ambiguous evidence", calls)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("reuse path created %d replacement container(s)", calls)
	}
}

// TestStartRefusesAfterDurableFenceWithPreparationIntact covers the crash window
// created by writing the fence before any destructive step: the fence exists
// while the preparation record and activation files are still intact. Start must
// refuse from the fence alone, without creating a container, and Observe must
// report absent without proof rather than reading the intact preparation as
// never-started evidence.
func TestStartRefusesAfterDurableFenceWithPreparationIntact(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	if err := fx.adapter.writeFence(context.Background(), fx.request.Identity, fx.request.WorkspaceID); err != nil {
		t.Fatalf("writing the removal fence: %v", err)
	}
	if !hostFileExists(t, preparationPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("test fixture removed the preparation record")
	}

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start crossed the durable removal fence")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("Start created %d container(s) after the durable removal fence", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("Start issued %d StartContainer call(s) after the durable removal fence", calls)
	}

	observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("Observe after the durable fence returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after the durable fence = %+v, want absent", observation)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("a fenced identity with an intact preparation was reported as never-started: %+v", observation)
	}
}

// TestStartCancelledAfterContainerCreationIssuesNoStart pins the post-create
// cancellation guard. The container may already exist, but a cancelled Start may
// not issue StartContainer and may not record a start-attempt marker: the marker
// means "a start request may have been delivered", and no request was issued.
func TestStartCancelledAfterContainerCreationIssuesNoStart(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fx.libpod.onCreate(cancel)

	result, err := fx.adapter.Start(ctx, startRequest(fx))
	if err == nil {
		t.Fatal("Start hid a cancellation after container creation")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("cancelled Start issued %d StartContainer call(s)", calls)
	}
	if hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("cancelled Start recorded a start attempt without issuing a start request")
	}
}

// TestStartRefusesToReplayWhenTheAttemptedContainerIsGone pins the same
// invariant for an out-of-band container deletion: a recorded start attempt is
// never replayed by creating a fresh container.
func TestStartRefusesToReplayWhenTheAttemptedContainerIsGone(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.libpod.failNext("StartContainer", errFakeUnavailable, false)
	if _, err := fx.startResult(t); err == nil {
		t.Fatal("Start hid a transport error")
	}
	assertStartAttemptRecorded(t, fx)
	if err := fx.libpod.RemoveContainer(context.Background(), fx.containerName); err != nil {
		t.Fatalf("removing the container out of band: %v", err)
	}

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start replayed an identity whose attempted container disappeared")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("Start created a replacement container after an attempted start: %d creates", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
		t.Fatalf("Start re-issued StartContainer after an attempted start: %d starts", calls)
	}

	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("removal after the recorded attempt = %+v, want fenced success", removal)
	}
	if _, err := newAdapterWithRoot(t, fx).Start(context.Background(), startRequest(fx)); err == nil {
		t.Fatal("Start crossed the durable removal fence after cleanup")
	}
}

// assertStartAttemptRecorded fails unless the durable marker exists outside the
// read-only activation bind.
func assertStartAttemptRecorded(t *testing.T, fx *fixture) {
	t.Helper()
	if !hostFileExists(t, startAttemptPath(fx.root, fx.request.Identity.WorkloadID)) {
		t.Fatal("no durable start attempt marker was recorded before the start request")
	}
	if hostFileExists(t, filepath.Join(fx.kodamaRoot(), startAttemptRecordName)) {
		t.Fatal("the start attempt marker leaked into the read-only activation bind")
	}
}
