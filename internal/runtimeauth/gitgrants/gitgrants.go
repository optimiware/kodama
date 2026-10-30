// Package gitgrants binds the runtime authority's Git ports (T-150) to the Git
// authority (internal/gitauth), the push authorizer (internal/gitpush), execution
// and the gates module. It holds no business rule: each adapter translates
// identifiers and error classes between two owners, and none of them mints
// authority that its provider does not.
//
//   - Grants is execution.GitGrants (the attempt's Git credential, issued before
//     its workload starts and revoked with it) and the renewal of that
//     credential for a still-live attempt.
//   - Leases is gitauth.Attempts and gitpush.Attempts: the liveness and the lease
//     deadline of an attempt, read from the execution record.
//   - Issuer is runtimeauth.GitIssuer: the seed and integration grants issued
//     during the attempt.
//   - Approvals is runtimeauth.IntegrationApprovals over the gates module, and
//     GateDecisions is gitauth.Gates over the same approval.
//
// Nothing here is wired by a composition; T-132 builds the adapters (see
// README.md of internal/runtimeauth).
package gitgrants

import (
	"context"
	"errors"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
)

// Runs reads the execution record of runs and attempts; execution.Store
// implements it. Update is used only for its lock: the run row is taken FOR UPDATE
// (the lock the cancelling transaction also takes) to read an attempt's liveness
// with no cancellation in flight, and nothing is written.
type Runs interface {
	RunOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
	Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error)
}

func clockOrNow(c func() time.Time) func() time.Time {
	if c == nil {
		return time.Now
	}
	return c
}

// ---- liveness ----------------------------------------------------------------------

// Leases answers the Git services' question "is this attempt live, and until
// when" from execution's committed record. It implements gitauth.Attempts (Lease)
// and gitpush.Attempts (Live).
//
// The answer is the record's, read at the moment of the call: an attempt is live
// while it is running, its authority is not revoked (cancellation, failure drain,
// timeout, supersession or an accepted result all end it), its run is neither
// draining nor terminal and the run's deadline has not passed. The lease deadline
// is the run's own deadline, so a grant bounded by it never outlives the lease,
// which is what execution's GitGrants contract requires. Each Git service
// re-reads it at every use of a credential and at every commit intent, so the
// revocation that joins the cancelling transaction is the cleanup and not the
// authority: a cancelled attempt is refused at its next push even before its
// grants are revoked.
type Leases struct {
	runs Runs
	now  func() time.Time
}

var (
	_ gitauth.Attempts = (*Leases)(nil)
	_ gitpush.Attempts = (*Leases)(nil)
)

// NewLeases requires the execution reader; the clock defaults to time.Now.
func NewLeases(runs Runs, clock func() time.Time) (*Leases, error) {
	if runs == nil {
		return nil, errors.New("gitgrants: an execution reader is required")
	}
	return &Leases{runs: runs, now: clockOrNow(clock)}, nil
}

func (l *Leases) lease(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (execbridge.AttemptLease, error) {
	run, err := l.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return execbridge.AttemptLease{}, err
	}
	agg, err := l.runs.Get(ctx, scope, run)
	if err != nil {
		return execbridge.AttemptLease{}, err
	}
	return execbridge.LeaseOf(agg, attempt, l.now())
}

func isAbsent(err error) bool {
	return errors.Is(err, execution.ErrNotFound) || errors.Is(err, execution.ErrHidden) || errors.Is(err, runtimeauth.ErrNotFound)
}

// Lease implements gitauth.Attempts. An unknown attempt of the scope is
// gitauth.ErrNotFound.
func (l *Leases) Lease(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (gitauth.AttemptLease, error) {
	lease, err := l.lease(ctx, scope, attempt)
	if err != nil {
		if isAbsent(err) {
			return gitauth.AttemptLease{}, gitauth.ErrNotFound
		}
		return gitauth.AttemptLease{}, err
	}
	return gitauth.AttemptLease{Live: lease.Live, Deadline: lease.Deadline}, nil
}

// Live implements gitpush.Attempts. An unknown attempt of the scope is
// gitpush.ErrNotFound (not live).
func (l *Leases) Live(ctx context.Context, scope domain.Scope, attemptID string) (bool, error) {
	lease, err := l.lease(ctx, scope, domain.AttemptID(attemptID))
	if err != nil {
		if isAbsent(err) {
			return false, gitpush.ErrNotFound
		}
		return false, err
	}
	return lease.Live, nil
}

// ---- the attempt's own credential ----------------------------------------------------

// Authority is the part of *gitauth.Service the adapters use.
type Authority interface {
	IssueAttempt(ctx context.Context, scope domain.Scope, req gitauth.AttemptGrantRequest) (gitauth.GrantHandle, error)
	IssueIntegration(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (gitauth.GrantHandle, error)
	RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error
	Claims(ctx context.Context, tenant domain.TenantID, grantID string) (gitv1.GrantClaims, error)
}

var _ Authority = (*gitauth.Service)(nil)

// Grants is execution.GitGrants over the Git authority. It delivers the attempt's
// credential to execution's drive (which hands it to the runtime adapter once,
// redacted everywhere else) and renews it for a still-live attempt.
type Grants struct {
	auth Authority
	runs Runs
	now  func() time.Time
}

var _ execution.GitGrants = (*Grants)(nil)

// NewGrants requires the Git authority and the execution reader (the renewal
// reads the committed context and the lease); the clock defaults to time.Now.
func NewGrants(auth Authority, runs Runs, clock func() time.Time) (*Grants, error) {
	if auth == nil || runs == nil {
		return nil, errors.New("gitgrants: the Git authority and an execution reader are required")
	}
	return &Grants{auth: auth, runs: runs, now: clockOrNow(clock)}, nil
}

func handleOf(h gitauth.GrantHandle) execution.GitGrantHandle {
	return execution.GitGrantHandle{GrantID: h.GrantID, Credential: execution.NewGitSecret(h.Credential.Reveal()), ExpiresAt: h.ExpiresAt}
}

// IssueAttempt implements execution.GitGrants: the request is execution's own
// derivation from the pinned context (execution.AttemptGrantFor: the context's
// repositories and ref patterns, bounded by the run's deadline and the contract's
// workload lifetime), converted without change. The Git authority refuses an
// attempt that is not live (attempt_inactive) or whose lease would end before the
// grant does; the lease deadline of Leases is the run's, which is what
// AttemptGrantFor caps the grant at. The credential is never logged or stored.
func (g *Grants) IssueAttempt(ctx context.Context, scope domain.Scope, req execution.AttemptGrantRequest) (execution.GitGrantHandle, error) {
	h, err := g.auth.IssueAttempt(ctx, scope, gitauth.AttemptGrantRequest{Kind: req.Kind, Actor: req.Actor, Repositories: req.Repositories, ExpiresAt: req.ExpiresAt})
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	if req.Actor.AttemptID != nil {
		if err := g.closeIssuanceRace(ctx, scope, domain.AttemptID(*req.Actor.AttemptID)); err != nil {
			return execution.GitGrantHandle{}, err
		}
	}
	return handleOf(h), nil
}

// closeIssuanceRace closes the window between the Git authority's lease read and
// its insert, which a cancellation can land in. It reads the attempt's liveness
// under the run's row lock, the lock the cancelling transaction takes before it
// revokes the attempt's grants: the grant is already committed here, so a live
// answer means any later cancellation finds it and revokes it, and an ended
// attempt gets it revoked now and nothing is delivered. A read that merely re-reads
// the record without the lock only narrows the window; this closes it.
func (g *Grants) closeIssuanceRace(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	run, err := g.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return err
	}
	live := false
	if _, _, err := g.runs.Update(ctx, scope, run, execution.UpdateOptions{}, func(_ context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		lease, lerr := execbridge.LeaseOf(agg, attempt, env.Now())
		live = lerr == nil && lease.Live
		return execution.Effects{}, nil
	}); err != nil {
		return err
	}
	if live {
		return nil
	}
	if err := g.auth.RevokeAttempt(ctx, scope, attempt); err != nil {
		return execution.Unavailablef(err, "the grant of an ended attempt could not be revoked")
	}
	return fmt.Errorf("%w: the attempt ended while its grant was issued", execution.ErrAttemptRevoked)
}

// RevokeAttempt implements execution.GitGrants: every grant of the attempt, in
// its own transaction, idempotently. The revocation that belongs to a transition
// joins the transition's transaction through the stores' GitGrants peer instead.
func (g *Grants) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return g.auth.RevokeAttempt(ctx, scope, attempt)
}

// IssuePreparation issues the read-only grant of a source-set run's one
// preparation workload (execution.PreparationGrantFor: every repository of the
// set, no update pattern anywhere, expiring with the preparation's deadline). A
// run without a source set has no preparation. The Git authority keys the grant
// by the workload identity and replays it with a fresh credential.
func (g *Grants) IssuePreparation(ctx context.Context, run *execution.Run, workload string, deadline time.Time) (execution.GitGrantHandle, error) {
	req, err := execution.PreparationGrantFor(run, workload, deadline, g.now())
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	return g.IssueAttempt(ctx, run.Scope, req)
}

// RenewAttempt rotates the Git credential of a still-live attempt: the same grant
// with a fresh secret that replaces the earlier one (the Git authority stores
// digests only, so the delivered secret cannot be returned again). It is the one
// documented way a credential is returned a second time, and it is for the
// controller side that wrote the credential file (T-152); the workload itself has
// no route for it.
//
// Renewal is bounded by the attempt's liveness and by the grant it renews: the
// request is rebuilt from the attempt's committed context (never from the
// caller), the lease is read before and after, an attempt that is not running, was
// revoked, whose run is draining or terminal or past its deadline is refused
// (execution.ErrAttemptRevoked) and a revoked, consumed or expired grant is
// refused by the Git authority. A renewal never extends the grant's expiry (a
// replay keeps the first issuance's), so it can never reach past the run's
// deadline or the contract's workload lifetime. An attempt that ended while the
// credential was rotated has its grants revoked and gets nothing.
func (g *Grants) RenewAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (execution.GitGrantHandle, error) {
	agg, at, err := g.liveAttempt(ctx, scope, attempt)
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](at.Context)
	if err != nil || c.RunID != agg.Run.ID || c.AttemptID != attempt {
		return execution.GitGrantHandle{}, fmt.Errorf("%w: the attempt is not a source-set attempt", execution.ErrContextVersionMismatch)
	}
	req, needed, err := execution.AttemptGrantFor(c, g.now())
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	if !needed {
		return execution.GitGrantHandle{}, fmt.Errorf("%w: the attempt holds no static Git grant", execution.ErrNotFound)
	}
	h, err := g.IssueAttempt(ctx, scope, req)
	if err != nil {
		return execution.GitGrantHandle{}, err
	}
	if _, _, err := g.liveAttempt(ctx, scope, attempt); err != nil {
		if errors.Is(err, execution.ErrAttemptRevoked) {
			if rerr := g.auth.RevokeAttempt(ctx, scope, attempt); rerr != nil {
				return execution.GitGrantHandle{}, execution.Unavailablef(rerr, "the grant of an ended attempt could not be revoked")
			}
		}
		return execution.GitGrantHandle{}, err
	}
	return h, nil
}

func (g *Grants) liveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (*execution.Aggregate, *execution.Attempt, error) {
	run, err := g.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return nil, nil, err
	}
	agg, err := g.runs.Get(ctx, scope, run)
	if err != nil {
		return nil, nil, err
	}
	lease, err := execbridge.LeaseOf(agg, attempt, g.now())
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", execution.ErrNotFound, err)
	}
	if !lease.Live {
		return nil, nil, fmt.Errorf("%w: the attempt's authority ended", execution.ErrAttemptRevoked)
	}
	return agg, agg.Attempt(attempt), nil
}
