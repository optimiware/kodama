package services_test

// Recovery: a merge effect that already settled must never run a second
// time when the assignment is replayed. The dangerous window is the interval
// between the effect commit and the request resolution: a coordinator that
// releases the claim there (and whose store discards the released identity)
// lets the replayed assignment start a fresh operation and repeat the merge.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

// assignedMergeRequest is one exact assignment-keyed merge request.
func assignedMergeRequest(executionID, actionID string) services.MergeRequest {
	request := validMergeRequest()
	request.Assignment = services.MergeAssignment{
		ExecutionID: id.ExecutionID(executionID),
		ActionID:    id.ActionID(actionID),
	}
	return request
}

// TestMergeReplayAfterEffectBeforeResolveRunsOneEffect reproduces the replay
// window: merge once, stop before the request is resolved, then replay the same
// assignment. The persisted intent and result must be adopted, so exactly one
// effect executes and the same observation is returned.
func TestMergeReplayAfterEffectBeforeResolveRunsOneEffect(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	first, err := coordinator.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the single first effect", effects.executeCount())
	}
	// The process stops here: the effect settled, the request was never
	// resolved, and nothing tells the coordinator that.

	replayed, err := coordinator.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("replayed Merge: %v", err)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls after the replay = %d; want exactly one merge effect", effects.executeCount())
	}
	if replayed != first {
		t.Fatalf("replayed result = %+v; want the durable first result %+v", replayed, first)
	}
}

// TestMergeReplayAfterLeaseExpiryRunsOneEffect proves the durable record, not
// the process, owns the assignment identity: a takeover after lease expiry
// adopts the settled result instead of repeating it.
func TestMergeReplayAfterLeaseExpiryRunsOneEffect(t *testing.T) {
	directory := t.TempDir()
	clock := &fakeClock{now: testNow}
	request := validMergeRequest()

	firstStore, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	firstEffects := &fakeMergeEffects{log: &callLog{}}
	firstEffects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "refs/heads/work/merge-1 merged"), nil
	}
	first := newCoordinator(t, firstStore, firstEffects, clock, "merge-worker-1")
	settled, err := first.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	if firstEffects.executeCount() != 1 {
		t.Fatalf("first execute calls = %d; want 1", firstEffects.executeCount())
	}

	// The first process dies after the effect and before the resolution. Its
	// lease elapses and a second process opens the same durable directory.
	clock.Advance(2 * time.Minute)
	reopened, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	secondEffects := &fakeMergeEffects{log: &callLog{}}
	second := newCoordinatorNamed(t, reopened, secondEffects, clock, "merge-worker-2", "restart")
	replayed, err := second.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("takeover Merge: %v", err)
	}
	if secondEffects.executeCount() != 0 {
		t.Fatalf("takeover execute calls = %d; want no repeated effect", secondEffects.executeCount())
	}
	if replayed != settled {
		t.Fatalf("takeover result = %+v; want the durable settled result %+v", replayed, settled)
	}
}

// TestCoordinatorSettlesOnlyAfterAnExplicitResolution proves the merge result
// stays durable and held until the adapter reports the request resolution, and
// that an identical replay converges instead of executing again.
func TestCoordinatorSettlesOnlyAfterAnExplicitResolution(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := assignedMergeRequest("exec-1", "action-1")

	result, err := coordinator.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("result state = %q; want applied", result.State)
	}
	held, _ := store.record(t, testTenant, request.Key)
	if held.Released {
		t.Fatal("Merge released the assignment before the request resolution")
	}
	pending, err := coordinator.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Request != request || pending[0].State != services.EffectApplied {
		t.Fatalf("pending assignments = %+v; want the held applied assignment", pending)
	}

	if err := coordinator.Settle(context.Background(), request, services.EffectNotStarted); !errors.Is(err, services.ErrOperationConflict) {
		t.Fatalf("Settle(not_started) error = %v; want ErrOperationConflict", err)
	}
	if err := coordinator.Settle(context.Background(), request, services.EffectApplied); err != nil {
		t.Fatalf("Settle(applied): %v", err)
	}
	released, _ := store.record(t, testTenant, request.Key)
	if !released.Released {
		t.Fatal("Settle did not release the assignment")
	}
	pending, err = coordinator.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending after settle: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after settle = %+v; want none", pending)
	}

	// An identical Settle replay is a no-op, and an identical Merge replay is
	// answered from the durable record.
	if err := coordinator.Settle(context.Background(), request, services.EffectApplied); err != nil {
		t.Fatalf("identical Settle replay: %v", err)
	}
	replayed, err := coordinator.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("identical Merge replay: %v", err)
	}
	if replayed != result || effects.executeCount() != 1 {
		t.Fatalf("replayed result/effects = %+v/%d; want %+v/1", replayed, effects.executeCount(), result)
	}
}

// TestCoordinatorRefusesASecondAssignmentWhileHeld proves serialization: a
// different assignment on the same repository/ref must not start a second
// operation before the first assignment is settled.
func TestCoordinatorRefusesASecondAssignmentWhileHeld(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	first := assignedMergeRequest("exec-1", "action-1")

	if _, err := coordinator.Merge(context.Background(), first); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	second := first
	second.Assignment = services.MergeAssignment{ExecutionID: "exec-2", ActionID: "action-2"}
	second.Candidate = "candidate-3333333"
	if _, err := coordinator.Merge(context.Background(), second); !errors.Is(err, services.ErrOperationConflict) {
		t.Fatalf("second Merge error = %v; want ErrOperationConflict", err)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; a second assignment ran before settlement", effects.executeCount())
	}
}

// TestCoordinatorNewAssignmentAfterSettleExecutesOnce proves a settled
// assignment does not block the next generation.
func TestCoordinatorNewAssignmentAfterSettleExecutesOnce(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	first := assignedMergeRequest("exec-1", "action-1")

	if _, err := coordinator.Merge(context.Background(), first); err != nil {
		t.Fatalf("first Merge: %v", err)
	}
	if err := coordinator.Settle(context.Background(), first, services.EffectApplied); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	second := first
	second.Assignment = services.MergeAssignment{ExecutionID: "exec-2", ActionID: "action-2"}
	second.Candidate = "candidate-3333333"
	if _, err := coordinator.Merge(context.Background(), second); err != nil {
		t.Fatalf("second Merge after settlement: %v", err)
	}
	if effects.executeCount() != 2 {
		t.Fatalf("execute calls = %d; want one effect per assignment", effects.executeCount())
	}
	operations := effects.executedOperations()
	if operations[0].OperationID == operations[1].OperationID {
		t.Fatalf("both assignments used operation %q; want a fresh operation after settlement", operations[0].OperationID)
	}
}

// TestCoordinatorSettleRefusesAnUnknownOrMismatchedAssignment pins the settle
// guard rails.
func TestCoordinatorSettleRefuseUnknownOrMismatchedAssignment(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	coordinator := newCoordinator(t, store, &fakeMergeEffects{log: log}, clock, "merge-worker-1")
	request := assignedMergeRequest("exec-1", "action-1")

	if err := coordinator.Settle(context.Background(), request, services.EffectApplied); !errors.Is(err, services.ErrNoClaim) {
		t.Fatalf("Settle without an intent error = %v; want ErrNoClaim", err)
	}
	if _, err := coordinator.Merge(context.Background(), request); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	foreign := request
	foreign.Assignment = services.MergeAssignment{ExecutionID: "exec-2", ActionID: "action-2"}
	if err := coordinator.Settle(context.Background(), foreign, services.EffectApplied); !errors.Is(err, services.ErrOperationConflict) {
		t.Fatalf("Settle for a foreign assignment error = %v; want ErrOperationConflict", err)
	}
	held, _ := store.record(t, testTenant, request.Key)
	if held.Released {
		t.Fatal("a mismatched assignment released the record")
	}
}

// TestCoordinatorAssignmentIdentityIsExact proves the durable assignment is
// identified by both exact identities, the execution and the action. Every case
// holds the repository, ref, base, candidate and decision evidence identical,
// so only the changed identity can be what refuses the replay. A comparison
// that ignores the action id, or the assignment entirely, would adopt the held
// or settled result and skip the effect.
func TestCoordinatorAssignmentIdentityIsExact(t *testing.T) {
	base := assignedMergeRequest("exec-1", "action-1")
	changes := map[string]func(*services.MergeRequest){
		"only the action id changes": func(request *services.MergeRequest) {
			request.Assignment.ActionID = "action-2"
		},
		"only the execution id changes": func(request *services.MergeRequest) {
			request.Assignment.ExecutionID = "exec-2"
		},
	}
	for name, change := range changes {

		t.Run("held merge refuses when "+name, func(t *testing.T) {
			log := &callLog{}
			store := newFakeCoordinationStore(log)
			clock := &fakeClock{now: testNow}
			effects := &fakeMergeEffects{log: log}
			coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")

			if _, err := coordinator.Merge(context.Background(), base); err != nil {
				t.Fatalf("first Merge: %v", err)
			}
			changed := base
			change(&changed)
			if _, err := coordinator.Merge(context.Background(), changed); !errors.Is(err, services.ErrOperationConflict) {
				t.Fatalf("Merge of the %s error = %v; want ErrOperationConflict", name, err)
			}
			if effects.executeCount() != 1 {
				t.Fatalf("execute calls = %d; a differing assignment ran a second effect on the held record", effects.executeCount())
			}
			held, _ := store.record(t, testTenant, base.Key)
			if held.Released || held.Request != base {
				t.Fatalf("held record = %+v released=%t; want the original assignment still held", held.Request, held.Released)
			}
		})

		t.Run("settle refuses when "+name, func(t *testing.T) {
			log := &callLog{}
			store := newFakeCoordinationStore(log)
			clock := &fakeClock{now: testNow}
			effects := &fakeMergeEffects{log: log}
			coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")

			if _, err := coordinator.Merge(context.Background(), base); err != nil {
				t.Fatalf("Merge: %v", err)
			}
			changed := base
			change(&changed)
			if err := coordinator.Settle(context.Background(), changed, services.EffectApplied); !errors.Is(err, services.ErrOperationConflict) {
				t.Fatalf("Settle of the %s error = %v; want ErrOperationConflict", name, err)
			}
			held, _ := store.record(t, testTenant, base.Key)
			if held.Released || held.Request != base {
				t.Fatalf("record = %+v released=%t; a mismatched assignment released it", held.Request, held.Released)
			}
			pending, err := coordinator.Pending(context.Background())
			if err != nil {
				t.Fatalf("Pending: %v", err)
			}
			if len(pending) != 1 || pending[0].Request != base {
				t.Fatalf("pending = %+v; want the original assignment still pending", pending)
			}
		})

		t.Run("settled replay runs the new assignment when "+name, func(t *testing.T) {
			log := &callLog{}
			store := newFakeCoordinationStore(log)
			clock := &fakeClock{now: testNow}
			effects := &fakeMergeEffects{log: log}
			coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")

			if _, err := coordinator.Merge(context.Background(), base); err != nil {
				t.Fatalf("first Merge: %v", err)
			}
			if err := coordinator.Settle(context.Background(), base, services.EffectApplied); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			changed := base
			change(&changed)
			result, err := coordinator.Merge(context.Background(), changed)
			if err != nil {
				t.Fatalf("Merge of the %s after settlement: %v", name, err)
			}
			if result.State != services.EffectApplied {
				t.Fatalf("result state = %q; want a freshly applied effect", result.State)
			}
			if effects.executeCount() != 2 {
				t.Fatalf("execute calls = %d; the new assignment adopted the settled result instead of running its effect", effects.executeCount())
			}
			operations := effects.executedOperations()
			if operations[0].OperationID == operations[1].OperationID {
				t.Fatalf("both assignments used operation %q; want a fresh operation for the new assignment", operations[0].OperationID)
			}
			held, _ := store.record(t, testTenant, base.Key)
			if held.Released || held.Request != changed {
				t.Fatalf("record = %+v released=%t; want the new assignment held", held.Request, held.Released)
			}
		})
	}
}

// TestCoordinatorSettleRefusesUnreconciledNotStartedIntent proves a
// not_started intent that was never reconciled cannot be released: the
// coordinator cannot rule out that an earlier process already dispatched it.
// Reconcile is the evidence that makes the release safe, and then the identical
// Settle succeeds.
func TestCoordinatorSettleRefusesUnreconciledNotStartedIntent(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := assignedMergeRequest("exec-1", "action-1")

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	intent, created, err := coordinator.Begin(context.Background(), claim, request)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !created || intent.State != services.EffectNotStarted || intent.Reconciled {
		t.Fatalf("intent = %+v created=%t; want a fresh, unreconciled not_started intent", intent, created)
	}

	if err := coordinator.Settle(context.Background(), request, services.EffectNotStarted); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("Settle(not_started) before reconciliation error = %v; want ErrUncertain", err)
	}
	held, _ := store.record(t, testTenant, request.Key)
	if held.Released {
		t.Fatal("Settle released a not_started intent that was never reconciled")
	}
	pending, err := coordinator.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Request != request || pending[0].State != services.EffectNotStarted {
		t.Fatalf("pending = %+v; want the unreconciled not_started assignment still listed", pending)
	}

	reconciled, err := coordinator.Reconcile(context.Background(), claim)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reconciled.State != services.EffectNotStarted {
		t.Fatalf("reconciled state = %q; want not_started", reconciled.State)
	}
	if effects.executeCount() != 0 {
		t.Fatalf("execute calls = %d; reconciliation must never execute", effects.executeCount())
	}
	if effects.observeCount() != 1 {
		t.Fatalf("observe calls = %d; want exactly one reconciliation observation", effects.observeCount())
	}

	if err := coordinator.Settle(context.Background(), request, services.EffectNotStarted); err != nil {
		t.Fatalf("Settle(not_started) after reconciliation: %v", err)
	}
	released, _ := store.record(t, testTenant, request.Key)
	if !released.Released {
		t.Fatal("Settle did not release the reconciled not_started assignment")
	}
	pending, err = coordinator.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending after settle: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after settle = %+v; want none", pending)
	}
}
