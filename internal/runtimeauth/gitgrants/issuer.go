package gitgrants

import (
	"context"
	"errors"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gitauth"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// ---- integration approvals (gates) ----------------------------------------------------

// GateApprovals is the part of *gates.Service the adapters use.
type GateApprovals interface {
	IntegrationApproval(ctx context.Context, scope domain.Scope, run domain.RunID, gate gates.ID, repository string) (gates.IntegrationApproval, error)
}

var _ GateApprovals = (*gates.Service)(nil)

// Approvals is runtimeauth.IntegrationApprovals over the gates module: the one
// query a grant issuer consumes. It asks the gates module, which derives the
// approval from the gate and a fresh read of the default branch and mints
// nothing, and maps its answers:
//
//   - an approval: the facts, as runtime v3's IntegrationApproval;
//   - gates.ErrNoApproval, and an absent or foreign gate: runtimeauth.ErrNoApproval
//     (no grant exists);
//   - gates.ErrAlreadyIntegrated: *runtimeauth.ApprovalApplied (the update is
//     applied, nothing is left to authorize, no grant);
//   - a *gates.StaleError (the default branch is at neither the base nor the
//     candidate): *runtimeauth.ApprovalStale with the superseding commit;
//   - anything else, an outage included: runtimeauth.ErrUnavailable, never an
//     approval.
type Approvals struct{ gates GateApprovals }

var _ runtimeauth.IntegrationApprovals = (*Approvals)(nil)

// NewApprovals requires the gates module.
func NewApprovals(g GateApprovals) (*Approvals, error) {
	if g == nil {
		return nil, errors.New("gitgrants: the gates module is required")
	}
	return &Approvals{gates: g}, nil
}

// IntegrationApproval implements runtimeauth.IntegrationApprovals.
func (a *Approvals) IntegrationApproval(ctx context.Context, scope domain.Scope, run domain.RunID, gateID, repositoryID string) (runtimev3.IntegrationApproval, error) {
	got, err := a.gates.IntegrationApproval(ctx, scope, run, gates.ID(gateID), repositoryID)
	if err != nil {
		var applied *gates.AlreadyIntegratedError
		var stale *gates.StaleError
		switch {
		case errors.As(err, &applied):
			return runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalApplied{Approval: approvalOf(applied.Approval)}
		case errors.As(err, &stale):
			return runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalStale{SupersedingCommit: stale.SupersedingCommit}
		case errors.Is(err, gates.ErrNoApproval), errors.Is(err, gates.ErrNotFound), errors.Is(err, gates.ErrHidden), errors.Is(err, gates.ErrDenied):
			return runtimev3.IntegrationApproval{}, runtimeauth.ErrNoApproval
		}
		return runtimev3.IntegrationApproval{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	return approvalOf(got), nil
}

func approvalOf(a gates.IntegrationApproval) runtimev3.IntegrationApproval {
	return runtimev3.IntegrationApproval{
		GateID: string(a.GateID), RepositoryID: a.RepositoryID, Ref: a.Ref, Base: a.Base, Candidate: a.Candidate, CurrentTip: a.CurrentTip,
	}
}

type queryKey struct{}

// integrationQuery is what the Git authority's Gates port cannot carry in its
// signature (Decision(scope, gateID)): the run that asks, the attempt that holds
// the grant and the repository. It travels in the context of the issuance the
// Issuer starts, so a Decision made outside an issuance by this adapter has no
// answer and fails closed.
type integrationQuery struct {
	run        domain.RunID
	attempt    domain.AttemptID
	repository string
}

// GateDecisions is gitauth.Gates over the same approval: gitauth.IssueIntegration
// re-reads it when it issues, so the grant is bound to the gate's current,
// human-approved decision at the moment of issuance and not only when the
// runtime route asked. Approved and Current are both true exactly when the gates
// module returned an approval; every refusal (no approval, a moved or an applied
// base) is "not approved", so gitauth refuses the grant. A Decision outside an
// issuance by Issuer is ErrNotFound.
type GateDecisions struct {
	approvals runtimeauth.IntegrationApprovals
}

var _ gitauth.Gates = (*GateDecisions)(nil)

// NewGateDecisions requires the approvals.
func NewGateDecisions(a runtimeauth.IntegrationApprovals) (*GateDecisions, error) {
	if a == nil {
		return nil, errors.New("gitgrants: integration approvals are required")
	}
	return &GateDecisions{approvals: a}, nil
}

// Decision implements gitauth.Gates.
func (g *GateDecisions) Decision(ctx context.Context, scope domain.Scope, gateID string) (gitauth.GateDecision, error) {
	q, ok := ctx.Value(queryKey{}).(integrationQuery)
	if !ok {
		return gitauth.GateDecision{}, gitauth.ErrNotFound
	}
	a, err := g.approvals.IntegrationApproval(ctx, scope, q.run, gateID, q.repository)
	var stale *runtimeauth.ApprovalStale
	var applied *runtimeauth.ApprovalApplied
	switch {
	case err == nil:
	case errors.Is(err, runtimeauth.ErrNoApproval), errors.As(err, &stale), errors.As(err, &applied):
		return gitauth.GateDecision{}, nil // not approved (Approved false): gitauth answers forbidden
	case errors.Is(err, runtimeauth.ErrNotFound):
		return gitauth.GateDecision{}, gitauth.ErrNotFound
	default:
		return gitauth.GateDecision{}, err
	}
	if a.CurrentTip != a.Base {
		return gitauth.GateDecision{}, nil
	}
	return gitauth.GateDecision{
		Approved: true, Current: true, RunID: q.run, AttemptID: q.attempt, RepositoryID: a.RepositoryID, Base: a.Base, Candidate: a.Candidate,
	}, nil
}

// ---- the runtime grants (gitauth) -----------------------------------------------------

// Issuer is runtimeauth.GitIssuer over the Git authority.
type Issuer struct {
	auth Authority
	now  func() time.Time
}

var _ runtimeauth.GitIssuer = (*Issuer)(nil)

// NewIssuer requires the Git authority; the clock defaults to time.Now. The
// authority must have been built with GateDecisions as its gates (gitauth.Config.Gates),
// or every integration issuance is refused.
func NewIssuer(auth Authority, clock func() time.Time) (*Issuer, error) {
	if auth == nil {
		return nil, errors.New("gitgrants: the Git authority is required")
	}
	return &Issuer{auth: auth, now: clockOrNow(clock)}, nil
}

func (i *Issuer) claims(ctx context.Context, scope domain.Scope, h gitauth.GrantHandle) (runtimeauth.IssuedGitGrant, error) {
	c, err := i.auth.Claims(ctx, scope.TenantID, h.GrantID)
	if err != nil {
		return runtimeauth.IssuedGitGrant{}, err
	}
	return runtimeauth.IssuedGitGrant{Claims: c, Credential: runtimeauth.NewGitCredential(h.Credential.Reveal())}, nil
}

// IssueSeed implements runtimeauth.GitIssuer: gitauth's seed issuance for the
// attempt (kind seed, the repository's current default branch as its only update,
// the seed lock and eligibility under it, at most the contract's seed lifetime),
// bounded by notAfter.
func (i *Issuer) IssueSeed(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repo gitv1.Repository, notAfter time.Time) (runtimeauth.IssuedGitGrant, error) {
	now := i.now().UTC()
	expires := now.Add(gitv1.MaxSeedLifetime)
	if notAfter.Before(expires) {
		expires = notAfter
	}
	r, a := string(run), string(attempt)
	h, err := i.auth.IssueAttempt(ctx, scope, gitauth.AttemptGrantRequest{
		Kind:         gitv1.GrantSeed,
		Actor:        gitv1.Actor{Type: gitv1.ActorWorkload, ID: a, RunID: &r, AttemptID: &a},
		Repositories: []gitv1.GrantRepository{{ID: repo.ID, Read: true, Update: []string{gitv1.HeadsPrefix + repo.DefaultBranch}, Seed: true}},
		ExpiresAt:    expires.Truncate(time.Second),
	})
	if err != nil {
		return runtimeauth.IssuedGitGrant{}, err
	}
	return i.claims(ctx, scope, h)
}

// IssueIntegration implements runtimeauth.GitIssuer: gitauth's integration
// issuance, which re-reads the gate's decision through GateDecisions.
func (i *Issuer) IssueIntegration(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (runtimeauth.IssuedGitGrant, error) {
	ctx = context.WithValue(ctx, queryKey{}, integrationQuery{run: run, attempt: attempt, repository: repositoryID})
	h, err := i.auth.IssueIntegration(ctx, scope, attempt, repositoryID, c)
	if err != nil {
		return runtimeauth.IssuedGitGrant{}, err
	}
	return i.claims(ctx, scope, h)
}

// RevokeAttempt implements runtimeauth.GitIssuer.
func (i *Issuer) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	return i.auth.RevokeAttempt(ctx, scope, attempt)
}
