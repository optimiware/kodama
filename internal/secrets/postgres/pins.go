package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// PinRequest registers the secret versions one consumer retains.
type PinRequest struct {
	Scope    domain.Scope
	Consumer secrets.ConsumerRef
	// Bindings are the pinned references of the effective snapshot
	// (EffectiveSnapshot.SecretBindings).
	Bindings []runtimev1.SecretBinding
}

func (s *Store) requireTenantContext(ctx context.Context, tx pgx.Tx, tenant domain.TenantID) error {
	var current *string
	if err := tx.QueryRow(ctx, `SELECT kodama_tenant_id()`).Scan(&current); err != nil {
		return err
	}
	if current == nil || *current != string(tenant) {
		return fmt.Errorf("%w: transaction tenant context differs from the request's tenant", secrets.ErrNotFound)
	}
	return nil
}

// RegisterPins records, inside the caller's tenant transaction tx, that the
// consumer retains each pinned secret version. The caller (run admission,
// T-020) calls it in the same transaction that creates the run, so a run never
// exists with unregistered pins and a pin never exists for a run that did not
// commit. It never begins, commits or rolls back tx and makes no network call.
//
// Each secret row is locked FOR SHARE and its version row FOR SHARE before the
// pin is inserted. DeleteSecret and RevokeSecretVersion take the same rows FOR
// UPDATE, so registration and deletion/revocation are totally ordered: a pin
// cannot appear after deletion closed admission or after the version was
// revoked, and a pin that committed first is visible to the deletion report and
// to revocation's consumer accounting. Bindings are processed in a fixed order
// so concurrent registrations cannot deadlock each other. Registering the same
// pin again is a no-op, which makes admission retries safe.
//
// Refusals: ErrNotFound (no such secret or version in the project), ErrDeleted,
// ErrRevoked.
func (s *Store) RegisterPins(ctx context.Context, tx pgx.Tx, req PinRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return err
	}
	if err := req.Consumer.Validate(); err != nil {
		return err
	}
	if err := s.requireTenantContext(ctx, tx, req.Scope.TenantID); err != nil {
		return err
	}
	bs := append([]runtimev1.SecretBinding(nil), req.Bindings...)
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].Ref != bs[j].Ref {
			return bs[i].Ref < bs[j].Ref
		}
		return bs[i].Version < bs[j].Version
	})
	now := s.now()
	for _, b := range bs {
		name, err := secrets.ParseSecretRef(b.Ref)
		if err != nil {
			return err
		}
		if b.Version < 1 || b.Version > domain.MaxSafeInteger {
			return fmt.Errorf("%w: pinned version out of range", secrets.ErrInvalid)
		}
		sec, err := getSecretByName(ctx, tx, req.Scope, name, " FOR SHARE")
		if err != nil {
			return err
		}
		if sec.Deleted() {
			return fmt.Errorf("%w: %s", secrets.ErrDeleted, b.Ref)
		}
		v, err := getVersion(ctx, tx, req.Scope, sec.ID(), b.Version, " FOR SHARE")
		if err != nil {
			return err
		}
		if v.Revoked() {
			return fmt.Errorf("%w: %s version %d", secrets.ErrRevoked, b.Ref, b.Version)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO secret_pins
(tenant_id, project_id, consumer_kind, consumer_id, secret_id, secret_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, consumer_kind, consumer_id, secret_id, secret_version) DO NOTHING`,
			string(req.Scope.TenantID), string(req.Scope.ProjectID), req.Consumer.Kind, req.Consumer.ID, sec.ID(), b.Version, now); err != nil {
			return err
		}
	}
	return nil
}

// ReleasePins marks every pin of the consumer released, inside the consumer's
// own termination transaction (completion, cancellation, expiry). Resolution
// finalization locks the pin row, so a resolution either commits before the
// release (and is audited) or finds the pin released and releases nothing. This
// is the ordering guard that cancellation relies on; it needs no network call
// inside a SQL transaction. It returns the number of pins newly released.
func (s *Store) ReleasePins(ctx context.Context, tx pgx.Tx, scope domain.Scope, consumer secrets.ConsumerRef) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := consumer.Validate(); err != nil {
		return 0, err
	}
	if err := s.requireTenantContext(ctx, tx, scope.TenantID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE secret_pins SET released_at = $5
WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = $3 AND consumer_id = $4 AND released_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), consumer.Kind, consumer.ID, s.now())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// HasPin reports whether the consumer retains the exact version (used by
// diagnostics and tests; resolution rechecks it under lock).
func (s *Store) HasPin(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, secretID string, version int64) (found bool, released bool, err error) {
	err = s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM secret_pins
WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = $3 AND consumer_id = $4 AND secret_id = $5 AND secret_version = $6`,
			string(scope.TenantID), string(scope.ProjectID), consumer.Kind, consumer.ID, secretID, version).Scan(&released)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		found = true
		return nil
	})
	return found, released, err
}
