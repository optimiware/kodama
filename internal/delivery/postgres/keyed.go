package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

// Keyed joins the human-authorized deployment creation to the keyed (idempotent)
// create transaction of the HTTP foundation: the deployment, its history, audit,
// event and the stored (metadata-only) response commit or roll back together.
// A lost commit acknowledgment is recovered by repeating the request with the
// same key, which replays the stored response without running the registration.
type Keyed struct {
	idem  *httppg.Idempotency
	store *Store
}

// NewKeyed requires the idempotency adapter and the store.
func NewKeyed(idem *httppg.Idempotency, store *Store) (*Keyed, error) {
	if idem == nil || store == nil {
		return nil, errors.New("delivery/postgres: idempotency adapter and store are required")
	}
	return &Keyed{idem: idem, store: store}, nil
}

// RegisterKeyed runs the registration exactly once for the request's idempotency
// identity. A replay is authorized again through authorizeReplay (against the
// deployment) before the stored response is released, and never runs the
// registration. render builds the success response from the committed or
// observed deployment.
func (k *Keyed) RegisterKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	cmd delivery.RegisterCommand, render func(delivery.RegisterResult) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, delivery.RegisterResult, error) {
	if err := checkRegister(cmd); err != nil {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, err
	}
	// The human path only: a workload mutation must run through Store.Register,
	// whose transaction repeats the runtime session check. The keyed transaction
	// has no session guard, so a runtime command is refused rather than run
	// without one.
	if cmd.Mutation.Runtime || cmd.Mutation.Actor.Type != delivery.ActorUser {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{},
			delivery.Fail(delivery.ErrDenied, nil, "keyed creation is for authorized human callers")
	}
	if id.TenantID != cmd.Deployment.Scope().TenantID || id.ProjectID != cmd.Deployment.Scope().ProjectID {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{},
			delivery.Fail(delivery.ErrDenied, nil, "the request identity is not of the deployment's project")
	}
	var res delivery.RegisterResult
	out, err := k.idem.Execute(ctx, httppg.Request{
		Identity:        id,
		AuthorizeReplay: authorizeReplay,
		Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			r, err := k.store.registerTx(ctx, tx, cmd)
			if err != nil {
				return httpapi.Outcome{}, classify(err)
			}
			res = r
			return render(r)
		},
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, delivery.RegisterResult{}, err
	}
	return out.Response, out.Replayed, res, nil
}
