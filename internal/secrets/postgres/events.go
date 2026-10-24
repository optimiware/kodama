package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/secrets"
)

// EventsFrom adapts the durable event bus's transaction-local append to an
// EventAppender. A full backlog becomes secrets.ErrBackpressure, so the
// producer's business change rolls back; broker availability is never required.
func EventsFrom(store *eventspg.Store) EventAppender {
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		_, err := store.Append(ctx, tx, env)
		if errors.Is(err, events.ErrBacklogFull) {
			return fmt.Errorf("%w: %v", secrets.ErrBackpressure, err)
		}
		return err
	}
}
