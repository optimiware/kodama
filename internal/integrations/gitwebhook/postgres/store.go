// Package postgres is the PostgreSQL adapter of the native Git trigger
// (internal/integrations/gitwebhook): the trigger store, the durable delivery
// claim with its admission lease and write-once outcome, and the admission
// ledger that decides, once per idempotency key, between the run admission
// created and the refusal admission recorded.
//
// Every request-serving statement runs inside storage.WithTenant (READ
// COMMITTED) under the non-owner runtime role, names tenant and project
// explicitly, and is further constrained by forced row level security. The one
// statement outside a tenant context is the trigger-address locator
// (kodama_git_trigger_locate): the hook URL carries only the address, so the
// owner must be found before the tenant transaction can start.
//
// Lock order: the ledger key's advisory lock (admission and refusal only), then
// the work item and run rows taken by run admission, then the ledger row; the
// delivery row (FOR UPDATE) is never held together with any of them. No network
// call is made inside a transaction.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/workspace"
)

// Options tune the store. The zero value selects the defaults.
type Options struct {
	// NewID creates audit record identifiers; the default is random.
	NewID func() (string, error)
}

// Store implements gitwebhook.Store over PostgreSQL.
type Store struct {
	db    storage.TxBeginner
	newID func() (string, error)
}

var _ gw.Store = (*Store)(nil)

// NewStore requires a database handle (use the non-owner runtime pool).
func NewStore(db storage.TxBeginner, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("gitwebhook/postgres: a database is required")
	}
	if opts.NewID == nil {
		opts.NewID = randomAuditID
	}
	return &Store{db: db, newID: opts.NewID}, nil
}

func randomAuditID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "gha_" + hex.EncodeToString(b[:]), nil
}

// classify maps infrastructure failures to the module's classes. The module's
// own sentinels pass through unchanged.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	for _, s := range []error{gw.ErrNotFound, gw.ErrConflict, gw.ErrInvalid, gw.ErrDenied, gw.ErrUnavailable, gw.ErrAdmissionClosed, gw.ErrRunRefused} {
		if errors.Is(err, s) {
			return err
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505": // uniqueness: the address or claim exists
			return fmt.Errorf("%w: %s", gw.ErrConflict, op)
		case pgErr.Code == "23503": // composite foreign key: not in this scope
			return fmt.Errorf("%w: %s: the referenced record is not in the scope", gw.ErrNotFound, op)
		case pgErr.Code == "23514": // check or guard: the change is not allowed
			return fmt.Errorf("%w: %s: %s", gw.ErrConflict, op, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("%w: %s: %w", gw.ErrUnavailable, op, err)
}

func (s *Store) inTenant(ctx context.Context, op string, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(op, storage.WithTenant(ctx, s.db, tenant, fn))
}

// locate resolves a trigger address to its owning scope, outside any tenant
// context, through the SECURITY DEFINER locator.
func (s *Store) locate(ctx context.Context, id gw.TriggerID) (domain.Scope, error) {
	if err := id.Validate(); err != nil {
		return domain.Scope{}, gw.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Scope{}, classify("locate trigger", err)
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()
	var tenant, project string
	err = tx.QueryRow(ctx, `SELECT tenant_id, project_id FROM kodama_git_trigger_locate($1)`, string(id)).Scan(&tenant, &project)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Scope{}, gw.ErrNotFound
	case err != nil:
		return domain.Scope{}, classify("locate trigger", err)
	}
	return domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}, nil
}

const triggerColumns = `tenant_id, project_id, id, repository_id, definition_id, work_item_id, branches, secret_ref, installed_by, enabled, version, created_at, updated_at`

func scanTrigger(row pgx.Row) (gw.Trigger, error) {
	var (
		t                   gw.Trigger
		tenant, project, id string
		repo, def           string
		workItem            *string
		created, updated    time.Time
	)
	if err := row.Scan(&tenant, &project, &id, &repo, &def, &workItem, &t.Branches, &t.SecretRef, &t.InstalledBy, &t.Enabled, &t.Version, &created, &updated); err != nil {
		return gw.Trigger{}, err
	}
	t.ID = gw.TriggerID(id)
	t.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	t.RepositoryID = workspace.RepositoryID(repo)
	t.DefinitionID = domain.DefinitionID(def)
	if workItem != nil {
		t.WorkItemID = domain.WorkItemID(*workItem)
	}
	t.CreatedAt, t.UpdatedAt = created.UTC(), updated.UTC()
	return t, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InsertTrigger implements gitwebhook.Store.
func (s *Store) InsertTrigger(ctx context.Context, t gw.Trigger, audit gw.AuditRecord) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.Version < 1 {
		return fmt.Errorf("%w: a new trigger starts at version 1", gw.ErrInvalid)
	}
	if audit.Scope != t.Scope {
		return fmt.Errorf("%w: the audit record names another scope", gw.ErrInvalid)
	}
	return s.inTenant(ctx, "insert trigger", t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO git_triggers (`+triggerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			string(t.Scope.TenantID), string(t.Scope.ProjectID), string(t.ID), string(t.RepositoryID), string(t.DefinitionID), nullable(string(t.WorkItemID)),
			t.Branches, t.SecretRef, t.InstalledBy, t.Enabled, t.Version, t.CreatedAt.UTC(), t.UpdatedAt.UTC()); err != nil {
			return err
		}
		return s.writeAudit(ctx, tx, audit)
	})
}

// Trigger implements gitwebhook.Store: the address is located, then the record
// is read inside its tenant's transaction with explicit predicates.
func (s *Store) Trigger(ctx context.Context, id gw.TriggerID) (gw.Trigger, error) {
	scope, err := s.locate(ctx, id)
	if err != nil {
		return gw.Trigger{}, err
	}
	var out gw.Trigger
	err = s.inTenant(ctx, "read trigger", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		t, err := scanTrigger(tx.QueryRow(ctx, `SELECT `+triggerColumns+` FROM git_triggers WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(err, pgx.ErrNoRows) {
			return gw.ErrNotFound
		}
		out = t
		return err
	})
	return out, err
}

// UpdateTrigger implements gitwebhook.Store: optimistic, atomic with its audit.
func (s *Store) UpdateTrigger(ctx context.Context, t gw.Trigger, expected int64, audit gw.AuditRecord) (gw.Trigger, error) {
	if err := t.Validate(); err != nil {
		return gw.Trigger{}, err
	}
	if audit.Scope != t.Scope {
		return gw.Trigger{}, fmt.Errorf("%w: the audit record names another scope", gw.ErrInvalid)
	}
	var out gw.Trigger
	err := s.inTenant(ctx, "update trigger", t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		next, err := scanTrigger(tx.QueryRow(ctx, `UPDATE git_triggers
SET definition_id = $4, work_item_id = $5, branches = $6, secret_ref = $7, installed_by = $8, enabled = $9, version = version + 1, updated_at = $10
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $11
RETURNING `+triggerColumns, string(t.Scope.TenantID), string(t.Scope.ProjectID), string(t.ID),
			string(t.DefinitionID), nullable(string(t.WorkItemID)), t.Branches, t.SecretRef, t.InstalledBy, t.Enabled, t.UpdatedAt.UTC(), expected))
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT true FROM git_triggers WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
				string(t.Scope.TenantID), string(t.Scope.ProjectID), string(t.ID)).Scan(&exists); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return gw.ErrNotFound
				}
				return err
			}
			return gw.ErrConflict
		}
		if err != nil {
			return err
		}
		out = next
		return s.writeAudit(ctx, tx, audit)
	})
	if err != nil {
		return gw.Trigger{}, err
	}
	return out, nil
}

const deliveryColumns = `tenant_id, project_id, trigger_id, delivery_id, body_digest, ref, commit_id, received_at, admitting, lease_holder, lease_until, done, outcome, reason, run_id, completed_at`

func scanDelivery(row pgx.Row) (gw.DeliveryRecord, error) {
	var (
		r                        gw.DeliveryRecord
		tenant, project, trigger string
		commit                   string
		received                 time.Time
		holder, outcome, reason  *string
		runID                    *string
		leaseUntil, completed    *time.Time
	)
	if err := row.Scan(&tenant, &project, &trigger, &r.Key.DeliveryID, &r.BodyDigest, &r.Ref, &commit, &received, &r.Admitting,
		&holder, &leaseUntil, &r.Done, &outcome, &reason, &runID, &completed); err != nil {
		return gw.DeliveryRecord{}, err
	}
	r.Key.TriggerID = gw.TriggerID(trigger)
	r.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	r.Commit = workspace.Commit(commit)
	r.ReceivedAt = received.UTC()
	if holder != nil {
		r.LeaseHolder = *holder
	}
	if leaseUntil != nil {
		r.LeaseUntil = leaseUntil.UTC()
	}
	if outcome != nil {
		r.Outcome = gw.Outcome(*outcome)
	}
	if reason != nil {
		r.Reason = *reason
	}
	if runID != nil {
		r.RunID = domain.RunID(*runID)
	}
	if completed != nil {
		r.CompletedAt = completed.UTC()
	}
	return r, nil
}

func (s *Store) deliveryScope(ctx context.Context, key gw.DeliveryKey) (domain.Scope, error) {
	if err := gw.ValidateDeliveryID(key.DeliveryID); err != nil {
		return domain.Scope{}, gw.ErrNotFound
	}
	return s.locate(ctx, key.TriggerID)
}

// ClaimDelivery implements gitwebhook.Store: the primary key is the claim, so
// two concurrent identical deliveries produce one created record and every
// other caller receives it.
func (s *Store) ClaimDelivery(ctx context.Context, rec gw.DeliveryRecord) (gw.DeliveryRecord, bool, error) {
	if err := rec.Key.TriggerID.Validate(); err != nil {
		return gw.DeliveryRecord{}, false, err
	}
	if err := gw.ValidateDeliveryID(rec.Key.DeliveryID); err != nil {
		return gw.DeliveryRecord{}, false, err
	}
	if err := rec.Scope.Validate(); err != nil {
		return gw.DeliveryRecord{}, false, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	var out gw.DeliveryRecord
	var created bool
	err := s.inTenant(ctx, "claim delivery", rec.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO git_trigger_deliveries (tenant_id, project_id, trigger_id, delivery_id, body_digest, ref, commit_id, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
			string(rec.Scope.TenantID), string(rec.Scope.ProjectID), string(rec.Key.TriggerID), rec.Key.DeliveryID, rec.BodyDigest, rec.Ref, string(rec.Commit), rec.ReceivedAt.UTC())
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		// A conflicting claim has committed (the insert waited for it), so a new
		// statement sees it.
		out, err = scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM git_trigger_deliveries
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4`,
			string(rec.Scope.TenantID), string(rec.Scope.ProjectID), string(rec.Key.TriggerID), rec.Key.DeliveryID))
		if errors.Is(err, pgx.ErrNoRows) {
			return gw.ErrNotFound // the claim exists in another scope: not visible here
		}
		return err
	})
	if err != nil {
		return gw.DeliveryRecord{}, false, err
	}
	return out, created, nil
}

// lockDelivery reads the delivery FOR UPDATE inside the tenant transaction.
func lockDelivery(ctx context.Context, tx pgx.Tx, scope domain.Scope, key gw.DeliveryKey) (gw.DeliveryRecord, error) {
	r, err := scanDelivery(tx.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM git_trigger_deliveries
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4 FOR UPDATE`,
		string(scope.TenantID), string(scope.ProjectID), string(key.TriggerID), key.DeliveryID))
	if errors.Is(err, pgx.ErrNoRows) {
		return gw.DeliveryRecord{}, gw.ErrNotFound
	}
	return r, err
}

// BeginAdmission implements gitwebhook.Store: the admitting mark is durable
// before the first admission attempt and is never withdrawn.
func (s *Store) BeginAdmission(ctx context.Context, key gw.DeliveryKey) (gw.DeliveryRecord, error) {
	scope, err := s.deliveryScope(ctx, key)
	if err != nil {
		return gw.DeliveryRecord{}, err
	}
	var out gw.DeliveryRecord
	err = s.inTenant(ctx, "begin admission", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockDelivery(ctx, tx, scope, key)
		if err != nil {
			return err
		}
		if !cur.Done && !cur.Admitting {
			if _, err := tx.Exec(ctx, `UPDATE git_trigger_deliveries SET admitting = true
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4`,
				string(scope.TenantID), string(scope.ProjectID), string(key.TriggerID), key.DeliveryID); err != nil {
				return err
			}
			cur.Admitting = true
		}
		out = cur
		return nil
	})
	return out, err
}

// AcquireAdmission implements gitwebhook.Store.
func (s *Store) AcquireAdmission(ctx context.Context, key gw.DeliveryKey, holder string, now time.Time, ttl time.Duration) (gw.DeliveryRecord, bool, error) {
	if holder == "" || len(holder) > 256 || ttl <= 0 {
		return gw.DeliveryRecord{}, false, fmt.Errorf("%w: a lease needs a holder and a positive ttl", gw.ErrInvalid)
	}
	scope, err := s.deliveryScope(ctx, key)
	if err != nil {
		return gw.DeliveryRecord{}, false, err
	}
	var out gw.DeliveryRecord
	var acquired bool
	err = s.inTenant(ctx, "acquire admission", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockDelivery(ctx, tx, scope, key)
		if err != nil {
			return err
		}
		switch {
		case cur.Done:
			out, acquired = cur, false
			return nil
		case !cur.Admitting:
			return gw.ErrConflict
		case cur.LeaseHolder != "" && cur.LeaseHolder != holder && now.Before(cur.LeaseUntil):
			out, acquired = cur, false
			return nil
		}
		until := now.Add(ttl).UTC()
		if _, err := tx.Exec(ctx, `UPDATE git_trigger_deliveries SET lease_holder = $5, lease_until = $6
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4`,
			string(scope.TenantID), string(scope.ProjectID), string(key.TriggerID), key.DeliveryID, holder, until); err != nil {
			return err
		}
		cur.LeaseHolder, cur.LeaseUntil = holder, until
		out, acquired = cur, true
		return nil
	})
	if err != nil {
		return gw.DeliveryRecord{}, false, err
	}
	return out, acquired, nil
}

// ReleaseAdmission implements gitwebhook.Store: only the named holder's own
// lease on an unfinished delivery is dropped.
func (s *Store) ReleaseAdmission(ctx context.Context, key gw.DeliveryKey, holder string) error {
	if holder == "" {
		return nil
	}
	scope, err := s.deliveryScope(ctx, key)
	if err != nil {
		if errors.Is(err, gw.ErrNotFound) {
			return nil
		}
		return err
	}
	return s.inTenant(ctx, "release admission", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE git_trigger_deliveries SET lease_holder = NULL, lease_until = NULL
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4 AND NOT done AND lease_holder = $5`,
			string(scope.TenantID), string(scope.ProjectID), string(key.TriggerID), key.DeliveryID, holder)
		return err
	})
}

// FinishDelivery implements gitwebhook.Store. The first finish wins (a finished
// delivery is returned unchanged and no audit is written); the admission fence
// and the holder check are evaluated under the row lock, so a stale holder can
// never record a rejection after another processor owns the outcome.
func (s *Store) FinishDelivery(ctx context.Context, key gw.DeliveryKey, fin gw.Finish, audit gw.AuditRecord) (gw.DeliveryRecord, error) {
	switch fin.Outcome {
	case gw.OutcomeStarted:
		if fin.RunID == "" || len(fin.Reason) > 128 {
			return gw.DeliveryRecord{}, fmt.Errorf("%w: a started delivery names its run", gw.ErrInvalid)
		}
		if err := fin.RunID.Validate(); err != nil {
			return gw.DeliveryRecord{}, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
		}
	case gw.OutcomeIgnored, gw.OutcomeRejected:
		if fin.RunID != "" || fin.Reason == "" || len(fin.Reason) > 128 {
			return gw.DeliveryRecord{}, fmt.Errorf("%w: ignored and rejected deliveries carry a reason and no run", gw.ErrInvalid)
		}
	default:
		return gw.DeliveryRecord{}, fmt.Errorf("%w: unknown outcome", gw.ErrInvalid)
	}
	scope, err := s.deliveryScope(ctx, key)
	if err != nil {
		return gw.DeliveryRecord{}, err
	}
	if audit.Scope != scope {
		return gw.DeliveryRecord{}, gw.ErrNotFound
	}
	var out gw.DeliveryRecord
	err = s.inTenant(ctx, "finish delivery", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := lockDelivery(ctx, tx, scope, key)
		if err != nil {
			return err
		}
		if cur.Done {
			out = cur
			return nil
		}
		if cur.Admitting != fin.Admission {
			return gw.ErrConflict
		}
		if fin.Admission && fin.Outcome != gw.OutcomeStarted && (fin.Holder == "" || cur.LeaseHolder != fin.Holder) {
			return gw.ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE git_trigger_deliveries
SET done = true, outcome = $5, reason = $6, run_id = $7, completed_at = $8, lease_holder = NULL, lease_until = NULL
WHERE tenant_id = $1 AND project_id = $2 AND trigger_id = $3 AND delivery_id = $4`,
			string(scope.TenantID), string(scope.ProjectID), string(key.TriggerID), key.DeliveryID,
			string(fin.Outcome), nullable(fin.Reason), nullable(string(fin.RunID)), audit.At.UTC()); err != nil {
			return err
		}
		if err := s.writeAudit(ctx, tx, audit); err != nil {
			return err
		}
		cur.LeaseHolder, cur.LeaseUntil = "", time.Time{}
		cur.Done, cur.Outcome, cur.Reason, cur.RunID, cur.CompletedAt = true, fin.Outcome, fin.Reason, fin.RunID, audit.At.UTC()
		out = cur
		return nil
	})
	if err != nil {
		return gw.DeliveryRecord{}, err
	}
	return out, nil
}

// RecordAudit implements gitwebhook.Store.
func (s *Store) RecordAudit(ctx context.Context, audit gw.AuditRecord) error {
	if err := audit.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	return s.inTenant(ctx, "record audit", audit.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.writeAudit(ctx, tx, audit)
	})
}
