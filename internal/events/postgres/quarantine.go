package postgres

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

type quarantineRecord struct {
	// reopen is true for outcomes of the inbox transaction (a replayed event
	// failed again), which start a new quarantine cycle for a resolved entry.
	// A raw redelivery of already resolved poison keeps its resolution.
	reopen      bool
	tenant      domain.TenantID
	consumer    string
	deliveryKey string
	eventID     string
	eventType   string
	reason      string
	diagnostic  string
	identity    events.DeliveryIdentity
	payload     []byte
}

// insertQuarantine records (or refreshes) the quarantine entry of one delivery
// in the caller's tenant transaction. Redelivery of the same poison message
// updates the reason, diagnostic and delivery count and keeps the original
// payload and transport identity, so quarantine is idempotent.
func insertQuarantine(ctx context.Context, tx pgx.Tx, q quarantineRecord) (string, error) {
	payload, truncated := q.payload, false
	if len(payload) > maxQuarantinePayload {
		payload, truncated = payload[:maxQuarantinePayload], true
	}
	var stream, subject *string
	if q.identity.Stream != "" {
		s := q.identity.Stream
		stream = &s
	}
	if q.identity.Subject != "" {
		s := truncateString(q.identity.Subject, 512)
		subject = &s
	}
	var seq *int64
	if q.identity.StreamSeq != 0 {
		v := int64(q.identity.StreamSeq)
		seq = &v
	}
	// Display metadata is untrusted: PostgreSQL text cannot hold NUL or invalid
	// UTF-8, and a failing insert would be retried forever without evidence.
	// Unrepresentable values are omitted or replaced; the original bytes stay in
	// the bytea payload.
	eventType := q.eventType
	if !events.IsSafeText(eventType) {
		eventType = ""
	}
	if stream != nil {
		v := events.SafeText(*stream)
		stream = &v
	}
	if subject != nil {
		v := events.SafeText(*subject)
		subject = &v
	}
	var id string
	// A resolved entry is reopened (new cycle) only by an inbox outcome. A
	// resolved entry redelivered as raw poison keeps its terminal outcome, so a
	// discarded message does not demand a second decision.
	err := tx.QueryRow(ctx, `INSERT INTO event_quarantine
		(tenant_id, id, consumer, delivery_key, event_id, event_type, reason_code, diagnostic, stream, stream_seq, subject, delivery_count, payload, payload_truncated)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (tenant_id, consumer, delivery_key) DO UPDATE SET
			reason_code = CASE WHEN $15 OR event_quarantine.state = 'quarantined' THEN EXCLUDED.reason_code ELSE event_quarantine.reason_code END,
			diagnostic = CASE WHEN $15 OR event_quarantine.state = 'quarantined' THEN EXCLUDED.diagnostic ELSE event_quarantine.diagnostic END,
			delivery_count = GREATEST(event_quarantine.delivery_count, EXCLUDED.delivery_count),
			cycle = CASE WHEN $15 AND event_quarantine.state = 'resolved' THEN event_quarantine.cycle + 1 ELSE event_quarantine.cycle END,
			state = CASE WHEN $15 THEN 'quarantined' ELSE event_quarantine.state END,
			resolved_at = CASE WHEN $15 THEN NULL ELSE event_quarantine.resolved_at END,
			resolution = CASE WHEN $15 THEN NULL ELSE event_quarantine.resolution END,
			resolved_by = CASE WHEN $15 THEN NULL ELSE event_quarantine.resolved_by END,
			replay_id = CASE WHEN $15 AND event_quarantine.state = 'resolved' THEN NULL ELSE event_quarantine.replay_id END,
			replay_by = CASE WHEN $15 AND event_quarantine.state = 'resolved' THEN NULL ELSE event_quarantine.replay_by END
		RETURNING id`,
		string(q.tenant), newID("q_"), q.consumer, truncateString(q.deliveryKey, 200), nullable(truncateString(q.eventID, 128)), nullable(truncateString(eventType, 128)),
		q.reason, events.BoundedMessage(q.diagnostic), stream, seq, subject, q.identity.DeliveryNum, payload, truncated, q.reopen).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("events: quarantine delivery: %w", err)
	}
	return id, nil
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// QuarantineEntry is the operator-visible description of a quarantined
// delivery. It never includes the payload.
type QuarantineEntry struct {
	ID            string
	Consumer      string
	EventID       string
	EventType     string
	Reason        string
	Diagnostic    string
	Stream        string
	StreamSeq     int64
	Subject       string
	DeliveryCount int
	State         string
	QuarantinedAt time.Time
}

// ListQuarantine returns up to limit unresolved quarantine entries of the
// tenant, oldest first, optionally for one consumer.
func (s *Store) ListQuarantine(ctx context.Context, tenant domain.TenantID, consumer string, limit int) ([]QuarantineEntry, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 1000", events.ErrInvalidConfiguration)
	}
	var out []QuarantineEntry
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, `SELECT id, consumer, COALESCE(event_id, ''), COALESCE(event_type, ''), reason_code, diagnostic,
				COALESCE(stream, ''), COALESCE(stream_seq, 0), COALESCE(subject, ''), delivery_count, state, quarantined_at
			FROM event_quarantine WHERE tenant_id = $1 AND state = 'quarantined' AND ($2 = '' OR consumer = $2)
			ORDER BY quarantined_at, id LIMIT $3`, string(tenant), consumer, limit)
		if err != nil {
			return fmt.Errorf("events: list quarantine: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e QuarantineEntry
			if err := rows.Scan(&e.ID, &e.Consumer, &e.EventID, &e.EventType, &e.Reason, &e.Diagnostic, &e.Stream, &e.StreamSeq, &e.Subject, &e.DeliveryCount, &e.State, &e.QuarantinedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// DiscardQuarantine resolves one quarantined delivery without replay (for
// example an unsupported-version message that will never be processed). The
// operator and reason are audited in the same transaction. The audit identity
// includes the quarantine cycle: an entry that a later failure reopens (and the
// operator resolves again) gets a distinct audit row for each resolution. The authorizer
// decides, with the entry's consumer, whether the operator may do this.
func (s *Store) DiscardQuarantine(ctx context.Context, tenant domain.TenantID, id string, operator events.Operator, reason string) error {
	if err := domain.ProjectID(id).Validate(); err != nil {
		return fmt.Errorf("%w: quarantine id: %v", events.ErrNotFound, err)
	}
	return s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var consumer string
		var cycle int
		err := tx.QueryRow(ctx, `SELECT consumer, cycle FROM event_quarantine WHERE tenant_id = $1 AND id = $2 AND state = 'quarantined' FOR UPDATE`, string(tenant), id).Scan(&consumer, &cycle)
		if err != nil {
			return fmt.Errorf("%w: quarantine entry %s", events.ErrNotFound, id)
		}
		if err := s.authorizeOperator(ctx, tenant, consumer, operator, reason); err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `UPDATE event_quarantine SET state = 'resolved', resolved_at = $3, resolution = 'discarded', resolved_by = $4 WHERE tenant_id = $1 AND id = $2`,
			string(tenant), id, now, operator.ID); err != nil {
			return fmt.Errorf("events: discard quarantine: %w", err)
		}
		return insertAudit(ctx, tx, auditRow{tenant: tenant, id: fmt.Sprintf("%s_discarded_%d", id, cycle), operator: operator.ID, action: "event.quarantine_discarded",
			resourceType: "event_quarantine", resourceID: id, reason: reason, metadata: map[string]any{"consumer": consumer, "cycle": cycle}})
	})
}
