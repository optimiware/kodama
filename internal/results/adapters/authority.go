// Package adapters binds the results module's consumer-owned ports to the
// execution, artifact, workspace and runtime-authority modules. Nothing here
// holds business rules: each adapter translates identifiers and error classes
// between two owners and keeps provider and driver details out of the
// application layer.
package adapters

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/results"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Generations reports the signing generation in force. runtimeauth's token
// issuer (jwt.KeyRing, Signer) satisfies it.
type Generations interface{ Generation() int64 }

// Authority implements results.Authority over runtimeauth. Scope, run, attempt
// and session come from the verified token and the durable grant; the request
// supplies only the bearer and the route's attempt.
type Authority struct {
	verifier runtimeauth.TokenVerifier
	gens     Generations
	store    runtimeauth.Store
	clock    func() time.Time
}

var _ results.Authority = (*Authority)(nil)

// NewAuthority requires every dependency; there is no allow-all fallback.
func NewAuthority(verifier runtimeauth.TokenVerifier, gens Generations, store runtimeauth.Store, clock func() time.Time) (*Authority, error) {
	if verifier == nil || gens == nil || store == nil {
		return nil, errors.New("results/adapters: a token verifier, a signing generation and a grant store are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Authority{verifier: verifier, gens: gens, store: store, clock: clock}, nil
}

var allowed = map[results.Action]runtimeauth.Action{
	results.ActionSourcePropose: runtimeauth.ActionSourcePropose,
	results.ActionResultSubmit:  runtimeauth.ActionResultSubmit,
}

// Authenticate verifies the bearer and applies runtimeauth.CheckSession in a
// tenant transaction. result.submit keeps the receipt exception CheckSession
// implements: after the attempt leaves active, an unrevoked, still-bound grant
// reaches execution, which answers the original acknowledgment or conflict.
func (a *Authority) Authenticate(ctx context.Context, req results.AuthRequest) (results.Principal, error) {
	action, ok := allowed[req.Action]
	if !ok {
		return results.Principal{}, results.Fail(results.ErrForbidden, nil, "the action is not granted")
	}
	claims, err := a.verifier.Verify(ctx, req.Bearer)
	if err != nil {
		return results.Principal{}, MapAuthority(err)
	}
	var p results.Principal
	err = a.store.Do(ctx, claims.Tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		sess := rpg.Session{Tenant: claims.Tenant, ID: claims.Session, Generation: claims.Generation, Route: req.Attempt, Action: action}
		g, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
			Route: sess.Route, Session: sess.ID, TokenGeneration: sess.Generation, CurrentGeneration: a.gens.Generation(), Action: action, Now: a.clock(),
		})
		if err != nil {
			return err
		}
		if g.Scope.TenantID != claims.Tenant {
			return errors.Join(runtimeauth.ErrInvalidCredential, errors.New("token of another tenant"))
		}
		if req.Wire != 0 && g.Wire() != req.Wire {
			// An attempt of another runtime wire: nothing is read or changed.
			return errors.Join(runtimeauth.ErrContextVersionMismatch, errors.New("the attempt belongs to another runtime version"))
		}
		p = results.Principal{Scope: g.Scope, RunID: g.RunID, AttemptID: g.AttemptID, Action: req.Action, Proof: sess}
		return nil
	})
	if err != nil {
		return results.Principal{}, MapAuthority(err)
	}
	return p, nil
}

// Bind implements results.Authority.
func (a *Authority) Bind(ctx context.Context, p results.Principal) (context.Context, error) {
	sess, ok := p.Proof.(rpg.Session)
	if !ok || sess.Route != p.AttemptID {
		return ctx, results.Fail(results.ErrUnauthenticated, nil, "the principal was not issued by this authority")
	}
	return rpg.WithSession(ctx, sess), nil
}

// MapAuthority translates runtimeauth's failure classes. Credential, path and
// grant problems never disclose which of them applied.
func MapAuthority(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrContextVersionMismatch):
		return results.Fail(results.ErrContextVersionMismatch, err, "the attempt belongs to another runtime version")
	case errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return results.Fail(results.ErrAttemptRevoked, err, "the attempt is no longer authorized")
	case errors.Is(err, runtimeauth.ErrForbidden):
		return results.Fail(results.ErrForbidden, err, "the action is not granted")
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return results.Fail(results.ErrUnauthenticated, err, "the credential is not valid")
	}
	return results.Fail(results.ErrUnavailable, err, "the authority could not decide")
}

func isAuthority(err error) bool {
	for _, k := range []error{runtimeauth.ErrContextVersionMismatch, runtimeauth.ErrInvalidCredential, runtimeauth.ErrForbidden, runtimeauth.ErrAttemptRevoked, runtimeauth.ErrNotFound, runtimeauth.ErrInvalidRequest} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}
