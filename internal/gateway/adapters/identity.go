// Package adapters bridges the model gateway's consumer-owned ports to the
// peer modules that satisfy them (identity, runtime authority, execution and
// secrets) without changing those modules. Each bridge translates the peer's
// sentinel errors into the gateway's classes: a refusal stays a refusal, a
// missing resource is indistinguishable from a foreign one, and any other
// failure is gateway.ErrUnavailable, never access. Nothing here has an
// allow-all default.
package adapters

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

var permissions = map[gateway.Permission]identity.Permission{
	gateway.PermRead:     identity.PermConfigRead,
	gateway.PermSettings: identity.PermProjectSettings,
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return gateway.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return gateway.ErrDenied
	}
	return gateway.FailWith(gateway.ErrUnavailable, err, "the identity authority is unavailable")
}

func authorize(ctx context.Context, a identity.Authority, actor gateway.Actor, scope domain.Scope, perm gateway.Permission) error {
	p, ok := permissions[perm]
	if !ok {
		return gateway.ErrDenied // unknown permissions deny by default
	}
	return translate(a.Authorize(ctx, identity.Actor{ID: actor.ID, Type: identity.ActorUser},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p))
}

// Authority implements gateway.Authority over the selected identity authority.
type Authority struct{ a identity.Authority }

var _ gateway.Authority = (*Authority)(nil)

// NewAuthority requires the identity authority; there is no default grant and
// no fallback between the standalone and hosted profiles (the selection happens
// in identity.SelectAuthority).
func NewAuthority(a identity.Authority) (*Authority, error) {
	if a == nil {
		return nil, errors.New("gateway/adapters: an identity authority is required")
	}
	return &Authority{a: a}, nil
}

// Authorize implements gateway.Authority.
func (a *Authority) Authorize(ctx context.Context, actor gateway.Actor, scope domain.Scope, perm gateway.Permission) error {
	return authorize(ctx, a.a, actor, scope, perm)
}

type txManagement struct{ a identitypg.TxAuthority }

func (t txManagement) Authorize(ctx context.Context, actor gateway.Actor, scope domain.Scope, perm gateway.Permission) error {
	return authorize(ctx, t.a, actor, scope, perm)
}

// BindManagement is the binder the PostgreSQL store takes: the permission check
// reads current bindings through the settings transaction and holds the user's
// authorization revision FOR SHARE until it ends, so a revocation committed
// while the write waited on a lock is seen and one racing the write waits for
// it.
func BindManagement(tx pgx.Tx) gatewaypg.ManagementAuthority {
	return txManagement{a: identitypg.NewTxAuthority(tx)}
}
