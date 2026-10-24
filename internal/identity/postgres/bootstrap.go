package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
)

// BootstrapStore provisions the initial tenant with the administrative (owner)
// identity. Its pool must never serve requests: the composition root opens it
// only for the explicitly invoked operator command. The runtime role holds no
// privilege on identity_bootstrap.
type BootstrapStore struct {
	admin  storage.TxBeginner
	events EventAppender
}

var _ identity.BootstrapStore = (*BootstrapStore)(nil)

// NewBootstrapStore requires the administrative pool and the event appender.
func NewBootstrapStore(admin storage.TxBeginner, appender EventAppender) (*BootstrapStore, error) {
	if admin == nil || appender == nil {
		return nil, errors.New("identity/postgres: an administrative database and an event appender are required")
	}
	return &BootstrapStore{admin: admin, events: appender}, nil
}

// bootstrapLockKey serializes concurrent bootstrap invocations.
const bootstrapLockKey = "kodama.identity.bootstrap"

// Complete implements identity.BootstrapStore. Everything commits in one
// transaction: tenant, user mapping, tenant-wide admin binding, authorization
// revision, audit, membership.changed and the completion record. Concurrent
// invocations queue on an advisory lock; the later ones observe the committed
// record, so exactly one provisions.
func (s *BootstrapStore) Complete(ctx context.Context, plan identity.BootstrapPlan) (identity.BootstrapResult, error) {
	var res identity.BootstrapResult
	ranToCommit := false
	err := storage.WithTenant(ctx, s.admin, plan.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, bootstrapLockKey); err != nil {
			return err
		}
		var tenant, user, binding, digest string
		err := tx.QueryRow(ctx, `SELECT tenant_id, user_id, binding_id, params_digest FROM identity_bootstrap`).Scan(&tenant, &user, &binding, &digest)
		switch {
		case err == nil:
			if digest != plan.Digest {
				return identity.ErrBootstrapConflict
			}
			res = identity.BootstrapResult{Tenant: plan.Tenant, UserID: identity.UserID(user), BindingID: identity.BindingID(binding)}
			ranToCommit = true
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, key, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
			string(plan.Tenant), plan.TenantKey, plan.DisplayName, micros(plan.OccurredAt)); err != nil {
			if isPg(err, "23505", "") {
				return fmt.Errorf("%w: the tenant already exists outside bootstrap", identity.ErrBootstrapConflict)
			}
			return err
		}
		userID := string(plan.UserID)
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE issuer = $1 AND subject = $2`, plan.Principal.Issuer, plan.Principal.Subject).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO users (id, issuer, subject, created_at) VALUES ($1, $2, $3, $4)`,
				userID, plan.Principal.Issuer, plan.Principal.Subject, micros(plan.OccurredAt)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		b, err := identity.Grant(plan.BindingID, plan.Tenant, identity.UserID(userID), "", identity.RoleAdmin, plan.OccurredAt)
		if err != nil {
			return err
		}
		t := &txImpl{tx: tx, tenant: plan.Tenant, events: s.events}
		if err := t.LockAuthorization(ctx, []identity.UserID{b.User}, nil); err != nil {
			return err
		}
		if err := t.InsertBinding(ctx, b); err != nil {
			return err
		}
		rev, err := t.BumpAuthorization(ctx, b.User)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_bootstrap (tenant_id, user_id, binding_id, params_digest, operator_id, completed_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			string(plan.Tenant), userID, string(b.ID), plan.Digest, plan.OperatorID, micros(plan.OccurredAt)); err != nil {
			return err
		}
		if err := t.Record(ctx, identity.Change{
			ID: plan.ChangeID, Kind: identity.ChangeBindingGranted, Actor: identity.Actor{ID: plan.OperatorID, Type: identity.ActorOperator},
			OccurredAt: plan.OccurredAt, Tenant: plan.Tenant,
			Bindings: []identity.BindingChange{{User: b.User, EffectiveVersion: rev,
				Ops: []identity.BindingOp{{ID: b.ID, Kind: identity.ChangeBindingGranted, Role: b.Role, Version: b.Version}}}},
		}); err != nil {
			return err
		}
		res = identity.BootstrapResult{Tenant: plan.Tenant, UserID: b.User, BindingID: b.ID, Created: true}
		ranToCommit = true
		return nil
	})
	if err != nil && ranToCommit {
		return identity.BootstrapResult{}, identity.Unavailablef(err, "the outcome of the bootstrap is unknown; observe the completion record by retrying with identical parameters")
	}
	return res, classify(err)
}
