package execution

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
)

// CancelMutation records a cancellation request on the locked run. It is
// idempotent. Dispatch is refused immediately, live attempts lose authority and
// are queued for termination, and the run stays nonterminal until its attempts
// settle. actor, when set, is re-authorized through the transaction. The store
// ends the runtime grants of every attempt it revokes in the same transaction,
// so no cancelled attempt keeps a usable credential once the request commits.
func CancelMutation(reason Reason, actor *Actor) Mutation {
	return func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		if actor != nil {
			if err := env.Authorize(ctx, *actor, agg.Run.Scope, PermRunManage); err != nil {
				return Effects{}, normalizeAuth(err)
			}
		}
		agg.RequestCancel(reason, env.Now())
		after, err := advance(env, agg)
		return Effects{After: after}, err
	}
}

// Cancel idempotently requests cancellation of a run and returns its current
// state. The run is not terminal until admitted work is settled; poll it.
func (s *Service) Cancel(ctx context.Context, caller Caller, scope domain.Scope, id domain.RunID) (*Run, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	actor, err := s.authorize(ctx, caller, scope, PermRunManage)
	if err != nil {
		return nil, err
	}
	ctx = WithRequestID(WithActor(ctx, actor), caller.RequestID)
	agg, _, err := s.d.Store.Update(ctx, scope, id, UpdateOptions{}, CancelMutation(ReasonCancelled, &actor))
	if err != nil {
		return nil, err
	}
	s.terminateRevoked(ctx, agg)
	return agg.Run, nil
}

// RequestCancellation is the work module's cancellation requester: it records,
// idempotently, the item-scoped intent and cancels the item's runs. Repeating it
// is a no-op.
func (s *Service) RequestCancellation(ctx context.Context, c WorkCancellation) error {
	if err := c.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.WorkItemID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	return s.d.Store.RequestCancellation(ctx, c)
}

// ReconcileCancellations rescans a tenant's cancelled work items so a missing or
// reordered event can neither permit dispatch nor strand running work.
func (s *Service) ReconcileCancellations(ctx context.Context, tenant domain.TenantID) (int, error) {
	if s.d.CancelledWork == nil {
		return 0, ErrNotConfigured
	}
	var cursor CancelCursor
	total := 0
	for {
		items, err := s.d.CancelledWork.CancelledItems(ctx, tenant, cursor, s.o.Batch)
		if err != nil {
			return total, err
		}
		for _, it := range items {
			if err := s.RequestCancellation(ctx, it); err != nil {
				return total, err
			}
			total++
		}
		if len(items) < s.o.Batch {
			return total, nil
		}
		last := items[len(items)-1]
		cursor = CancelCursor{ProjectID: last.Scope.ProjectID, ID: last.WorkItemID}
	}
}
