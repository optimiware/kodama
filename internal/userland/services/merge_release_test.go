package services_test

// Observe-only reconciliation of a held uncertain merge assignment.
//
// A merge effect whose outcome is unknown keeps the assignment held. When the
// request can no longer be answered because it was cancelled or its deadline
// elapsed, the adapter observes the effect once and releases the assignment
// only when that observation is conclusive. It never executes a second effect.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/services"
)

// uncertainMergeFixture builds an adapter whose single assignment already
// reached the uncertain state.
func uncertainMergeFixture(t *testing.T) *mergeTestFixture {
	t.Helper()
	fixture := newMergeTestFixture(t, singleMergeItem(validMergeInboxItem()), nil)
	fixture.effects.executeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain, Base: operation.Base}, errors.New("connection reset during the merge effect")
	}
	if err := fixture.adapter.RunOnce(context.Background()); !errors.Is(err, services.ErrUncertain) {
		t.Fatalf("first RunOnce error = %v; want ErrUncertain", err)
	}
	held, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if held.Released || held.State != services.EffectUncertain {
		t.Fatalf("record after the uncertain effect = %+v; want a held uncertain assignment", held)
	}
	// The request is no longer offered by the inbox; only the durable
	// assignment can converge it from now on.
	fixture.handler.inboxFunc = func(api.TaskInboxRequest) (api.TaskInboxResult, error) {
		return api.TaskInboxResult{}, nil
	}
	return fixture
}

// TestMergeAdapterReleasesUncertainAfterCancellation proves a cancelled request
// releases a held uncertain assignment after one observe-only reconciliation.
func TestMergeAdapterReleasesUncertainAfterCancellation(t *testing.T) {
	fixture := uncertainMergeFixture(t)
	observationsBefore := fixture.effects.observeCount()
	fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return mergeInspect("exec-1", "action-1", 1, "cancelled", ""), nil
	}
	fixture.effects.observeFunc = func(services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectNotStarted}, nil
	}

	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; reconciliation must never execute a second effect", fixture.effects.executeCount())
	}
	if observations := fixture.effects.observeCount() - observationsBefore; observations != 1 {
		t.Fatalf("observe calls = %d; want exactly one observe-only reconciliation", observations)
	}
	released, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if !released.Released || released.State != services.EffectNotStarted {
		t.Fatalf("record after cancellation = %+v; want the reconciled assignment released", released)
	}
	pending, err := fixture.coordinator.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %+v; want the reconciled assignment no longer held", pending)
	}
}

// TestMergeAdapterReleasesUncertainAfterDeadlineExpiry proves an elapsed request
// deadline releases a held uncertain assignment after observation. The observed
// applied effect is what the release records; cancellation does not fabricate a
// not_started outcome.
func TestMergeAdapterReleasesUncertainAfterDeadlineExpiry(t *testing.T) {
	fixture := uncertainMergeFixture(t)
	observationsBefore := fixture.effects.observeCount()
	expired := testNow.Add(-time.Second)
	fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		inspected := mergeInspect("exec-1", "action-1", 1, "running", "")
		inspected.Actions[0].Deadline = &expired
		return inspected, nil
	}
	fixture.effects.observeFunc = func(operation services.MergeOperation) (services.MergeObservation, error) {
		return appliedObservation(operation, "merged"), nil
	}

	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; an expired request must not run a second effect", fixture.effects.executeCount())
	}
	if observations := fixture.effects.observeCount() - observationsBefore; observations != 1 {
		t.Fatalf("observe calls = %d; want exactly one observation", observations)
	}
	released, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if !released.Released || released.State != services.EffectApplied {
		t.Fatalf("record after expiry = %+v; want the observed applied assignment released", released)
	}
	if released.Result.ObservedOutput != "merged" {
		t.Fatalf("observed output = %q; want the observed applied result", released.Result.ObservedOutput)
	}
}

// TestMergeAdapterKeepsUncertainWhenObservationIsInconclusive proves an
// inconclusive observation never releases the assignment.
func TestMergeAdapterKeepsUncertainWhenObservationIsInconclusive(t *testing.T) {
	fixture := uncertainMergeFixture(t)
	fixture.handler.inspectFunc = func(api.ExecutionInspectRequest) (api.ExecutionInspectResult, error) {
		return mergeInspect("exec-1", "action-1", 1, "cancelled", ""), nil
	}
	fixture.effects.observeFunc = func(services.MergeOperation) (services.MergeObservation, error) {
		return services.MergeObservation{State: services.EffectUncertain}, errors.New("remote repository unavailable")
	}

	if err := fixture.adapter.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	held, _ := fixture.store.record(t, testTenant, mergeTestKey())
	if held.Released || held.State != services.EffectUncertain {
		t.Fatalf("record after an inconclusive observation = %+v; want it still held", held)
	}
	if fixture.effects.executeCount() != 1 {
		t.Fatalf("execute calls = %d; want no second effect", fixture.effects.executeCount())
	}
}
