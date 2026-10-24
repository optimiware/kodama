package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// PruneResult counts rows removed by one bounded pass.
type PruneResult struct {
	Outbox       int
	Inbox        int
	Observations int
	Quarantine   int
	Replays      int
}

// Prune removes, in bounded batches (RetentionPolicy.BatchSize per kind), the
// records whose independent retention has elapsed:
//
//   - published outbox rows and their attempt history (never before the broker
//     redelivery horizon has passed since publication),
//   - completed inbox entries (deduplication evidence), except while the event
//     is still unpublished, was published within the broker redelivery horizon,
//     or lies in an active replay request's unprocessed range,
//   - the oldest observations of each scope, advancing the scope's retention
//     floor in the same transaction,
//   - resolved quarantine records and completed replay requests.
//
// It never removes pending or claimed publications, pending or quarantined
// inbox entries, unresolved quarantine, or active replay requests: the database
// policies forbid those deletes outright. Observations still needed by an
// active replay request are kept, and nothing is deleted to satisfy a size cap.
// One kind failing does not stop the others; errors are joined.
func (s *Store) Prune(ctx context.Context, tenant domain.TenantID, now time.Time) (PruneResult, error) {
	var res PruneResult
	var errs []error
	r := s.opts.Retention
	batch := r.BatchSize
	run := func(name string, dst *int, fn func(ctx context.Context, tx pgx.Tx) (int, error)) {
		err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
			*dst, err = fn(ctx, tx)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", name, err))
		}
	}
	// A published row is the persisted record of when the broker last accepted
	// the event. It is kept for the broker's redelivery horizon at least, so the
	// inbox pruning below can rely on it (see the inbox predicate) even when the
	// configured outbox retention is shorter.
	brokerCutoff := now.Add(-r.BrokerRedelivery)
	outboxCutoff := now.Add(-max(r.PublishedOutbox, r.BrokerRedelivery))
	run("outbox", &res.Outbox, func(ctx context.Context, tx pgx.Tx) (int, error) {
		tag, err := tx.Exec(ctx, `DELETE FROM outbox o USING (
				SELECT event_id FROM outbox WHERE tenant_id = $1 AND state = 'published' AND published_at < $2 ORDER BY published_at LIMIT $3) d
			WHERE o.tenant_id = $1 AND o.event_id = d.event_id AND o.state = 'published'`, string(tenant), outboxCutoff, batch)
		return int(tag.RowsAffected()), err
	})
	// Deduplication evidence (a completed inbox entry) is forgotten only when
	// every way the event could still reach the consumer has closed:
	//
	//   * its completion is older than the inbox retention;
	//   * its outbox row is not unpublished: a pending or claimed event can still
	//     be (re)published, starting a new broker lifetime, however long the
	//     outage lasted, and a consumer may have completed it before the
	//     publisher marked it (broker copy, then repair) or never saw it;
	//   * its publication is older than the broker redelivery horizon (an absent
	//     row was pruned only after that horizon, see above);
	//   * no active replay request still has the event in its unprocessed range,
	//     so the replay finds the evidence instead of repeating the effect.
	//
	// The cutoffs are also persisted (event_dedup_boundary) in the deleting
	// transaction, so the guard below holds across restarts and configuration
	// changes: a larger Inbox later cannot bring back what was forgotten.
	//
	// Copies that still arrive afterwards (a publisher's reconnect buffer, a
	// stalled consumer or repair worker) are refused when their transaction
	// would create a new entry: see checkEvidenceHorizon, which mirrors this rule.
	run("inbox", &res.Inbox, func(ctx context.Context, tx pgx.Tx) (int, error) {
		// Serialized with replay admission: the pin below must be able to see
		// every request whose classification relied on this evidence.
		if err := lockReplayAdmission(ctx, tx, tenant); err != nil {
			return 0, err
		}
		// Persist what is about to be forgotten before forgetting it, in this
		// transaction: later configurations, restarts and other processes must
		// never treat the missing evidence as "never processed".
		if err := recordDedupBoundary(ctx, tx, tenant, now.Add(-r.Inbox), brokerCutoff, now); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM event_inbox i USING (
				SELECT c.consumer, c.event_id FROM event_inbox c
				WHERE c.tenant_id = $1 AND c.state = 'completed' AND c.completed_at < $2
				  AND NOT EXISTS (SELECT 1 FROM outbox o WHERE o.tenant_id = c.tenant_id AND o.event_id = c.event_id
				                  AND (o.state <> 'published' OR o.published_at >= $4))
				  AND NOT EXISTS (SELECT 1 FROM event_replay_requests r
				                  JOIN event_observations ob ON ob.tenant_id = r.tenant_id AND ob.scope_key = r.scope_key AND ob.seq BETWEEN r.next_seq AND r.to_seq
				                  WHERE r.tenant_id = c.tenant_id AND r.consumer = c.consumer AND r.state <> 'completed' AND ob.event_id = c.event_id)
				ORDER BY c.completed_at LIMIT $3) d
			WHERE i.tenant_id = $1 AND i.consumer = d.consumer AND i.event_id = d.event_id AND i.state = 'completed'`,
			string(tenant), now.Add(-r.Inbox), batch, brokerCutoff)
		return int(tag.RowsAffected()), err
	})
	run("quarantine", &res.Quarantine, func(ctx context.Context, tx pgx.Tx) (int, error) {
		tag, err := tx.Exec(ctx, `DELETE FROM event_quarantine q USING (
				SELECT id FROM event_quarantine WHERE tenant_id = $1 AND state = 'resolved' AND resolved_at < $2 ORDER BY resolved_at LIMIT $3) d
			WHERE q.tenant_id = $1 AND q.id = d.id AND q.state = 'resolved'`, string(tenant), now.Add(-r.ResolvedQuarantine), batch)
		return int(tag.RowsAffected()), err
	})
	run("replays", &res.Replays, func(ctx context.Context, tx pgx.Tx) (int, error) {
		tag, err := tx.Exec(ctx, `DELETE FROM event_replay_requests q USING (
				SELECT id FROM event_replay_requests WHERE tenant_id = $1 AND state = 'completed' AND completed_at < $2 ORDER BY completed_at LIMIT $3) d
			WHERE q.tenant_id = $1 AND q.id = d.id AND q.state = 'completed'`, string(tenant), now.Add(-r.CompletedReplay), batch)
		return int(tag.RowsAffected()), err
	})

	var scopes []string
	if err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT scope_key FROM event_scopes WHERE tenant_id = $1 ORDER BY scope_key`, string(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return err
			}
			scopes = append(scopes, k)
		}
		return rows.Err()
	}); err != nil {
		errs = append(errs, fmt.Errorf("prune observations: %w", err))
		return res, errors.Join(errs...)
	}
	for _, key := range scopes {
		var n int
		err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
			n, err = s.pruneObservations(ctx, tx, tenant, key, now.Add(-r.Observation), batch)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("prune observations of scope %q: %w", key, err))
			continue
		}
		res.Observations += n
	}
	return res, errors.Join(errs...)
}

// lockReplayAdmission takes the tenant's replay-admission lock until the end of
// the transaction. Replay admission holds it from classification to commit, and
// inbox pruning holds it for its delete, so a request is either committed (and
// pins its evidence) before the delete's statement starts, or classified after
// the delete committed. Inbox rows have no scope row to lock, hence an advisory
// lock.
func lockReplayAdmission(ctx context.Context, tx pgx.Tx, tenant domain.TenantID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kodama.events.replay-admission:' || $1::text, 0))`, string(tenant)); err != nil {
		return fmt.Errorf("events: lock replay admission: %w", err)
	}
	return nil
}

// lockAdmissionShared takes the same lock in shared mode: appends and event
// processing coexist, but not with inbox pruning (which advances the durable
// deduplication boundary), replay admission or fencing. Every transaction that
// takes it does so before any row lock, so the order is the same everywhere:
// admission lock, then request/inbox/backlog/scope rows.
func lockAdmissionShared(ctx context.Context, tx pgx.Tx, tenant domain.TenantID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('kodama.events.replay-admission:' || $1::text, 0))`, string(tenant)); err != nil {
		return fmt.Errorf("events: lock append admission: %w", err)
	}
	return nil
}

// pruneObservations deletes the oldest contiguous observations of one scope
// older than cutoff and raises the scope floor to the last one deleted, in one
// transaction. The scope row is locked first (the same row appends lock), so
// the floor and the deletion are atomic with respect to appenders and readers
// see either the old or the new state. The contiguous-prefix rule keeps the
// floor meaningful even though recorded_at is not strictly monotonic in
// sequence. A replay request's unprocessed range is never deleted.
func (s *Store) pruneObservations(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, scopeKey string, cutoff time.Time, batch int) (int, error) {
	var floor int64
	err := tx.QueryRow(ctx, `SELECT floor_seq FROM event_scopes WHERE tenant_id = $1 AND scope_key = $2 FOR UPDATE`, string(tenant), scopeKey).Scan(&floor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lock scope: %w", err)
	}
	// Active replay requests pin the observations they have yet to process.
	var pinned *int64
	if err := tx.QueryRow(ctx, `SELECT min(next_seq) FROM event_replay_requests WHERE tenant_id = $1 AND scope_key = $2 AND state <> 'completed'`,
		string(tenant), scopeKey).Scan(&pinned); err != nil {
		return 0, fmt.Errorf("read active replays: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT seq, recorded_at FROM event_observations WHERE tenant_id = $1 AND scope_key = $2 AND seq > $3 ORDER BY seq LIMIT $4`,
		string(tenant), scopeKey, floor, batch)
	if err != nil {
		return 0, fmt.Errorf("select observations: %w", err)
	}
	var last int64
	stop := false
	for rows.Next() {
		var seq int64
		var at time.Time
		if err := rows.Scan(&seq, &at); err != nil {
			rows.Close()
			return 0, err
		}
		if stop {
			continue
		}
		if !at.Before(cutoff) || (pinned != nil && seq >= *pinned) {
			stop = true
			continue
		}
		last = seq
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if last == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `DELETE FROM event_observations WHERE tenant_id = $1 AND scope_key = $2 AND seq > $3 AND seq <= $4`, string(tenant), scopeKey, floor, last)
	if err != nil {
		return 0, fmt.Errorf("delete observations: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE event_scopes SET floor_seq = $3 WHERE tenant_id = $1 AND scope_key = $2`, string(tenant), scopeKey, last); err != nil {
		return 0, fmt.Errorf("advance floor: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PruneReconciliation adapts Prune to the Reconciler so cleanup runs on the
// periodic pass.
func (s *Store) PruneReconciliation() events.Reconciliation {
	return events.Reconciliation{Name: "event-retention", Run: func(ctx context.Context, tenant domain.TenantID) error {
		_, err := s.Prune(ctx, tenant, s.now())
		return err
	}}
}
