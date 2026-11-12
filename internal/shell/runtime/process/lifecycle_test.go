// Lifecycle behavior: stable identities, start outcomes, restart recovery, the
// declared observation vocabulary, the process-group stop barrier, idempotent
// identity-scoped cleanup and credential refresh without restart.
package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func pathExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %q: %v", path, err)
	return false
}

// TestStartAcceptedSpawnsOneStableGroup proves Start accepts the prepared
// identity, spawns exactly one group and never spawns a replacement on retry,
// even after the group exited.
func TestStartAcceptedSpawnsOneStableGroup(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)

	result := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want %q", result.Outcome, engine.StartAccepted)
	}
	if result.WorkloadID != fx.request.Identity.WorkloadID {
		t.Fatalf("start workload = %q; want the persisted identity", result.WorkloadID)
	}
	if result.Detail == "" {
		t.Fatal("accepted start has no detail")
	}
	if fx.executor.spawnCount() != 1 || fx.executor.spawnRequestCount() != 1 {
		t.Fatalf("executor saw %d spawns/%d requests; want exactly 1",
			fx.executor.spawnCount(), fx.executor.spawnRequestCount())
	}

	again := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if again.Outcome != engine.StartAccepted {
		t.Fatalf("retry outcome = %q; want accepted reuse", again.Outcome)
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("retry spawned a second process")
	}

	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	afterExit := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if afterExit.Outcome != engine.StartAccepted {
		t.Fatalf("start after exit outcome = %q; want accepted reuse", afterExit.Outcome)
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("start after exit replayed the command")
	}
}

// TestStartProvenNotStartedOnlyOnAffirmativeRefusal proves only an explicit
// executor refusal is affirmative proof, and that proof makes the identity
// removable without stop evidence.
func TestStartProvenNotStartedOnlyOnAffirmativeRefusal(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	fx.executor.setSpawn(SpawnRefused, "exec refused before exec", false, nil)

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("affirmative refusal returned error: %v", err)
	}
	if result.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("start outcome = %q; want %q", result.Outcome, engine.StartProvenNotStarted)
	}
	if result.Detail == "" {
		t.Fatal("proven-not-started result has no proof detail")
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("affirmative refusal created a process")
	}

	observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationNeverStarted {
		t.Fatalf("observation kind = %q; want %q", observation.Kind, kernel.ObservationNeverStarted)
	}
	if !observation.ProofOfNonexecution() || observation.Proof == "" {
		t.Fatalf("never-started observation has no affirmative proof: %+v", observation)
	}
	if !observation.Valid() {
		t.Fatalf("observation is not internally consistent: %+v", observation)
	}

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("prepared never-started identity was not removable: %+v", removal)
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("removing a never-started identity signalled a process group")
	}

	after, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe after removal returned error: %v", err)
	}
	if after.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after removal = %q; want absent", after.Kind)
	}
}

// TestStartUncertainNeverCollapsesToProvenNotStarted covers every ambiguous
// start outcome: transport failure without effect, a lost response after the
// effect, a zero outcome with an error and an unreachable process table on
// retry.
func TestStartUncertainNeverCollapsesToProvenNotStarted(t *testing.T) {
	t.Run("transport error without effect", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		fx.executor.setSpawn(SpawnUncertain, "executor unreachable", false, errors.New("executor unreachable"))

		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start hid an unreachable executor")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q; want %q", result.Outcome, engine.StartUnknown)
		}
		if fx.executor.spawnCount() != 0 {
			t.Fatal("uncertain start created a process")
		}
		observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind == kernel.ObservationNeverStarted {
			t.Fatal("an uncertain start was observed as never started")
		}
	})

	t.Run("lost response after effect", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		fx.executor.setSpawn(SpawnUncertain, "connection reset after spawn", true, errors.New("connection reset"))

		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start hid a lost spawn response")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("lost-response outcome = %q; want %q", result.Outcome, engine.StartUnknown)
		}
		if fx.executor.spawnCount() != 1 {
			t.Fatalf("lost response created %d processes; want 1", fx.executor.spawnCount())
		}

		retry := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
		if retry.Outcome != engine.StartAccepted {
			t.Fatalf("retry outcome = %q; want accepted reuse", retry.Outcome)
		}
		if fx.executor.spawnCount() != 1 || fx.executor.spawnRequestCount() != 1 {
			t.Fatalf("retry replayed the command: %d spawns/%d requests",
				fx.executor.spawnCount(), fx.executor.spawnRequestCount())
		}
	})

	t.Run("zero outcome with error", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		fx.executor.setSpawn(SpawnOutcome(""), "ambiguous", false, errors.New("ambiguous spawn"))

		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start hid an ambiguous spawn outcome")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q; want %q", result.Outcome, engine.StartUnknown)
		}
		if result.Outcome == engine.StartProvenNotStarted {
			t.Fatal("an ambiguous outcome collapsed into proven-not-started")
		}
	})

	t.Run("status unreachable on retry", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
		fx.observer.setStatusError(fx.groupID(t), errors.New("process table unavailable"))

		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start hid an unreachable process table")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("outcome = %q; want %q", result.Outcome, engine.StartUnknown)
		}
		if fx.executor.spawnCount() != 1 {
			t.Fatal("unreachable status replayed the command")
		}
	})
}

// TestStartProvenNotStartedPersistsNeverStarted proves affirmative
// never-created evidence for a persisted spawn intent is recorded as
// groupNeverStarted, so a retry no longer carries a stale spawning/unknown state.
func TestStartProvenNotStartedPersistsNeverStarted(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	seedGroupState(t, fx, groupSpawning, "spawn intent persisted before the effect")
	fx.observer.markNeverCreated(fx.groupID(t))

	result := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if result.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("start outcome = %q; want %q", result.Outcome, engine.StartProvenNotStarted)
	}
	if result.Detail == "" {
		t.Fatal("proven-not-started result has no proof detail")
	}

	state, found, err := fx.adapter.readGroupState(context.Background(), fx.resourceRoot(t))
	if err != nil || !found {
		t.Fatalf("readGroupState = found=%v, err=%v; want persisted state", found, err)
	}
	if state.State != groupNeverStarted {
		t.Fatalf("persisted state = %q; want %q", state.State, groupNeverStarted)
	}
	if state.Detail != result.Detail {
		t.Fatalf("persisted proof = %q; want %q", state.Detail, result.Detail)
	}

	again := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if again.Outcome != engine.StartProvenNotStarted {
		t.Fatalf("retry outcome = %q; want %q", again.Outcome, engine.StartProvenNotStarted)
	}
	if fx.executor.spawnRequestCount() != 0 || fx.executor.spawnCount() != 0 {
		t.Fatal("proven-not-started start reached the executor")
	}
}

// TestStartRejectsWorkspaceMismatch proves a start never rebinds to a workspace
// other than the prepared one.
func TestStartRejectsWorkspaceMismatch(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)

	otherWorkspace, err := WorkspaceDir(fx.workspaceRoot, "workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if err := os.MkdirAll(otherWorkspace, 0o755); err != nil {
		t.Fatalf("creating second workspace: %v", err)
	}

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: "workspace-2",
	})
	assertRefusedClass(t, err, fault.InvalidInput, fault.Forbidden)
	if result.Outcome == engine.StartAccepted || result.Outcome == engine.StartProvenNotStarted {
		t.Fatalf("mismatched start outcome = %q", result.Outcome)
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("mismatched start spawned a process")
	}
}

// TestStartRefusesWhenWorkspaceDisappeared proves a lost workspace is a failure
// and is never recreated.
func TestStartRefusesWhenWorkspaceDisappeared(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	assertRefusedClass(t, err, fault.NotFound)
	if result.Outcome == engine.StartAccepted {
		t.Fatal("start accepted a disappeared workspace")
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("start spawned a process without a workspace")
	}
	assertNoPath(t, fx.workspaceDir)
}

// TestStartRequiresCompletedPreparation proves Start never prepares implicitly.
func TestStartRequiresCompletedPreparation(t *testing.T) {
	fx := newFixture(t)

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Start accepted an unprepared identity")
	}
	if result.Outcome == engine.StartAccepted || result.Outcome == engine.StartProvenNotStarted {
		t.Fatalf("unprepared start outcome = %q", result.Outcome)
	}
	if fx.executor.spawnRequestCount() != 0 {
		t.Fatal("unprepared start reached the executor")
	}
}

// TestAdapterReconstructionReusesExistingGroup proves a fresh adapter on the
// same root observes and reuses the stable group instead of spawning again.
func TestAdapterReconstructionReusesExistingGroup(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)

	reconstructed := newTestAdapter(t, fx.config())
	result := startTestWorkload(t, reconstructed, fx.request.Identity, fx.request.WorkspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("reconstructed start outcome = %q; want accepted", result.Outcome)
	}
	if fx.executor.spawnCount() != 1 || fx.executor.spawnRequestCount() != 1 {
		t.Fatalf("reconstructed start spawned a duplicate: %d spawns/%d requests",
			fx.executor.spawnCount(), fx.executor.spawnRequestCount())
	}

	observation, err := reconstructed.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("reconstructed Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationRunning {
		t.Fatalf("reconstructed observation = %q; want running", observation.Kind)
	}

	prepareTestWorkload(t, reconstructed, fx.request)
	if fx.executor.spawnCount() != 1 {
		t.Fatal("reconstructed Prepare restarted the command")
	}
}

// TestReconstructionRefusesDifferentIdempotencyKey proves the persisted
// idempotency key is part of the stable identity.
func TestReconstructionRefusesDifferentIdempotencyKey(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)

	reconstructed := newTestAdapter(t, fx.config())
	other := fx.request.Identity
	other.IdempotencyKey = "idem-other"
	result, err := reconstructed.Start(context.Background(), engine.StartRequest{
		Identity:    other,
		WorkspaceID: fx.request.WorkspaceID,
	})
	assertRefusedClass(t, err, fault.ChangedIdempotencyContent, fault.Forbidden)
	if result.Outcome == engine.StartAccepted {
		t.Fatal("a different idempotency key reused the group")
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("a different idempotency key spawned another process")
	}
}

// TestObserveUsesKernelVocabulary proves every observation kind is reported
// distinctly and that absence, a leader exit and a signal acknowledgment never
// collapse into exit or stop evidence.
func TestObserveUsesKernelVocabulary(t *testing.T) {
	cases := []struct {
		name     string
		start    bool
		setup    func(t *testing.T, fx *fixture)
		want     kernel.ObservationKind
		wantCode int
		wantNone bool
	}{
		{
			name:  "running group",
			start: true,
			setup: func(_ *testing.T, fx *fixture) { fx.observer.markRunning(fx.groupID(t)) },
			want:  kernel.ObservationRunning,
		},
		{
			name:  "leader exited but group alive",
			start: true,
			setup: func(_ *testing.T, fx *fixture) {
				fx.observer.markLeaderExited(fx.groupID(t), 0, false)
			},
			want: kernel.ObservationRunning,
		},
		{
			name:  "whole group gone",
			start: true,
			setup: func(_ *testing.T, fx *fixture) {
				fx.observer.markLeaderExited(fx.groupID(t), 137, true)
			},
			want:     kernel.ObservationExited,
			wantCode: 137,
		},
		{
			name:  "absent group",
			start: true,
			setup: func(_ *testing.T, fx *fixture) { fx.observer.markAbsent(fx.groupID(t)) },
			want:  kernel.ObservationAbsent,
		},
		{
			name:     "prepared but never created",
			start:    false,
			setup:    func(_ *testing.T, fx *fixture) { fx.observer.markNeverCreated(fx.groupID(t)) },
			want:     kernel.ObservationNeverStarted,
			wantNone: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fx := newFixture(t)
			prepareTestWorkload(t, fx.adapter, fx.request)
			if testCase.start {
				startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
			}
			testCase.setup(t, fx)

			observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
				Identity:    fx.request.Identity,
				WorkspaceID: fx.request.WorkspaceID,
			})
			if err != nil {
				t.Fatalf("Observe returned error: %v", err)
			}
			if observation.Kind != testCase.want {
				t.Fatalf("observation kind = %q; want %q", observation.Kind, testCase.want)
			}
			if !observation.Valid() {
				t.Fatalf("observation is not internally consistent: %+v", observation)
			}
			if testCase.want == kernel.ObservationExited {
				code, ok := observation.ExitStatus()
				if !ok || code != testCase.wantCode {
					t.Fatalf("exit status = %d (present=%v); want %d", code, ok, testCase.wantCode)
				}
				if !observation.StopConfirmed() {
					t.Fatal("affirmative whole-group termination did not confirm the stop barrier")
				}
				return
			}
			if observation.StopConfirmed() {
				t.Fatalf("observation %q claims a confirmed stop", observation.Kind)
			}
			if observation.ProofOfNonexecution() != testCase.wantNone {
				t.Fatalf("ProofOfNonexecution = %v for %q; want %v",
					observation.ProofOfNonexecution(), observation.Kind, testCase.wantNone)
			}
		})
	}

	t.Run("status unreachable is unknown", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		fx.observer.setStatusError(fx.groupID(t), errors.New("process table unavailable"))

		observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Observe hid an unreachable process table")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation kind = %q; want unknown", observation.Kind)
		}
	})
}

// TestObserveNeverStartedRequiresPreparation proves affirmative never-created
// evidence alone is not never_started: the identity must also have been
// prepared, otherwise the observation stays absent.
func TestObserveNeverStartedRequiresPreparation(t *testing.T) {
	fx := newFixture(t)
	fx.observer.markNeverCreated(fx.groupID(t))

	observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation kind = %q; want absent", observation.Kind)
	}
	if observation.ProofOfNonexecution() {
		t.Fatal("an unprepared identity was reported as never started")
	}
}

// TestObserveSpawnUncertainUsesAffirmativeEvidence proves a persisted spawn
// intent or lost spawn response never masks affirmative process-group evidence:
// a running group stays running, an affirmatively terminated group reports its
// leader exit, and only affirmative never-created evidence proves nonexecution.
func TestObserveSpawnUncertainUsesAffirmativeEvidence(t *testing.T) {
	// newLostResponse drives the real Start path so the persisted state is the
	// adapter's own groupUnknown bookkeeping after a lost spawn response.
	newLostResponse := func(t *testing.T) *fixture {
		t.Helper()
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		fx.executor.setSpawn(SpawnUncertain, "connection reset after spawn", true, errors.New("connection reset"))
		result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start hid a lost spawn response")
		}
		if result.Outcome != engine.StartUnknown {
			t.Fatalf("lost-response outcome = %q; want %q", result.Outcome, engine.StartUnknown)
		}
		state, found, err := fx.adapter.readGroupState(context.Background(), fx.resourceRoot(t))
		if err != nil || !found || state.State != groupUnknown {
			t.Fatalf("persisted state = %+v, found=%v, err=%v; want groupUnknown", state, found, err)
		}
		return fx
	}

	observe := func(t *testing.T, fx *fixture) (kernel.Observation, error) {
		t.Helper()
		return fx.adapter.Observe(context.Background(), engine.ObserveRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
	}

	t.Run("persisted spawn intent with a running group", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		seedGroupState(t, fx, groupSpawning, "spawn intent persisted before the effect")
		fx.observer.markRunning(fx.groupID(t))

		observation, err := observe(t, fx)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation kind = %q; want running", observation.Kind)
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a running group was reported as never started")
		}
	})

	t.Run("lost response with a running group", func(t *testing.T) {
		fx := newLostResponse(t)
		fx.observer.markRunning(fx.groupID(t))

		observation, err := observe(t, fx)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationRunning {
			t.Fatalf("observation kind = %q; want running", observation.Kind)
		}
		if observation.StopConfirmed() {
			t.Fatal("a running group reported a confirmed stop")
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a running group was reported as never started")
		}
	})

	t.Run("lost response with the group gone and a leader exit", func(t *testing.T) {
		fx := newLostResponse(t)
		fx.observer.markLeaderExited(fx.groupID(t), 7, true)

		observation, err := observe(t, fx)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationExited {
			t.Fatalf("observation kind = %q; want exited", observation.Kind)
		}
		code, ok := observation.ExitStatus()
		if !ok || code != 7 {
			t.Fatalf("exit status = %d (present=%v); want 7", code, ok)
		}
		if !observation.StopConfirmed() {
			t.Fatal("affirmative whole-group termination did not confirm the stop")
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a completed process was reported as never started")
		}
	})

	t.Run("lost response with zero status", func(t *testing.T) {
		fx := newLostResponse(t)
		fx.observer.markAbsent(fx.groupID(t))

		observation, err := observe(t, fx)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation kind = %q; want unknown", observation.Kind)
		}
		if observation.Kind == kernel.ObservationAbsent {
			t.Fatal("a persisted spawn intent collapsed into absence")
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("a zero status was treated as never-started proof")
		}
	})

	t.Run("lost response with unreachable status", func(t *testing.T) {
		fx := newLostResponse(t)
		fx.observer.setStatusError(fx.groupID(t), errors.New("process table unavailable"))

		observation, err := observe(t, fx)
		if err == nil {
			t.Fatal("Observe hid an unreachable process table")
		}
		if observation.Kind != kernel.ObservationUnknown {
			t.Fatalf("observation kind = %q; want unknown", observation.Kind)
		}
		if observation.ProofOfNonexecution() {
			t.Fatal("an unreachable status was treated as never-started proof")
		}
	})

	t.Run("lost response with affirmative never-created evidence", func(t *testing.T) {
		fx := newLostResponse(t)
		fx.observer.markNeverCreated(fx.groupID(t))

		observation, err := observe(t, fx)
		if err != nil {
			t.Fatalf("Observe returned error: %v", err)
		}
		if observation.Kind != kernel.ObservationNeverStarted {
			t.Fatalf("observation kind = %q; want never started", observation.Kind)
		}
		if !observation.ProofOfNonexecution() || observation.Proof == "" {
			t.Fatalf("never-started observation has no affirmative proof: %+v", observation)
		}
		if !observation.Valid() {
			t.Fatalf("observation is not internally consistent: %+v", observation)
		}
	})
}

// TestStopAcknowledgmentIsNotConfirmation proves a delivered signal, a leader
// exit and elapsed time never confirm the stop barrier.
func TestStopAcknowledgmentIsNotConfirmation(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markRunning(fx.groupID(t))

	evidence, err := fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatal("a signal acknowledgment confirmed the stop")
	}
	if evidence.Detail == "" {
		t.Fatal("pending stop has no detail")
	}
	if fx.executor.signalCount() != 1 {
		t.Fatalf("signals = %d; want 1", fx.executor.signalCount())
	}
	if groups := fx.executor.signaledGroups(); len(groups) != 1 || groups[0] != fx.groupID(t) {
		t.Fatalf("signalled groups = %v; want the stable group %q", groups, fx.groupID(t))
	}

	fx.observer.markLeaderExited(fx.groupID(t), 0, false)
	evidence, err = fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if evidence.IsConfirmed() {
		t.Fatal("a leader exit with live group members confirmed the stop")
	}

	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	evidence, err = fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatal("affirmative whole-group termination did not confirm the stop")
	}
	if evidence.Detail == "" {
		t.Fatal("confirmed stop has no detail")
	}
}

// TestStopLostResponseStaysPending proves a lost signal response stays pending
// until affirmative evidence arrives.
func TestStopLostResponseStaysPending(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markRunning(fx.groupID(t))
	fx.executor.setSignalError(errors.New("signal outcome unknown"))

	evidence, err := fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Stop hid a lost signal response")
	}
	if evidence.IsConfirmed() {
		t.Fatal("a lost stop response was treated as confirmed")
	}

	fx.executor.setSignalError(nil)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	evidence, err = fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("retry Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatal("observed termination after a lost response did not confirm stop")
	}
}

// TestRemoveRefusesWhileGroupMayRun proves Remove retains every owned resource
// and names the group while the process group may still run.
func TestRemoveRefusesWhileGroupMayRun(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markRunning(fx.groupID(t))
	resourceRoot := fx.resourceRoot(t)

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if removal.Removed {
		t.Fatal("Remove reported success while the group may still run")
	}
	if !strings.Contains(removal.Detail, string(fx.groupID(t))) {
		t.Fatalf("removal detail %q does not name the retained group %q", removal.Detail, fx.groupID(t))
	}
	if !pathExists(t, resourceRoot) {
		t.Fatal("Remove deleted the resource root while the group may still run")
	}
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q", got)
	}
	if calls := fx.files.callsFor("RemoveAll"); len(calls) != 0 {
		t.Fatalf("Remove deleted resources while the group may still run: %v", calls)
	}

	second, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("repeated Remove returned error: %v", err)
	}
	if second.Removed {
		t.Fatal("repeated Remove reported success while the group may still run")
	}
}

// TestRemoveAfterConfirmedStopPreservesWorkspace proves cleanup removes only the
// identity's resources, is idempotent and never deletes the workspace.
func TestRemoveAfterConfirmedStopPreservesWorkspace(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	resourceRoot := fx.resourceRoot(t)

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the owned resources removed", removal)
	}
	if removal.Detail == "" {
		t.Fatal("removal has no detail")
	}
	assertNoPath(t, resourceRoot)
	if !pathExists(t, fx.workspaceDir) {
		t.Fatal("Remove deleted the execution workspace")
	}
	if entries := workspaceEntries(t, fx.workspaceDir); !slices.Equal(entries, []string{workspaceSentinel}) {
		t.Fatalf("workspace entries after removal = %v; want only the seeded sentinel", entries)
	}
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q", got)
	}
	for _, path := range fx.files.callsFor("RemoveAll") {
		if path == fx.workspaceDir || strings.HasPrefix(path, fx.workspaceDir+string(os.PathSeparator)) {
			t.Fatalf("Remove removed the workspace path %q", path)
		}
	}

	second, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("repeated Remove returned error: %v", err)
	}
	if !second.Removed {
		t.Fatalf("repeated removal = %+v; want idempotent success", second)
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("Remove spawned a process")
	}
}

// TestRemovePreparedNeverStarted proves a prepared never-started identity is
// removable without stop evidence and without signalling.
func TestRemovePreparedNeverStarted(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	fx.observer.markNeverCreated(fx.groupID(t))

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("prepared never-started identity was not removable: %+v", removal)
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("Remove signalled a never-started group")
	}
	assertNoPath(t, fx.resourceRoot(t))
	if !pathExists(t, fx.workspaceDir) {
		t.Fatal("Remove deleted the execution workspace")
	}
}

// seedGroupState persists one spawn state directly, modelling a crash between
// the persisted spawn intent and the spawn effect.
func seedGroupState(t *testing.T, fx *fixture, kind groupStateKind, detail string) {
	t.Helper()
	state := groupState{
		Group:    fx.groupID(t),
		Identity: fx.request.Identity,
		State:    kind,
		Detail:   detail,
	}
	if err := fx.adapter.writeGroupState(context.Background(), fx.resourceRoot(t), state); err != nil {
		t.Fatalf("seeding %q group state: %v", kind, err)
	}
}

// TestRemoveSpawnIntentWithoutEffect proves an identity whose spawn intent was
// persisted but whose effect affirmatively never happened is removable without
// signalling or removing a group, while still tombstoning and never touching
// the workspace.
func TestRemoveSpawnIntentWithoutEffect(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	seedGroupState(t, fx, groupSpawning, "spawn intent persisted before the effect")
	fx.observer.markNeverCreated(fx.groupID(t))
	resourceRoot := fx.resourceRoot(t)

	observation, err := fx.adapter.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationNeverStarted {
		t.Fatalf("observation kind = %q; want never started", observation.Kind)
	}

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("spawn intent without an effect was not removable: %+v", removal)
	}
	if !strings.Contains(removal.Detail, string(fx.groupID(t))) {
		t.Fatalf("removal detail %q does not name the group %q", removal.Detail, fx.groupID(t))
	}
	assertNoPath(t, resourceRoot)
	if !pathExists(t, fx.workspaceDir) {
		t.Fatal("Remove deleted the execution workspace")
	}
	if entries := workspaceEntries(t, fx.workspaceDir); !slices.Equal(entries, []string{workspaceSentinel}) {
		t.Fatalf("workspace entries after removal = %v; want only the seeded sentinel", entries)
	}
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q", got)
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("Remove signalled a group that was never created")
	}
	if fx.executor.removalCount() != 0 {
		t.Fatal("Remove asked the executor to remove a group that was never created")
	}
	if !pathExists(t, fx.adapter.tombstonePath(fx.request.Identity.WorkloadID)) {
		t.Fatal("Remove did not persist a removal tombstone")
	}

	_, err = fx.adapter.Prepare(context.Background(), fx.request)
	assertRefusedClass(t, err, fault.Forbidden)

	result, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err == nil {
		t.Fatal("Start accepted a removed identity")
	}
	if result.Outcome != engine.StartUnknown {
		t.Fatalf("start after removal outcome = %q; want unknown", result.Outcome)
	}
	if fx.executor.spawnCount() != 0 {
		t.Fatal("Remove spawned a process")
	}
}

// TestRemoveLostSpawnResponseAfterGroupGone proves a lost spawn response whose
// group has affirmatively terminated is removable: the executor removes the
// group, the identity is tombstoned and cleanup proceeds.
func TestRemoveLostSpawnResponseAfterGroupGone(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	fx.executor.setSpawn(SpawnUncertain, "connection reset after spawn", true, errors.New("connection reset"))
	if _, err := fx.adapter.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err == nil {
		t.Fatal("Start hid a lost spawn response")
	}
	state, found, err := fx.adapter.readGroupState(context.Background(), fx.resourceRoot(t))
	if err != nil || !found || state.State != groupUnknown {
		t.Fatalf("persisted state = %+v, found=%v, err=%v; want groupUnknown", state, found, err)
	}
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	resourceRoot := fx.resourceRoot(t)

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("lost spawn response with a gone group was not removable: %+v", removal)
	}
	if fx.executor.removalCount() != 1 {
		t.Fatalf("RemoveGroup calls = %d; want 1", fx.executor.removalCount())
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("Remove signalled a group that had already terminated")
	}
	assertNoPath(t, resourceRoot)
	if !pathExists(t, fx.workspaceDir) {
		t.Fatal("Remove deleted the execution workspace")
	}
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q", got)
	}
	if !pathExists(t, fx.adapter.tombstonePath(fx.request.Identity.WorkloadID)) {
		t.Fatal("Remove did not persist a removal tombstone")
	}
}

// TestRemoveLostSpawnResponseRetainsWithoutAffirmativeEvidence proves a lost
// spawn response is retained without a tombstone or cleanup unless the observer
// affirmatively reports the group gone or never created.
func TestRemoveLostSpawnResponseRetainsWithoutAffirmativeEvidence(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, fx *fixture)
	}{
		{
			name:  "running group",
			setup: func(_ *testing.T, fx *fixture) { fx.observer.markRunning(fx.groupID(t)) },
		},
		{
			name: "unreachable status",
			setup: func(_ *testing.T, fx *fixture) {
				fx.observer.setStatusError(fx.groupID(t), errors.New("process table unavailable"))
			},
		},
		{
			name:  "zero status",
			setup: func(_ *testing.T, fx *fixture) { fx.observer.markAbsent(fx.groupID(t)) },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fx := newFixture(t)
			prepareTestWorkload(t, fx.adapter, fx.request)
			fx.executor.setSpawn(SpawnUncertain, "connection reset after spawn", true, errors.New("connection reset"))
			if _, err := fx.adapter.Start(context.Background(), engine.StartRequest{
				Identity:    fx.request.Identity,
				WorkspaceID: fx.request.WorkspaceID,
			}); err == nil {
				t.Fatal("Start hid a lost spawn response")
			}
			testCase.setup(t, fx)
			resourceRoot := fx.resourceRoot(t)

			removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
				Identity:    fx.request.Identity,
				WorkspaceID: fx.request.WorkspaceID,
			})
			if err != nil {
				t.Fatalf("Remove returned error: %v", err)
			}
			if removal.Removed {
				t.Fatal("Remove reported success without affirmative termination evidence")
			}
			if !strings.Contains(removal.Detail, string(fx.groupID(t))) {
				t.Fatalf("removal detail %q does not attribute the retained group %q", removal.Detail, fx.groupID(t))
			}
			if calls := fx.files.callsFor("RemoveAll"); len(calls) != 0 {
				t.Fatalf("Remove deleted resources without affirmative evidence: %v", calls)
			}
			if !pathExists(t, resourceRoot) {
				t.Fatal("Remove deleted the resource root without affirmative evidence")
			}
			if pathExists(t, fx.adapter.tombstonePath(fx.request.Identity.WorkloadID)) {
				t.Fatal("Remove tombstoned an identity that may still be running")
			}
			if !pathExists(t, fx.workspaceDir) {
				t.Fatal("Remove deleted the execution workspace")
			}
		})
	}
}

// TestRemoveIsIdentityScoped proves removing one identity never touches another
// identity's resources or either workspace.
func TestRemoveIsIdentityScoped(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)

	otherWorkspace, err := WorkspaceDir(fx.workspaceRoot, "workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if err := os.MkdirAll(otherWorkspace, 0o755); err != nil {
		t.Fatalf("creating second workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherWorkspace, workspaceSentinel), []byte(workspaceSentinelData), 0o644); err != nil {
		t.Fatalf("seeding second workspace: %v", err)
	}
	otherRequest := fx.request
	otherRequest.Identity = engine.EffectIdentity{
		ExecutionID:    "exec-2",
		ActionID:       "action-2",
		WorkloadID:     "workload-2",
		IdempotencyKey: "idem-2",
	}
	otherRequest.WorkspaceID = "workspace-2"
	otherRequest.Inputs = map[id.InputKey]string{"input-b": "other"}
	prepareTestWorkload(t, fx.adapter, otherRequest)
	startTestWorkload(t, fx.adapter, otherRequest.Identity, otherRequest.WorkspaceID)
	otherGroup, err := GroupName(otherRequest.Identity.WorkloadID)
	if err != nil {
		t.Fatalf("GroupName returned error: %v", err)
	}
	fx.observer.markLeaderExited(otherGroup, 0, true)

	otherRoot, err := ResourceRoot(fx.root, otherRequest.Identity.WorkloadID)
	if err != nil {
		t.Fatalf("ResourceRoot returned error: %v", err)
	}

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want success", removal)
	}
	assertNoPath(t, fx.resourceRoot(t))
	if !pathExists(t, otherRoot) {
		t.Fatal("Remove deleted another identity's resources")
	}
	if !pathExists(t, fx.workspaceDir) || !pathExists(t, otherWorkspace) {
		t.Fatal("Remove deleted an execution workspace")
	}
}

// TestRemoveBoundsRetriesWithInjectedBackoff proves cleanup retries are bounded
// by policy, use the injected clock and leave visible pending work with
// attribution instead of a false success.
func TestRemoveBoundsRetriesWithInjectedBackoff(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	resourceRoot := fx.resourceRoot(t)

	fx.files.injectBefore("RemoveAll", "", errors.New("resource busy"))
	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if removal.Removed {
		t.Fatal("Remove reported success after every cleanup attempt failed")
	}
	if !strings.Contains(removal.Detail, string(fx.groupID(t))) {
		t.Fatalf("removal detail %q does not attribute the retained group %q", removal.Detail, fx.groupID(t))
	}
	if !pathExists(t, resourceRoot) {
		t.Fatal("Remove deleted the resource root after failed attempts")
	}
	if attempts := len(fx.files.callsFor("RemoveAll")); attempts != 3 {
		t.Fatalf("RemoveAll attempts = %d; want the configured 3", attempts)
	}
	sleeps := fx.clock.recordedSleeps()
	if len(sleeps) != 2 {
		t.Fatalf("backoff waits = %v; want 2 waits between 3 attempts", sleeps)
	}
	for _, sleep := range sleeps {
		if sleep != 2*time.Second {
			t.Fatalf("backoff wait = %v; want the configured 2s", sleep)
		}
	}

	fx.files.clear("RemoveAll", "")
	removal, err = fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove retry returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal retry = %+v; want success once cleanup succeeds", removal)
	}
	assertNoPath(t, resourceRoot)
}

// TestRemovedIdentityIsNotReusable proves a completed removal is a durable
// tombstone: Prepare and Start are refused and Observe reports absent, never
// never_started, including after reconstruction.
func TestRemovedIdentityIsNotReusable(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want success", removal, err)
	}

	// The executor now claims no process was ever created; the removal
	// tombstone must still win.
	fx.observer.markNeverCreated(fx.groupID(t))
	reconstructed := newTestAdapter(t, fx.config())

	_, err = reconstructed.Prepare(context.Background(), fx.request)
	assertRefusedClass(t, err, fault.Forbidden, fault.NotFound)

	result, err := reconstructed.Start(context.Background(), engine.StartRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err == nil && result.Outcome == engine.StartAccepted {
		t.Fatal("a removed identity was started again")
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("a removed identity spawned a process")
	}

	observation, err := reconstructed.Observe(context.Background(), engine.ObserveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Observe returned error: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("observation after removal = %q; want absent", observation.Kind)
	}
	if observation.ProofOfNonexecution() {
		t.Fatal("a removed identity was reported as never started")
	}

	refresh, err := reconstructed.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-revive",
	})
	if err == nil {
		t.Fatal("RefreshCredential revived a removed identity")
	}
	if refresh.Delivered {
		t.Fatal("a removed identity reported a delivered credential")
	}
	assertNoPath(t, fx.credentialPath(t))
}

// TestRefreshCredentialReplacesOnlyMountedFile proves token replacement updates
// the credential file without restarting or re-signalling the command.
func TestRefreshCredentialReplacesOnlyMountedFile(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markRunning(fx.groupID(t))
	beforeStatus := fx.observer.statusOf(fx.groupID(t))

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-new",
	})
	if err != nil {
		t.Fatalf("RefreshCredential returned error: %v", err)
	}
	if !refresh.Delivered {
		t.Fatalf("refresh = %+v; want delivered", refresh)
	}
	assertHostFile(t, fx.credentialPath(t), "tok-new")
	assertHostFile(t, fx.hostPath(t, "/kodama/secrets/API_KEY"), "s3cr3t")
	if fx.executor.spawnCount() != 1 || fx.executor.spawnRequestCount() != 1 {
		t.Fatal("RefreshCredential restarted the command")
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("RefreshCredential signalled the process group")
	}
	if fx.observer.statusOf(fx.groupID(t)) != beforeStatus {
		t.Fatal("RefreshCredential changed process-group evidence")
	}
	assertNoSecrets(t, "refresh detail", refresh.Detail)
}

// TestRefreshCredentialRecoversLostUpdateResponse proves an update whose
// response was lost is recoverable and never restarts the command.
func TestRefreshCredentialRecoversLostUpdateResponse(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markRunning(fx.groupID(t))

	// Either an in-place write or a temp-file rename may deliver the update;
	// both lose the response after the effect is visible.
	fx.files.injectAfter("WriteFile", "/KODAMA_TOKEN", errors.New("connection reset after write"))
	fx.files.injectAfter("Rename", "/KODAMA_TOKEN", errors.New("connection reset after rename"))

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-lost",
	})
	if err == nil {
		t.Fatal("RefreshCredential hid a lost update response")
	}
	if refresh.Delivered {
		t.Fatal("a lost update response was reported as delivered")
	}
	if pathExists(t, fx.credentialPath(t)) {
		if got := readHostFile(t, fx.credentialPath(t)); got != "tok-lost" {
			t.Fatalf("credential after the lost response = %q; want the new value", got)
		}
	}
	if fx.executor.spawnCount() != 1 {
		t.Fatal("a lost update restarted the command")
	}

	fx.files.clear("WriteFile", "/KODAMA_TOKEN")
	fx.files.clear("Rename", "/KODAMA_TOKEN")
	retry, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-lost",
	})
	if err != nil {
		t.Fatalf("retry returned error: %v", err)
	}
	if !retry.Delivered {
		t.Fatal("retry after a lost response was not delivered")
	}
	assertHostFile(t, fx.credentialPath(t), "tok-lost")
	if fx.executor.spawnCount() != 1 {
		t.Fatal("retry restarted the command")
	}
}

// TestRefreshCredentialNeverRevivesRemovedIdentity proves refresh refuses after
// removal and never recreates the credential file.
func TestRefreshCredentialNeverRevivesRemovedIdentity(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	if _, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	}); err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-revive",
	})
	if err == nil {
		t.Fatal("RefreshCredential accepted a removed identity")
	}
	if refresh.Delivered {
		t.Fatal("a removed identity reported a delivered credential")
	}
	assertNoPath(t, fx.credentialPath(t))
	if fx.executor.spawnCount() != 1 {
		t.Fatal("RefreshCredential restarted a removed identity")
	}
}

// TestRefreshCredentialRefusesEmpty proves an empty credential never reaches the
// mounted file.
func TestRefreshCredentialRefusesEmpty(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	writesBefore := len(fx.files.callsFor("WriteFile"))

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "",
	})
	assertRefusedClass(t, err, fault.InvalidInput)
	if refresh.Delivered {
		t.Fatal("an empty credential was reported as delivered")
	}
	if len(fx.files.callsFor("WriteFile")) != writesBefore {
		t.Fatal("an empty credential reached the file boundary")
	}
	assertHostFile(t, fx.credentialPath(t), "tok-abc")
}

// TestDiagnosticsNeverContainSecrets proves no operation detail or refusal
// carries secret or credential material.
func TestDiagnosticsNeverContainSecrets(t *testing.T) {
	fx := newFixture(t)

	preparation := prepareTestWorkload(t, fx.adapter, fx.request)
	assertNoSecrets(t, "prepare detail", preparation.Detail)

	start := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	assertNoSecrets(t, "start detail", start.Detail)

	refresh, err := fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "tok-new",
	})
	if err != nil {
		t.Fatalf("RefreshCredential returned error: %v", err)
	}
	assertNoSecrets(t, "refresh detail", refresh.Detail)

	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	stop, err := fx.adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	assertNoSecrets(t, "stop detail", stop.Detail)

	removal, err := fx.adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	assertNoSecrets(t, "remove detail", removal.Detail)

	changed := fx.request
	changed.Inputs = map[id.InputKey]string{"input-a": "different"}
	_, err = fx.adapter.Prepare(context.Background(), changed)
	if err != nil {
		assertNoSecrets(t, "prepare refusal", err.Error())
	}
	_, err = fx.adapter.RefreshCredential(context.Background(), engine.CredentialRequest{
		Identity:   fx.request.Identity,
		Credential: "",
	})
	if err != nil {
		assertNoSecrets(t, "refresh refusal", err.Error())
	}
}
