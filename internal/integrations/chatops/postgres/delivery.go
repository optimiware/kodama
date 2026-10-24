package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

const deliveryColumns = `tenant_id, project_id, id, destination_id, event_id, event_type, binding_revision, payload_digest, state, attempts, cycle_attempts,
replays, next_attempt_at, claim_token, lease_expires_at, last_class, last_uncertain, last_status, receipt, delivered_at, failed_reason, version, created_at, updated_at`

// deliveryColumnsWithPayload adds the stored bytes, which only the worker needs.
const deliveryColumnsWithPayload = deliveryColumns + `, payload`

func scanDelivery(row pgx.Row, withPayload bool) (chatops.Delivery, error) {
	var d chatops.Delivery
	var tenant, project, id, destination, state, class, reason string
	var lease, delivered *time.Time
	dest := []any{&tenant, &project, &id, &destination, &d.EventID, &d.EventType, &d.BindingRevision, &d.PayloadDigest, &state, &d.Attempts,
		&d.CycleAttempts, &d.Replays, &d.NextAttemptAt, &d.ClaimToken, &lease, &class, &d.LastUncertain, &d.LastStatus, &d.Receipt, &delivered,
		&reason, &d.Version, &d.CreatedAt, &d.UpdatedAt}
	if withPayload {
		dest = append(dest, &d.Payload)
	}
	if err := row.Scan(dest...); err != nil {
		return chatops.Delivery{}, err
	}
	d.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	d.ID, d.DestinationID, d.State = chatops.DeliveryID(id), chatops.DestinationID(destination), chatops.DeliveryState(state)
	d.LastClass, d.FailedReason = chatops.FailureClass(class), chatops.FailedReason(reason)
	if lease != nil {
		d.LeaseExpiresAt = lease.UTC()
	}
	if delivered != nil {
		d.DeliveredAt = delivered.UTC()
	}
	d.NextAttemptAt, d.CreatedAt, d.UpdatedAt = d.NextAttemptAt.UTC(), d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	return d, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// saveDelivery writes the mutable columns of d, conditioned on the version the
// caller read under lock: a row that changed is never overwritten.
func saveDelivery(ctx context.Context, tx pgx.Tx, d chatops.Delivery, fromVersion int64) error {
	tag, err := tx.Exec(ctx, `UPDATE chatops_deliveries SET state = $4, attempts = $5, cycle_attempts = $6, replays = $7, next_attempt_at = $8,
claim_token = $9, lease_expires_at = $10, last_class = $11, last_uncertain = $12, last_status = $13, receipt = $14, delivered_at = $15,
failed_reason = $16, binding_revision = $17, version = $18, updated_at = $19
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $20`,
		string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.ID), string(d.State), d.Attempts, d.CycleAttempts, d.Replays, d.NextAttemptAt.UTC(),
		d.ClaimToken, nullTime(d.LeaseExpiresAt), string(d.LastClass), d.LastUncertain, d.LastStatus, d.Receipt, nullTime(d.DeliveredAt),
		string(d.FailedReason), d.BindingRevision, d.Version, d.UpdatedAt.UTC(), fromVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return chatops.Fail(chatops.ErrVersionConflict, nil, "the delivery changed")
	}
	return nil
}

// ---- management reads ----------------------------------------------------------

// ListDeliveries implements chatops.Store: newest first with a keyset position.
func (s *Store) ListDeliveries(ctx context.Context, c chatops.Caller, scope domain.Scope, f chatops.DeliveryFilter, after chatops.DeliveryPosition, limit int) (out []chatops.Delivery, err error) {
	err = s.protected(ctx, c, scope, chatops.PermRead, false, func(ctx context.Context, tx pgx.Tx) error {
		var afterTime *time.Time
		if !after.IsZero() {
			t := after.CreatedAt.UTC()
			afterTime = &t
		}
		rows, e := tx.Query(ctx, `SELECT `+deliveryColumns+` FROM chatops_deliveries
WHERE tenant_id = $1 AND project_id = $2
  AND ($3 = '' OR state = $3) AND ($4 = '' OR destination_id = $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5::timestamptz, $6::text))
ORDER BY created_at DESC, id DESC LIMIT $7`,
			string(scope.TenantID), string(scope.ProjectID), string(f.State), string(f.Destination), afterTime, string(after.ID), limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			d, e := scanDelivery(rows, false)
			if e != nil {
				return e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// GetDelivery implements chatops.Store.
func (s *Store) GetDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID) (out chatops.DeliveryView, err error) {
	err = s.protected(ctx, c, scope, chatops.PermRead, false, func(ctx context.Context, tx pgx.Tx) error {
		d, e := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM chatops_deliveries WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)), false)
		if errors.Is(e, pgx.ErrNoRows) {
			return chatops.Fail(chatops.ErrNotFound, e, "not found")
		}
		if e != nil {
			return e
		}
		out.Delivery = d
		if e := tx.QueryRow(ctx, `SELECT name FROM chatops_destinations WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(d.DestinationID)).Scan(&out.DestinationName); e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		out.Attempts, e = loadAttempts(ctx, tx, scope, id)
		return e
	})
	return out, err
}

func loadAttempts(ctx context.Context, tx pgx.Tx, scope domain.Scope, id chatops.DeliveryID) ([]chatops.Attempt, error) {
	rows, err := tx.Query(ctx, `SELECT attempt, claim_token, replay, started_at, finished_at, result, class, uncertain, http_status, receipt
FROM chatops_delivery_attempts WHERE tenant_id = $1 AND project_id = $2 AND delivery_id = $3 ORDER BY attempt`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatops.Attempt
	for rows.Next() {
		var a chatops.Attempt
		var finished *time.Time
		var class string
		if err := rows.Scan(&a.Number, &a.ClaimToken, &a.Replay, &a.StartedAt, &finished, &a.Result, &class, &a.Uncertain, &a.HTTPStatus, &a.Receipt); err != nil {
			return nil, err
		}
		a.Class, a.StartedAt = chatops.FailureClass(class), a.StartedAt.UTC()
		if finished != nil {
			a.FinishedAt = finished.UTC()
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReplayDelivery implements chatops.Store. The delivery row and the destination
// are read under lock, the replay is applied by the entity under the destination's
// current binding, and the row, the replay metadata and the audit entry commit
// together. A concurrent duplicate request finds the delivery pending (the
// transition is refused) or at a newer version (412): one replay, one history.
func (s *Store) ReplayDelivery(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DeliveryID, expected int64, reason string, now time.Time) (out chatops.Delivery, err error) {
	err = s.protected(ctx, c, scope, chatops.PermSettings, true, func(ctx context.Context, tx pgx.Tx) error {
		d, e := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM chatops_deliveries
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), string(id)), false)
		if errors.Is(e, pgx.ErrNoRows) {
			return chatops.Fail(chatops.ErrNotFound, e, "not found")
		}
		if e != nil {
			return e
		}
		if d.Version != expected {
			return chatops.Fail(chatops.ErrVersionConflict, nil, "the delivery changed")
		}
		var binding int64
		if e := tx.QueryRow(ctx, `SELECT binding_revision FROM chatops_destinations WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
			string(scope.TenantID), string(scope.ProjectID), string(d.DestinationID)).Scan(&binding); e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return chatops.Fail(chatops.ErrNotFound, e, "not found")
			}
			return e
		}
		before := d
		if e := d.Replay(now, reason, binding); e != nil {
			return e
		}
		if e := saveDelivery(ctx, tx, d, before.Version); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `INSERT INTO chatops_replays (tenant_id, project_id, delivery_id, actor_id, request_id, reason, from_version, to_version,
from_binding, to_binding, occurred_at) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8, $9, $10, $11)`,
			string(scope.TenantID), string(scope.ProjectID), string(id), c.Actor.ID, c.RequestID, reason, before.Version, d.Version,
			before.BindingRevision, d.BindingRevision, now.UTC()); e != nil {
			return e
		}
		out = d
		return audit(ctx, tx, callerAudit(c, scope, now, "chatops.delivery.replay", "chatops_delivery", string(id), d.Version, reason,
			map[string]any{"fromVersion": before.Version, "fromBinding": before.BindingRevision, "toBinding": d.BindingRevision,
				"previousClass": string(before.LastClass), "attempts": before.Attempts}))
	})
	if err != nil {
		return chatops.Delivery{}, err
	}
	return out, nil
}

// ---- worker ----------------------------------------------------------------------

// ClaimDue implements chatops.WorkStore. One transaction selects due rows
// (FOR UPDATE SKIP LOCKED, so competing workers never receive the same row),
// verifies each destination's current eligibility, applies the claim by the
// entity and records the attempt before the transaction commits. The caller
// calls the receiver only afterwards.
func (s *Store) ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int, p chatops.RetryPolicy) (chatops.ClaimBatch, error) {
	var batch chatops.ClaimBatch
	err := s.system(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		batch = chatops.ClaimBatch{}
		var fenced bool
		switch err := tx.QueryRow(ctx, `SELECT fenced FROM chatops_delivery_fences WHERE tenant_id = $1`, string(tenant)).Scan(&fenced); {
		case err == nil && fenced:
			return nil // the restore fence: nothing is claimed until it is cleared
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		// Rows of a disabled destination are not selected at all, so a large
		// backlog behind a disabled destination cannot starve the others of
		// their batch slots; admit still rechecks the destination under lock.
		rows, err := tx.Query(ctx, `SELECT `+deliveryColumnsWithPayload+` FROM chatops_deliveries
WHERE tenant_id = $1 AND ((state = 'pending' AND next_attempt_at <= $2) OR (state = 'claimed' AND lease_expires_at <= $2))
  AND EXISTS (SELECT 1 FROM chatops_destinations x WHERE x.tenant_id = chatops_deliveries.tenant_id AND x.project_id = chatops_deliveries.project_id
              AND x.id = chatops_deliveries.destination_id AND x.enabled)
ORDER BY CASE state WHEN 'pending' THEN next_attempt_at ELSE lease_expires_at END, id
LIMIT $3 FOR UPDATE OF chatops_deliveries SKIP LOCKED`, string(tenant), now.UTC(), limit)
		if err != nil {
			return err
		}
		var due []chatops.Delivery
		for rows.Next() {
			d, err := scanDelivery(rows, true)
			if err != nil {
				rows.Close()
				return err
			}
			due = append(due, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range due {
			claim, kind, err := s.admit(ctx, tx, d, now, lease, p)
			if err != nil {
				return err
			}
			switch kind {
			case admitClaimed:
				batch.Claims = append(batch.Claims, claim)
			case admitRefused:
				batch.Refused++
			case admitExhausted:
				batch.Exhausted++
			}
		}
		return nil
	})
	if err != nil {
		return chatops.ClaimBatch{}, err
	}
	return batch, nil
}

type admission int

const (
	admitSkipped admission = iota
	admitClaimed
	admitRefused
	admitExhausted
)

// admit decides one due row. A disabled destination skips the row (it stays
// where it is and resumes when the destination is enabled); a changed binding is
// refused, not redirected; otherwise the entity claims it and the attempt row
// is inserted.
func (s *Store) admit(ctx context.Context, tx pgx.Tx, d chatops.Delivery, now time.Time, lease time.Duration, p chatops.RetryPolicy) (chatops.Claim, admission, error) {
	dest, err := scanDestination(tx.QueryRow(ctx, `SELECT `+destinationColumns+` FROM chatops_destinations
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`, string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.DestinationID)))
	if err != nil {
		return chatops.Claim{}, admitSkipped, err
	}
	if !dest.Enabled {
		return chatops.Claim{}, admitSkipped, nil
	}
	from := d.Version
	reclaimed := d.State == chatops.StateClaimed
	if dest.BindingRevision != d.BindingRevision {
		if err := d.Refuse(now); err != nil {
			return chatops.Claim{}, admitSkipped, err
		}
		if err := saveDelivery(ctx, tx, d, from); err != nil {
			return chatops.Claim{}, admitSkipped, err
		}
		if reclaimed {
			if err := abandonAttempt(ctx, tx, d, now); err != nil {
				return chatops.Claim{}, admitSkipped, err
			}
		}
		return chatops.Claim{}, admitRefused, nil
	}
	res, err := d.Claim(now, lease, p)
	if err != nil {
		return chatops.Claim{}, admitSkipped, err
	}
	if err := saveDelivery(ctx, tx, d, from); err != nil {
		return chatops.Claim{}, admitSkipped, err
	}
	if res.Reclaimed {
		// The previous holder stopped without recording an outcome: its attempt
		// is closed as abandoned (it may have reached the receiver).
		if err := abandonAttempt(ctx, tx, d, now); err != nil {
			return chatops.Claim{}, admitSkipped, err
		}
	}
	if res.Exhausted {
		return chatops.Claim{}, admitExhausted, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chatops_delivery_attempts (tenant_id, project_id, delivery_id, attempt, claim_token, replay, started_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.ID), d.Attempts, d.ClaimToken, d.Replays, now.UTC()); err != nil {
		return chatops.Claim{}, admitSkipped, err
	}
	return chatops.Claim{Delivery: d, Destination: dest, Attempt: d.Attempts, Token: d.ClaimToken, LeaseExpiresAt: d.LeaseExpiresAt}, admitClaimed, nil
}

// abandonAttempt closes the newest unfinished attempt of a delivery whose claim
// expired. After a reclaim the delivery's attempt counter has already advanced,
// so the abandoned one is the unfinished row below it.
func abandonAttempt(ctx context.Context, tx pgx.Tx, d chatops.Delivery, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE chatops_delivery_attempts SET finished_at = $4, result = 'abandoned', class = 'lease_expired', uncertain = true
WHERE tenant_id = $1 AND project_id = $2 AND delivery_id = $3 AND finished_at IS NULL`,
		string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.ID), now.UTC())
	return err
}

// Finish implements chatops.WorkStore. The delivery row is locked and the
// outcome is applied by the entity, which honors only the current fencing token;
// a stale holder gets ErrStaleClaim and changes nothing. A repeat of a finalization
// that already committed (a retry after an unknown commit outcome) returns the
// recorded result without a second history entry.
func (s *Store) Finish(ctx context.Context, c chatops.Claim, o chatops.Outcome, now time.Time, p chatops.RetryPolicy) (chatops.Delivery, error) {
	var out chatops.Delivery
	err := s.system(ctx, c.Delivery.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		scope := c.Delivery.Scope
		d, err := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM chatops_deliveries
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), string(c.Delivery.ID)), false)
		if errors.Is(err, pgx.ErrNoRows) {
			return chatops.Fail(chatops.ErrNotFound, err, "not found")
		}
		if err != nil {
			return err
		}
		var finished bool
		var token int64
		var recorded string
		switch err := tx.QueryRow(ctx, `SELECT finished_at IS NOT NULL, claim_token, result FROM chatops_delivery_attempts
WHERE tenant_id = $1 AND project_id = $2 AND delivery_id = $3 AND attempt = $4`, string(scope.TenantID), string(scope.ProjectID), string(c.Delivery.ID), c.Attempt).Scan(&finished, &token, &recorded); {
		case errors.Is(err, pgx.ErrNoRows):
			return chatops.Fail(chatops.ErrStaleClaim, err, "the claim is no longer current")
		case err != nil:
			return err
		case finished && recorded == chatops.ResultAbandoned:
			return chatops.Fail(chatops.ErrStaleClaim, nil, "the claim expired and was taken over")
		case finished && token == c.Token:
			out = d // already recorded by this very claim
			return nil
		}
		from := d.Version
		result := chatops.ResultFailed
		if o.Delivered {
			if err := d.Succeed(c.Token, now, o); err != nil {
				return err
			}
			result = chatops.ResultDelivered
		} else {
			retry, err := d.Fail(c.Token, now, o, p)
			if err != nil {
				return err
			}
			if retry {
				result = chatops.ResultRetry
			}
		}
		if err := saveDelivery(ctx, tx, d, from); err != nil {
			return err
		}
		receipt := ""
		if o.Delivered {
			receipt = d.Receipt
		}
		if _, err := tx.Exec(ctx, `UPDATE chatops_delivery_attempts SET finished_at = $5, result = $6, class = $7, uncertain = $8, http_status = $9, receipt = $10
WHERE tenant_id = $1 AND project_id = $2 AND delivery_id = $3 AND attempt = $4 AND finished_at IS NULL`,
			string(scope.TenantID), string(scope.ProjectID), string(c.Delivery.ID), c.Attempt, now.UTC(), result, string(o.Class), o.Uncertain, o.HTTPStatus, receipt); err != nil {
			return err
		}
		out = d
		return nil
	})
	if err != nil {
		return chatops.Delivery{}, err
	}
	return out, nil
}

// SetDeliveryFence implements chatops.FenceStore. The fence and its audit entry
// commit together; claims in other processes observe it from their next claim
// transaction.
func (s *Store) SetDeliveryFence(ctx context.Context, tenant domain.TenantID, fenced bool, op chatops.Operator, reason string, now time.Time) error {
	return s.system(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO chatops_delivery_fences (tenant_id, fenced, reason, operator_id, updated_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id) DO UPDATE SET fenced = EXCLUDED.fenced, reason = EXCLUDED.reason, operator_id = EXCLUDED.operator_id, updated_at = EXCLUDED.updated_at`,
			string(tenant), fenced, reason, op.ID, now.UTC()); err != nil {
			return err
		}
		action := "chatops.fence.set"
		if !fenced {
			action = "chatops.fence.clear"
		}
		return audit(ctx, tx, auditEntry{Scope: domain.Scope{TenantID: tenant}, ActorType: "operator", ActorID: op.ID, Reason: reason, Action: action,
			ResourceType: "chatops_delivery_fence", ResourceID: string(tenant), At: now, Metadata: map[string]any{"fenced": fenced}})
	})
}
