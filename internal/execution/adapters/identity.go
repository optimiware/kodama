package adapters

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

var runPermissions = map[execution.Permission]identity.Permission{
	execution.PermRunRead:   identity.PermRunRead,
	execution.PermRunManage: identity.PermRunManage,
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return execution.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return execution.ErrDenied
	}
	return execution.Unavailablef(err, "the identity authority is unavailable")
}

func authorize(ctx context.Context, a identity.Authority, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	p, ok := runPermissions[perm]
	if !ok {
		return execution.ErrDenied // unknown permissions deny by default
	}
	ia := identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)}
	return translate(a.Authorize(ctx, ia, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p))
}

// Authority implements execution.Authority over the selected identity
// authority and the current-binding reader (for actor resolution): a principal
// is a user ID and is hidden from tenants where it holds no binding.
type Authority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ execution.Authority = (*Authority)(nil)

// NewAuthority requires both collaborators; there is no default grant.
func NewAuthority(a identity.Authority, r identity.AccessReader) (*Authority, error) {
	if a == nil || r == nil {
		return nil, errors.New("execution/adapters: an authority and an access reader are required")
	}
	return &Authority{authority: a, reader: r}, nil
}

// ResolveActor implements execution.Authority.
func (a *Authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (execution.Actor, error) {
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return execution.Actor{}, execution.ErrHidden
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return execution.Actor{}, translate(err)
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	for _, b := range bindings {
		if b.Applies(target) {
			return execution.Actor{ID: principalID, Type: execution.ActorUser}, nil
		}
	}
	return execution.Actor{}, execution.ErrHidden
}

// Authorize implements execution.Authority.
func (a *Authority) Authorize(ctx context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	return authorize(ctx, a.authority, actor, scope, perm)
}

// TxAuthority is the permission check bound to one open tenant transaction: it
// reads current bindings through the transaction and holds the user's
// authorization revision FOR SHARE until it ends, so a revocation committed
// while the dispatch waited on a lock is seen and a revocation racing a running
// dispatch waits for it.
type TxAuthority struct{ a identitypg.TxAuthority }

var _ execution.TxAuthority = TxAuthority{}

// Authorize implements execution.TxAuthority.
func (t TxAuthority) Authorize(ctx context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	return authorize(ctx, t.a, actor, scope, perm)
}

// BindAuthority is the binder the PostgreSQL store takes.
func BindAuthority(tx pgx.Tx) execution.TxAuthority {
	return TxAuthority{a: identitypg.NewTxAuthority(tx)}
}
