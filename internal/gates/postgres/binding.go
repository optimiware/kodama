package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/gates"
)

// DecisionConsumerName is the default inbox identity of the decision wakeup.
const DecisionConsumerName = "gates-decision"

// DecisionBinding is the event consumer that wakes a decided gate. A gate.decided
// event is only a notification: the handler takes nothing from the payload but
// the gate's identity, reloads the current gate, run and facts in the consumer's
// own transaction and applies the same explicit step transition reconciliation
// would. Duplicated and reordered deliveries repeat an idempotent
// state-based transition; a missing delivery is covered by the periodic scan
// (gates.Service.Reconcile). A decision that is not applicable now (expired,
// stale, reviewer revoked) is not an error and grants nothing: the step keeps
// waiting. Only an outage is retried.
func (s *Store) DecisionBinding(name string) (eventspg.Binding, error) {
	if name == "" {
		name = DecisionConsumerName
	}
	spec := events.ConsumerSpec{
		Name: name, Mode: events.ModeCurrentState, Effect: events.EffectLocal, IdempotentBeyondHorizon: true,
		Types: []string{eventsv1.TypeGateDecided},
	}
	if err := spec.Validate(); err != nil {
		return eventspg.Binding{}, err
	}
	return eventspg.Binding{Spec: spec, Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		if env.Resource.Type != "gate" {
			return events.Permanent(fmt.Errorf("gate.decided without a gate resource"))
		}
		id := gates.ID(env.Resource.ID)
		if err := id.Validate(); err != nil {
			return events.Permanent(err)
		}
		return s.wakeInTx(ctx, tx, domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID}, id)
	}}, nil
}

func (s *Store) wakeInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id gates.ID) error {
	if err := scope.Validate(); err != nil {
		return events.Permanent(err)
	}
	g, err := s.loadGate(ctx, tx, scope, id, lockNone)
	switch {
	case errors.Is(err, gates.ErrNotFound):
		return nil // nothing to wake; no authority is implied by an event
	case err != nil:
		return events.Transient(err)
	}
	switch g.Status {
	case gates.StatusApproved:
		_, err = s.completeInTx(ctx, tx, scope, g)
	case gates.StatusRejected:
		_, err = s.rejectInTx(ctx, tx, scope, g)
	default:
		return nil
	}
	err = mapExecError(err)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gates.ErrStaleSubject), errors.Is(err, gates.ErrConformance), errors.Is(err, gates.ErrReviewerAuthority),
		errors.Is(err, gates.ErrInvalidTransition), errors.Is(err, gates.ErrNotFound):
		// Not applicable now; reconciliation re-evaluates and records any
		// definitive staleness.
		return nil
	}
	return events.Transient(err)
}
