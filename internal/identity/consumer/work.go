// Package consumer bridges the identity authority to the ports other modules
// own, without changing those modules: work.Authority for the work service and a
// transaction-bound work.PermissionChecker for its mutating transactions.
//
// The bridges translate identity outcomes to the consumer's documented
// sentinels: hidden -> work.ErrHidden, denied -> work.ErrDenied, anything else
// (including an unknown permission or an authority outage) -> work.ErrUnavailable
// or denial, never access. Later consumers (logs, streams, source, artifacts,
// human gates) ask the same identity.Authority seam directly with the
// permissions of the fixed matrix; the resource owner stays responsible for the
// scoped lookup of its own resources, and identity never reads another module's
// tables.
package consumer

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/work"
)

// workPermissions is the explicit mapping from the work module's permission names
// to the identity matrix. A permission missing here is denied.
var workPermissions = map[work.Permission]identity.Permission{
	work.PermRead:          identity.PermWorkRead,
	work.PermCreate:        identity.PermWorkCreate,
	work.PermEditSpec:      identity.PermWorkEditSpec,
	work.PermEditTechnical: identity.PermWorkEditTechnical,
	work.PermRefine:        identity.PermWorkRefine,
	work.PermWork:          identity.PermWorkWork,
	work.PermComplete:      identity.PermWorkComplete,
	work.PermReopen:        identity.PermWorkReopen,
	work.PermCancel:        identity.PermWorkCancel,
}

func identityActor(a work.Actor) identity.Actor {
	return identity.Actor{ID: a.ID, Type: identity.ActorType(a.Type)}
}

// translate maps an identity outcome to work's sentinels.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return work.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return work.ErrDenied
	}
	return work.Unavailablef(err, "the identity authority is unavailable")
}

func authorize(ctx context.Context, a identity.Authority, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	p, ok := workPermissions[perm]
	if !ok {
		return work.ErrDenied // unknown permissions deny by default
	}
	target := identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	return translate(a.Authorize(ctx, identityActor(actor), target, p))
}

// WorkAuthority implements work.Authority over the selected identity.Authority
// and the current-binding reader (for actor resolution).
type WorkAuthority struct {
	authority identity.Authority
	reader    identity.AccessReader
}

var _ work.Authority = (*WorkAuthority)(nil)

// NewWorkAuthority requires both collaborators; there is no default grant.
func NewWorkAuthority(a identity.Authority, r identity.AccessReader) (*WorkAuthority, error) {
	if a == nil || r == nil {
		return nil, errors.New("identity/consumer: an authority and an access reader are required")
	}
	return &WorkAuthority{authority: a, reader: r}, nil
}

// ResolveActor resolves a verified principal ID (a user ID) to a human actor when
// the user holds a current binding in the scope's tenant; otherwise the tenant is
// hidden from them. The actor type is always the identity authority's decision,
// never the identifier's spelling.
func (w *WorkAuthority) ResolveActor(ctx context.Context, scope domain.Scope, principalID string) (work.Actor, error) {
	user := identity.UserID(principalID)
	if err := user.Validate(); err != nil {
		return work.Actor{}, work.ErrHidden
	}
	bindings, err := w.reader.CurrentBindings(ctx, scope.TenantID, user)
	if err != nil {
		return work.Actor{}, translate(err)
	}
	for _, b := range bindings {
		if b.Applies(identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}) {
			return work.Actor{ID: principalID, Type: work.ActorUser}, nil
		}
	}
	return work.Actor{}, work.ErrHidden
}

// Authorize implements work.PermissionChecker against current state.
func (w *WorkAuthority) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	return authorize(ctx, w.authority, actor, scope, perm)
}

// TxPermissions is the work.PermissionChecker bound to one mutating transaction:
// it reads the bindings through that transaction and holds the user's
// authorization revision FOR SHARE until the transaction ends, so a revocation
// committed while the mutation waited on a lock is seen, and a revocation
// racing a running mutation waits for it.
type TxPermissions struct{ a identitypg.TxAuthority }

var _ work.PermissionChecker = TxPermissions{}

// NewTxPermissions binds the checker to tx (which carries the tenant context).
func NewTxPermissions(tx pgx.Tx) TxPermissions {
	return TxPermissions{a: identitypg.NewTxAuthority(tx)}
}

// Authorize implements work.PermissionChecker.
func (t TxPermissions) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	return authorize(ctx, t.a, actor, scope, perm)
}

// BindPermissions completes a work guard binder with identity-backed
// permissions: the returned binder is what work/postgres.NewStore takes. The
// remaining guards (writers, dependencies, subjects) come from next.
func BindPermissions(next func(tx pgx.Tx) work.TxGuards) func(tx pgx.Tx) work.TxGuards {
	return func(tx pgx.Tx) work.TxGuards {
		g := work.TxGuards{}
		if next != nil {
			g = next(tx)
		}
		g.Permissions = NewTxPermissions(tx)
		return g
	}
}
