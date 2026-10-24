package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

// Keyed joins run creation to the keyed (idempotent) create transaction of the
// HTTP foundation: the pinned run, its history, audit and outbox rows, its
// secret pins, its retained-source intent and the stored response commit or
// roll back together. A lost commit acknowledgment is recovered by repeating
// the request with the same key: it replays the stored response without
// resolving new inputs.
type Keyed struct {
	idem  *httppg.Idempotency
	store *Store
}

// NewKeyed wires the helper.
func NewKeyed(idem *httppg.Idempotency, store *Store) (*Keyed, error) {
	if idem == nil || store == nil {
		return nil, errors.New("execution/postgres: idempotency adapter and store are required")
	}
	return &Keyed{idem: idem, store: store}, nil
}

// CreateKeyed runs the admission exactly once for the request's idempotency
// identity. A replay is authorized again through authorizeReplay (against the
// created run) before the stored response is released, and never runs the
// admission. render builds the success response from the committed run.
func (k *Keyed) CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *execution.AdmissionPlan, render func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error) {
	var created *execution.Run
	res, err := k.idem.Execute(ctx, httppg.Request{
		Identity:        id,
		AuthorizeReplay: authorizeReplay,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			run, _, err := k.store.AdmitInTx(ctx, tx, plan)
			if err != nil {
				return httpapi.Outcome{}, err
			}
			created = run
			return render(run)
		},
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	return res.Response, res.Replayed, created, nil
}
