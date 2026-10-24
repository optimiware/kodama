package adapters

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

// The gate permissions map onto the existing fixed matrix: a decision is the
// human-only gate:decide (reviewer), and reading a gate with its evidence is
// evidence:read. Nothing here adds a role or a permission; an unknown
// permission is denied. The HTTP route rules must map gate.list and gate.read
// to evidence:read and gate.decide to gate:decide.
var permissions = map[gates.Permission]identity.Permission{
	gates.PermDecide: identity.PermGateDecide,
	gates.PermRead:   identity.PermEvidenceRead,
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return gates.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return gates.ErrDenied
	}
	return gates.Unavailablef(err, "the identity authority is unavailable")
}

func authorize(ctx context.Context, a identity.Authority, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	p, ok := permissions[perm]
	if !ok {
		return gates.ErrDenied
	}
	ia := identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)}
	return translate(a.Authorize(ctx, ia, identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p))
}

// Authority implements gates.Authority over the selected identity authority and
// the current-binding reader: a principal is a user ID and is hidden from
// tenants where it holds no binding. The selected authority (standalone or
// hosted) decides every permission; there is no local fallback.
type Authority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ gates.Authority = (*Authority)(nil)

// NewAuthority requires both collaborators; there is no default grant.
func NewAuthority(a identity.Authority, r identity.AccessReader) (*Authority, error) {
	if a == nil || r == nil {
		return nil, errors.New("gates/adapters: an authority and an access reader are required")
	}
	return &Authority{authority: a, reader: r}, nil
}

// ResolveActor implements gates.Authority.
func (a *Authority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (gates.Actor, error) {
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return gates.Actor{}, gates.ErrHidden
	}
	bindings, err := a.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return gates.Actor{}, translate(err)
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	for _, b := range bindings {
		if b.Applies(target) {
			return gates.Actor{ID: principalID, Type: gates.ActorUser}, nil
		}
	}
	return gates.Actor{}, gates.ErrHidden
}

// Authorize implements gates.Authority.
func (a *Authority) Authorize(ctx context.Context, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	return authorize(ctx, a.authority, actor, scope, perm)
}

// TxAuthority is the permission check bound to one open tenant transaction: it
// reads current bindings through the transaction and holds the user's
// authorization revision FOR SHARE until it ends, so a revocation committed
// while a decision or consumption waited on a lock is seen, and a revocation
// racing a running decision waits for it.
type TxAuthority struct{ a identitypg.TxAuthority }

var _ gates.TxAuthority = TxAuthority{}

// Authorize implements gates.TxAuthority.
func (t TxAuthority) Authorize(ctx context.Context, actor gates.Actor, scope domain.Scope, perm gates.Permission) error {
	return authorize(ctx, t.a, actor, scope, perm)
}

// BindAuthority is the binder the PostgreSQL store takes.
func BindAuthority(tx pgx.Tx) gates.TxAuthority {
	return TxAuthority{a: identitypg.NewTxAuthority(tx)}
}
