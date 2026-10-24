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

var (
	_ events.PublicationStore = (*Store)(nil)
	_ events.StatsReader      = (*Store)(nil)
	_ events.TenantSource     = (*Store)(nil)
	_ events.SubjectResolver  = (*Store)(nil)
	_ events.Processor        = (*Store)(nil)
)

type outboxRow struct {
	pub     events.Publication
	typ     string
	payload []byte
	token   string
	bytes   int64
}

const outboxColumns = `o.event_id, o.event_type, o.payload, o.state, o.attempts, o.claim_token,
	o.lease_expires_at, o.next_attempt_at, o.published_at, o.last_failure_class, o.last_error`

func scanOutbox(row pgx.Row, tenant string, withToken bool) (outboxRow, error) {
	var r outboxRow
	var state string
	var lease, published *time.Time
	var class, lastErr *string
	dest := []any{&r.pub.EventID, &r.typ, &r.payload, &state, &r.pub.Attempts, &r.pub.ClaimToken, &lease, &r.pub.NextAttemptAt, &published, &class, &lastErr}
	if withToken {
		dest = append(dest, &r.token)
	}
	if err := row.Scan(dest...); err != nil {
		return r, err
	}
	r.pub.TenantID = tenant
	r.pub.State = events.PublicationState(state)
	if lease != nil {
		r.pub.LeaseExpiresAt = *lease
	}
	if published != nil {
		r.pub.PublishedAt = *published
	}
	if class != nil {
		r.pub.LastClass = events.FailureClass(*class)
	}
	if lastErr != nil {
		r.pub.LastError = *lastErr
	}
	r.bytes = int64(len(r.payload))
	return r, nil
}

// ClaimDue leases up to limit due outbox rows of one tenant under new fencing
// tokens. It uses FOR UPDATE SKIP LOCKED, so concurrent publishers receive
// disjoint rows, and commits before the caller touches the network: no SQL lock
// is held while publishing. Rows whose lease expired are reclaimed and their
// previous attempt is recorded as an unknown outcome.
func (s *Store) ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int) ([]events.Claim, error) {
	if limit < 1 {
		return nil, fmt.Errorf("%w: claim limit must be positive", events.ErrInvalidConfiguration)
	}
	var claims []events.Claim
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		claims = claims[:0]
		rows, err := tx.Query(ctx, `SELECT `+outboxColumns+`, s.subject_token
			FROM outbox o JOIN event_tenant_subjects s ON s.tenant_id = o.tenant_id
			WHERE o.tenant_id = $1
			  AND ((o.state = 'pending' AND o.next_attempt_at <= $2) OR (o.state = 'claimed' AND o.lease_expires_at <= $2))
			ORDER BY o.next_attempt_at, o.event_id
			LIMIT $3
			FOR UPDATE OF o SKIP LOCKED`, string(tenant), now, limit)
		if err != nil {
			return fmt.Errorf("events: select due outbox rows: %w", err)
		}
		var due []outboxRow
		for rows.Next() {
			r, err := scanOutbox(rows, string(tenant), true)
			if err != nil {
				rows.Close()
				return fmt.Errorf("events: scan outbox row: %w", err)
			}
			due = append(due, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("events: select due outbox rows: %w", err)
		}
		for _, r := range due {
			reclaimed := r.pub.State == events.PublicationClaimed
			token, err := r.pub.Claim(now, lease)
			if err != nil {
				return err // the query selected a non-due row: an invariant violation
			}
			if reclaimed {
				if _, err := tx.Exec(ctx, `UPDATE outbox_attempts SET outcome = 'transient', finished_at = $3, error = 'claim expired before completion; outcome unknown'
					WHERE tenant_id = $1 AND event_id = $2 AND outcome = 'claimed'`, string(tenant), r.pub.EventID, now); err != nil {
					return fmt.Errorf("events: close expired attempt: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'claimed', attempts = $3, claim_token = $4, lease_expires_at = $5
				WHERE tenant_id = $1 AND event_id = $2`, string(tenant), r.pub.EventID, r.pub.Attempts, token, r.pub.LeaseExpiresAt); err != nil {
				return fmt.Errorf("events: claim outbox row: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO outbox_attempts (tenant_id, event_id, attempt, claim_token, started_at) VALUES ($1, $2, $3, $4, $5)`,
				string(tenant), r.pub.EventID, r.pub.Attempts, token, now); err != nil {
				return fmt.Errorf("events: record attempt: %w", err)
			}
			claims = append(claims, events.Claim{TenantID: tenant, EventID: r.pub.EventID, EventType: r.typ, SubjectToken: r.token, Payload: r.payload, Token: token, Attempt: r.pub.Attempts, LeaseExpiresAt: r.pub.LeaseExpiresAt})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (s *Store) loadClaimed(ctx context.Context, tx pgx.Tx, c events.Claim) (outboxRow, error) {
	r, err := scanOutbox(tx.QueryRow(ctx, `SELECT `+outboxColumns+` FROM outbox o WHERE o.tenant_id = $1 AND o.event_id = $2 FOR UPDATE`, string(c.TenantID), c.EventID), string(c.TenantID), false)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: outbox event %s", events.ErrNotFound, c.EventID)
	}
	if err != nil {
		return r, fmt.Errorf("events: load outbox row: %w", err)
	}
	return r, nil
}

// Complete marks the event published iff c.Token is still the row's current
// fencing token, and releases its backlog accounting. A stale holder gets
// events.ErrStaleClaim and changes nothing.
func (s *Store) Complete(ctx context.Context, c events.Claim, now time.Time) error {
	return s.inTenant(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.loadClaimed(ctx, tx, c)
		if err != nil {
			return err
		}
		if err := r.pub.Complete(c.Token, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'published', published_at = $3, lease_expires_at = NULL WHERE tenant_id = $1 AND event_id = $2`,
			string(c.TenantID), c.EventID, now); err != nil {
			return fmt.Errorf("events: mark published: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_attempts SET outcome = 'published', finished_at = $4 WHERE tenant_id = $1 AND event_id = $2 AND attempt = $3`,
			string(c.TenantID), c.EventID, r.pub.Attempts, now); err != nil {
			return fmt.Errorf("events: close attempt: %w", err)
		}
		// Appenders never lock existing outbox rows, so locking the backlog
		// row after the outbox row cannot deadlock with them.
		if _, err := tx.Exec(ctx, `UPDATE event_backlog SET pending_count = pending_count - 1, pending_bytes = pending_bytes - $2 WHERE tenant_id = $1`,
			string(c.TenantID), r.bytes); err != nil {
			return fmt.Errorf("events: release backlog: %w", err)
		}
		return nil
	})
}

// Fail records a classified failure and schedules the retry iff c.Token is the
// current fencing token.
func (s *Store) Fail(ctx context.Context, c events.Claim, class events.FailureClass, message string, now time.Time) error {
	return s.inTenant(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := s.loadClaimed(ctx, tx, c)
		if err != nil {
			return err
		}
		if err := r.pub.Fail(c.Token, class, message, now, s.opts.Retry); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET state = 'pending', lease_expires_at = NULL, next_attempt_at = $3,
			last_failure_class = $4, last_failure_at = $5, last_error = $6 WHERE tenant_id = $1 AND event_id = $2`,
			string(c.TenantID), c.EventID, r.pub.NextAttemptAt, string(class), now, r.pub.LastError); err != nil {
			return fmt.Errorf("events: schedule retry: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox_attempts SET outcome = $4, finished_at = $5, error = $6 WHERE tenant_id = $1 AND event_id = $2 AND attempt = $3`,
			string(c.TenantID), c.EventID, r.pub.Attempts, string(class), now, r.pub.LastError); err != nil {
			return fmt.Errorf("events: record failure: %w", err)
		}
		return nil
	})
}

// PublicationStats reports one tenant's unpublished backlog, oldest age, next
// retry and last failure without exposing payloads.
func (s *Store) PublicationStats(ctx context.Context, tenant domain.TenantID, now time.Time) (events.PublicationStats, error) {
	var st events.PublicationStats
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var oldest, nextRetry, lastAt *time.Time
		var lastClass, lastErr *string
		err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE state = 'claimed'),
				COALESCE(sum(octet_length(payload)), 0)::bigint, min(created_at), COALESCE(sum(attempts), 0)::bigint,
				min(next_attempt_at) FILTER (WHERE state = 'pending')
			FROM outbox WHERE tenant_id = $1 AND state <> 'published'`, string(tenant)).
			Scan(&st.Pending, &st.Claimed, &st.BacklogBytes, &oldest, &st.Attempts, &nextRetry)
		if err != nil {
			return fmt.Errorf("events: publication stats: %w", err)
		}
		if oldest != nil {
			st.OldestUnpublishedAge = now.Sub(*oldest)
		}
		if nextRetry != nil {
			st.NextRetryAt = *nextRetry
		}
		err = tx.QueryRow(ctx, `SELECT last_failure_class, last_error, last_failure_at FROM outbox
			WHERE tenant_id = $1 AND state <> 'published' AND last_failure_at IS NOT NULL ORDER BY last_failure_at DESC LIMIT 1`, string(tenant)).
			Scan(&lastClass, &lastErr, &lastAt)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("events: publication stats: %w", err)
		}
		if lastClass != nil {
			st.LastClass = events.FailureClass(*lastClass)
			st.LastError = *lastErr
			st.LastFailureAt = *lastAt
		}
		return nil
	})
	return st, err
}
