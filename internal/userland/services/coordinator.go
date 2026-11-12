package services

import (
	"context"
	"fmt"
	"sort"

	"github.com/optimiware/kodama/internal/id"
)

// NewCoordinator validates a complete coordinator configuration. It starts no
// process and opens no connection.
func NewCoordinator(config CoordinatorConfig) (*Coordinator, error) {
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return nil, fmt.Errorf("coordinator tenant %q is invalid: %w", config.TenantID, err)
	}
	if config.Owner == "" {
		return nil, fmt.Errorf("coordinator owner is empty")
	}
	if config.Lease <= 0 {
		return nil, fmt.Errorf("coordinator lease must be positive")
	}
	if config.Store == nil {
		return nil, fmt.Errorf("coordinator store is required")
	}
	if config.Effects == nil {
		return nil, fmt.Errorf("coordinator effects are required")
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("coordinator clock is required")
	}
	if config.NewOperationID == nil {
		return nil, fmt.Errorf("coordinator operation identity generator is required")
	}
	return &Coordinator{
		tenant:         config.TenantID,
		owner:          config.Owner,
		lease:          config.Lease,
		store:          config.Store,
		effects:        config.Effects,
		clock:          config.Clock,
		newOperationID: config.NewOperationID,
	}, nil
}

// Claim returns this coordinator's live claim for the key or creates the next
// generation through the store. A live claim owned by another coordinator is
// ErrClaimed.
func (coordinator *Coordinator) Claim(ctx context.Context, key MergeKey) (Claim, error) {
	if err := key.Validate(); err != nil {
		return Claim{}, err
	}
	now := normalizedTime(coordinator.clock.Now())
	record, ok, err := coordinator.store.Load(ctx, coordinator.tenant, key)
	if err != nil {
		return Claim{}, err
	}
	if ok && !record.Released && !record.Expired(now) {
		if record.Owner == coordinator.owner {
			return normalizeClaim(record.Claim), nil
		}
		return Claim{}, fmt.Errorf("%w: owned by %q", ErrClaimed, record.Owner)
	}
	operationID, err := coordinator.nextOperationID()
	if err != nil {
		return Claim{}, err
	}
	created, err := coordinator.store.CreateClaim(ctx, coordinator.tenant, key, coordinator.owner, operationID, now.Add(coordinator.lease), now)
	if err != nil {
		return Claim{}, err
	}
	return normalizeClaim(created.Claim), nil
}

// nextOperationID generates and validates one durable operation identity.
func (coordinator *Coordinator) nextOperationID() (id.OperationID, error) {
	operationID, err := coordinator.newOperationID()
	if err != nil {
		return "", err
	}
	if _, err := id.ParseOperationID(string(operationID)); err != nil {
		return "", fmt.Errorf("generated operation identity %q is invalid: %w", operationID, err)
	}
	return operationID, nil
}

// Begin validates the claim and persists the operation intent before any
// effect. The bool reports whether a new intent was persisted.
func (coordinator *Coordinator) Begin(ctx context.Context, claim Claim, request MergeRequest) (CoordinationRecord, bool, error) {
	record, err := coordinator.loadClaim(ctx, claim)
	if err != nil {
		return CoordinationRecord{}, false, err
	}
	if err := request.Validate(); err != nil {
		return CoordinationRecord{}, false, err
	}
	if request.Key != claim.Key {
		return CoordinationRecord{}, false, fmt.Errorf("%w: request key differs from the claim key", ErrOperationConflict)
	}
	if record.Request == request {
		return record, false, nil
	}
	if record.Request != (MergeRequest{}) {
		return CoordinationRecord{}, false, fmt.Errorf("%w: persisted intent differs", ErrOperationConflict)
	}
	if record.State != EffectNotStarted {
		return CoordinationRecord{}, false, fmt.Errorf("%w: record is already in state %q", ErrOperationConflict, record.State)
	}
	next := record
	next.Request = request
	next.State = EffectNotStarted
	next.Result = MergeResult{}
	next.Reconciled = false
	next.Released = false
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	if err := coordinator.store.Transition(ctx, coordinator.tenant, record, next); err != nil {
		return CoordinationRecord{}, false, err
	}
	return next, true, nil
}

// Execute loads the persisted intent, performs the effect once, and persists
// the observation before returning.
func (coordinator *Coordinator) Execute(ctx context.Context, claim Claim) (MergeResult, error) {
	record, err := coordinator.loadClaim(ctx, claim)
	if err != nil {
		return MergeResult{}, err
	}
	if record.Request == (MergeRequest{}) {
		return MergeResult{}, fmt.Errorf("%w: no persisted operation intent", ErrNoClaim)
	}
	switch record.State {
	case EffectApplied:
		return record.Result, nil
	case EffectUncertain:
		return record.Result, fmt.Errorf("%w: operation is uncertain", ErrUncertain)
	}
	// A not_started intent that was never reconciled may belong to an earlier
	// process that already dispatched it. Observe before dispatching: a fresh
	// intent is observed as not_started, while a settled effect is adopted
	// instead of repeated.
	if !record.Reconciled {
		reconciled, reconcileErr := coordinator.Reconcile(ctx, claim)
		if reconcileErr != nil {
			return reconciled, reconcileErr
		}
		if reconciled.State == EffectApplied {
			return reconciled, nil
		}
		record, err = coordinator.loadClaim(ctx, claim)
		if err != nil {
			return MergeResult{}, err
		}
		if record.State == EffectApplied {
			return record.Result, nil
		}
		if record.State != EffectNotStarted {
			return record.Result, fmt.Errorf("%w: operation is uncertain", ErrUncertain)
		}
	}
	// Persist the in-flight marker before the effect. A crash during the effect
	// then leaves an uncertain record, never a not_started intent that a
	// takeover could mistake for a safe release or repeat blindly.
	inFlight := record
	inFlight.State = EffectUncertain
	inFlight.Result = MergeResult{}
	inFlight.Reconciled = false
	inFlight.UpdatedAt = normalizedTime(coordinator.clock.Now())
	if err := coordinator.store.Transition(ctx, coordinator.tenant, record, inFlight); err != nil {
		// No effect runs when the marker cannot be persisted.
		return MergeResult{}, err
	}
	operation := coordinator.operation(record)
	observation, effectErr := coordinator.effects.Execute(ctx, operation)
	if err := observation.Validate(); err != nil {
		observation = MergeObservation{State: EffectUncertain}
	}
	target := observationTarget(observation, effectErr)
	result := observedResult(record.Claim.Key, target, observation)
	next := inFlight
	next.State = target
	next.Result = result
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	if err := coordinator.store.Transition(ctx, coordinator.tenant, inFlight, next); err != nil {
		return MergeResult{}, err
	}
	switch target {
	case EffectApplied:
		return result, nil
	case EffectNotStarted:
		return result, fmt.Errorf("%w: effect did not start", ErrUncertain)
	default:
		if effectErr != nil {
			return result, fmt.Errorf("%w: %v", ErrUncertain, effectErr)
		}
		return result, fmt.Errorf("%w: effect outcome is unknown", ErrUncertain)
	}
}

// Reconcile observes an existing intent and never executes. Observing
// not_started marks the record reconciled so a later release is safe.
func (coordinator *Coordinator) Reconcile(ctx context.Context, claim Claim) (MergeResult, error) {
	record, err := coordinator.loadClaim(ctx, claim)
	if err != nil {
		return MergeResult{}, err
	}
	if record.Request == (MergeRequest{}) {
		return MergeResult{}, fmt.Errorf("%w: no persisted operation intent", ErrNoClaim)
	}
	if record.State == EffectApplied {
		return record.Result, nil
	}
	operation := coordinator.operation(record)
	observation, observeErr := coordinator.effects.Observe(ctx, operation)
	if err := observation.Validate(); err != nil {
		observation = MergeObservation{State: EffectUncertain}
	}
	target := observationTarget(observation, nil)
	if target == EffectNotStarted && observeErr != nil {
		target = EffectUncertain
	}
	if target != EffectApplied && target != EffectNotStarted {
		target = EffectUncertain
	}
	result := observedResult(record.Claim.Key, target, observation)
	next := record
	next.State = target
	next.Result = result
	next.Reconciled = target == EffectNotStarted
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	if err := coordinator.store.Transition(ctx, coordinator.tenant, record, next); err != nil {
		return MergeResult{}, err
	}
	switch target {
	case EffectApplied:
		return result, nil
	case EffectNotStarted:
		return result, nil
	default:
		if observeErr != nil {
			return result, fmt.Errorf("%w: %v", ErrUncertain, observeErr)
		}
		return result, fmt.Errorf("%w: observed outcome is unknown", ErrUncertain)
	}
}

// Release persists a released record for settled evidence. An uncertain record
// is never released: lease expiry is not evidence about an effect.
func (coordinator *Coordinator) Release(ctx context.Context, claim Claim, evidence EffectState) error {
	record, err := coordinator.loadClaim(ctx, claim)
	if err != nil {
		return err
	}
	if record.State == EffectUncertain {
		return fmt.Errorf("%w: reconcile the uncertain operation before releasing it", ErrUncertain)
	}
	if !evidence.Valid() || !evidence.Releasable() {
		return fmt.Errorf("%w: evidence %q is not releasable", ErrOperationConflict, evidence)
	}
	if evidence != record.State {
		return fmt.Errorf("%w: evidence %q does not match state %q", ErrOperationConflict, evidence, record.State)
	}
	if record.State == EffectNotStarted && record.Request != (MergeRequest{}) && !record.Reconciled {
		return fmt.Errorf("%w: the persisted intent was never reconciled", ErrUncertain)
	}
	next := record
	next.Released = true
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	return coordinator.store.Transition(ctx, coordinator.tenant, record, next)
}

// Merge claims, begins, reconciles an already-persisted intent and only then
// executes. The intent and its result are keyed by the exact assignment, so a
// replay after a lost response adopts the durable result instead of repeating
// the effect. The assignment stays held until the caller reports that the
// request resolution converged through Settle; an uncertain operation is never
// released.
func (coordinator *Coordinator) Merge(ctx context.Context, request MergeRequest) (MergeResult, error) {
	if err := request.Validate(); err != nil {
		return MergeResult{}, err
	}
	// An identical replay of an already settled assignment is answered from
	// the durable record. This is the last line of defence for a stale inbox
	// page delivered after the request settled.
	settled, ok, err := coordinator.store.Load(ctx, coordinator.tenant, request.Key)
	if err != nil {
		return MergeResult{}, err
	}
	if ok && settled.Released && settled.Request == request {
		return settled.Result, nil
	}
	claim, err := coordinator.Claim(ctx, request.Key)
	if err != nil {
		return MergeResult{}, err
	}
	record, err := coordinator.loadClaim(ctx, claim)
	if err != nil {
		return MergeResult{}, err
	}
	if record.Request != (MergeRequest{}) {
		if record.Request != request {
			return MergeResult{}, fmt.Errorf("%w: persisted intent differs", ErrOperationConflict)
		}
		switch record.State {
		case EffectApplied:
			// The effect settled and the assignment is still held for its
			// resolution. Adopt it without releasing or repeating it.
			return record.Result, nil
		case EffectUncertain:
			result, reconcileErr := coordinator.Reconcile(ctx, claim)
			if reconcileErr != nil {
				return result, reconcileErr
			}
			if result.State == EffectApplied {
				return result, nil
			}
		}
	} else if _, _, err := coordinator.Begin(ctx, claim, request); err != nil {
		return MergeResult{}, err
	}
	result, err := coordinator.Execute(ctx, claim)
	if err != nil {
		return result, err
	}
	// The assignment is deliberately not released here: only the request
	// resolution proves this assignment is settled end to end.
	return result, nil
}

// Settle releases the exact durable assignment after its request resolution
// converged. It is idempotent for an identical replay and refuses an unknown,
// mismatched or unsettled assignment.
func (coordinator *Coordinator) Settle(ctx context.Context, request MergeRequest, evidence EffectState) error {
	if err := request.Validate(); err != nil {
		return err
	}
	record, ok, err := coordinator.store.Load(ctx, coordinator.tenant, request.Key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no durable merge assignment", ErrNoClaim)
	}
	if record.Request != request {
		return fmt.Errorf("%w: persisted assignment differs", ErrOperationConflict)
	}
	if record.Released {
		if record.State == evidence {
			return nil
		}
		return fmt.Errorf("%w: released assignment state %q differs from evidence %q", ErrOperationConflict, record.State, evidence)
	}
	if !evidence.Valid() || !evidence.Releasable() {
		return fmt.Errorf("%w: evidence %q is not releasable", ErrOperationConflict, evidence)
	}
	if evidence != record.State {
		return fmt.Errorf("%w: evidence %q does not match state %q", ErrOperationConflict, evidence, record.State)
	}
	if record.State == EffectNotStarted && !record.Reconciled {
		return fmt.Errorf("%w: the persisted intent was never reconciled", ErrUncertain)
	}
	next := record
	next.Released = true
	next.UpdatedAt = normalizedTime(coordinator.clock.Now())
	return coordinator.store.Transition(ctx, coordinator.tenant, record, next)
}

// Pending lists the assignments that still need settlement. A store without the
// optional enumeration capability reports none.
func (coordinator *Coordinator) Pending(ctx context.Context) ([]PendingMerge, error) {
	scanner, ok := coordinator.store.(CoordinationScanner)
	if !ok {
		return nil, nil
	}
	records, err := scanner.Scan(ctx, coordinator.tenant)
	if err != nil {
		return nil, err
	}
	pending := make([]PendingMerge, 0, len(records))
	for _, record := range records {
		if record.Released || record.Request.Key == (MergeKey{}) {
			continue
		}
		pending = append(pending, PendingMerge{Request: record.Request, State: record.State})
	}
	sort.Slice(pending, func(first, second int) bool {
		if pending[first].Request.Key != pending[second].Request.Key {
			return pending[first].Request.Key.Repository < pending[second].Request.Key.Repository ||
				(pending[first].Request.Key.Repository == pending[second].Request.Key.Repository &&
					pending[first].Request.Key.Ref < pending[second].Request.Key.Ref)
		}
		return pending[first].Request.Assignment.String() < pending[second].Request.Assignment.String()
	})
	return pending, nil
}

// loadClaim validates the claim against the durable record before any action.
func (coordinator *Coordinator) loadClaim(ctx context.Context, claim Claim) (CoordinationRecord, error) {
	if err := claim.Validate(); err != nil {
		return CoordinationRecord{}, err
	}
	if claim.Owner != coordinator.owner {
		return CoordinationRecord{}, fmt.Errorf("%w: claim owner %q is not this coordinator", ErrStaleClaim, claim.Owner)
	}
	if claim.Expired(coordinator.clock.Now()) {
		return CoordinationRecord{}, fmt.Errorf("%w: claim lease elapsed", ErrStaleClaim)
	}
	record, ok, err := coordinator.store.Load(ctx, coordinator.tenant, claim.Key)
	if err != nil {
		return CoordinationRecord{}, err
	}
	if !ok || record.Released || !claimsEqual(record.Claim, claim) {
		return CoordinationRecord{}, fmt.Errorf("%w: claim does not match the durable record", ErrStaleClaim)
	}
	return record, nil
}

// operation derives the durable effect identity from the persisted record.
func (coordinator *Coordinator) operation(record CoordinationRecord) MergeOperation {
	return MergeOperation{
		Key:         record.Claim.Key,
		OperationID: record.Claim.OperationID,
		Base:        record.Request.Base,
		Candidate:   record.Request.Candidate,
	}
}

// observationTarget classifies one observation. Only an applied observation is
// a settled effect; a not_started observation after an effect error is unknown.
func observationTarget(observation MergeObservation, effectErr error) EffectState {
	if err := observation.Validate(); err != nil {
		return EffectUncertain
	}
	switch observation.State {
	case EffectApplied:
		return EffectApplied
	case EffectUncertain:
		return EffectUncertain
	default:
		if effectErr != nil {
			return EffectUncertain
		}
		return EffectNotStarted
	}
}

// observedResult copies exactly what was observed; it never fabricates a base,
// candidate or output.
func observedResult(key MergeKey, state EffectState, observation MergeObservation) MergeResult {
	result := MergeResult{Key: key, State: state}
	if state == EffectNotStarted {
		return result
	}
	result.ObservedBase = observation.Base
	result.ObservedCandidate = observation.Candidate
	result.ObservedOutput = observation.Output
	return result
}

// compile-time assertions that the coordinator satisfies the declared port and
// the adapter's merge dependency.
var (
	_ MergeCoordinator = (*Coordinator)(nil)
	_ MergeRunner      = (*Coordinator)(nil)
)
