package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Tx is the outbox side of one store transaction. The state mutation of the same
// transaction is done through the adapter's own transaction handle (for the
// PostgreSQL adapter, the same pgx transaction the Tx wraps).
type Tx interface {
	// Enqueue records the row. A row whose event id already exists is not
	// inserted again and reports inserted=false: the intent is idempotent in the
	// event id, so a replayed command cannot double an event.
	Enqueue(ctx context.Context, row Row) (inserted bool, err error)
}

// Store is the consumer-owned port of the outbox table. All methods are scoped
// by tenant except Tenants.
type Store interface {
	// Transact runs fn in one transaction: everything done through the Tx and the
	// adapter's own handle commits when fn returns nil and is rolled back when it
	// returns an error or the commit fails.
	Transact(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Tenants lists the tenants that have unpublished rows.
	Tenants(ctx context.Context) ([]domain.TenantID, error)
	// Pending lists up to limit unpublished rows of the tenant whose next
	// attempt is due at now, ordered by next attempt and Seq.
	Pending(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]Row, error)
	// Unpublished lists up to limit unpublished rows of the tenant created at or
	// before cutoff with Seq greater than after, ordered by Seq, regardless of
	// their next attempt.
	Unpublished(ctx context.Context, tenant domain.TenantID, cutoff time.Time, after int64, limit int) ([]Row, error)
	// MarkPublished records the acknowledged publication of the row.
	MarkPublished(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error
	// MarkFailed counts a failed attempt and defers the next one to next; the row
	// stays in the outbox.
	MarkFailed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error
}

// Writer commits a state change together with its events.
type Writer struct {
	Store Store
	// Wake is called after a successful commit that enqueued events (for
	// example Publisher.Kick); the publisher also polls, so it is optional.
	Wake func()
	Now  func() time.Time
}

// Commit runs mutate and enqueues the intents in one transaction, then wakes the
// publisher. Every intent is validated before the transaction opens, so a bad
// event fails the command instead of committing state without it. If mutate or
// an enqueue fails, or the commit fails, neither the state nor any event is
// recorded. mutate receives the Tx so adapters can bind their own handle to it.
func (w Writer) Commit(ctx context.Context, mutate func(ctx context.Context, tx Tx) error, intents ...Intent) error {
	if w.Store == nil {
		return errors.New("outbox: a writer needs a store")
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	rows := make([]Row, len(intents))
	for i, in := range intents {
		r, err := in.Row(now())
		if err != nil {
			return fmt.Errorf("outbox: intent %d: %w", i, err)
		}
		rows[i] = r
	}
	enqueued := false
	err := w.Store.Transact(ctx, func(ctx context.Context, tx Tx) error {
		enqueued = false
		if mutate != nil {
			if err := mutate(ctx, tx); err != nil {
				return err
			}
		}
		for _, r := range rows {
			inserted, err := tx.Enqueue(ctx, r)
			if err != nil {
				return fmt.Errorf("outbox: enqueue %s: %w", r.ID, err)
			}
			enqueued = enqueued || inserted
		}
		return nil
	})
	if err != nil {
		return err
	}
	if enqueued && w.Wake != nil {
		w.Wake()
	}
	return nil
}
