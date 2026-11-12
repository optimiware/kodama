package services_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/services"
)

func newCoordinator(t *testing.T, store services.CoordinationStore, effects services.MergeEffects, clock services.Clock, owner string) *services.Coordinator {
	t.Helper()
	return newCoordinatorNamed(t, store, effects, clock, owner, "op")
}

func newCoordinatorNamed(t *testing.T, store services.CoordinationStore, effects services.MergeEffects, clock services.Clock, owner, prefix string) *services.Coordinator {
	t.Helper()
	config := validCoordinatorConfig(store, effects, clock)
	config.Owner = owner
	config.NewOperationID = operationIDs(prefix)
	coordinator, err := services.NewCoordinator(config)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return coordinator
}

func appliedObservation(operation services.MergeOperation, output string) services.MergeObservation {
	return services.MergeObservation{
		State:     services.EffectApplied,
		Base:      operation.Base,
		Candidate: operation.Candidate,
		Output:    output,
	}
}

func TestCoordinatorClaimIncrementsGeneration(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	coordinator := newCoordinator(t, store, &fakeMergeEffects{log: log}, clock, "merge-worker-1")
	key := validMergeKey()

	claim, err := coordinator.Claim(context.Background(), key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := claim.Validate(); err != nil {
		t.Fatalf("Claim returned an invalid claim: %v", err)
	}
	if claim.Generation != 1 {
		t.Fatalf("generation = %d; want 1", claim.Generation)
	}
	if claim.OperationID != "op-1" {
		t.Fatalf("operation id = %q; want op-1", claim.OperationID)
	}
	if claim.Owner != "merge-worker-1" {
		t.Fatalf("owner = %q; want merge-worker-1", claim.Owner)
	}
	if !claim.ExpiresAt.Equal(testNow.Add(time.Minute)) {
		t.Fatalf("expiry = %v; want %v", claim.ExpiresAt, testNow.Add(time.Minute))
	}

	// The same owner re-claims its live record without a new generation.
	again, err := coordinator.Claim(context.Background(), key)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if again != claim {
		t.Fatalf("second Claim = %+v; want the live claim %+v", again, claim)
	}

	// A settled record whose lease elapsed starts a new operation.
	clock.Advance(2 * time.Minute)
	next, err := coordinator.Claim(context.Background(), key)
	if err != nil {
		t.Fatalf("third Claim: %v", err)
	}
	if next.Generation != 2 {
		t.Fatalf("generation = %d; want 2", next.Generation)
	}
	if next.OperationID == claim.OperationID {
		t.Fatalf("a settled record reused operation %q", claim.OperationID)
	}
}

func TestCoordinatorFileStoreRoundTripsClaimTimes(t *testing.T) {
	instants := map[string]func() time.Time{
		"monotonic reading": func() time.Time { return time.Now() },
		"non-utc location": func() time.Time {
			return time.Now().In(time.FixedZone("UTC+05:30", 5*60*60+30*60))
		},
	}
	for name, instant := range instants {
		t.Run(name, func(t *testing.T) {
			store, err := services.NewFileCoordinationStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewFileCoordinationStore: %v", err)
			}
			effects := &fakeMergeEffects{log: &callLog{}}
			coordinator := newCoordinator(t, store, effects, funcClock(instant), "merge-worker-1")
			request := validMergeRequest()

			claim, err := coordinator.Claim(context.Background(), request.Key)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			again, err := coordinator.Claim(context.Background(), request.Key)
			if err != nil {
				t.Fatalf("second Claim: %v", err)
			}
			if again != claim {
				t.Fatalf("live claim changed across a store round trip: %+v != %+v", again, claim)
			}
			if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
				t.Fatalf("Begin after a store round trip: %v", err)
			}
			result, err := coordinator.Execute(context.Background(), claim)
			if err != nil {
				t.Fatalf("Execute after a store round trip: %v", err)
			}
			if err := coordinator.Release(context.Background(), claim, result.State); err != nil {
				t.Fatalf("Release after a store round trip: %v", err)
			}
			stored, ok, err := store.Load(context.Background(), testTenant, request.Key)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !ok || !stored.Released || stored.State != services.EffectApplied {
				t.Fatalf("stored record = %+v; want a released applied merge", stored)
			}
		})
	}
}

func TestCoordinatorRefusesCompetingClaim(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	first := newCoordinator(t, store, effects, clock, "merge-worker-1")
	second := newCoordinatorNamed(t, store, effects, clock, "merge-worker-2", "second")
	key := validMergeKey()

	claim, err := first.Claim(context.Background(), key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := second.Claim(context.Background(), key); !errors.Is(err, services.ErrClaimed) {
		t.Fatalf("competing Claim error = %v; want ErrClaimed", err)
	}
	record, ok := store.record(t, testTenant, key)
	if !ok || record.Owner != claim.Owner {
		t.Fatalf("record owner = %q; want %q", record.Owner, claim.Owner)
	}

	// A competing coordinator must not start an effect either.
	if _, err := second.Merge(context.Background(), validMergeRequest()); !errors.Is(err, services.ErrClaimed) {
		t.Fatalf("competing Merge error = %v; want ErrClaimed", err)
	}
	if effects.executeCount() != 0 {
		t.Fatal("a competing coordinator executed a merge")
	}
}

func TestCoordinatorBeginPersistsIntentBeforeEffects(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	record, persisted, err := coordinator.Begin(context.Background(), claim, request)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if !persisted {
		t.Fatal("Begin did not report a new intent")
	}
	if record.State != services.EffectNotStarted || record.Request != request {
		t.Fatalf("record = %+v; want a not_started intent with the request", record)
	}
	if record.Reconciled {
		t.Fatal("a fresh intent is already reconciled")
	}
	intentIndex := log.indexOf("transition:not_started")
	claimIndex := log.indexOf("create-claim")
	if claimIndex < 0 || intentIndex < 0 || claimIndex > intentIndex {
		t.Fatalf("log = %v; want the claim before the intent", log.snapshot())
	}
	if effects.executeCount() != 0 {
		t.Fatal("Begin executed an effect")
	}

	// A repeated Begin with identical content is idempotent.
	repeated, persistedAgain, err := coordinator.Begin(context.Background(), claim, request)
	if err != nil {
		t.Fatalf("repeated Begin: %v", err)
	}
	if persistedAgain {
		t.Fatal("repeated Begin reported a new intent")
	}
	if repeated != record {
		t.Fatalf("repeated Begin = %+v; want %+v", repeated, record)
	}

	// Different content is a conflict and leaves the stored intent unchanged.
	changed := request
	changed.Candidate = "candidate-3333333"
	if _, _, err := coordinator.Begin(context.Background(), claim, changed); !errors.Is(err, services.ErrOperationConflict) {
		t.Fatalf("changed Begin error = %v; want ErrOperationConflict", err)
	}
	stored, ok := store.record(t, testTenant, request.Key)
	if !ok || stored.Request != request {
		t.Fatalf("stored request = %+v; want the original %+v", stored.Request, request)
	}
}

func TestCoordinatorExecutePersistsObservationBeforeReturn(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	result, err := coordinator.Execute(context.Background(), claim)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("result state = %q; want applied", result.State)
	}
	if result.ObservedBase != request.Base || result.ObservedCandidate != request.Candidate {
		t.Fatalf("result observations = %+v; want the request base and candidate", result)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("Execute returned an invalid result: %v", err)
	}

	intentIndex := log.indexOf("transition:not_started")
	executeIndex := log.indexOf("execute:op-1")
	appliedIndex := log.indexOf("transition:applied")
	if intentIndex < 0 || executeIndex < 0 || appliedIndex < 0 {
		t.Fatalf("log = %v; want intent, execute and applied transitions", log.snapshot())
	}
	if !(intentIndex < executeIndex && executeIndex < appliedIndex) {
		t.Fatalf("log = %v; want intent before effect and the observation persisted before return", log.snapshot())
	}

	stored, ok := store.record(t, testTenant, request.Key)
	if !ok || stored.State != services.EffectApplied || stored.Result != result {
		t.Fatalf("stored record = %+v; want the applied result %+v", stored, result)
	}
	if stored.Released {
		t.Fatal("Execute released the claim")
	}
}

func TestCoordinatorExecuteNeverReportsSuccessWhenPersistenceFails(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()
	boom := errors.New("store unavailable")

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	store.failPersisting(services.EffectApplied, boom)

	result, err := coordinator.Execute(context.Background(), claim)
	if !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v; want the persistence failure", err)
	}
	if result.State == services.EffectApplied {
		t.Fatal("Execute reported an applied result it could not persist")
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.State != services.EffectUncertain {
		t.Fatalf("stored state = %q; want the persisted in-flight marker", stored.State)
	}

	// A later reconciliation adopts the effect without executing it twice.
	store.clearFailures()
	effects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	reconciled, err := coordinator.Reconcile(context.Background(), claim)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reconciled.State != services.EffectApplied {
		t.Fatalf("reconciled state = %q; want applied", reconciled.State)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the single original effect", effects.executeCount())
	}
}

func TestCoordinatorCrashDuringExecuteLeavesInFlightMarker(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	boom := errors.New("process stopped before the observation was persisted")
	first := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := first.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := first.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// The effect settles, but the process stops before persisting the
	// observation: only the in-flight marker is durable.
	store.failPersisting(services.EffectApplied, boom)
	if _, err := first.Execute(context.Background(), claim); !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v; want the lost observation %v", err, boom)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want 1", effects.executeCount())
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.State != services.EffectUncertain {
		t.Fatalf("stored state = %q; want the in-flight uncertain marker", stored.State)
	}

	// A takeover after lease expiry must neither release nor repeat the effect.
	store.clearFailures()
	clock.Advance(2 * time.Minute)
	second := newCoordinatorNamed(t, store, effects, clock, "merge-worker-2", "second")
	taken, err := second.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("takeover Claim: %v", err)
	}
	if err := second.Release(context.Background(), taken, services.EffectNotStarted); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("takeover Release(not_started) error = %v; want ErrUncertain", err)
	}
	if _, err := second.Execute(context.Background(), taken); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("takeover Execute error = %v; want ErrUncertain", err)
	}
	if effects.executeCount() != 1 {
		t.Fatal("a takeover repeated the effect without observing it first")
	}

	// Only an observation can adopt the settled effect.
	effects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	result, err := second.Reconcile(context.Background(), taken)
	if err != nil {
		t.Fatalf("takeover Reconcile: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("reconciled state = %q; want applied", result.State)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the single original effect", effects.executeCount())
	}
}

func TestCoordinatorRunsNoEffectWhenInFlightMarkerCannotPersist(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	boom := errors.New("cannot persist in-flight marker")
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	store.failPersisting(services.EffectUncertain, boom)
	if _, err := coordinator.Execute(context.Background(), claim); !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v; want the marker persistence failure %v", err, boom)
	}
	if effects.executeCount() != 0 {
		t.Fatal("an effect ran before its in-flight marker was durable")
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.State != services.EffectNotStarted {
		t.Fatalf("stored state = %q; want the reconciled not_started intent", stored.State)
	}
	if !stored.Reconciled {
		t.Fatal("the pre-effect observation did not confirm the effect never started")
	}
}

func TestCoordinatorUncertainEffectIsNeverReleasedOnLeaseExpiry(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	effectErr := errors.New("connection reset during push")
	effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, effectErr
	}
	first := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := first.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := first.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	result, err := first.Execute(context.Background(), claim)
	if !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("Execute error = %v; want ErrUncertain", err)
	}
	if result.State != services.EffectUncertain {
		t.Fatalf("result state = %q; want uncertain", result.State)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("an uncertain result with only partial observations is invalid: %v", err)
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.Released {
		t.Fatal("an uncertain operation was released")
	}
	if err := first.Release(context.Background(), claim, services.EffectUncertain); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("Release(uncertain) error = %v; want ErrUncertain", err)
	}

	// The lease elapses, but expiry is not evidence about the effect.
	clock.Advance(2 * time.Minute)
	secondEffects := &fakeMergeEffects{log: log}
	secondEffects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, nil
	}
	second := newCoordinatorNamed(t, store, secondEffects, clock, "merge-worker-2", "second")
	taken, err := second.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("takeover Claim: %v", err)
	}
	if taken.OperationID != claim.OperationID {
		t.Fatalf("takeover operation = %q; want the unsettled %q", taken.OperationID, claim.OperationID)
	}
	if taken.Generation != claim.Generation+1 {
		t.Fatalf("takeover generation = %d; want %d", taken.Generation, claim.Generation+1)
	}
	takenRecord, ok := store.record(t, testTenant, request.Key)
	if !ok || takenRecord.State != services.EffectUncertain {
		t.Fatalf("takeover record = %+v; want the uncertain intent", takenRecord)
	}

	if _, err := second.Merge(context.Background(), request); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("takeover Merge error = %v; want ErrUncertain", err)
	}
	if secondEffects.executeCount() != 0 {
		t.Fatal("a takeover re-executed an uncertain operation")
	}
	if effects.executeCount() != 1 {
		t.Fatalf("total execute calls = %d; want exactly one effect", effects.executeCount())
	}
	after, _ := store.record(t, testTenant, request.Key)
	if after.Released {
		t.Fatal("lease expiry released an uncertain operation")
	}
	if err := second.Release(context.Background(), taken, services.EffectUncertain); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("takeover Release(uncertain) error = %v; want ErrUncertain", err)
	}
}

func TestCoordinatorLostResponseAfterEffectIsAdoptedWithoutReexecution(t *testing.T) {
	directory := t.TempDir()
	clock := &fakeClock{now: testNow}
	request := validMergeRequest()
	boom := errors.New("response lost after the effect settled")

	store, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	effects := &fakeMergeEffects{log: &callLog{}}
	effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	first := newCoordinator(t, &faultingStore{inner: store, failState: services.EffectApplied, failErr: boom}, effects, clock, "merge-worker-1")

	claim, err := first.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := first.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := first.Execute(context.Background(), claim); !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v; want the lost response", err)
	}
	if effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want 1", effects.executeCount())
	}

	// A restarted process opens the same durable directory.
	reopened, err := services.NewFileCoordinationStore(directory)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	clock.Advance(2 * time.Minute)
	restartedEffects := &fakeMergeEffects{log: &callLog{}}
	restartedEffects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	restarted := newCoordinatorNamed(t, reopened, restartedEffects, clock, "merge-worker-2", "restart")

	result, err := restarted.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("restarted Merge: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("restarted result state = %q; want applied", result.State)
	}
	if result.ObservedOutput != "Merge made by the 'ort' strategy." {
		t.Fatalf("observed output = %q; want the observed merge output", result.ObservedOutput)
	}
	if restartedEffects.executeCount() != 0 {
		t.Fatal("a restarted coordinator re-executed an effect that had already settled")
	}
	if err := restarted.Settle(context.Background(), request, result.State); err != nil {
		t.Fatalf("restarted Settle: %v", err)
	}
	stored, ok, err := reopened.Load(context.Background(), testTenant, request.Key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !ok || !stored.Released || stored.State != services.EffectApplied {
		t.Fatalf("stored record = %+v; want a released applied operation", stored)
	}
}

func TestCoordinatorReconcilesBeforeRetryAfterCrashBeforeEffect(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	firstEffects := &fakeMergeEffects{log: log}
	first := newCoordinator(t, store, firstEffects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := first.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, _, err := first.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// The process stops after persisting the intent and before any effect.

	clock.Advance(2 * time.Minute)
	retryEffects := &fakeMergeEffects{log: log}
	retryEffects.observeFunc = func(services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectNotStarted}, nil
	}
	retry := newCoordinatorNamed(t, store, retryEffects, clock, "merge-worker-2", "retry")

	result, err := retry.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("result state = %q; want applied", result.State)
	}
	if retryEffects.observeCount() != 1 {
		t.Fatalf("observe calls = %d; want a reconciliation before retry", retryEffects.observeCount())
	}
	if retryEffects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want the retried effect", retryEffects.executeCount())
	}
	observeIndex := log.indexOf("observe:op-1")
	executeIndex := log.indexOf("execute:op-1")
	if observeIndex < 0 || executeIndex < 0 || observeIndex > executeIndex {
		t.Fatalf("log = %v; want reconciliation before the retried effect", log.snapshot())
	}
}

func TestCoordinatorReconcileNeverExecutes(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	claim, err := coordinator.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := coordinator.Reconcile(context.Background(), claim); !errors.Is(err, services.ErrNoClaim) {
		t.Fatalf("Reconcile without an intent error = %v; want ErrNoClaim", err)
	}

	if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	effects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, nil
	}
	result, err := coordinator.Reconcile(context.Background(), claim)
	if !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("Reconcile error = %v; want ErrUncertain", err)
	}
	if result.State != services.EffectUncertain {
		t.Fatalf("result state = %q; want uncertain", result.State)
	}
	if effects.executeCount() != 0 {
		t.Fatal("Reconcile executed the effect")
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.State != services.EffectUncertain || stored.Released {
		t.Fatalf("stored record = %+v; want a held uncertain operation", stored)
	}
}

func TestCoordinatorReleaseRules(t *testing.T) {
	newFixture := func(t *testing.T) (*services.Coordinator, *fakeCoordinationStore, *fakeMergeEffects, services.Claim, services.MergeRequest) {
		t.Helper()
		log := &callLog{}
		store := newFakeCoordinationStore(log)
		effects := &fakeMergeEffects{log: log}
		coordinator := newCoordinator(t, store, effects, &fakeClock{now: testNow}, "merge-worker-1")
		request := validMergeRequest()
		claim, err := coordinator.Claim(context.Background(), request.Key)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if _, _, err := coordinator.Begin(context.Background(), claim, request); err != nil {
			t.Fatalf("Begin: %v", err)
		}
		return coordinator, store, effects, claim, request
	}

	t.Run("unreconciled intent refused", func(t *testing.T) {
		coordinator, store, _, claim, request := newFixture(t)
		if err := coordinator.Release(context.Background(), claim, services.EffectNotStarted); !errors.Is(err, services.ErrUncertain) {
			t.Fatalf("Release error = %v; want ErrUncertain", err)
		}
		stored, _ := store.record(t, testTenant, request.Key)
		if stored.Released {
			t.Fatal("an unreconciled intent was released")
		}
	})

	t.Run("mismatched evidence refused", func(t *testing.T) {
		coordinator, _, _, claim, _ := newFixture(t)
		if err := coordinator.Release(context.Background(), claim, services.EffectApplied); !errors.Is(err, services.ErrOperationConflict) {
			t.Fatalf("Release error = %v; want ErrOperationConflict", err)
		}
	})

	t.Run("unknown evidence refused", func(t *testing.T) {
		coordinator, _, _, claim, _ := newFixture(t)
		if err := coordinator.Release(context.Background(), claim, services.EffectState("maybe")); !errors.Is(err, services.ErrOperationConflict) {
			t.Fatalf("Release error = %v; want ErrOperationConflict", err)
		}
	})

	t.Run("reconciled not started released", func(t *testing.T) {
		coordinator, store, effects, claim, request := newFixture(t)
		effects.observeFunc = func(services.MergeOperation) (services.MergeObservation, error) {
			return services.MergeObservation{State: services.EffectNotStarted}, nil
		}
		if _, err := coordinator.Reconcile(context.Background(), claim); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		stored, _ := store.record(t, testTenant, request.Key)
		if !stored.Reconciled {
			t.Fatal("a reconciled not_started intent was not marked reconciled")
		}
		if err := coordinator.Release(context.Background(), claim, services.EffectNotStarted); err != nil {
			t.Fatalf("Release: %v", err)
		}
		released, _ := store.record(t, testTenant, request.Key)
		if !released.Released || released.State != services.EffectNotStarted {
			t.Fatalf("stored record = %+v; want a released not_started record", released)
		}
	})

	t.Run("applied released", func(t *testing.T) {
		coordinator, store, _, claim, request := newFixture(t)
		if _, err := coordinator.Execute(context.Background(), claim); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if err := coordinator.Release(context.Background(), claim, services.EffectApplied); err != nil {
			t.Fatalf("Release: %v", err)
		}
		released, _ := store.record(t, testTenant, request.Key)
		if !released.Released || released.State != services.EffectApplied {
			t.Fatalf("stored record = %+v; want a released applied record", released)
		}
	})

	t.Run("uncertain refused", func(t *testing.T) {
		coordinator, _, effects, claim, _ := newFixture(t)
		effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
			return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, errors.New("timeout")
		}
		if _, err := coordinator.Execute(context.Background(), claim); !errors.Is(err, services.ErrUncertain) {
			t.Fatalf("Execute error = %v; want ErrUncertain", err)
		}
		if err := coordinator.Release(context.Background(), claim, services.EffectUncertain); !errors.Is(err, services.ErrUncertain) {
			t.Fatalf("Release error = %v; want ErrUncertain", err)
		}
	})
}

func TestCoordinatorStaleClaimRefused(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	first := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	stale, err := first.Claim(context.Background(), request.Key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	clock.Advance(2 * time.Minute)
	second := newCoordinatorNamed(t, store, effects, clock, "merge-worker-2", "second")
	if _, err := second.Claim(context.Background(), request.Key); err != nil {
		t.Fatalf("takeover Claim: %v", err)
	}

	if _, _, err := first.Begin(context.Background(), stale, request); !errors.Is(err, services.ErrStaleClaim) {
		t.Fatalf("stale Begin error = %v; want ErrStaleClaim", err)
	}
	if _, err := first.Execute(context.Background(), stale); !errors.Is(err, services.ErrStaleClaim) {
		t.Fatalf("stale Execute error = %v; want ErrStaleClaim", err)
	}
	if _, err := first.Reconcile(context.Background(), stale); !errors.Is(err, services.ErrStaleClaim) {
		t.Fatalf("stale Reconcile error = %v; want ErrStaleClaim", err)
	}
	if err := first.Release(context.Background(), stale, services.EffectNotStarted); !errors.Is(err, services.ErrStaleClaim) {
		t.Fatalf("stale Release error = %v; want ErrStaleClaim", err)
	}

	forged := stale
	forged.Owner = "intruder"
	if err := first.Release(context.Background(), forged, services.EffectNotStarted); !errors.Is(err, services.ErrStaleClaim) {
		t.Fatalf("forged-owner Release error = %v; want ErrStaleClaim", err)
	}
	if effects.executeCount() != 0 {
		t.Fatal("a stale claim started an effect")
	}
}

func TestCoordinatorMergeReportsObservedOutputOnly(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "Merge made by the 'ort' strategy."), nil
	}
	coordinator := newCoordinator(t, store, effects, clock, "merge-worker-1")
	request := validMergeRequest()

	result, err := coordinator.Merge(context.Background(), request)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.State != services.EffectApplied {
		t.Fatalf("result state = %q; want applied", result.State)
	}
	if result.ObservedBase != request.Base || result.ObservedCandidate != request.Candidate {
		t.Fatalf("result = %+v; want the observed base and candidate", result)
	}
	if result.ObservedOutput != "Merge made by the 'ort' strategy." {
		t.Fatalf("observed output = %q; want the observed merge output", result.ObservedOutput)
	}
	stored, _ := store.record(t, testTenant, request.Key)
	if stored.Released {
		t.Fatal("Merge released the assignment before its request resolution")
	}
	if err := coordinator.Settle(context.Background(), request, result.State); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	released, _ := store.record(t, testTenant, request.Key)
	if !released.Released {
		t.Fatal("a settled merge was not released after its resolution")
	}

	// A merge result and the coordinator itself can never satisfy human review.
	assertNoFieldMatching(t, reflect.TypeFor[services.MergeResult](), "approval", "role", "reviewer", "authority")
	assertNoMethodMatching(t, reflect.TypeFor[*services.Coordinator](), "approve", "review", "authorize")
}

func TestCoordinatorCompetingClaimsElectOneOwner(t *testing.T) {
	log := &callLog{}
	store := newFakeCoordinationStore(log)
	clock := &fakeClock{now: testNow}
	effects := &fakeMergeEffects{log: log}
	first := newCoordinator(t, store, effects, clock, "merge-worker-1")
	second := newCoordinatorNamed(t, store, effects, clock, "merge-worker-2", "second")

	start := make(chan struct{})
	errorsByOwner := make([]error, 2)
	var wait sync.WaitGroup
	for index, coordinator := range []*services.Coordinator{first, second} {
		wait.Add(1)
		go func(index int, coordinator *services.Coordinator) {
			defer wait.Done()
			<-start
			_, errorsByOwner[index] = coordinator.Claim(context.Background(), validMergeKey())
		}(index, coordinator)
	}
	close(start)
	wait.Wait()

	winners, refused := 0, 0
	for _, err := range errorsByOwner {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, services.ErrClaimed):
			refused++
		default:
			t.Fatalf("unexpected Claim error: %v", err)
		}
	}
	if winners != 1 || refused != 1 {
		t.Fatalf("claims: %d winners, %d refused; want exactly one exclusive owner", winners, refused)
	}
	record, ok := store.record(t, testTenant, validMergeKey())
	if !ok || record.Generation != 1 {
		t.Fatalf("record = %+v; want the single generation-1 claim", record)
	}
}

func TestCoordinatorRefusesInvalidKey(t *testing.T) {
	store := newFakeCoordinationStore(&callLog{})
	coordinator := newCoordinator(t, store, &fakeMergeEffects{log: &callLog{}}, &fakeClock{now: testNow}, "merge-worker-1")
	if _, err := coordinator.Claim(context.Background(), services.MergeKey{}); err == nil {
		t.Fatal("Claim accepted an invalid merge key")
	}
	if store.createCount() != 0 {
		t.Fatal("an invalid merge key reached the store")
	}
}
