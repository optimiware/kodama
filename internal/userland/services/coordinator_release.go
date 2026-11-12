package services

// Observe-only release for a held uncertain merge assignment.
//
// Cancellation and deadline expiry end the request, but they are not evidence
// about an external merge. This path therefore observes the effect boundary
// once and releases the assignment only when that observation is conclusive.
// It never executes a second merge or push.

import (
	"context"
	"fmt"
)

// ReleaseObserved reconciles one held uncertain assignment by observation and
// releases it when the effect is conclusively applied or not started. An
// inconclusive observation leaves the assignment held and returns ErrUncertain.
func (coordinator *Coordinator) ReleaseObserved(ctx context.Context, request MergeRequest) (MergeResult, error) {
	if err := request.Validate(); err != nil {
		return MergeResult{}, err
	}
	record, ok, err := coordinator.store.Load(ctx, coordinator.tenant, request.Key)
	if err != nil {
		return MergeResult{}, err
	}
	if !ok {
		return MergeResult{}, fmt.Errorf("%w: no durable merge assignment", ErrNoClaim)
	}
	if record.Request != request {
		return MergeResult{}, fmt.Errorf("%w: persisted assignment differs", ErrOperationConflict)
	}
	if record.Released {
		return record.Result, nil
	}
	if record.State != EffectUncertain {
		return MergeResult{}, fmt.Errorf("%w: record state %q is not uncertain", ErrOperationConflict, record.State)
	}
	observation, observeErr := coordinator.effects.Observe(ctx, coordinator.operation(record))
	if err := observation.Validate(); err != nil {
		observation = MergeObservation{State: EffectUncertain}
		observeErr = err
	}
	if observeErr != nil {
		observation = MergeObservation{State: EffectUncertain}
	}
	if observation.State == EffectUncertain || !observation.State.Valid() {
		return MergeResult{Key: request.Key, State: EffectUncertain}, fmt.Errorf("%w: observed outcome is unknown", ErrUncertain)
	}
	target := observation.State
	result := observedResult(request.Key, target, observation)
	next := record
	next.State = target
	next.Result = result
	next.Reconciled = target == EffectNotStarted
	next.Released = true
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	if err := coordinator.store.Transition(ctx, coordinator.tenant, record, next); err != nil {
		return MergeResult{}, err
	}
	return result, nil
}
