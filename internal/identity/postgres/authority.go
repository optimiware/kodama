package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/identity"
)

// TxAuthority is the identity.Authority bound to one open tenant transaction of
// another module (for example the work store's mutation). Each check takes the
// checked user's authorization revision FOR SHARE and reads the current bindings
// in the same transaction. The share lock is held until the caller's transaction
// ends: a revocation either committed before this check (and is seen) or waits
// for the protected mutation to finish, so a revoked reviewer cannot complete
// work after a lock wait and a revocation never returns while a mutation that
// relied on the old grant is still open.
type TxAuthority struct {
	tx pgx.Tx
}

var _ identity.Authority = TxAuthority{}

// NewTxAuthority binds the authority to tx, which must carry the tenant context
// (storage.WithTenant) of the target's tenant. A target in another tenant finds
// no bindings and is hidden.
func NewTxAuthority(tx pgx.Tx) TxAuthority { return TxAuthority{tx: tx} }

// Authorize implements identity.Authority.
func (a TxAuthority) Authorize(ctx context.Context, actor identity.Actor, target identity.Target, perm identity.Permission) error {
	if err := target.TenantID.Validate(); err != nil {
		return identity.ErrHidden
	}
	if actor.Type != identity.ActorUser || actor.Validate() != nil {
		return identity.ErrDenied
	}
	var rev int64
	err := a.tx.QueryRow(ctx, `SELECT revision FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR SHARE`,
		string(target.TenantID), actor.ID).Scan(&rev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return identity.Unavailablef(err, "locking the authorization revision")
	}
	bindings, err := readBindings(ctx, a.tx, target.TenantID, identity.UserID(actor.ID))
	if err != nil {
		return identity.Unavailablef(err, "reading current bindings")
	}
	return identity.Decide(bindings, actor, target, perm)
}

// ResolveUser reports whether the user has a current binding in the tenant (the
// only users a tenant transaction can see). It returns identity.ErrHidden for a
// user with none.
func (a TxAuthority) ResolveUser(ctx context.Context, tenant string, user identity.UserID) error {
	var one int
	err := a.tx.QueryRow(ctx, `SELECT 1 FROM memberships WHERE tenant_id = $1 AND user_id = $2 LIMIT 1`, tenant, string(user)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrHidden
	}
	if err != nil {
		return identity.Unavailablef(err, "reading memberships")
	}
	return nil
}
