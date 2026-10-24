package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

// Keyed joins a decision to the keyed (idempotent) transaction of the HTTP
// foundation: the decision, its audit row, its event and the stored response
// commit or roll back together, so a lost response is recovered by repeating
// the request with the same key and never records a second decision. A replay
// is authorized again, outside any transaction, before the stored response is
// released.
type Keyed struct {
	idem  *httppg.Idempotency
	store *Store
}

// NewKeyed wires the helper.
func NewKeyed(idem *httppg.Idempotency, store *Store) (*Keyed, error) {
	if idem == nil || store == nil {
		return nil, errors.New("gates/postgres: idempotency adapter and store are required")
	}
	return &Keyed{idem: idem, store: store}, nil
}

// DecideKeyed runs the decision exactly once for the request's idempotency
// identity. render builds the success response from the committed gate. The
// returned gate is nil for a replay.
func (k *Keyed) DecideKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *gates.DecisionPlan, render func(*gates.Gate) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *gates.Gate, error) {
	var decided *gates.Gate
	res, err := k.idem.Execute(ctx, httppg.Request{
		Identity:        id,
		AuthorizeReplay: authorizeReplay,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			g, err := k.store.DecideInTx(ctx, tx, plan)
			if err != nil {
				return httpapi.Outcome{}, err
			}
			decided = g
			return render(g)
		},
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	return res.Response, res.Replayed, decided, nil
}
