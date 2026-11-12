// Lifecycle behavior: stable identities, start outcomes, restart recovery, the
// declared observation vocabulary, the whole-container stop barrier, idempotent
// identity-scoped cleanup, the durable removal fence and credential refresh
// without a restart.
package podman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func startRequest(fx *fixture) engine.StartRequest {
	return engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}
}

func observeRequest(fx *fixture) engine.ObserveRequest {
	return engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}
}

func stopRequest(fx *fixture) engine.StopRequest {
	return engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}
}

func removeRequest(fx *fixture) engine.RemoveRequest {
	return engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}
}

func TestStartCreatesOneStableContainerAndReusesOnRetry(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	result := fx.start(t)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q, want %q", result.Outcome, engine.StartAccepted)
	}
	if result.WorkloadID != fx.request.Identity.WorkloadID {
		t.Fatalf("start workload = %q, want the persisted identity", result.WorkloadID)
	}
	if result.Detail == "" {
		t.Fatal("accepted start has no detail")
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("CreateContainer calls = %d, want exactly 1", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
		t.Fatalf("StartContainer calls = %d, want exactly 1", calls)
	}
	state, ok := fx.libpod.container(fx.containerName)
	if !ok {
		t.Fatal("Start did not create the stable container")
	}
	if state.Status != ContainerStatusRunning {
		t.Fatalf("container status = %q, want running", state.Status)
	}

	again := fx.start(t)
	if again.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q, want accepted reuse", again.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("retry created a second container: %d creates", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
		t.Fatalf("retry restarted the running container: %d starts", calls)
	}

	fx.seedExited(t, 0, true)
	afterExit := fx.start(t)
	if afterExit.Outcome != engine.StartAccepted {
		t.Fatalf("start after exit outcome = %q, want accepted reuse", afterExit.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("start after exit replayed the command: %d creates", calls)
	}
}

func TestStartLostCreateReplyStaysUnknownAndRetryReuses(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.libpod.failNext("CreateContainer", errFakeLost, true)

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid a lost create response")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("lost-create outcome = %q, want %q", result.Outcome, engine.StartUnknown)
	}
	if result.Outcome == engine.StartProvenNotStarted {
		t.Fatal("a lost create response collapsed into proven-not-started")
	}
	if count := fx.libpod.containerCount(); count != 1 {
		t.Fatalf("lost create response left %d containers, want 1", count)
	}

	retry := fx.start(t)
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q, want accepted reuse", retry.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("retry created a second container: %d creates", calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
		t.Fatalf("retry started %d times, want the one stable start", calls)
	}
}

func TestStartCreateFailureBeforeEffectStaysUnknownThenRetryCreates(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.libpod.failNext("CreateContainer", errFakeUnavailable, false)

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid an unavailable daemon")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if count := fx.libpod.containerCount(); count != 0 {
		t.Fatalf("failed create left %d containers", count)
	}

	retry := fx.start(t)
	if retry.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q, want accepted", retry.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 2 {
		t.Fatalf("CreateContainer calls = %d, want one failed and one successful", calls)
	}
	if count := fx.libpod.containerCount(); count != 1 {
		t.Fatalf("containers after retry = %d, want 1", count)
	}
}

func TestStartLostStartReplyStaysUnknownAndNeverReplays(t *testing.T) {
	t.Run("lost response after the effect", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.failNext("StartContainer", errFakeLost, true)

		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start hid a lost start response")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("lost-start outcome = %q, want unknown", result.Outcome)
		}
		state, ok := fx.libpod.container(fx.containerName)
		if !ok || state.Status != ContainerStatusRunning {
			t.Fatalf("container after lost start = %+v, want running", state)
		}

		retry := fx.start(t)
		if retry.Outcome != engine.StartAccepted {
			t.Fatalf("retry outcome = %q, want accepted reuse", retry.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
			t.Fatalf("retry created a second container: %d", calls)
		}
		if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
			t.Fatalf("retry re-issued start after the effect: %d starts", calls)
		}
	})

	t.Run("failure before the effect", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		// A transport error does not prove the start request had no effect: the
		// durable attempt marker was already written, so the outcome stays
		// unknown and the start is never replayed.
		fx.libpod.failNext("StartContainer", errFakeUnavailable, false)

		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start hid an unavailable daemon")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		state, ok := fx.libpod.container(fx.containerName)
		if !ok || state.Status != ContainerStatusCreated || state.Started {
			t.Fatalf("container after failed start = %+v, want created-never-started", state)
		}
		if observation := fx.observe(t); observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation after an ambiguous start = %q, want unknown", observation.Kind)
		}
		if removal := fx.remove(t); removal.Removed {
			t.Fatal("Remove succeeded while a start may still be delivered")
		}

		retry, err := fx.startResult(t)
		if err == nil {
			t.Fatal("retry hid the recorded start attempt")
		}
		if retry.Outcome != engine.StartUnknown {
			t.Fatalf("retry outcome = %q, want unknown", retry.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
			t.Fatalf("retry created a second container: %d", calls)
		}
		if calls := fx.libpod.callsFor("StartContainer"); calls != 1 {
			t.Fatalf("retry starts = %d, want the ambiguous start never replayed", calls)
		}
		if count := fx.libpod.containerCount(); count != 1 {
			t.Fatalf("containers after retry = %d, want 1", count)
		}

		if evidence := fx.stop(t); !evidence.IsConfirmed() {
			t.Fatal("Stop on a marked created container did not confirm whole-container termination")
		}
		if removal := fx.remove(t); !removal.Removed {
			t.Fatalf("removal after confirmed stop = %+v, want success", removal)
		}
	})
}

func TestStartPendingDelayedStartIsUnknownAndNotRestarted(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, true)

	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid a delayed start")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("Start re-issued a delayed start %d times", calls)
	}

	observation := fx.observe(t)
	if observation.Kind != kernel.ObservationUnknown {
		t.Fatalf("observation = %q, want unknown while a delayed start is possible", observation.Kind)
	}

	fx.seedRunning(t)
	observation = fx.observe(t)
	if observation.Kind != kernel.ObservationRunning {
		t.Fatalf("observation after delayed start = %q, want running", observation.Kind)
	}
}

func TestStartRefusesWithoutPreparationOrWorkspace(t *testing.T) {
	t.Run("no preparation", func(t *testing.T) {
		fx := newFixture(t)
		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start without preparation returned no error")
		}
		assertFaultClass(t, err, fault.InvalidInput)
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatal("Start created a container without preparation")
		}
	})

	t.Run("missing workspace volume after preparation", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.libpod.deleteVolume(fx.volumeName)
		result, err := fx.startResult(t)
		if err == nil {
			t.Fatal("Start hid a missing workspace volume")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatal("Start created a container without the retained workspace volume")
		}
	})

	t.Run("workspace mismatch", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: "workspace-2",
		})
		if err == nil {
			t.Fatal("Start hid a workspace mismatch")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q, want unknown", result.Outcome)
		}
		if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
			t.Fatal("Start created a container for a mismatched workspace")
		}
	})
}

func TestStartAfterReconstructionUsesPersistedIdentity(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	reconstructed := newAdapterWithRoot(t, fx)
	result, err := reconstructed.Start(context.Background(), startRequest(fx))
	if err != nil {
		t.Fatalf("Start after reconstruction returned error: %v", err)
	}
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("outcome = %q, want accepted", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("CreateContainer calls = %d, want 1", calls)
	}
	specs := fx.libpod.createSpecs()
	if len(specs) != 1 || specs[0].Name != fx.containerName || specs[0].Image != testImage {
		t.Fatalf("reconstructed spec = %+v", specs)
	}

	again := newAdapterWithRoot(t, fx)
	result, err = again.Start(context.Background(), startRequest(fx))
	if err != nil {
		t.Fatalf("second reconstructed Start returned error: %v", err)
	}
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("outcome = %q, want accepted reuse", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 1 {
		t.Fatalf("reconstruction replayed the container: %d creates", calls)
	}
}

func TestObserveUsesKernelVocabulary(t *testing.T) {
	t.Run("absent after a recorded start attempt", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		// Absence is only "absent" when a start may have run. Without a durable
		// start-attempt marker the same container evidence is affirmative
		// never-started proof, not absence; see
		// TestObserveAbsentAfterPreparationWithoutStartAttemptProvesNonexecution.
		if err := fx.adapter.writeStartAttempt(context.Background(), fx.request.Identity, fx.request.WorkspaceID); err != nil {
			t.Fatalf("recording the start attempt: %v", err)
		}
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationAbsent {
			t.Fatalf("observation = %q, want absent", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("absence was treated as proof of nonexecution")
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})

	t.Run("created never started", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedCreated(t, false)
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationNeverStarted {
			t.Fatalf("observation = %q, want never_started", observation.Kind)
		}
		if !observation.ProofOfNonexecution() || observation.Proof == "" {
			t.Fatalf("never-started observation has no affirmative proof: %+v", observation)
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})

	t.Run("delayed start pending", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedCreated(t, true)
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %q, want unknown while a delayed start is possible", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a pending start was treated as never started")
		}
	})

	t.Run("running", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation = %q, want running", observation.Kind)
		}
		if observation.StopConfirmed() {
			t.Fatal("a running container reported a confirmed stop")
		}
	})

	t.Run("exited with whole-container stop", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedExited(t, 7, true)
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationExited {
			t.Fatalf("observation = %q, want exited", observation.Kind)
		}
		code, ok := observation.ExitStatus()
		if !ok || code != 7 {
			t.Fatalf("exit status = %d (present=%v), want 7", code, ok)
		}
		if !observation.StopConfirmed() {
			t.Fatal("affirmative whole-container termination did not confirm the stop")
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})

	t.Run("exited without whole-container evidence", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedExited(t, 7, false)
		observation := fx.observe(t)
		if observation.Kind != kernel.ObservationExited {
			t.Fatalf("observation = %q, want exited", observation.Kind)
		}
		if observation.StopConfirmed() {
			t.Fatal("an init exit without whole-container evidence confirmed the stop")
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})

	t.Run("inspect error", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)
		fx.libpod.setInspectError(errFakeUnavailable)
		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe hid an unreachable daemon")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %q, want unknown", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("an unreachable daemon was treated as never started")
		}
	})

	t.Run("foreign container labels", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		state := fx.seedRunning(t)
		state.Labels[LabelWorkload] = "other-workload"
		fx.libpod.setContainer(state)
		observation, err := fx.adapter.Observe(context.Background(), observeRequest(fx))
		if err == nil {
			t.Fatal("Observe accepted a foreign container")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation = %q, want unknown for a foreign container", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a foreign container was treated as never started")
		}
	})
}

func TestStopAcknowledgmentIsNotConfirmation(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)
	fx.libpod.setStopEffect(false)

	evidence := fx.stop(t)
	if evidence.IsConfirmed() {
		t.Fatal("a stop acknowledgment confirmed the stop")
	}
	if evidence.Detail == "" {
		t.Fatal("pending stop has no detail")
	}
	if calls := fx.libpod.stoppedNames(); len(calls) != 1 || calls[0] != fx.containerName {
		t.Fatalf("stopped names = %v, want the stable container %q", calls, fx.containerName)
	}

	fx.seedExited(t, 0, false)
	evidence = fx.stop(t)
	if evidence.IsConfirmed() {
		t.Fatal("an init exit without whole-container evidence confirmed the stop")
	}

	fx.seedExited(t, 0, true)
	evidence = fx.stop(t)
	if !evidence.IsConfirmed() {
		t.Fatal("affirmative whole-container termination did not confirm the stop")
	}
	if evidence.Detail == "" {
		t.Fatal("confirmed stop has no detail")
	}
}

func TestStopOnAbsentContainerStaysPending(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	evidence := fx.stop(t)
	if evidence.IsConfirmed() {
		t.Fatal("stopping an absent container confirmed the stop")
	}
	if evidence.Detail == "" {
		t.Fatal("pending stop has no detail")
	}
}

func TestStopLostResponseStaysPending(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)
	fx.libpod.failNext("StopContainer", errFakeLost, false)

	evidence, err := fx.adapter.Stop(context.Background(), stopRequest(fx))
	if err == nil {
		t.Fatal("Stop hid a lost stop response")
	}
	if evidence.IsConfirmed() {
		t.Fatal("a lost stop response was treated as confirmed")
	}

	fx.seedExited(t, 0, true)
	evidence = fx.stop(t)
	if !evidence.IsConfirmed() {
		t.Fatal("observed whole-container termination after a lost response did not confirm the stop")
	}
}

func TestRemoveIsIdempotentAndIdentityScoped(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedExited(t, 0, true)

	otherWorkload := id.WorkloadID("workload-2")
	otherName, err := ContainerName(otherWorkload)
	if err != nil {
		t.Fatalf("ContainerName returned error: %v", err)
	}
	otherRoot := filepath.Join(fx.root, string(otherWorkload))
	if err := os.MkdirAll(filepath.Join(otherRoot, "kodama"), 0o755); err != nil {
		t.Fatalf("creating other workload root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherRoot, "kodama", "keep"), []byte("keep"), 0o444); err != nil {
		t.Fatalf("seeding other workload: %v", err)
	}
	fx.libpod.setContainer(ContainerState{
		ID:     "container-" + otherName,
		Name:   otherName,
		Status: ContainerStatusRunning,
		Labels: map[string]string{LabelWorkload: string(otherWorkload)},
	})

	removal := fx.remove(t)
	if !removal.Removed {
		t.Fatalf("removal = %+v, want the owned resources removed", removal)
	}
	if removal.Detail == "" {
		t.Fatal("removal has no detail")
	}
	if _, ok := fx.libpod.container(fx.containerName); ok {
		t.Fatal("Remove left the identity's container behind")
	}
	if _, ok := fx.libpod.container(otherName); !ok {
		t.Fatal("Remove deleted another identity's container")
	}
	if hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove left the identity's materialization behind")
	}
	if !hostFileExists(t, filepath.Join(otherRoot, "kodama", "keep")) {
		t.Fatal("Remove deleted another identity's materialization")
	}
	if volume, ok := fx.libpod.volume(fx.volumeName); !ok {
		t.Fatal("Remove deleted the retained workspace volume")
	} else if volume.Labels[LabelWorkspace] != string(fx.request.WorkspaceID) {
		t.Fatalf("workspace volume changed: %+v", volume)
	}
	for _, name := range fx.libpod.removedNames() {
		if name != fx.containerName {
			t.Fatalf("Remove removed a container it does not own: %q", name)
		}
	}

	second := fx.remove(t)
	if !second.Removed {
		t.Fatalf("repeated removal = %+v, want idempotent success", second)
	}
	if _, ok := fx.libpod.container(otherName); !ok {
		t.Fatal("repeated Remove deleted another identity's container")
	}
}

func TestRemoveRetainsWhileContainerMayRun(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)
		removal := fx.remove(t)
		if removal.Removed {
			t.Fatal("Remove reported success while the container may still run")
		}
		if !strings.Contains(removal.Detail, fx.containerName) {
			t.Fatalf("removal detail %q does not name the retained container %q", removal.Detail, fx.containerName)
		}
		if !hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove deleted the materialization while the container may run")
		}
		if calls := fx.libpod.removedNames(); len(calls) != 0 {
			t.Fatalf("Remove removed a live container: %v", calls)
		}
	})

	t.Run("exited without whole-container evidence", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedExited(t, 0, false)
		removal := fx.remove(t)
		if removal.Removed {
			t.Fatal("Remove reported success without whole-container stop evidence")
		}
		if !hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove deleted the materialization without stop evidence")
		}
	})

	t.Run("created never started", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedCreated(t, false)
		removal := fx.remove(t)
		if !removal.Removed {
			t.Fatalf("a created-never-started container was not removable: %+v", removal)
		}
		if _, ok := fx.libpod.container(fx.containerName); ok {
			t.Fatal("Remove left the never-started container behind")
		}
		if hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove left the materialization behind")
		}
	})
}

func TestRemoveBoundsRetriesWithInjectedBackoff(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	fx.libpod.failRemoveTimes(2, errFakeUnavailable)

	removal := fx.remove(t)
	if !removal.Removed {
		t.Fatalf("removal after transient failures = %+v, want success", removal)
	}
	sleeps := fx.clock.sleepDurations()
	if len(sleeps) != 2 {
		t.Fatalf("backoff sleeps = %v, want exactly 2", sleeps)
	}
	for _, duration := range sleeps {
		if duration != time.Second {
			t.Fatalf("backoff = %v, want 1s", duration)
		}
	}
	if calls := fx.libpod.callsFor("RemoveContainer"); calls != 3 {
		t.Fatalf("RemoveContainer calls = %d, want 3 bounded attempts", calls)
	}
}

func TestRemoveExhaustionLeavesVisiblePendingWork(t *testing.T) {
	fx := newFixture(t)
	adapter, err := New(Config{
		Libpod:    fx.libpod,
		Files:     fx.files,
		Root:      fx.root,
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 2, Backoff: time.Second},
		Clock:     fx.clock,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	fx.adapter = adapter
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	fx.libpod.failRemoveTimes(2, errFakeUnavailable)

	removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
	if err != nil {
		t.Fatalf("Remove returned error on exhaustion: %v", err)
	}
	if removal.Removed {
		t.Fatal("Remove reported success after exhausting cleanup attempts")
	}
	if !strings.Contains(removal.Detail, fx.containerName) {
		t.Fatalf("exhaustion detail %q does not name the retained container %q", removal.Detail, fx.containerName)
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("exhausted cleanup deleted the materialization it must retain")
	}
	if sleeps := fx.clock.sleepDurations(); len(sleeps) != 1 {
		t.Fatalf("backoff sleeps = %v, want 1", sleeps)
	}
	if calls := fx.libpod.callsFor("RemoveContainer"); calls != 2 {
		t.Fatalf("RemoveContainer calls = %d, want the bounded 2", calls)
	}

	fx.libpod.failRemoveTimes(0, nil)
	if retry := fx.remove(t); !retry.Removed {
		t.Fatalf("retry after exhaustion = %+v, want success", retry)
	}
}

func TestRemoveCancellationDuringBackoff(t *testing.T) {
	fx := newFixture(t)
	adapter, err := New(Config{
		Libpod:    fx.libpod,
		Files:     fx.files,
		Root:      fx.root,
		KodamaURL: testKodamaURL,
		Cleanup:   CleanupPolicy{Attempts: 3, Backoff: time.Second},
		Clock:     fx.clock,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	fx.adapter = adapter
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	fx.libpod.failRemoveTimes(3, errFakeUnavailable)

	gate := fx.clock.newGate()
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		removal engine.Removal
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		removal, err := fx.adapter.Remove(ctx, removeRequest(fx))
		done <- outcome{removal: removal, err: err}
	}()
	waitFor(t, gate.entered, "the injected backoff")
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("cancelled Remove returned no error")
		}
		if got.removal.Removed {
			t.Fatal("cancelled Remove reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Remove did not return after cancellation")
	}
	if sleeps := fx.clock.sleepDurations(); len(sleeps) != 1 {
		t.Fatalf("backoff sleeps = %v, want cancellation during the first backoff", sleeps)
	}
	if calls := fx.libpod.callsFor("RemoveContainer"); calls != 1 {
		t.Fatalf("RemoveContainer calls = %d, want no attempt after cancellation", calls)
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("cancelled Remove deleted the materialization")
	}
}

func TestRemoveFenceRejectsPrepareAndStartAfterReconstruction(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("removal = %+v, want success", removal)
	}

	reconstructed := newAdapterWithRoot(t, fx)
	if _, err := reconstructed.Prepare(context.Background(), fx.request); err == nil {
		t.Fatal("Prepare re-created a removed identity")
	} else {
		assertFaultClass(t, err, fault.NotFound)
	}

	result, err := reconstructed.Start(context.Background(), startRequest(fx))
	if err == nil {
		t.Fatal("Start revived a removed identity")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("start outcome = %q, want unknown", result.Outcome)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != 0 {
		t.Fatalf("Start created a container for a removed identity: %d", calls)
	}

	observation, err := reconstructed.Observe(context.Background(), observeRequest(fx))
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after removal = %q, want absent", observation.Kind)
	}
	if observation.ProofOfNonexecution() {
		t.Fatal("removal was reported as never-started proof")
	}
}

// TestRemoveWritesTheFenceBeforeEveryDestructiveStep pins the ordering that the
// absent-container proof depends on: the durable removal fence must exist before
// Remove touches the container or the workload materialization. If the fence
// were written after either step, a crash or a partial materialization removal
// could delete the start-attempt marker while leaving the preparation record,
// which Observe would read as affirmative never-started proof for a command that
// ran; the Kernel would then skip Stop, release the workspace and replace the
// identity.
func TestRemoveWritesTheFenceBeforeEveryDestructiveStep(t *testing.T) {
	workload := testIdentity().WorkloadID
	fenceExists := func(t *testing.T, fx *fixture) bool {
		t.Helper()
		return hostFileExists(t, fencePath(fx.root, workload))
	}

	// runRemove starts Remove in a goroutine, waits for the injected barrier,
	// records whether the fence already exists at that point, then releases the
	// barrier and returns the settled removal. It never calls t.Fatalf while the
	// Remove goroutine is blocked, so a failure cannot leak a stuck goroutine.
	runRemove := func(t *testing.T, fx *fixture, entered <-chan struct{}, release chan struct{}, barrier string) (bool, engine.Removal) {
		t.Helper()
		type outcome struct {
			removal engine.Removal
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
			done <- outcome{removal: removal, err: err}
		}()
		waitFor(t, entered, "Remove at the "+barrier)
		fenceBefore := fenceExists(t, fx)
		close(release)

		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("Remove returned error after the %s barrier: %v", barrier, got.err)
			}
			return fenceBefore, got.removal
		case <-time.After(5 * time.Second):
			t.Fatalf("Remove did not return after the %s barrier was released", barrier)
			return false, engine.Removal{}
		}
	}

	t.Run("before the container removal", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedExited(t, 0, true)
		gate := fx.libpod.newGate("RemoveContainer")

		fenceBefore, removal := runRemove(t, fx, gate.entered, gate.release, "container removal")
		if !removal.Removed {
			t.Fatalf("removal = %+v, want success", removal)
		}
		if !fenceBefore {
			t.Fatal("Remove reached RemoveContainer before writing the durable removal fence")
		}
	})

	t.Run("before the materialization removal", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedCreated(t, false)
		gate := fx.files.newGate("RemoveAll")

		fenceBefore, removal := runRemove(t, fx, gate.entered, gate.release, "materialization removal")
		if !removal.Removed {
			t.Fatalf("removal = %+v, want success", removal)
		}
		if !fenceBefore {
			t.Fatal("Remove reached RemoveAll of the workload root before writing the durable removal fence")
		}
	})
}

// TestRemoveFenceSurvivesPartialMaterializationRemoval reproduces the failure
// that the fence ordering prevents. An identity whose start was attempted and
// whose container is gone loses the start-attempt marker while the preparation
// record survives a failed materialization removal. Because the fence is written
// first, Observe must report the identity as absent without proof; it must never
// collapse the partial state into affirmative never-started evidence. A retry
// after the partial failure then completes the removal.
func TestRemoveFenceSurvivesPartialMaterializationRemoval(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	workload := fx.request.Identity.WorkloadID

	// A start was attempted (the durable marker exists) and the container it
	// targeted is gone, so absence alone is not proof.
	fx.libpod.failNext("StartContainer", errFakeUnavailable, false)
	if _, err := fx.startResult(t); err == nil {
		t.Fatal("Start hid a transport error")
	}
	assertStartAttemptRecorded(t, fx)
	if err := fx.libpod.RemoveContainer(context.Background(), fx.containerName); err != nil {
		t.Fatalf("removing the container out of band: %v", err)
	}
	if observation := fx.observe(t); observation.Kind != kernel.ObservationAbsent || observation.ProofOfNonexecution() {
		t.Fatalf("pre-removal observation = %+v, want absent without proof", observation)
	}

	fenceAtRemoval := false
	fx.files.onRemoveAll(filepath.Join(string(filepath.Separator), string(workload)), func(string) error {
		fenceAtRemoval = hostFileExists(t, fencePath(fx.root, workload))
		// Simulate a partial RemoveAll: the start-attempt marker is deleted but
		// the preparation record survives, then removal fails.
		if err := os.Remove(startAttemptPath(fx.root, workload)); err != nil {
			t.Fatalf("simulating a partial materialization removal: %v", err)
		}
		return errFakeUnavailable
	})

	removal := fx.remove(t)
	if removal.Removed {
		t.Fatalf("Remove reported success although the materialization removal failed: %+v", removal)
	}
	if !fenceAtRemoval {
		t.Fatal("Remove started removing the workload root before writing the durable removal fence")
	}
	if !hostFileExists(t, fencePath(fx.root, workload)) {
		t.Fatal("Remove left no durable removal fence after the partial failure")
	}
	if hostFileExists(t, startAttemptPath(fx.root, workload)) {
		t.Fatal("the simulated partial removal did not delete the start-attempt marker")
	}
	if !hostFileExists(t, preparationPath(fx.root, workload)) {
		t.Fatal("the simulated partial removal deleted the preparation record")
	}

	observation := fx.observe(t)
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after a partial removal = %+v, want absent", observation)
	}
	if observation.ProofOfNonexecution() {
		t.Fatalf("a partially removed attempted identity was reported as never-started: %+v", observation)
	}

	// The failure was one-shot and only the fenced identity remains, so a retry
	// completes the removal.
	retry := fx.remove(t)
	if !retry.Removed {
		t.Fatalf("retry after the partial removal = %+v, want success", retry)
	}
	if hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("retry left the materialization behind")
	}
	if !hostFileExists(t, fencePath(fx.root, workload)) {
		t.Fatal("retry removed the durable removal fence")
	}
}

// TestRemoveFenceWriteFailureRetainsEverything pins that a failed fence write is
// a failed removal. Continuing to remove the container or the materialization
// without the durable fence would leave exactly the unfenced partial state the
// absent-container proof forbids.
func TestRemoveFenceWriteFailureRetainsEverything(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, false)
	workload := fx.request.Identity.WorkloadID
	fx.files.injectBefore("WriteFile", filepath.Join(fenceDirectory, string(workload)+".json"), errFakeUnavailable)

	removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
	if err != nil {
		t.Fatalf("Remove returned error on a fence write failure: %v", err)
	}
	if removal.Removed {
		t.Fatalf("Remove reported success although the fence write failed: %+v", removal)
	}
	if _, ok := fx.libpod.container(fx.containerName); !ok {
		t.Fatal("Remove removed the container without a durable removal fence")
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove deleted the materialization without a durable removal fence")
	}
	if hostFileExists(t, fencePath(fx.root, workload)) {
		t.Fatal("a removal fence exists despite the injected write failure")
	}
}

// TestRemoveRefusesWhenStartAttemptStateIsUnreadable pins the Remove-side
// evidence rule: an unreadable start-attempt marker is unknown, never "no
// attempt". Removing a possibly-started container on an unreadable marker would
// destroy the only evidence that the command may have run.
func TestRemoveRefusesWhenStartAttemptStateIsUnreadable(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, false)
	fx.files.injectBefore("Lstat", startAttemptRecordName, errFakeUnavailable)

	removal, err := fx.adapter.Remove(context.Background(), removeRequest(fx))
	if err == nil {
		t.Fatal("Remove hid an unreadable start attempt state")
	}
	if removal.Removed {
		t.Fatalf("Remove reported success on unreadable start-attempt state: %+v", removal)
	}
	if _, ok := fx.libpod.container(fx.containerName); !ok {
		t.Fatal("Remove removed the container while the start attempt was unreadable")
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove deleted the materialization while the start attempt was unreadable")
	}
}

// TestRemoveRetainsCreatedContainerWithoutAffirmativeProof pins the two created
// states that are not affirmative never-started evidence: a delayed start that
// may still be delivered and an ambiguous accepted start without a durable
// attempt marker. Neither may be removed.
func TestRemoveRetainsCreatedContainerWithoutAffirmativeProof(t *testing.T) {
	t.Run("delayed start pending", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedCreated(t, true)

		removal := fx.remove(t)
		if removal.Removed {
			t.Fatalf("Remove reported success while a delayed start was possible: %+v", removal)
		}
		if _, ok := fx.libpod.container(fx.containerName); !ok {
			t.Fatal("Remove deleted a container with a pending start")
		}
		if !hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove deleted the materialization with a pending start")
		}
	})

	t.Run("ambiguous accepted start", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		state := fx.seedCreated(t, false)
		state.Started = true
		fx.libpod.setContainer(state)

		removal := fx.remove(t)
		if removal.Removed {
			t.Fatalf("Remove reported success for an ambiguous accepted start: %+v", removal)
		}
		if _, ok := fx.libpod.container(fx.containerName); !ok {
			t.Fatal("Remove deleted a container with an ambiguous accepted start")
		}
		if !hostFileExists(t, fx.workloadRoot()) {
			t.Fatal("Remove deleted the materialization with an ambiguous accepted start")
		}
	})
}

// TestRemoveRefusesForeignContainerWithSameName pins identity scoping beyond the
// container name. A container that happens to carry this workload's name but a
// different execution identity is never removed.
func TestRemoveRefusesForeignContainerWithSameName(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	state := fx.seedCreated(t, false)
	state.Labels[LabelExecution] = "exec-other"
	fx.libpod.setContainer(state)

	removal := fx.remove(t)
	if removal.Removed {
		t.Fatalf("Remove deleted a container owned by another identity: %+v", removal)
	}
	if _, ok := fx.libpod.container(fx.containerName); !ok {
		t.Fatal("Remove deleted a container owned by another identity")
	}
	if !hostFileExists(t, fx.workloadRoot()) {
		t.Fatal("Remove deleted the materialization of a foreign container identity")
	}
}

// TestRepeatedRemoveDoesNotRewriteTheDurableFence pins idempotent cleanup: once
// the fence exists, a repeated Remove needs no further fence write. A failed
// rewrite must not turn an already-completed removal into a retained one.
func TestRepeatedRemoveDoesNotRewriteTheDurableFence(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedCreated(t, false)
	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("first removal = %+v, want success", removal)
	}
	workload := fx.request.Identity.WorkloadID
	fx.files.injectBefore("WriteFile", filepath.Join(fenceDirectory, string(workload)+".json"), errFakeUnavailable)

	second := fx.remove(t)
	if !second.Removed {
		t.Fatalf("repeated removal = %+v, want idempotent success without a fence rewrite", second)
	}
}

// TestRemoveIssuesNoContainerRemovalForAnAbsentContainer pins bounded cleanup:
// when inspection proves the container is absent, Remove completes without
// issuing a RemoveContainer call for an object that does not exist.
func TestRemoveIssuesNoContainerRemovalForAnAbsentContainer(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	removal := fx.remove(t)
	if !removal.Removed {
		t.Fatalf("removal of an absent container = %+v, want success", removal)
	}
	if calls := fx.libpod.callsFor("RemoveContainer"); calls != 0 {
		t.Fatalf("Remove issued %d RemoveContainer call(s) for an absent container", calls)
	}
}

func TestRefreshCredentialReplacesTokenWithoutRestart(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)

	createCalls := fx.libpod.callsFor("CreateContainer")
	startCalls := fx.libpod.callsFor("StartContainer")
	stopCalls := fx.libpod.callsFor("StopContainer")
	removeCalls := fx.libpod.callsFor("RemoveContainer")
	directWrites := fx.files.countPath("WriteFile", fx.credentialPath())
	renames := fx.files.countPath("Rename", fx.credentialPath())

	refresh := fx.refresh(t, "tok-new")
	if !refresh.Delivered {
		t.Fatalf("refresh = %+v, want delivered", refresh)
	}
	if refresh.Detail == "" {
		t.Fatal("refresh has no detail")
	}
	if got := readHostFile(t, fx.credentialPath()); string(got) != "tok-new" {
		t.Fatalf("credential bytes = %q, want tok-new", got)
	}
	if mode := hostFileMode(t, fx.credentialPath()); mode != 0o444 {
		t.Fatalf("credential mode = %o, want 0444", mode)
	}
	if calls := fx.libpod.callsFor("CreateContainer"); calls != createCalls {
		t.Fatalf("refresh created a container: %d -> %d", createCalls, calls)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != startCalls {
		t.Fatalf("refresh restarted the container: %d -> %d", startCalls, calls)
	}
	if calls := fx.libpod.callsFor("StopContainer"); calls != stopCalls {
		t.Fatalf("refresh stopped the container: %d -> %d", stopCalls, calls)
	}
	if calls := fx.libpod.callsFor("RemoveContainer"); calls != removeCalls {
		t.Fatalf("refresh removed the container: %d -> %d", removeCalls, calls)
	}
	if got := fx.files.countPath("WriteFile", fx.credentialPath()); got != directWrites {
		t.Fatalf("refresh wrote the token in place (%d direct writes), want an atomic rename", got)
	}
	if got := fx.files.countPath("Rename", fx.credentialPath()); got != renames+1 {
		t.Fatalf("refresh renames = %d, want one atomic replacement", got-renames)
	}
	if !hostFileExists(t, filepath.Join(fx.kodamaRoot(), "secrets", "API_KEY")) {
		t.Fatal("refresh disturbed the declared secret mounts")
	}
}

func TestRefreshCredentialRecoversLostResponseAndAtomicity(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)

	fx.files.injectBefore("Rename", "KODAMA_TOKEN", errFakeLost)
	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-failed",
	})
	if err == nil {
		t.Fatal("refresh hid a failed atomic replacement")
	}
	if refresh.Delivered {
		t.Fatal("a failed replacement reported delivery")
	}
	if got := readHostFile(t, fx.credentialPath()); string(got) != "tok-abc" {
		t.Fatalf("failed replacement corrupted the previous token: %q", got)
	}

	fx.files.clear("Rename", "KODAMA_TOKEN")
	refresh = fx.refresh(t, "tok-b")
	if !refresh.Delivered {
		t.Fatalf("retry refresh = %+v, want delivered", refresh)
	}
	if got := readHostFile(t, fx.credentialPath()); string(got) != "tok-b" {
		t.Fatalf("credential bytes = %q, want tok-b", got)
	}

	// A lost response after the rename still leaves the new token in place and
	// a retry converges without a restart.
	fx.files.injectAfter("Rename", "KODAMA_TOKEN", errFakeLost)
	refresh, err = fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-c",
	})
	if err == nil {
		t.Fatal("refresh hid a lost replacement response")
	}
	if refresh.Delivered {
		t.Fatal("a lost replacement response reported delivery")
	}
	if got := readHostFile(t, fx.credentialPath()); string(got) != "tok-c" {
		t.Fatalf("lost-response token = %q, want tok-c", got)
	}
	fx.files.clear("Rename", "KODAMA_TOKEN")
	refresh = fx.refresh(t, "tok-c")
	if !refresh.Delivered {
		t.Fatalf("recovered refresh = %+v, want delivered", refresh)
	}
	if calls := fx.libpod.callsFor("StartContainer"); calls != 0 {
		t.Fatalf("credential recovery restarted the container %d times", calls)
	}

	if _, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "  ",
	}); err == nil {
		t.Fatal("refresh accepted an empty credential")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
}

func TestRefreshRefusedAfterRemoval(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("removal = %+v, want success", removal)
	}
	writesBefore := fx.files.callCount("WriteFile")
	renamesBefore := fx.files.callCount("Rename")

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-after-removal",
	})
	if err == nil {
		t.Fatal("refresh accepted a removed identity")
	}
	if refresh.Delivered {
		t.Fatal("refresh delivered a credential for a removed identity")
	}
	if writes := fx.files.callCount("WriteFile"); writes != writesBefore {
		t.Fatalf("refresh wrote %d files for a removed identity", writes-writesBefore)
	}
	if renames := fx.files.callCount("Rename"); renames != renamesBefore {
		t.Fatalf("refresh renamed %d files for a removed identity", renames-renamesBefore)
	}
}

func TestSecretValuesNeverAppearInDiagnostics(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)

	forbidden := []string{"s3cr3t", "tok-abc"}
	assertClean := func(where string, values ...string) {
		t.Helper()
		for _, value := range values {
			for _, secret := range forbidden {
				if strings.Contains(value, secret) {
					t.Fatalf("%s leaked %q: %q", where, secret, value)
				}
			}
		}
	}

	fx.libpod.failNext("CreateContainer", errFakeUnavailable, false)
	result, err := fx.startResult(t)
	if err == nil {
		t.Fatal("Start hid an unavailable daemon")
	}
	assertClean("start error", err.Error(), result.Detail, fmt.Sprintf("%+v", result))

	fx.files.injectBefore("Rename", "KODAMA_TOKEN", errFakeUnavailable)
	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-abc",
	})
	if err == nil {
		t.Fatal("refresh hid a failed replacement")
	}
	assertClean("refresh error", err.Error(), refresh.Detail)
}

func TestRemoveNeverCallsAnExternalVolumeRemoval(t *testing.T) {
	// Volume removal is exclusively the engine.Workspace Release operation.
	// This test pins the behavioral consequence for Remove: cleanup leaves the
	// workspace volume and never issues a volume removal.
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedExited(t, 0, true)
	if removal := fx.remove(t); !removal.Removed {
		t.Fatalf("removal = %+v, want success", removal)
	}
	if removed := fx.libpod.removedVolumes(); len(removed) != 0 {
		t.Fatalf("Remove issued volume removals %v, want none", removed)
	}
	volume, ok := fx.libpod.volume(fx.volumeName)
	if !ok {
		t.Fatal("the retained workspace volume disappeared")
	}
	if volume.Name != fx.volumeName {
		t.Fatalf("workspace volume name = %q, want %q", volume.Name, fx.volumeName)
	}
}
