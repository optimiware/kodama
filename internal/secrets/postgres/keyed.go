package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/secrets"
)

// Keyed runs a management write atomically with its idempotency record: the
// business state, audit row, event and the stored (metadata-only) response
// commit or roll back together. It adapts T-004's keyed transaction helper to
// the module's Writer so HTTP handlers never see pgx.
type Keyed struct {
	idem  *httppg.Idempotency
	store *Store
}

// NewKeyed wires the helper.
func NewKeyed(idem *httppg.Idempotency, store *Store) (*Keyed, error) {
	if idem == nil || store == nil {
		return nil, errors.New("secrets/postgres: idempotency adapter and store are required")
	}
	return &Keyed{idem: idem, store: store}, nil
}

// Execute runs do exactly once for the request's idempotency identity. A replay
// is authorized again through authorizeReplay before the stored response is
// returned and never runs do. An unknown commit is a 503 wrapping
// httppg.ErrOutcomeUnknown; retrying with the same key replays or executes.
func (k *Keyed) Execute(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(ctx context.Context, ref httpapi.Reference) error,
	do func(ctx context.Context, w secrets.Writer) (httpapi.Outcome, error)) (response httpapi.StoredResponse, replayed bool, err error) {
	res, err := k.idem.Execute(ctx, httppg.Request{
		Identity:        id,
		AuthorizeReplay: authorizeReplay,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			return do(ctx, &writer{s: k.store, tx: tx, tenant: id.TenantID})
		},
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, err
	}
	return res.Response, res.Replayed, nil
}
