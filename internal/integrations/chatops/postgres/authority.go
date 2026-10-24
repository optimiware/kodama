package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// TxAuthorizer is the transaction-bound permission check of ChatOps mutations
// and reads. It runs inside the operation's own tenant transaction, after the
// project lock and before any ChatOps row is touched, so a revocation orders
// against the operation: the authorization revision it takes stays held until
// the transaction ends. Results use the module's classes: nil allows,
// chatops.ErrNotFound hides the target, chatops.ErrDenied refuses, and any other
// error is an outage that never grants access.
type TxAuthorizer interface {
	Authorize(ctx context.Context, tx pgx.Tx, actor chatops.Actor, scope domain.Scope, perm chatops.Permission) error
}

// IdentityAuthorizer is the standalone binding: T-010's transaction-bound
// authority (identitypg.NewTxAuthority) over Kodama's own memberships. Hosted
// installations supply another TxAuthorizer through the same port; this one is
// never a fallback for it.
type IdentityAuthorizer struct{}

var _ TxAuthorizer = IdentityAuthorizer{}

// The adapter-local mapping to existing identity permissions. There is no new
// role: configuration and replay need project:settings, inspection project:read.
var permissions = map[chatops.Permission]identity.Permission{
	chatops.PermSettings: identity.PermProjectSettings,
	chatops.PermRead:     identity.PermProjectRead,
}

// Authorize implements TxAuthorizer.
func (IdentityAuthorizer) Authorize(ctx context.Context, tx pgx.Tx, actor chatops.Actor, scope domain.Scope, perm chatops.Permission) error {
	p, ok := permissions[perm]
	if !ok || actor.Type != chatops.ActorUser {
		return chatops.Fail(chatops.ErrDenied, nil, "the permission is not known")
	}
	err := identitypg.NewTxAuthority(tx).Authorize(ctx,
		identity.Actor{ID: actor.ID, Type: identity.ActorUser},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return chatops.Fail(chatops.ErrNotFound, err, "not found")
	case errors.Is(err, identity.ErrDenied):
		return chatops.Fail(chatops.ErrDenied, err, "the caller lacks the permission")
	}
	return chatops.Fail(chatops.ErrUnavailable, err, "the identity authority is unavailable")
}
