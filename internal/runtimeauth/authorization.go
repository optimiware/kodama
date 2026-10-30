package runtimeauth

import (
	"context"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Authorization is the verified authority of a workload for one action.
// Consumers (secret, artifact, source, result routes and later gateways) use
// the identifiers it carries, never ones from the request.
//
// An Authorization is a decision at a moment, not a durable permit: a
// mutation that must not race with cancellation takes the attempt lock itself
// (postgres.LockAndCheck) in its own transaction.
type Authorization struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Session   SessionID
	Action    Action
	// Actions is the effective action set at the time of the decision.
	Actions ActionSet
	// ContextVersion is the runtime wire the attempt belongs to (WireV1 or
	// WireV2), fixed by its pinned context.
	ContextVersion int64
	// TokenGeneration is the signing generation of the verified token. Together
	// with Session, Action and the route it is everything a mutation needs to
	// recheck this authority inside its own transaction (SessionCheck).
	TokenGeneration int64
}

// SessionCheck is the check that re-verifies this authorization inside a
// transaction (CheckSession through postgres.Bind): the seam an execution
// mutation that must not race with revocation calls in its own transaction.
func (a Authorization) SessionCheck(currentGeneration int64, now time.Time) SessionCheck {
	return SessionCheck{Route: a.AttemptID, Session: a.Session, TokenGeneration: a.TokenGeneration, CurrentGeneration: currentGeneration, Action: a.Action, Now: now}
}

// SessionCheck is one authorization decision over a token's claims.
type SessionCheck struct {
	// Route is the attempt named by the request path.
	Route domain.AttemptID
	// Session and TokenGeneration come from the verified token.
	Session         SessionID
	TokenGeneration int64
	// CurrentGeneration is the signing generation in force now.
	CurrentGeneration int64
	Action            Action
	Now               time.Time
}

// CheckSession applies every authority check inside the caller's transaction
// and in the module-wide lock order: the attempt's execution state is re-read
// and locked first, then the grant is locked FOR SHARE. It checks the session,
// the route attempt, the signing generation, the attempt phase and authority
// revision, the attempt's currently allowed actions, the grant ceiling and the
// session lifetime.
//
// Protected mutations that must not race with cancellation call this from
// their own transaction (see postgres.Bind) and perform the mutation before
// committing: a check made earlier by middleware is not a permit.
func CheckSession(ctx context.Context, tx GrantTx, c SessionCheck) (AdmissionGrant, AttemptState, error) {
	first, err := tx.GrantBySession(ctx, c.Session, LockNone)
	if err != nil {
		return AdmissionGrant{}, AttemptState{}, missing(err)
	}
	if first.AttemptID != c.Route {
		return AdmissionGrant{}, AttemptState{}, fail(ErrInvalidCredential, "the token is not valid for this attempt")
	}
	state, err := tx.LockAttempt(ctx, first.Scope, first.AttemptID)
	if err != nil {
		return AdmissionGrant{}, AttemptState{}, missing(err)
	}
	if err := state.Validate(); err != nil {
		return AdmissionGrant{}, AttemptState{}, err
	}
	g, err := tx.GrantBySession(ctx, c.Session, LockShare)
	if err != nil {
		return AdmissionGrant{}, AttemptState{}, missing(err)
	}
	if g.AttemptID != first.AttemptID || g.Scope != first.Scope {
		return AdmissionGrant{}, AttemptState{}, fail(ErrUnavailable, "the grant changed during the check")
	}
	if c.TokenGeneration != g.Generation || g.Generation != c.CurrentGeneration {
		return AdmissionGrant{}, AttemptState{}, fail(ErrInvalidCredential, "the token's signing generation is retired")
	}
	// Result receipt replay is not a new control action. An accepted result makes
	// the attempt terminal, and the frozen contract still requires the original
	// 200 or 409 result_conflict. Explicit revocation still rejects the callback.
	// Execution distinguishes replay, conflict, and a late result.
	receipt := c.Action == ActionResultSubmit && !g.Revoked() && sameBinding(state, g)
	if !receipt && (g.Revoked() || state.Phase != AttemptActive || !sameBinding(state, g)) {
		return AdmissionGrant{}, AttemptState{}, fail(ErrAttemptRevoked, "the attempt is no longer authorized")
	}
	if !state.Actions.Contains(c.Action) {
		return AdmissionGrant{}, AttemptState{}, fail(ErrForbidden, "the action is not currently allowed")
	}
	if err := g.Authorize(c.Action, c.Now); err != nil {
		return AdmissionGrant{}, AttemptState{}, err
	}
	return g, state, nil
}

// withSession verifies the bearer, then runs CheckSession and fn in one tenant
// transaction.
func (s *Service) withSession(ctx context.Context, route domain.AttemptID, bearer string, action Action, wire int64, fn func(g AdmissionGrant, now time.Time) error) (Authorization, error) {
	var out Authorization
	claims, err := s.verifyAttempt(ctx, bearer)
	if err != nil {
		return out, err
	}
	err = s.cfg.Store.Do(ctx, claims.Tenant, func(ctx context.Context, tx GrantTx) error {
		now := s.now()
		g, state, err := CheckSession(ctx, tx, SessionCheck{
			Route: route, Session: claims.Session, TokenGeneration: claims.Generation,
			CurrentGeneration: s.cfg.Tokens.Generation(), Action: action, Now: now,
		})
		if err != nil {
			return err
		}
		if g.Scope.TenantID != claims.Tenant {
			return fail(ErrInvalidCredential, "the token is not valid for this attempt")
		}
		if g.Wire() != wire {
			// Another runtime wire's attempt: nothing is issued or changed.
			return fail(ErrContextVersionMismatch, "the attempt belongs to another runtime version")
		}
		out = Authorization{Scope: g.Scope, RunID: g.RunID, AttemptID: g.AttemptID, Session: claims.Session, Action: action, Actions: intersect(g.Actions, state.Actions), ContextVersion: wire, TokenGeneration: claims.Generation}
		if fn != nil {
			return fn(g, now)
		}
		return nil
	})
	if err != nil {
		return Authorization{}, err
	}
	return out, nil
}

func intersect(a, b ActionSet) ActionSet {
	var keep []Action
	for _, x := range a.actions {
		if b.Contains(x) {
			keep = append(keep, x)
		}
	}
	return ActionSet{actions: keep}
}

// AuthorizeRequest is a bearer's request to use an action on an attempt.
type AuthorizeRequest struct {
	AttemptID domain.AttemptID
	Bearer    string
	Action    Action
	// ContextVersion is the runtime wire of the route being served: WireV1 (the
	// zero value) or WireV2. An attempt of the other wire is
	// ErrContextVersionMismatch before anything is done.
	ContextVersion int64
}

// Authorize decides whether the bearer may use req.Action on req.AttemptID now.
func (s *Service) Authorize(ctx context.Context, req AuthorizeRequest) (a Authorization, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "authorize", req.AttemptID, err) }()
	if !knownAction(req.Action) {
		return Authorization{}, fail(ErrForbidden, "the action is not granted")
	}
	if err := req.AttemptID.Validate(); err != nil {
		return Authorization{}, fail(ErrInvalidCredential, "the token is not valid for this attempt")
	}
	wire := req.ContextVersion
	if wire == 0 {
		wire = WireV1
	}
	if wire != WireV1 && wire != WireV2 {
		return Authorization{}, fail(ErrInvalidRequest, "unknown runtime wire")
	}
	return s.withSession(ctx, req.AttemptID, req.Bearer, req.Action, wire, nil)
}

func knownAction(a Action) bool {
	_, err := ParseAction(string(a))
	return err == nil
}

// Renew issues a fresh token for a session that is still authorized. The
// expiry is clamped to the maximum fixed at first admission; a renewal never
// moves it.
func (s *Service) Renew(ctx context.Context, route domain.AttemptID, bearer string) (resp runtimev1.TokenResponse, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "renew", route, err) }()
	return s.renew(ctx, route, bearer, WireV1)
}

// RenewV2 is Renew on the run_shared wire (the same attempt token, audience
// kodama-runtime).
func (s *Service) RenewV2(ctx context.Context, route domain.AttemptID, bearer string) (resp runtimev2.TokenResponse, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "renew", route, err) }()
	return s.renew(ctx, route, bearer, WireV2)
}

func (s *Service) renew(ctx context.Context, route domain.AttemptID, bearer string, wire int64) (resp runtimev1.TokenResponse, err error) {
	if err := route.Validate(); err != nil {
		return resp, fail(ErrInvalidCredential, "the token is not valid for this attempt")
	}
	_, err = s.withSession(ctx, route, bearer, ActionTokenRenew, wire, func(g AdmissionGrant, now time.Time) error {
		exp, err := g.TokenExpiry(now, s.cfg.Policy.TokenTTL)
		if err != nil {
			return err
		}
		token, err := s.cfg.Tokens.Issue(IssueRequest{
			Tenant: g.Scope.TenantID, Session: g.Consumption.SessionID, Generation: g.Generation, IssuedAt: now, ExpiresAt: exp,
		})
		if err != nil {
			return err
		}
		resp = runtimev1.TokenResponse{Token: tokenInfo(token, exp, g.Consumption.MaxExpiresAt)}
		return nil
	})
	if err != nil {
		return runtimev1.TokenResponse{}, err
	}
	if err := resp.Validate(); err != nil {
		return runtimev1.TokenResponse{}, failWith(ErrUnavailable, err, "the response is not a valid token")
	}
	return resp, nil
}

// Context returns the pinned context of the attempt, unchanged.
func (s *Service) Context(ctx context.Context, route domain.AttemptID, bearer string) (c runtimev1.Context, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "context", route, err) }()
	if err := route.Validate(); err != nil {
		return c, fail(ErrInvalidCredential, "the token is not valid for this attempt")
	}
	_, err = s.withSession(ctx, route, bearer, ActionContextRead, WireV1, func(g AdmissionGrant, _ time.Time) error {
		var derr error
		c, derr = decodeContext(g.Context)
		return derr
	})
	if err != nil {
		return runtimev1.Context{}, err
	}
	return c, nil
}

// ContextV2 returns the pinned ContextV2 of a run_shared attempt, unchanged.
func (s *Service) ContextV2(ctx context.Context, route domain.AttemptID, bearer string) (c runtimev2.ContextV2, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "context", route, err) }()
	if err := route.Validate(); err != nil {
		return c, fail(ErrInvalidCredential, "the token is not valid for this attempt")
	}
	_, err = s.withSession(ctx, route, bearer, ActionContextRead, WireV2, func(g AdmissionGrant, _ time.Time) error {
		var derr error
		c, derr = decodeContextV2(g.Context)
		return derr
	})
	if err != nil {
		return runtimev2.ContextV2{}, err
	}
	return c, nil
}

// Revoke irreversibly denies every grant of the attempt. It is idempotent and
// makes no claim about external effects the workload already started: it ends
// authority to start new ones through Kodama's interfaces (PX-08). Execution
// normally revokes inside its own cancellation transaction through
// postgres.RevokeAttempt; this entry point serves callers without one.
func (s *Service) Revoke(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "revoke", attempt, err) }()
	if err := firstErr(scope.Validate(), attempt.Validate()); err != nil {
		return fail(ErrInvalidRequest, "%v", err)
	}
	return s.cfg.Store.Do(ctx, scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		_, err := tx.RevokeAttempt(ctx, scope, attempt, s.now())
		return err
	})
}

// Reauthorize re-verifies, now and in one tenant transaction (the module-wide lock
// order), that the session of a previous authorization is still current: the
// grant is not revoked, the attempt is active and still holds the action, the
// signing generation is current and the attempt still belongs to the wire. A
// handler calls it after reading a request body and immediately before a durable
// mutation, because an authorization decided before the body arrived is a
// decision at a moment, not a permit. A mutation that must be atomic with
// revocation additionally runs CheckSession in its own transaction.
func (s *Service) Reauthorize(ctx context.Context, a Authorization) (err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "reauthorize", a.AttemptID, err) }()
	if a.Session.Validate() != nil || a.Scope.Validate() != nil || a.AttemptID.Validate() != nil {
		return fail(ErrInvalidCredential, "the authorization is malformed")
	}
	return s.cfg.Store.Do(ctx, a.Scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		g, _, err := CheckSession(ctx, tx, a.SessionCheck(s.cfg.Tokens.Generation(), s.now()))
		if err != nil {
			return err
		}
		if g.Scope != a.Scope || g.AttemptID != a.AttemptID || g.Wire() != a.ContextVersion {
			return fail(ErrInvalidCredential, "the authorization is not this session's")
		}
		return nil
	})
}
