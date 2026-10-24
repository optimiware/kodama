package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// Session is the verified bearer of one runtime request: what the transaction
// repeats under lock. Nothing in it is a decision; it is the input of
// runtimeauth.CheckSession.
type Session struct {
	Tenant domain.TenantID
	ID     runtimeauth.SessionID
	// Generation is the signing generation of the verified token.
	Generation int64
	// Route is the attempt named by the request path.
	Route  domain.AttemptID
	Action runtimeauth.Action
}

type sessionKey struct{}

// WithSession returns a context under which the guarded stores repeat the
// session check inside their transactions.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session attached by WithSession.
func SessionFrom(ctx context.Context) (Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(Session)
	return s, ok
}

// Guard repeats runtimeauth.CheckSession inside a caller-owned transaction.
type Guard struct {
	authority  rtpg.AttemptAuthority
	generation func() int64
	clock      func() time.Time
}

// NewGuard requires every dependency. generation reports the signing
// generation in force when the check runs, so a rotation that raced the
// request denies it.
func NewGuard(authority rtpg.AttemptAuthority, generation func() int64, clock func() time.Time) (*Guard, error) {
	if authority == nil || generation == nil {
		return nil, errors.New("results/postgres: an attempt authority and a signing generation are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Guard{authority: authority, generation: generation, clock: clock}, nil
}

// Check applies every session check in tx and returns the grant. The caller
// must already hold the execution locks its mutation needs; CheckSession then
// takes the attempt lock (a no-op under a held run lock) and the grant FOR
// SHARE, which blocks revocation until the transaction ends.
func (g *Guard) Check(ctx context.Context, tx pgx.Tx, scope domain.Scope, s Session) (runtimeauth.AdmissionGrant, error) {
	if s.Tenant != scope.TenantID {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another tenant"))
	}
	grant, _, err := runtimeauth.CheckSession(ctx, rtpg.Bind(tx, scope.TenantID, g.authority), runtimeauth.SessionCheck{
		Route: s.Route, Session: s.ID, TokenGeneration: s.Generation, CurrentGeneration: g.generation(), Action: s.Action, Now: g.clock(),
	})
	if err != nil {
		return runtimeauth.AdmissionGrant{}, err
	}
	if grant.Scope != scope {
		return runtimeauth.AdmissionGrant{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another project"))
	}
	return grant, nil
}
