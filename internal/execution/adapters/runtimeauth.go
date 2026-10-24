package adapters

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// RevokeAttemptGrants is the execution store's grant-revocation seam
// (execution/postgres.AttemptGrantRevoker) bound to the runtime authority's
// transaction-joining API. It revokes every runtime grant of the attempt in the
// caller's open tenant transaction: the statement joins tx, never opens another
// transaction, and is scoped by tenant and project. A repeat revokes nothing.
//
// It deliberately does not call runtimeauth.Service.Revoke, which opens its own
// transaction and so would not roll back with the execution mutation.
func RevokeAttemptGrants(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return rtpg.RevokeAttempt(ctx, tx, scope, attempt, now)
}
