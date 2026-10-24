// Package adapters bridges the MCP proxy to the merged modules it consumes:
// identity (management permissions), runtimeauth (workload authority), agents
// and execution (the pinned profile policy) and secrets (exact credential
// versions and run pins). The bridges are the only place where those modules'
// types meet the proxy's; the proxy's domain and use cases import none of them.
package adapters

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// RuntimeAuthority implements mcpproxy.RuntimeAuthority over runtimeauth and
// supplies the in-transaction recheck the proxy's PostgreSQL store runs at
// binding and call admission. Every request, whatever its MCP method, asks for
// the existing tool.invoke action: initialization, discovery, streams,
// notifications and termination included.
type RuntimeAuthority struct {
	svc        *runtimeauth.Service
	attempts   rtpg.AttemptAuthority
	generation func() int64
}

var _ mcpproxy.RuntimeAuthority = (*RuntimeAuthority)(nil)

// NewRuntimeAuthority requires the runtime-authority service, the execution
// owner's attempt authority (the same one runtimeauth's store uses, so lock
// order is shared) and the current signing generation.
func NewRuntimeAuthority(svc *runtimeauth.Service, attempts rtpg.AttemptAuthority, generation func() int64) (*RuntimeAuthority, error) {
	if svc == nil || attempts == nil || generation == nil {
		return nil, errors.New("mcpproxy/adapters: a runtime authority service, an attempt authority and a signing generation are required")
	}
	return &RuntimeAuthority{svc: svc, attempts: attempts, generation: generation}, nil
}

func mapRuntime(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound):
		return mcpproxy.ErrUnauthenticated
	case errors.Is(err, runtimeauth.ErrAttemptRevoked), errors.Is(err, runtimeauth.ErrForbidden):
		return mcpproxy.ErrDenied
	}
	return mcpproxy.Unavailablef(err, "the runtime authority could not answer")
}

// Authorize verifies the bearer for tool.invoke on the route's attempt and
// returns the scope, run, attempt and grant session of the durable grant.
func (a *RuntimeAuthority) Authorize(ctx context.Context, bearer string, attempt domain.AttemptID) (mcpproxy.Caller, error) {
	au, err := a.svc.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: bearer, Action: runtimeauth.ActionToolInvoke})
	if err != nil {
		return mcpproxy.Caller{}, mapRuntime(err)
	}
	return mcpproxy.Caller{Scope: au.Scope, RunID: au.RunID, AttemptID: au.AttemptID, Session: string(au.Session)}, nil
}

// Recheck applies runtimeauth.CheckSession inside the caller's transaction: the
// attempt's execution state is re-read and locked first, the grant FOR SHARE
// second, then session, revision, allowed actions and lifetime are checked. The
// grant's own signing generation is compared with the one in force, so a
// retired key ends authority at once. It satisfies postgres.RecheckFunc.
func (a *RuntimeAuthority) Recheck(ctx context.Context, tx pgx.Tx, c mcpproxy.Caller, now time.Time) error {
	gtx := rtpg.Bind(tx, c.Scope.TenantID, a.attempts)
	first, err := gtx.GrantBySession(ctx, runtimeauth.SessionID(c.Session), runtimeauth.LockNone)
	if err != nil {
		return mapRuntime(err)
	}
	g, _, err := runtimeauth.CheckSession(ctx, gtx, runtimeauth.SessionCheck{
		Route: c.AttemptID, Session: runtimeauth.SessionID(c.Session), TokenGeneration: first.Generation,
		CurrentGeneration: a.generation(), Action: runtimeauth.ActionToolInvoke, Now: now,
	})
	if err != nil {
		return mapRuntime(err)
	}
	if g.Scope != c.Scope || g.RunID != c.RunID || g.AttemptID != c.AttemptID {
		return mcpproxy.ErrUnauthenticated
	}
	return nil
}
