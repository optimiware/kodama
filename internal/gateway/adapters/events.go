package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/gateway"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
)

// EventsFrom adapts the event store's transactional append to the gateway's
// EventAppender. A full backlog becomes gateway.ErrBackpressure, which rolls the
// gateway's write back: usage and its event commit together or not at all.
func EventsFrom(store *eventspg.Store) gatewaypg.EventAppender {
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		_, err := store.Append(ctx, tx, env)
		if errors.Is(err, events.ErrBacklogFull) {
			return fmt.Errorf("%w: %v", gateway.ErrBackpressure, err)
		}
		return err
	}
}
