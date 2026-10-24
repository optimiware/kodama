package adapters

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/delivery"
	deliverypg "github.com/optimiware/kodama/internal/delivery/postgres"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Generations reports the signing generation in force. runtimeauth's token
// issuer (jwt.KeyRing, Signer) satisfies it.
type Generations interface{ Generation() int64 }

// Authority implements delivery.Authority over runtimeauth for the
// deployment.call action. Scope, run, attempt and session come from the verified
// token and the durable grant; the request supplies only the bearer and the
// route's attempt. Unlike result.submit there is no receipt exception: a
// terminal, revoked or wrong-generation attempt is denied.
type Authority struct {
	verifier runtimeauth.TokenVerifier
	gens     Generations
	store    runtimeauth.Store
	clock    func() time.Time
}

var _ delivery.Authority = (*Authority)(nil)

// NewAuthority requires every dependency; there is no allow-all fallback.
func NewAuthority(verifier runtimeauth.TokenVerifier, gens Generations, store runtimeauth.Store, clock func() time.Time) (*Authority, error) {
	if verifier == nil || gens == nil || store == nil {
		return nil, errors.New("delivery/adapters: a token verifier, a signing generation and a grant store are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Authority{verifier: verifier, gens: gens, store: store, clock: clock}, nil
}

// Authenticate implements delivery.Authority.
func (a *Authority) Authenticate(ctx context.Context, attempt domain.AttemptID, bearer string) (delivery.Principal, error) {
	claims, err := a.verifier.Verify(ctx, bearer)
	if err != nil {
		return delivery.Principal{}, MapAuthority(err)
	}
	var p delivery.Principal
	err = a.store.Do(ctx, claims.Tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		sess := deliverypg.Session{Tenant: claims.Tenant, ID: claims.Session, Generation: claims.Generation, Route: attempt}
		g, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
			Route: attempt, Session: sess.ID, TokenGeneration: sess.Generation, CurrentGeneration: a.gens.Generation(),
			Action: runtimeauth.ActionDeploymentCall, Now: a.clock(),
		})
		if err != nil {
			return err
		}
		if g.Scope.TenantID != claims.Tenant {
			return errors.Join(runtimeauth.ErrInvalidCredential, errors.New("token of another tenant"))
		}
		p = delivery.Principal{Scope: g.Scope, Run: g.RunID, Attempt: g.AttemptID, Proof: sess}
		return nil
	})
	if err != nil {
		return delivery.Principal{}, MapAuthority(err)
	}
	return p, nil
}

// Bind implements delivery.Authority. A principal this authority did not issue
// is refused: an unbound context would silently skip the in-transaction check.
func (a *Authority) Bind(ctx context.Context, p delivery.Principal) (context.Context, error) {
	sess, ok := p.Proof.(deliverypg.Session)
	if !ok || sess.Route != p.Attempt {
		return ctx, delivery.Fail(delivery.ErrUnauthenticated, nil, "the principal was not issued by this authority")
	}
	return deliverypg.WithSession(ctx, sess), nil
}

// MapAuthority translates runtimeauth's failure classes. Credential, path and
// grant problems never disclose which of them applied.
func MapAuthority(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return delivery.Fail(delivery.ErrAttemptRevoked, err, "the attempt is no longer authorized")
	case errors.Is(err, runtimeauth.ErrForbidden):
		return delivery.Fail(delivery.ErrDenied, err, "the action is not granted")
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return delivery.Fail(delivery.ErrUnauthenticated, err, "the credential is not valid")
	}
	return delivery.Fail(delivery.ErrUnavailable, err, "the authority could not decide")
}
