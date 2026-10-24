package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/work"
)

// TxCancellationRequester records, inside the consumer's transaction, the
// durable request to cancel the current work runs of an item. It must be
// idempotent (a unique key on the item) and make no network call; the execution
// module's worker acts on the record. It never returns success without having
// recorded the request.
type TxCancellationRequester func(ctx context.Context, tx pgx.Tx, req work.CancelRequest) error

// CancellationConsumerName is the default durable consumer identity.
const CancellationConsumerName = "work-cancellation"

// CancellationBinding returns the event-bus consumer binding that turns a
// committed work_item.transitioned-to-cancelled event into a durable
// cancellation request. The consumer is current-state and records only a
// durable intent, so duplicate or reordered delivery repeats an idempotent
// request, and the periodic scan (work.ReconcileCancellations) covers a lost
// delivery. Replay of the binding is safe only if the requester is idempotent,
// which this contract requires.
func CancellationBinding(name string, request TxCancellationRequester) (eventspg.Binding, error) {
	if name == "" || request == nil {
		return eventspg.Binding{}, fmt.Errorf("%w: a consumer name and a requester are required", work.ErrInvalid)
	}
	spec := events.ConsumerSpec{
		Name: name, Mode: events.ModeCurrentState, Effect: events.EffectExternalIntent,
		Types: []string{eventsv1.TypeWorkItemTransitioned},
	}
	if err := spec.Validate(); err != nil {
		return eventspg.Binding{}, err
	}
	return eventspg.Binding{Spec: spec, Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		req, ok, err := work.CancellationFromEvent(env)
		if err != nil {
			return events.Permanent(err)
		}
		if !ok {
			return nil
		}
		return request(ctx, tx, req)
	}}, nil
}
