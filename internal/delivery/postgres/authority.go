package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// Session is the verified bearer of one runtime request: what the transaction
// repeats under lock. It is the input of runtimeauth.CheckSession, not a
// decision.
type Session struct {
	Tenant domain.TenantID
	ID     runtimeauth.SessionID
	// Generation is the signing generation of the verified token.
	Generation int64
	// Route is the attempt named by the request path.
	Route domain.AttemptID
}

type sessionKey struct{}

// WithSession returns a context under which runtime mutations repeat the
// session check inside their transactions.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session attached by WithSession.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// Guard repeats runtimeauth.CheckSession for deployment.call inside a
// caller-owned transaction. Unlike result.submit it has no receipt exception:
// a revoked, inactive, wrong-generation or incorrectly bound attempt is denied
// even for a replay.
type Guard struct {
	authority  rtpg.AttemptAuthority
	generation func() int64
	clock      func() time.Time
}

// NewGuard requires every dependency. generation reports the signing generation
// in force when the check runs, so a rotation that raced the request denies it.
func NewGuard(authority rtpg.AttemptAuthority, generation func() int64, clock func() time.Time) (*Guard, error) {
	if authority == nil || generation == nil {
		return nil, errors.New("delivery/postgres: an attempt authority and a signing generation are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Guard{authority: authority, generation: generation, clock: clock}, nil
}

// Check applies every session check in tx: the execution-owned attempt lock
// first (a run lock FOR SHARE that conflicts with cancellation), then the grant
// FOR SHARE, which blocks revocation until the transaction ends. Callers take
// their own delivery locks afterwards, preserving the module-wide lock order.
func (g *Guard) Check(ctx context.Context, tx pgx.Tx, scope domain.Scope, s Session) (runtimeauth.AdmissionGrant, error) {
	if s.Tenant != scope.TenantID {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another tenant"))
	}
	grant, _, err := runtimeauth.CheckSession(ctx, rtpg.Bind(tx, scope.TenantID, g.authority), runtimeauth.SessionCheck{
		Route: s.Route, Session: s.ID, TokenGeneration: s.Generation, CurrentGeneration: g.generation(),
		Action: runtimeauth.ActionDeploymentCall, Now: g.clock(),
	})
	if err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	if grant.Scope != scope {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another project"))
	}
	return grant, nil
}

// mapAuthority translates runtimeauth's failure classes. Credential, path and
// grant problems never disclose which of them applied.
func mapAuthority(err error) error {
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

func isAuthority(err error) bool {
	for _, k := range []error{runtimeauth.ErrInvalidCredential, runtimeauth.ErrForbidden, runtimeauth.ErrAttemptRevoked, runtimeauth.ErrNotFound,
		runtimeauth.ErrInvalidRequest, runtimeauth.ErrUnavailable, runtimeauth.ErrConflict, runtimeauth.ErrAdmissionConsumed, runtimeauth.ErrProfileUnsupported} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}
