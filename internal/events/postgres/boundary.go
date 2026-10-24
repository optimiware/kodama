package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
)

// dedupBoundary is the durable record of what retention may already have
// forgotten for one tenant (table event_dedup_boundary). Completed inbox
// entries with a completion time before inboxBefore may have been deleted, and
// a publication older than brokerBefore no longer protected its event's
// evidence. The values only move forward.
//
// Absence of an inbox entry proves "never processed" only for events this
// boundary does not cover. The current process's RetentionPolicy is not that
// proof: raising a retention setting cannot restore deleted evidence, so every
// decision uses the later of the durable boundary and the policy's own cutoff.
type dedupBoundary struct {
	inboxBefore  time.Time
	brokerBefore time.Time
}

// loadDedupBoundary reads the tenant's durable boundary and merges it with the
// cutoffs the current policy implies at now. Call it in the transaction that
// acts on the verdict, after the statement that holds the evidence in place.
func (s *Store) loadDedupBoundary(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, now time.Time) (dedupBoundary, error) {
	b := dedupBoundary{
		inboxBefore:  now.Add(-s.opts.Retention.Inbox),
		brokerBefore: now.Add(-s.opts.Retention.BrokerRedelivery),
	}
	var inbox, broker time.Time
	switch err := tx.QueryRow(ctx, `SELECT inbox_before, broker_before FROM event_dedup_boundary WHERE tenant_id = $1`, string(tenant)).Scan(&inbox, &broker); {
	case err == nil:
		if inbox.After(b.inboxBefore) {
			b.inboxBefore = inbox
		}
		if broker.After(b.brokerBefore) {
			b.brokerBefore = broker
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return b, fmt.Errorf("events: read deduplication boundary: %w", err)
	}
	return b, nil
}

// recordDedupBoundary advances the durable boundary. Pruning calls it in the
// transaction that deletes completed inbox entries, before the delete, so the
// boundary is committed with (never after) the forgetting it describes.
func recordDedupBoundary(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, inboxBefore, brokerBefore, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO event_dedup_boundary AS d (tenant_id, inbox_before, broker_before, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET inbox_before = GREATEST(d.inbox_before, EXCLUDED.inbox_before),
			broker_before = GREATEST(d.broker_before, EXCLUDED.broker_before), updated_at = EXCLUDED.updated_at`,
		string(tenant), inboxBefore, brokerBefore, now)
	if err != nil {
		return fmt.Errorf("events: record deduplication boundary: %w", err)
	}
	return nil
}
