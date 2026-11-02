package runtimeauth

import (
	"context"
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// GitGrantAction is the action that authorizes the runtime Git grant routes.
// The action ceiling is a closed list stored in the database (a migration would
// be needed to extend it) and the begin fence already rides context.read for the
// same reason. It is not what makes the route safe: a grant is issued only for a
// capability the attempt's pinned context advertises (runtimeGrants.seed exactly
// for a provisioning run, runtimeGrants.integration exactly when the source set
// has a write source), only for a repository the control plane's own records
// allow, and never on the strength of anything in the request but the repository
// and gate names.
const GitGrantAction = ActionContextRead

func (s *Service) gitConfig() (*GitConfig, error) {
	if s.cfg.Git == nil {
		return nil, fail(ErrUnavailable, "runtime Git grants are not configured")
	}
	return s.cfg.Git, nil
}

// gitSession re-verifies, now and in one tenant transaction (the module-wide lock
// order: attempt, then grant), that the authorization is still the current
// session of an active attempt of the source-set wire, and returns the attempt's
// pinned context from the durable grant. The context is never read from the
// request or from the authorization.
func (s *Service) gitSession(ctx context.Context, a Authorization) (runtimev3.ContextV3, error) {
	var out runtimev3.ContextV3
	if a.Session.Validate() != nil || a.Scope.Validate() != nil || a.RunID.Validate() != nil || a.AttemptID.Validate() != nil ||
		a.Action != GitGrantAction || a.ContextVersion != WireV3 {
		return out, fail(ErrInvalidCredential, "the authorization is malformed")
	}
	err := s.cfg.Store.Do(ctx, a.Scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		g, _, err := CheckSession(ctx, tx, a.SessionCheck(s.cfg.Tokens.Generation(), s.now()))
		if err != nil {
			return err
		}
		if g.Scope != a.Scope || g.AttemptID != a.AttemptID || g.RunID != a.RunID {
			return fail(ErrInvalidCredential, "the authorization is not this session's")
		}
		if g.Wire() != WireV3 {
			return fail(ErrContextVersionMismatch, "the attempt belongs to another runtime version")
		}
		out, err = decodeContextV3(g.Context)
		return err
	})
	return out, err
}

// closeIssuanceRace re-reads the attempt's authority under its lock after a grant
// was issued and recorded. A cancellation that committed between the issuer's
// liveness read and its insert leaves a grant for an ended attempt (the issuer
// reads the lease outside the grant's own transaction); the push authorizer
// refuses it anyway because liveness is re-read at every push, and this call
// removes the grant: the credential is not delivered and the attempt's grants are
// revoked. A narrower window is not a closed one, so the locking read here, not
// the issuer's, decides what the caller returns.
func (s *Service) closeIssuanceRace(ctx context.Context, gc *GitConfig, a Authorization) error {
	_, err := s.gitSession(ctx, a)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrAttemptRevoked) || errors.Is(err, ErrInvalidCredential) {
		if rerr := gc.Issuer.RevokeAttempt(ctx, a.Scope, a.AttemptID); rerr != nil {
			return failWith(ErrUnavailable, rerr, "the grant of an ended attempt could not be revoked")
		}
	}
	return err
}

// mapIssuerError classifies the issuer's failure: a contract refusal keeps its
// code, an attempt that is no longer live is the attempt's revocation, and a
// second seed for another repository (the issuer keys a seed by its attempt) is a
// refusal; anything else is unclassified and so unavailable.
func mapIssuerError(err error) error {
	if err == nil {
		return nil
	}
	var ce *runtimev1.ContractError
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Code {
	case gitv1.CodeAttemptInactive:
		return failWith(ErrAttemptRevoked, err, "the attempt is no longer authorized")
	case domain.CodeIdempotencyConflict:
		return refuse(domain.CodeForbidden, "the attempt already holds a different runtime grant of this kind")
	case domain.CodeInvalidCredential:
		return failWith(ErrAttemptRevoked, err, "the attempt is no longer authorized")
	}
	return Refused(ce)
}

// issuerFailed classifies the issuer's failure. When it says the attempt is no
// longer live, a grant may still have been written (the issuer reads liveness
// before and after its insert, and the attempt can end between), so the attempt's
// grants are revoked: it is ended, there is nothing to keep.
func (s *Service) issuerFailed(ctx context.Context, gc *GitConfig, a Authorization, err error) error {
	mapped := mapIssuerError(err)
	if errors.Is(mapped, ErrAttemptRevoked) {
		if rerr := gc.Issuer.RevokeAttempt(ctx, a.Scope, a.AttemptID); rerr != nil {
			return failWith(ErrUnavailable, rerr, "the grant of an ended attempt could not be revoked")
		}
	}
	return mapped
}

func contractRefusal(err error) error {
	var ce *runtimev1.ContractError
	if errors.As(err, &ce) {
		return Refused(ce)
	}
	return err
}

func hasWriteSource(c runtimev3.ContextV3, repositoryID string) bool {
	for _, src := range c.Workspace.Sources {
		if src.RepositoryID == repositoryID && src.Access == gitv1.AccessWrite {
			return true
		}
	}
	return false
}

// SeedGitGrant answers POST /runtime/v3/attempts/{attemptId}/git/seed: the
// provisioning attempt obtains the single-use seed grant of a repository its run
// created. The caller supplies the verified authorization of the route (the
// handler authorized and re-authorized the bearer); everything else comes from
// the control plane's records. In order:
//
//  1. the attempt's own pinned context must advertise the seed capability, which
//     only a provisioning run has (403 forbidden otherwise: PX-25);
//  2. the repository must be one this run created in its project, otherwise 404
//     not_found, indistinguishable from an unknown or foreign repository;
//  3. the Git contract's seed eligibility, with its 409/422 codes (internal,
//     ready, empty, no unsettled push); the issuer repeats it under the
//     repository's seed lock;
//  4. the issuer issues the grant to this attempt (idempotent until consumed: a
//     repeat is the same grant with a fresh credential) and the grant is
//     recorded as the attempt's runtime grant, which is how its commit output is
//     later recognized;
//  5. the response is checked against the contract (runtimev3.CheckSeedGrant),
//     and the attempt's authority is read once more under its lock: an attempt
//     that ended while the grant was issued gets it revoked and no credential.
//
// The credential is returned exactly once per call and is never logged or stored.
func (s *Service) SeedGitGrant(ctx context.Context, a Authorization, req runtimev3.GitSeedRequest) (resp runtimev3.GitGrantResponse, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "seed_git_grant", a.AttemptID, err) }()
	gc, err := s.gitConfig()
	if err != nil {
		return resp, err
	}
	if err := req.Validate(); err != nil {
		return resp, failWith(ErrInvalidRequest, err, "the seed request is invalid")
	}
	pinned, err := s.gitSession(ctx, a)
	if err != nil {
		return resp, err
	}
	if pinned.Git == nil || pinned.Git.Runtime.Seed == nil {
		return resp, refuse(domain.CodeForbidden, "the attempt may not request a seed grant")
	}
	created, err := gc.Repositories.CreatedByRun(ctx, a.Scope, a.RunID, req.RepositoryID)
	if err != nil {
		return resp, err
	}
	if !created {
		return resp, refuse(domain.CodeNotFound, "repository not found")
	}
	repo, err := gc.Repositories.Repository(ctx, a.Scope, req.RepositoryID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return resp, refuse(domain.CodeNotFound, "repository not found")
		}
		return resp, err
	}
	unsettled, err := gc.Repositories.UnsettledPush(ctx, a.Scope, req.RepositoryID)
	if err != nil {
		return resp, err
	}
	if err := gitv1.CheckSeedEligible(repo, unsettled); err != nil {
		return resp, contractRefusal(err)
	}
	notAfter, err := pinned.Run.DeadlineAt.Time()
	if err != nil || !s.now().Before(notAfter) {
		return resp, fail(ErrAttemptRevoked, "the attempt's run deadline has passed")
	}
	issued, err := gc.Issuer.IssueSeed(ctx, a.Scope, a.RunID, a.AttemptID, repo, notAfter)
	if err != nil {
		return resp, s.issuerFailed(ctx, gc, a, err)
	}
	resp = runtimev3.GitGrantResponse{Grant: issued.Claims, Credential: issued.Credential.Reveal(), CredentialFile: pinned.Git.Runtime.Seed.CredentialFile}
	if err := runtimev3.CheckSeedGrant(pinned, req, resp, []string{req.RepositoryID}, repo, unsettled); err != nil {
		// The issuer returned something the contract does not allow: nothing is
		// delivered and the grant is withdrawn with the attempt's others.
		_ = gc.Issuer.RevokeAttempt(ctx, a.Scope, a.AttemptID)
		return runtimev3.GitGrantResponse{}, failWith(ErrUnavailable, err, "the issued grant is not a valid seed grant")
	}
	rec := IssuedGrant{Scope: a.Scope, RunID: a.RunID, AttemptID: a.AttemptID, Claims: issued.Claims}
	if err := rec.Validate(); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	if err := gc.Issued.RecordIssued(ctx, rec); err != nil {
		return runtimev3.GitGrantResponse{}, failWith(ErrUnavailable, err, "the issued grant could not be recorded")
	}
	if err := s.closeIssuanceRace(ctx, gc, a); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	return resp, nil
}

// IntegrationGitGrant answers POST /runtime/v3/attempts/{attemptId}/git/integration:
// an approved integration step obtains the grant of exactly one update of the
// repository's default branch, from the approved base to the approved candidate.
//
//  1. the attempt's own pinned context must advertise the integration capability
//     and name the repository as a write source (403 forbidden);
//  2. the gates module derives the approval from the gate and the repository,
//     never from the request: no approved candidate for that gate and repository
//     is 403 forbidden and no grant exists; a default branch that moved is 409
//     publication_conflict with the superseding tip (the pipeline recomputes the
//     candidate); a default branch already at the candidate is the same 409 with
//     reason already_integrated and no grant (the integration was applied);
//  3. the issuer issues the grant with the constraint {ref, from, to, gateId} to
//     this attempt (idempotent on gate and attempt) and it is recorded;
//  4. the response is checked against the approval (runtimev3.CheckIntegrationGrant)
//     and the attempt's authority is read once more under its lock (see
//     SeedGitGrant).
//
// Nothing about the candidate reaches the grant except what the gate approved: code
// on the task branch cannot widen it.
func (s *Service) IntegrationGitGrant(ctx context.Context, a Authorization, req runtimev3.GitIntegrationRequest) (resp runtimev3.GitGrantResponse, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "integration_git_grant", a.AttemptID, err) }()
	gc, err := s.gitConfig()
	if err != nil {
		return resp, err
	}
	if err := req.Validate(); err != nil {
		return resp, failWith(ErrInvalidRequest, err, "the integration request is invalid")
	}
	pinned, err := s.gitSession(ctx, a)
	if err != nil {
		return resp, err
	}
	if pinned.Git == nil || pinned.Git.Runtime.Integration == nil || !hasWriteSource(pinned, req.RepositoryID) {
		return resp, refuse(domain.CodeForbidden, "the attempt may not request an integration grant for this repository")
	}
	approval, err := gc.Approvals.IntegrationApproval(ctx, a.Scope, a.RunID, req.GateID, req.RepositoryID)
	if err != nil {
		var stale *ApprovalStale
		var applied *ApprovalApplied
		switch {
		case errors.As(err, &stale):
			return resp, &PublicationConflict{Reason: "base_moved", ObservedTip: stale.SupersedingCommit}
		case errors.As(err, &applied):
			return resp, &PublicationConflict{Reason: "already_integrated", ObservedTip: applied.Approval.Candidate}
		case errors.Is(err, ErrNoApproval), errors.Is(err, ErrNotFound):
			return resp, refuse(domain.CodeForbidden, "no approved candidate exists for this gate and repository")
		}
		return resp, err
	}
	if approval.GateID != req.GateID || approval.RepositoryID != req.RepositoryID {
		return resp, refuse(domain.CodeForbidden, "no approved candidate exists for this gate and repository")
	}
	if approval.CurrentTip != approval.Base {
		return resp, &PublicationConflict{Reason: "base_moved", ObservedTip: approval.CurrentTip}
	}
	constraint := gitv1.IntegrationConstraint{Ref: approval.Ref, From: approval.Base, To: approval.Candidate, GateID: approval.GateID}
	if err := constraint.Validate(); err != nil {
		return resp, failWith(ErrUnavailable, err, "the approval does not describe a valid update")
	}
	issued, err := gc.Issuer.IssueIntegration(ctx, a.Scope, a.RunID, a.AttemptID, req.RepositoryID, constraint)
	if err != nil {
		return resp, s.issuerFailed(ctx, gc, a, err)
	}
	resp = runtimev3.GitGrantResponse{Grant: issued.Claims, Credential: issued.Credential.Reveal(), CredentialFile: pinned.Git.Runtime.Integration.CredentialFile}
	if err := runtimev3.CheckIntegrationGrant(pinned, req, resp, &approval); err != nil {
		_ = gc.Issuer.RevokeAttempt(ctx, a.Scope, a.AttemptID)
		return runtimev3.GitGrantResponse{}, failWith(ErrUnavailable, err, "the issued grant is not the approved update")
	}
	rec := IssuedGrant{Scope: a.Scope, RunID: a.RunID, AttemptID: a.AttemptID, Claims: issued.Claims}
	if err := rec.Validate(); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	if err := gc.Issued.RecordIssued(ctx, rec); err != nil {
		return runtimev3.GitGrantResponse{}, failWith(ErrUnavailable, err, "the issued grant could not be recorded")
	}
	if err := s.closeIssuanceRace(ctx, gc, a); err != nil {
		return runtimev3.GitGrantResponse{}, err
	}
	return resp, nil
}

// IssuedGitGrants returns the claims of the runtime grants the control plane
// issued to the attempt, from its own records (never from a caller): the input
// of execution.ResultCommitV3.Issued and of the result module's verification.
func (s *Service) IssuedGitGrants(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (out []gitv1.GrantClaims, err error) {
	defer func() { err = Classify(err); s.logOp(ctx, "issued_git_grants", attempt, err) }()
	gc, err := s.gitConfig()
	if err != nil {
		return nil, err
	}
	if err := firstErr(scope.Validate(), attempt.Validate()); err != nil {
		return nil, fail(ErrInvalidRequest, "%v", err)
	}
	return gc.Issued.IssuedFor(ctx, scope, attempt)
}
