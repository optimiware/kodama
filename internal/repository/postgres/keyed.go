package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
	"github.com/optimiware/kodama/internal/repository"
)

// Keyed joins repository creation to the keyed (idempotent) create transaction
// of the HTTP foundation: the reservation, its audit record and the stored
// response commit or roll back together. A lost commit acknowledgment is
// recovered by repeating the request with the same key: it replays the stored
// response without reserving again. Provisioning is not part of the
// transaction: the worker is called after the commit, and the reconciler
// finishes an interrupted initialization.
type Keyed struct {
	idem  *httppg.Idempotency
	store *Store
}

// NewKeyed wires the helper.
func NewKeyed(idem *httppg.Idempotency, store *Store) (*Keyed, error) {
	if idem == nil || store == nil {
		return nil, errors.New("repository/postgres: idempotency adapter and store are required")
	}
	return &Keyed{idem: idem, store: store}, nil
}

// CreateKeyed reserves the repository exactly once for the request's
// idempotency identity. A replay is authorized again through authorizeReplay
// (against the created repository) before the stored response is released, and
// never inserts. render builds the success response from the committed
// repository and whether this call created it; an existing equivalent
// repository of the same name is returned (created false), a different one is
// repository_name_taken, and neither stores anything but the response.
func (k *Keyed) CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *repository.CreatePlan, render func(r *repository.Repository, created bool) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *repository.Repository, error) {
	var out *repository.Repository
	res, err := k.idem.Execute(ctx, httppg.Request{
		Identity:        id,
		AuthorizeReplay: authorizeReplay,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			persisted, created, err := k.store.InsertInTx(ctx, tx, plan.Repository, plan.Effects)
			if err != nil {
				return httpapi.Outcome{}, err
			}
			r, created, err := plan.Resolve(persisted, created)
			if err != nil {
				return httpapi.Outcome{}, err
			}
			out = r
			return render(r, created)
		},
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	return res.Response, res.Replayed, out, nil
}
