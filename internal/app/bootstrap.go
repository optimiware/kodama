package app

import (
	"context"
	"errors"
	"fmt"

	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// BootstrapRequest is the explicit operator request of the one-time standalone
// bootstrap: the first tenant and its first administrator. There is no route
// for it and no first-request-wins enrollment; only the configured identity,
// verified by the normal token verifier before the administrative transaction
// opens, can complete it.
type BootstrapRequest struct {
	// Env selects the authority construction (SD-12). It is required: the
	// administrator's token is verified by the environment's verifier, and
	// production refuses the OIDC fixture.
	Env Env
	// AdminDatabaseURL is the administrative (owner) identity; the serving
	// runtime role can neither read nor write the completion record.
	AdminDatabaseURL string
	OIDC             OIDCConfig
	Config           identity.BootstrapConfig
	// AccessToken is the eligible administrator's access token.
	AccessToken string
}

// Bootstrap provisions the initial tenant, administrator binding, audit row,
// event and completion record in one administrative transaction. An identical
// retry observes the committed result; changed parameters conflict.
func Bootstrap(ctx context.Context, req BootstrapRequest) (identity.BootstrapResult, error) {
	if req.AdminDatabaseURL == "" || req.AccessToken == "" {
		return identity.BootstrapResult{}, errors.New("bootstrap needs the administrative database URL and the administrator's access token")
	}
	// The environment and the verifier are settled before any connection opens
	// or any credential is verified.
	if req.Env == "" {
		return identity.BootstrapResult{}, ErrEnvRequired
	}
	cons, err := NewConstructors(req.Env)
	if err != nil {
		return identity.BootstrapResult{}, err
	}
	verifier, err := cons.Verifier(req.OIDC)
	if err != nil {
		return identity.BootstrapResult{}, fmt.Errorf("identity verifier: %w", err)
	}
	admin, err := storage.Open(ctx, storage.Config{URL: req.AdminDatabaseURL, MaxConns: 2, ApplicationName: "kodama-bootstrap"})
	if err != nil {
		return identity.BootstrapResult{}, fmt.Errorf("open the administrative database: %w", err)
	}
	defer admin.Close()
	tune := DefaultTuning().Events
	events, err := eventspg.NewStore(admin, eventspg.Options{Backlog: tune.Backlog, Retry: tune.Retry, Retention: tune.Retention, ReplayLease: tune.ReplayLease, ReplayBatch: tune.ReplayBatch})
	if err != nil {
		return identity.BootstrapResult{}, err
	}
	store, err := identitypg.NewBootstrapStore(admin, events)
	if err != nil {
		return identity.BootstrapResult{}, err
	}
	svc, err := identity.NewBootstrapService(req.Config, verifier, store)
	if err != nil {
		return identity.BootstrapResult{}, err
	}
	return svc.Run(ctx, req.AccessToken)
}
