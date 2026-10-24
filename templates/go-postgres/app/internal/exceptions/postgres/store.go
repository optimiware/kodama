// Package postgres is the PostgreSQL adapter of exceptions.Store.
//
// Every statement carries an explicit tenant predicate; row-level security
// (forced, with a non-owner runtime role and a transaction-local tenant) is the
// second, independent line of defense. Locks are always taken in the order
// exception row, then intent row, so concurrent commands cannot deadlock.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"example.invalid/inventory/internal/dbtx"
	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/tenancy"
)

// Store implements exceptions.Store over a runtime-role pool.
type Store struct{ pool *pgxpool.Pool }

var _ exceptions.Store = (*Store)(nil)

// NewStore requires the runtime pool. Never pass an owner or superuser pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("exceptions/postgres: a pool is required")
	}
	return &Store{pool: pool}, nil
}

const columns = `id, erp_ref, description, erp_quantity, counted_quantity, state, version, created_by, created_at, updated_at,
	resolution_operation_key, resolution_adjusted_quantity, resolution_reason, resolution_requested_by,
	resolution_payload_hash, resolution_erp_reference, resolution_detail`

func scan(tenant tenancy.TenantID, row pgx.Row) (exceptions.Exception, error) {
	var e exceptions.Exception
	var state string
	var opKey, reason, by, hash, ref, detail *string
	var adj *int64
	err := row.Scan(&e.ID, &e.ERPRef, &e.Description, &e.ERPQuantity, &e.CountedQuantity, &state, &e.Version, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt,
		&opKey, &adj, &reason, &by, &hash, &ref, &detail)
	if errors.Is(err, pgx.ErrNoRows) {
		return exceptions.Exception{}, exceptions.ErrNotFound
	}
	if err != nil {
		return exceptions.Exception{}, err
	}
	e.Tenant, e.State = tenant, exceptions.State(state)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	if opKey != nil {
		r := &exceptions.Resolution{OperationKey: *opKey}
		if adj != nil {
			r.AdjustedQuantity = *adj
		}
		r.Reason, r.RequestedBy, r.PayloadHash = deref(reason), deref(by), deref(hash)
		r.ERPReference, r.Detail = deref(ref), deref(detail)
		e.Resolution = r
	}
	return e, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Store) audit(ctx context.Context, tx pgx.Tx, tenant tenancy.TenantID, id exceptions.ID, actor, action, detail string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_events (tenant_id, exception_id, actor, action, detail, at) VALUES ($1, $2, $3, $4, $5, $6)`,
		string(tenant), string(id), actor, action, detail, at)
	return err
}

func (s *Store) lock(ctx context.Context, tx pgx.Tx, tenant tenancy.TenantID, id exceptions.ID) (exceptions.Exception, error) {
	return scan(tenant, tx.QueryRow(ctx, "SELECT "+columns+" FROM exceptions WHERE tenant_id = $1 AND id = $2 FOR UPDATE", string(tenant), string(id)))
}

func (s *Store) save(ctx context.Context, tx pgx.Tx, e exceptions.Exception, wasVersion int64) error {
	var r exceptions.Resolution
	if e.Resolution != nil {
		r = *e.Resolution
	}
	var adj *int64
	if e.Resolution != nil {
		adj = &r.AdjustedQuantity
	}
	tag, err := tx.Exec(ctx, `UPDATE exceptions SET state = $4, version = $5, updated_at = $6,
		resolution_operation_key = $7, resolution_adjusted_quantity = $8, resolution_reason = $9, resolution_requested_by = $10,
		resolution_payload_hash = $11, resolution_erp_reference = $12, resolution_detail = $13
		WHERE tenant_id = $1 AND id = $2 AND version = $3`,
		string(e.Tenant), string(e.ID), wasVersion, string(e.State), e.Version, e.UpdatedAt,
		nullable(r.OperationKey), adj, nullable(r.Reason), nullable(r.RequestedBy), nullable(r.PayloadHash), nullable(r.ERPReference), nullable(r.Detail))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return exceptions.ErrVersionConflict
	}
	return nil
}

// Create implements exceptions.Store.
func (s *Store) Create(ctx context.Context, a tenancy.Access, e exceptions.Exception) error {
	if e.Tenant != a.Tenant() {
		return fmt.Errorf("%w: exception belongs to another tenant", exceptions.ErrInvalid)
	}
	return dbtx.InTenant(ctx, s.pool, string(a.Tenant()), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO exceptions (tenant_id, id, erp_ref, description, erp_quantity, counted_quantity, state, version, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'open', 1, $7, $8, $8)`,
			string(e.Tenant), string(e.ID), e.ERPRef, e.Description, e.ERPQuantity, e.CountedQuantity, e.CreatedBy, e.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, e.Tenant, e.ID, a.Subject(), "created", "", e.CreatedAt)
	})
}

// Get implements exceptions.Store.
func (s *Store) Get(ctx context.Context, a tenancy.Access, id exceptions.ID) (exceptions.Exception, error) {
	var e exceptions.Exception
	err := dbtx.InTenant(ctx, s.pool, string(a.Tenant()), func(tx pgx.Tx) (err error) {
		e, err = scan(a.Tenant(), tx.QueryRow(ctx, "SELECT "+columns+" FROM exceptions WHERE tenant_id = $1 AND id = $2", string(a.Tenant()), string(id)))
		return err
	})
	return e, err
}

// List implements exceptions.Store.
func (s *Store) List(ctx context.Context, a tenancy.Access, q exceptions.ListQuery) (exceptions.Page, error) {
	var page exceptions.Page
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	err := dbtx.InTenant(ctx, s.pool, string(a.Tenant()), func(tx pgx.Tx) error {
		after := time.Time{}
		afterID := ""
		if q.After != nil {
			after, afterID = q.After.CreatedAt, string(q.After.ID)
		}
		rows, err := tx.Query(ctx, "SELECT "+columns+` FROM exceptions
			WHERE tenant_id = $1 AND ($2 = '' OR state = $2) AND ($3::timestamptz IS NULL OR (created_at, id) > ($3, $4::uuid))
			ORDER BY created_at, id LIMIT $5`,
			string(a.Tenant()), string(q.State), nullTime(after, q.After != nil), nullString(afterID), limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scan(a.Tenant(), rows)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, e)
		}
		return rows.Err()
	})
	if err != nil {
		return exceptions.Page{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = &exceptions.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func nullTime(t time.Time, ok bool) *time.Time {
	if !ok {
		return nil
	}
	return &t
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// History implements exceptions.Store.
func (s *Store) History(ctx context.Context, a tenancy.Access, id exceptions.ID) ([]exceptions.AuditEntry, error) {
	var out []exceptions.AuditEntry
	err := dbtx.InTenant(ctx, s.pool, string(a.Tenant()), func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM exceptions WHERE tenant_id = $1 AND id = $2)", string(a.Tenant()), string(id)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return exceptions.ErrNotFound
		}
		rows, err := tx.Query(ctx, `SELECT seq, exception_id, actor, action, detail, at FROM audit_events WHERE tenant_id = $1 AND exception_id = $2 ORDER BY seq`, string(a.Tenant()), string(id))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e exceptions.AuditEntry
			if err := rows.Scan(&e.Seq, &e.ExceptionID, &e.Actor, &e.Action, &e.Detail, &e.At); err != nil {
				return err
			}
			e.At = e.At.UTC()
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Submit implements exceptions.Store.
func (s *Store) Submit(ctx context.Context, a tenancy.Access, id exceptions.ID, req exceptions.ResolveRequest, decide func(*exceptions.Exception) (exceptions.Intent, error)) (exceptions.Receipt, error) {
	var receipt exceptions.Receipt
	tenant := a.Tenant()
	err := dbtx.InTenant(ctx, s.pool, string(tenant), func(tx pgx.Tx) error {
		cur, err := s.lock(ctx, tx, tenant, id) // 1. the exception row serializes all commands on it
		if err != nil {
			return err
		}
		hash := req.PayloadHash(id)
		var recHash string
		var recID string
		err = tx.QueryRow(ctx, "SELECT request_hash, exception_id FROM idempotency_records WHERE tenant_id = $1 AND idem_key = $2", string(tenant), req.IdempotencyKey).Scan(&recHash, &recID)
		switch {
		case err == nil:
			if recHash != hash || recID != string(id) {
				return exceptions.ErrIdempotencyConflict
			}
			receipt = exceptions.Receipt{Exception: cur, Replayed: true}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		next := cur
		in, err := decide(&next) // 2. the entity owns the transition rules
		if err != nil {
			return err
		}
		if err := s.save(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO idempotency_records (tenant_id, idem_key, request_hash, exception_id) VALUES ($1, $2, $3, $4)`,
			string(tenant), req.IdempotencyKey, hash, string(id)); err != nil {
			return mapUnique(err, exceptions.ErrIdempotencyConflict)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO resolution_intents (tenant_id, id, exception_id, operation_key, payload_hash, erp_ref, adjusted_quantity, reason, requested_by, status, created_at, updated_at)
			VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, 'queued', $9, $9)`,
			string(tenant), string(id), in.OperationKey, in.PayloadHash, in.ERPRef, in.AdjustedQuantity, in.Reason, req.Actor, next.UpdatedAt); err != nil {
			return mapUnique(err, exceptions.ErrInvalidState)
		}
		if err := s.audit(ctx, tx, tenant, id, a.Subject(), "resolution_requested", fmt.Sprintf("adjusted quantity %d", in.AdjustedQuantity), next.UpdatedAt); err != nil {
			return err
		}
		receipt = exceptions.Receipt{Exception: next}
		return nil
	})
	if err != nil {
		return exceptions.Receipt{}, err
	}
	return receipt, nil
}

func mapUnique(err, target error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return target
	}
	return err
}

// Claim implements exceptions.Store.
func (s *Store) Claim(ctx context.Context, a tenancy.Access, owner string, lease time.Duration, now time.Time, maxAttempts int) (*exceptions.Claim, error) {
	var claim *exceptions.Claim
	tenant := a.Tenant()
	err := dbtx.InTenant(ctx, s.pool, string(tenant), func(tx pgx.Tx) error {
		var in exceptions.Intent
		var status string
		err := tx.QueryRow(ctx, `SELECT id, exception_id, operation_key, payload_hash, erp_ref, adjusted_quantity, reason, status, attempts
			FROM resolution_intents
			WHERE tenant_id = $1 AND (
				status = 'queued'
				OR (status = 'dispatching' AND lease_until < $2)
				OR (status = 'unknown' AND next_attempt_at IS NOT NULL AND next_attempt_at <= $2 AND attempts < $3))
			ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, string(tenant), now, maxAttempts).
			Scan(&in.ID, &in.ExceptionID, &in.OperationKey, &in.PayloadHash, &in.ERPRef, &in.AdjustedQuantity, &in.Reason, &status, &in.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE resolution_intents SET status = 'dispatching', lease_owner = $3, lease_until = $4, attempts = attempts + 1, updated_at = $5
			WHERE tenant_id = $1 AND id = $2`, string(tenant), in.ID, owner, now.Add(lease), now); err != nil {
			return err
		}
		in.Tenant = tenant
		in.Attempts++
		claim = &exceptions.Claim{Intent: in, Owner: owner, Reconcile: status != "queued"}
		return nil
	})
	return claim, err
}

// Observe implements exceptions.Store.
func (s *Store) Observe(ctx context.Context, a tenancy.Access, o exceptions.Observation) (exceptions.Exception, error) {
	var out exceptions.Exception
	tenant := a.Tenant()
	err := dbtx.InTenant(ctx, s.pool, string(tenant), func(tx pgx.Tx) error {
		var excID string
		err := tx.QueryRow(ctx, "SELECT exception_id FROM resolution_intents WHERE tenant_id = $1 AND id = $2", string(tenant), o.IntentID).Scan(&excID)
		if errors.Is(err, pgx.ErrNoRows) {
			return exceptions.ErrNotFound
		}
		if err != nil {
			return err
		}
		cur, err := s.lock(ctx, tx, tenant, exceptions.ID(excID)) // exception first, then intent
		if err != nil {
			return err
		}
		var status, opKey string
		var leaseOwner *string
		if err := tx.QueryRow(ctx, "SELECT status, operation_key, lease_owner FROM resolution_intents WHERE tenant_id = $1 AND id = $2 FOR UPDATE", string(tenant), o.IntentID).
			Scan(&status, &opKey, &leaseOwner); err != nil {
			return err
		}
		if opKey != o.OperationKey {
			return exceptions.ErrOperationMismatch
		}
		terminal := status == "acknowledged" || status == "rejected"
		switch {
		case terminal && status == string(o.Kind):
			out = cur // the same fact observed again: nothing to do
			return nil
		case terminal:
			return fmt.Errorf("%w: intent is already %s", exceptions.ErrInvalidState, status)
		case o.Kind == exceptions.ObservedUnknown && (status != "dispatching" || deref(leaseOwner) != o.Owner):
			return exceptions.ErrLeaseLost
		}
		next := cur
		if err := exceptions.ApplyObservation(&next, o); err != nil {
			return err
		}
		if next.Version != cur.Version {
			if err := s.save(ctx, tx, next, cur.Version); err != nil {
				return err
			}
		} else if o.Kind == exceptions.ObservedUnknown {
			if _, err := tx.Exec(ctx, `UPDATE exceptions SET resolution_detail = $3 WHERE tenant_id = $1 AND id = $2`, string(tenant), excID, nullable(o.Detail)); err != nil {
				return err
			}
		}
		newStatus := string(o.Kind)
		var nextAt *time.Time
		if o.Kind == exceptions.ObservedUnknown && !o.RetryAt.IsZero() {
			nextAt = &o.RetryAt
		}
		if _, err := tx.Exec(ctx, `UPDATE resolution_intents SET status = $3, erp_reference = $4, detail = $5, next_attempt_at = $6, lease_owner = NULL, lease_until = NULL, updated_at = $7
			WHERE tenant_id = $1 AND id = $2`, string(tenant), o.IntentID, newStatus, nullable(o.Reference), nullable(o.Detail), nextAt, o.At); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, tenant, cur.ID, a.Subject(), "erp_"+string(o.Kind), o.Detail, o.At); err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}
