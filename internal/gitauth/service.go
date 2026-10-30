package gitauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// Permissions the Git service asks of the identity authority. The fixed role
// matrix has no Git permission, so Git maps onto the permissions that carry the
// same authority (git-repositories.md, "Interfaces and authorization"): reading
// a repository is reading source (viewer, analyst, engineer and reviewer; not
// admin), and updating or seeding one is managing the workspace, which only the
// engineer role holds.
const (
	PermRead  = identity.PermSourceRead
	PermWrite = identity.PermWorkspaceManage
)

// credentialUsername is the conventional username of the credential exchange;
// any username is accepted at use.
const credentialUsername = "kodama"

// Config wires the use cases. The store, repositories, pushes and authority are
// required. Attempts and Gates are required only by the attempt and integration
// use cases: a composition without execution or gates leaves them nil and those
// use cases fail closed (a grant bound to an attempt is then never verified).
type Config struct {
	Store        Store
	Repositories Repositories
	Pushes       Pushes
	Attempts     Attempts
	Gates        Gates
	Authority    identity.Authority
	// Policy defaults to DefaultPolicy.
	Policy Policy
	// Clock defaults to time.Now.
	Clock func() time.Time
	// Random is the entropy source of credentials and IDs; nil is crypto/rand.
	Random io.Reader
}

// Service holds the use cases of the Git authority.
type Service struct {
	store     Store
	repos     Repositories
	pushes    Pushes
	attempts  Attempts
	gates     Gates
	authority identity.Authority
	policy    Policy
	now       func() time.Time
	random    io.Reader
}

var (
	_ Credentials = (*Service)(nil)
	_ Verifier    = (*Service)(nil)
)

// NewService validates the configuration.
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Repositories == nil || cfg.Pushes == nil || cfg.Authority == nil {
		return nil, errors.New("gitauth: a store, repositories, pushes and an authority are required")
	}
	policy := cfg.Policy
	if policy == (Policy{}) {
		policy = DefaultPolicy()
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Service{store: cfg.Store, repos: cfg.Repositories, pushes: cfg.Pushes, attempts: cfg.Attempts, gates: cfg.Gates,
		authority: cfg.Authority, policy: policy, now: now, random: cfg.Random}, nil
}

// ---- helpers -----------------------------------------------------------------

func (s *Service) newGrantID() (string, error) {
	b := make([]byte, 16)
	r := s.random
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, b); err != nil {
		return "", unavailable(err, "no randomness available")
	}
	return "gg_" + hex.EncodeToString(b), nil
}

func scopeOf(tenant domain.TenantID, project string) (domain.Scope, error) {
	scope := domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(project)}
	if err := scope.Validate(); err != nil {
		return scope, invalidRequest("the tenant or project ID is malformed")
	}
	return scope, nil
}

// authorize is the issuance-time check: a caller no binding reaches learns
// nothing (not_found), a member without the permission is forbidden.
func (s *Service) authorize(ctx context.Context, user identity.UserID, scope domain.Scope, perm identity.Permission) error {
	err := s.authority.Authorize(ctx, identity.UserActor(user), identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden), errors.Is(err, identity.ErrNotFound):
		return notFound()
	case errors.Is(err, identity.ErrDenied):
		return forbidden("the caller's role does not allow this")
	}
	return unavailable(err, "authorization")
}

// allowed is the use-time check: it reports whether the person currently holds
// the permission; only an outage is an error.
func (s *Service) allowed(ctx context.Context, user identity.UserID, scope domain.Scope, perm identity.Permission) (bool, error) {
	err := s.authorize(ctx, user, scope, perm)
	switch {
	case err == nil:
		return true, nil
	case IsCode(err, domain.CodeNotFound), IsCode(err, domain.CodeForbidden):
		return false, nil
	}
	return false, err
}

func (s *Service) repository(ctx context.Context, scope domain.Scope, id string) (gitv1.Repository, error) {
	if !validID(id) {
		return gitv1.Repository{}, invalidRequest("the repository ID is malformed")
	}
	repo, err := s.repos.Repository(ctx, scope, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return gitv1.Repository{}, notFound()
	case err != nil:
		return gitv1.Repository{}, unavailable(err, "repository")
	case repo.ID != id:
		return gitv1.Repository{}, unavailable(errors.New("the repository port answered another repository"), "repository")
	}
	return repo, nil
}

// servable refuses a repository that cannot be served: an externally hosted one,
// an archived one and one still provisioning.
func servable(repo gitv1.Repository) error {
	switch {
	case repo.Hosting != gitv1.HostingInternal:
		return refuse(gitv1.CodeHostingMismatch, "the repository is not internally hosted")
	case repo.Status == gitv1.StatusArchived:
		return refuse(gitv1.CodeRepositoryArchived, "the repository is archived")
	case repo.Status != gitv1.StatusReady:
		return refuse(gitv1.CodeRepositoryNotReady, "the repository is not ready")
	}
	return nil
}

func (s *Service) newSecret() (Secret, error) { return GenerateSecret(s.random) }

func response(g Grant, secret Secret) (gitv1.CredentialResponse, error) {
	resp := gitv1.CredentialResponse{Username: credentialUsername, Password: secret.Reveal(), GrantID: g.ID(), ExpiresAt: g.Claims.ExpiresAt}
	if err := resp.Validate(); err != nil {
		return gitv1.CredentialResponse{}, unavailable(err, "credential response")
	}
	return resp, nil
}

func cloneClaims(c gitv1.GrantClaims) gitv1.GrantClaims {
	out := c
	out.Actor.RunID = clonePtr(c.Actor.RunID)
	out.Actor.AttemptID = clonePtr(c.Actor.AttemptID)
	out.Repositories = make([]gitv1.GrantRepository, len(c.Repositories))
	for i, r := range c.Repositories {
		r.Update = slices.Clone(r.Update)
		if r.Update == nil {
			r.Update = []string{}
		}
		if r.Integration != nil {
			ic := *r.Integration
			r.Integration = &ic
		}
		out.Repositories[i] = r
	}
	return out
}

func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (s *Service) insert(ctx context.Context, tx GrantTx, g Grant) error {
	if err := tx.Insert(ctx, g); err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			return unavailable(err, "grant identity collision")
		case errors.Is(err, ErrNotFound):
			return notFound()
		}
		return err
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// leaseOf reads an attempt's lease: unknown is not_found, one that is not live
// is attempt_inactive.
func (s *Service) leaseOf(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptLease, error) {
	if s.attempts == nil {
		return AttemptLease{}, unavailable(errors.New("no attempt authority is configured"), "attempt")
	}
	lease, err := s.attempts.Lease(ctx, scope, attempt)
	switch {
	case errors.Is(err, ErrNotFound):
		return AttemptLease{}, notFound()
	case err != nil:
		return AttemptLease{}, unavailable(err, "attempt")
	case !lease.Live:
		return AttemptLease{}, refuse(gitv1.CodeAttemptInactive, "the attempt is not live")
	}
	return lease, nil
}

// ---- human credentials --------------------------------------------------------

// Exchange is the credential exchange of POST /tenants/{tenantId}/git-credentials:
// the caller is the verified person of the access token (the router's
// principal), the request names the project and repository and whether it is a
// seed. The body is never a source of identity.
func (s *Service) Exchange(ctx context.Context, tenant domain.TenantID, user identity.UserID, req gitv1.CredentialRequest) (gitv1.CredentialResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.CredentialResponse{}, invalidRequest("%v", err)
	}
	if req.Seed {
		return s.IssueSeed(ctx, tenant, user, req.ProjectID, req.RepositoryID)
	}
	return s.IssueHuman(ctx, tenant, user, req.ProjectID, req.RepositoryID)
}

// IssueHuman issues a human credential for exactly one project and repository.
// The person must currently be allowed to read the repository (not_found when no
// binding reaches the project, forbidden when the role does not read), and the
// repository must be servable. The recorded update is gitv1.HumanUpdateCeiling
// for a person who may push now and [] for a read-only person; current roles are
// checked again at every use.
func (s *Service) IssueHuman(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error) {
	if err := user.Validate(); err != nil {
		return gitv1.CredentialResponse{}, invalidRequest("the caller is not identified")
	}
	scope, err := scopeOf(tenant, projectID)
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	if err := s.authorize(ctx, user, scope, PermRead); err != nil {
		return gitv1.CredentialResponse{}, err
	}
	repo, err := s.repository(ctx, scope, repositoryID)
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	if err := servable(repo); err != nil {
		return gitv1.CredentialResponse{}, err
	}
	push, err := s.allowed(ctx, user, scope, PermWrite)
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	update := []string{}
	if push {
		update = gitv1.HumanUpdateCeiling()
	}
	id, err := s.newGrantID()
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	secret, err := s.newSecret()
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	now := s.now().UTC()
	g := Grant{
		Claims: gitv1.GrantClaims{
			GrantID: id, Audience: gitv1.GrantAudience, TenantID: string(tenant), ProjectID: projectID,
			Actor: gitv1.Actor{Type: gitv1.ActorUser, ID: string(user)}, Kind: gitv1.GrantHuman,
			Repositories: []gitv1.GrantRepository{{ID: repositoryID, Read: true, Update: update}},
			IssuedAt:     timestamp(now), ExpiresAt: timestamp(now.Add(s.policy.HumanTTL)),
		},
		Digest: secret.Digest(),
	}
	if err := g.Claims.Validate(); err != nil {
		return gitv1.CredentialResponse{}, invalidRequest("%v", err)
	}
	if err := s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) error { return s.insert(ctx, tx, g) }); err != nil {
		return gitv1.CredentialResponse{}, err
	}
	return response(g, secret)
}

// ---- seed grants --------------------------------------------------------------

// IssueSeed issues the single-use seed credential of an empty repository (GR-18,
// S8). The caller must hold the engineer role (forbidden; not_found when no
// binding reaches the project). Under the repository's seed lock, in the
// transaction that records the grant, it requires the repository to exist in the
// project and gitv1.CheckSeedEligible over the repository and its push records:
// hosting_mismatch, repository_archived, repository_not_ready,
// repository_not_empty and seed_pending (any unsettled push of any kind). It
// revokes the repository's outstanding unconsumed seed grants, so at most one
// seed credential is usable at a time and a person who lost the secret asks
// again. The credential is never returned twice.
//
// Eligibility is decided inside the seed lock, which ConsumeSeedIn also takes in
// the push authorizer's transaction, from the repository's last reported state
// and its push records. A seed that was consumed has its push record in the
// transaction that consumed it, so "a consumed seed whose receive is unsettled"
// is exactly "an unsettled push record": seed_pending. The push authorizer
// still enforces the facts when the receive arrives (an empty repository per the
// worker's git facts, consumption of the grant, no other seed receive).
func (s *Service) IssueSeed(ctx context.Context, tenant domain.TenantID, user identity.UserID, projectID, repositoryID string) (gitv1.CredentialResponse, error) {
	if err := user.Validate(); err != nil {
		return gitv1.CredentialResponse{}, invalidRequest("the caller is not identified")
	}
	scope, err := scopeOf(tenant, projectID)
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	if err := s.authorize(ctx, user, scope, PermWrite); err != nil {
		return gitv1.CredentialResponse{}, err
	}
	now := s.now().UTC()
	actor := gitv1.Actor{Type: gitv1.ActorUser, ID: string(user)}
	var g Grant
	var secret Secret
	err = s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) (err error) {
		g, secret, err = s.seedIn(ctx, tx, scope, repositoryID, actor, now, now.Add(s.policy.SeedTTL), "", nil)
		return err
	})
	if err != nil {
		return gitv1.CredentialResponse{}, err
	}
	return response(g, secret)
}

// seedIn is the seed admission shared by a person's seed and a provisioning
// attempt's: seed lock, repository facts, eligibility, revocation of outstanding
// seeds, insert. wantUpdate, when set, is the update list the caller asked for
// and must be exactly the repository's default branch.
func (s *Service) seedIn(ctx context.Context, tx GrantTx, scope domain.Scope, repositoryID string, actor gitv1.Actor, now, expires time.Time, issueKey string, wantUpdate []string) (Grant, Secret, error) {
	if !validID(repositoryID) {
		return Grant{}, Secret{}, invalidRequest("the repository ID is malformed")
	}
	if err := tx.LockSeed(ctx, repositoryID); err != nil {
		return Grant{}, Secret{}, err
	}
	repo, err := s.repository(ctx, scope, repositoryID)
	if err != nil {
		return Grant{}, Secret{}, err
	}
	unsettled, err := s.pushes.UnsettledPush(ctx, scope, repositoryID)
	if err != nil {
		return Grant{}, Secret{}, unavailable(err, "push records")
	}
	if err := gitv1.CheckSeedEligible(repo, unsettled); err != nil {
		return Grant{}, Secret{}, err
	}
	update := []string{gitv1.HeadsPrefix + repo.DefaultBranch}
	if wantUpdate != nil && !slices.Equal(wantUpdate, update) {
		return Grant{}, Secret{}, invalidRequest("a seed grant creates exactly %s", update[0])
	}
	id, err := s.newGrantID()
	if err != nil {
		return Grant{}, Secret{}, err
	}
	secret, err := s.newSecret()
	if err != nil {
		return Grant{}, Secret{}, err
	}
	g := Grant{
		Claims: gitv1.GrantClaims{
			GrantID: id, Audience: gitv1.GrantAudience, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
			Actor: actor, Kind: gitv1.GrantSeed,
			Repositories: []gitv1.GrantRepository{{ID: repositoryID, Read: true, Update: update, Seed: true}},
			IssuedAt:     timestamp(now), ExpiresAt: timestamp(expires),
		},
		Digest: secret.Digest(), IssueKey: issueKey,
	}
	if err := g.Claims.CheckSeed(repo.DefaultBranch); err != nil {
		return Grant{}, Secret{}, invalidRequest("%v", err)
	}
	if _, err := tx.RevokeSeeds(ctx, repositoryID, now); err != nil {
		return Grant{}, Secret{}, err
	}
	if err := s.insert(ctx, tx, g); err != nil {
		return Grant{}, Secret{}, err
	}
	return g, secret, nil
}

// ---- workload grants (execution) ------------------------------------------------

// IssueAttempt issues a preparation, attempt or provisioning-seed grant for the
// repositories of an admitted source set. A grant bound to an attempt is refused
// for an attempt that is not live (attempt_inactive; not_found when unknown),
// its expiry may not exceed the attempt's lease or the kind's contract lifetime,
// and every repository must exist in the scope's project and be servable. A
// provisioning seed (kind seed) additionally follows the seed rules of IssueSeed
// with the attempt as the actor.
//
// It is idempotent on (kind, attempt), or (kind, workload ID) for a preparation:
// a retry of the same issuance returns the same grant with a fresh secret, which
// replaces the earlier one (only digests are stored, so the first secret cannot
// be returned again); a different issuance under the same key is
// idempotency_conflict, and a revoked, expired or consumed grant is forbidden.
// The credential is never logged or stored.
func (s *Service) IssueAttempt(ctx context.Context, scope domain.Scope, req AttemptGrantRequest) (GrantHandle, error) {
	if err := scope.Validate(); err != nil {
		return GrantHandle{}, invalidRequest("the scope is malformed")
	}
	switch req.Kind {
	case gitv1.GrantPreparation, gitv1.GrantAttempt, gitv1.GrantSeed:
	default:
		return GrantHandle{}, invalidRequest("execution issues preparation, attempt and seed grants only")
	}
	now := s.now().UTC()
	id, err := s.newGrantID()
	if err != nil {
		return GrantHandle{}, err
	}
	secret, err := s.newSecret()
	if err != nil {
		return GrantHandle{}, err
	}
	repos := make([]gitv1.GrantRepository, len(req.Repositories))
	for i, r := range req.Repositories {
		r.Update = slices.Clone(r.Update)
		if r.Update == nil {
			r.Update = []string{}
		}
		repos[i] = r
	}
	g := Grant{
		Claims: gitv1.GrantClaims{
			GrantID: id, Audience: gitv1.GrantAudience, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
			Actor: req.Actor, Kind: req.Kind, Repositories: repos,
			IssuedAt: timestamp(now), ExpiresAt: timestamp(req.ExpiresAt),
		},
		Digest: secret.Digest(),
	}
	if err := g.Claims.Validate(); err != nil {
		return GrantHandle{}, invalidRequest("%v", err)
	}
	subject := req.Actor.ID
	if attempt, ok := g.AttemptID(); ok {
		subject = string(attempt)
		lease, err := s.leaseOf(ctx, scope, attempt)
		if err != nil {
			return GrantHandle{}, err
		}
		if !lease.Deadline.IsZero() && req.ExpiresAt.After(lease.Deadline) {
			return GrantHandle{}, invalidRequest("the grant would outlive the attempt's lease")
		}
	}
	g.IssueKey = string(req.Kind) + ":" + subject
	if req.Kind != gitv1.GrantSeed {
		for _, r := range g.Claims.Repositories {
			repo, err := s.repository(ctx, scope, r.ID)
			if err != nil {
				return GrantHandle{}, err
			}
			if err := servable(repo); err != nil {
				return GrantHandle{}, err
			}
		}
	}
	var out Grant
	err = s.store.Do(ctx, scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		existing, err := tx.GrantByIssueKey(ctx, g.IssueKey, true)
		switch {
		case err == nil:
			out, err = s.replay(ctx, tx, existing, g, now)
			return err
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if req.Kind == gitv1.GrantSeed {
			out, secret, err = s.seedIn(ctx, tx, scope, g.Claims.Repositories[0].ID, req.Actor, now, req.ExpiresAt, g.IssueKey, g.Claims.Repositories[0].Update)
			return err
		}
		out = g
		return s.insert(ctx, tx, g)
	})
	if err != nil {
		return GrantHandle{}, err
	}
	return GrantHandle{GrantID: out.ID(), Credential: secret, ExpiresAt: out.ExpiresAt()}, nil
}

// replay is the idempotent answer of a repeated issuance: the same grant, a
// fresh secret.
func (s *Service) replay(ctx context.Context, tx GrantTx, existing, requested Grant, now time.Time) (Grant, error) {
	if !sameIssue(existing.Claims, requested.Claims) {
		return Grant{}, refuse(domain.CodeIdempotencyConflict, "a different grant was already issued under this key")
	}
	if existing.Window(now) != nil || existing.Consumption != nil {
		return Grant{}, forbidden("the grant is no longer usable")
	}
	existing.Digest = requested.Digest
	if err := tx.Update(ctx, existing); err != nil {
		return Grant{}, err
	}
	return existing, nil
}

// RevokeAttempt revokes every grant of the attempt in its own transaction. It is
// idempotent. Execution, which must revoke atomically with the attempt's
// cancellation, completion, revocation or supersession, calls RevokeAttemptIn in
// that transaction instead, through the adapter's Bind.
func (s *Service) RevokeAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	if err := scope.Validate(); err != nil {
		return invalidRequest("the scope is malformed")
	}
	if err := attempt.Validate(); err != nil {
		return invalidRequest("the attempt ID is malformed")
	}
	return s.store.Do(ctx, scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		_, err := RevokeAttemptIn(ctx, tx, scope, attempt, s.now())
		return err
	})
}

// RevokeAttemptIn revokes every grant of the attempt inside a transaction the
// caller owns (the execution owner's cancellation transaction, joined with
// postgres.Bind or memory.Bind). The caller locks the attempt row first (the
// module-wide order: attempt, then grants). A receive that already obtained
// commit intent completes; any later one is refused by the push authorizer.
func RevokeAttemptIn(ctx context.Context, tx GrantTx, scope domain.Scope, attempt domain.AttemptID, now time.Time) (int, error) {
	return tx.RevokeAttempt(ctx, scope, attempt, now.UTC())
}

// ---- integration grants (gates) ---------------------------------------------------

// IssueIntegration binds a grant to one approved gate decision: the gate is
// approved and current, its subject names exactly the base and candidate of the
// constraint for this repository and attempt, the constraint's ref is the
// repository's default branch, and the attempt is live. The grant permits
// exactly that update, once. Idempotent on (gate, attempt): a retry of the same
// constraint returns the same grant with a fresh secret; another constraint
// under the same key is idempotency_conflict (a moved target is a new
// constraint, never an edit).
func (s *Service) IssueIntegration(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, repositoryID string, c gitv1.IntegrationConstraint) (GrantHandle, error) {
	if err := firstErr(scope.Validate(), attempt.Validate()); err != nil || !validID(repositoryID) {
		return GrantHandle{}, invalidRequest("the scope, attempt or repository ID is malformed")
	}
	if err := c.Validate(); err != nil {
		return GrantHandle{}, invalidRequest("%v", err)
	}
	if s.gates == nil {
		return GrantHandle{}, unavailable(errors.New("no gate authority is configured"), "gate")
	}
	d, err := s.gates.Decision(ctx, scope, c.GateID)
	switch {
	case errors.Is(err, ErrNotFound):
		return GrantHandle{}, notFound()
	case err != nil:
		return GrantHandle{}, unavailable(err, "gate")
	case !d.Approved || !d.Current:
		return GrantHandle{}, forbidden("the gate is not approved")
	case d.RepositoryID != repositoryID || d.AttemptID != attempt || d.Base != c.From || d.Candidate != c.To:
		return GrantHandle{}, forbidden("the approved gate subject differs from the requested update")
	}
	repo, err := s.repository(ctx, scope, repositoryID)
	if err != nil {
		return GrantHandle{}, err
	}
	if err := servable(repo); err != nil {
		return GrantHandle{}, err
	}
	if c.Ref != gitv1.HeadsPrefix+repo.DefaultBranch {
		return GrantHandle{}, forbidden("an integration grant advances the repository's default branch only")
	}
	lease, err := s.leaseOf(ctx, scope, attempt)
	if err != nil {
		return GrantHandle{}, err
	}
	now := s.now().UTC()
	expires := now.Add(s.policy.IntegrationTTL)
	if !lease.Deadline.IsZero() && lease.Deadline.Before(expires) {
		expires = lease.Deadline
	}
	if !expires.After(now) {
		return GrantHandle{}, refuse(gitv1.CodeAttemptInactive, "the attempt's lease has ended")
	}
	id, err := s.newGrantID()
	if err != nil {
		return GrantHandle{}, err
	}
	secret, err := s.newSecret()
	if err != nil {
		return GrantHandle{}, err
	}
	attemptID, runID := string(attempt), string(d.RunID)
	g := Grant{
		Claims: gitv1.GrantClaims{
			GrantID: id, Audience: gitv1.GrantAudience, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
			Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: attemptID, RunID: &runID, AttemptID: &attemptID}, Kind: gitv1.GrantIntegration,
			Repositories: []gitv1.GrantRepository{{ID: repositoryID, Read: true, Update: []string{c.Ref}, Integration: &c}},
			IssuedAt:     timestamp(now), ExpiresAt: timestamp(expires),
		},
		Digest: secret.Digest(), IssueKey: "integration:" + c.GateID + ":" + attemptID,
	}
	if err := g.Claims.Validate(); err != nil {
		return GrantHandle{}, invalidRequest("%v", err)
	}
	var out Grant
	err = s.store.Do(ctx, scope.TenantID, func(ctx context.Context, tx GrantTx) error {
		existing, err := tx.GrantByIssueKey(ctx, g.IssueKey, true)
		switch {
		case err == nil:
			out, err = s.replay(ctx, tx, existing, g, now)
			return err
		case !errors.Is(err, ErrNotFound):
			return err
		}
		out = g
		return s.insert(ctx, tx, g)
	})
	if err != nil {
		return GrantHandle{}, err
	}
	return GrantHandle{GrantID: out.ID(), Credential: secret, ExpiresAt: out.ExpiresAt()}, nil
}

// ---- verification ---------------------------------------------------------------------

// Verify resolves a presented credential in the tenant and returns the grant's
// effective claims. The credential must have the credential syntax (a signed
// token never does), be known to the tenant, unexpired and unrevoked; then the
// authority that can change after issuance is re-read:
//
//   - a grant bound to an attempt requires the attempt to be live now (and
//     inside its lease): a cancelled, finished, revoked or superseded attempt
//     can no longer use Git even before its grants are revoked;
//   - a human grant requires the person to still read the repository's project
//     (source:read); its update set is the recorded ceiling only while the
//     person may still push (engineer), otherwise [] (read only), and never
//     more than was recorded;
//   - a person's seed grant requires the person to still be an engineer.
//
// Every refusal is invalid_credential with one message; a grant of another
// tenant is the same. An outage of a dependency is ErrUnavailable, never access.
// A consumed seed grant is still returned: push authorization refuses it as
// seed_consumed.
func (s *Service) Verify(ctx context.Context, tenant domain.TenantID, credential string) (gitv1.GrantClaims, error) {
	secret, err := ParseSecret(credential)
	if err != nil {
		return gitv1.GrantClaims{}, err
	}
	if tenant.Validate() != nil {
		return gitv1.GrantClaims{}, invalidCredential()
	}
	var g Grant
	err = s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) (err error) {
		g, err = tx.GrantByDigest(ctx, secret.Digest())
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return gitv1.GrantClaims{}, invalidCredential()
	case err != nil:
		return gitv1.GrantClaims{}, err
	}
	if !g.Digest.Equal(secret.Digest()) {
		return gitv1.GrantClaims{}, invalidCredential()
	}
	return s.effective(ctx, tenant, g)
}

// VerifyScoped is Verify followed by the scope comparison the Git endpoint needs:
// a grant of another project or repository is not_found, indistinguishable from
// a missing repository (GR-05).
func (s *Service) VerifyScoped(ctx context.Context, tenant domain.TenantID, credential, projectID, repositoryID string) (gitv1.GrantClaims, error) {
	claims, err := s.Verify(ctx, tenant, credential)
	if err != nil {
		return gitv1.GrantClaims{}, err
	}
	if err := claims.CheckScope(string(tenant), projectID, repositoryID); err != nil {
		return gitv1.GrantClaims{}, err
	}
	return claims, nil
}

// Claims returns the effective claims of a grant by ID, for the push authorizer
// that receives a grant ID from the worker (not a credential). The checks are
// those of Verify; an unknown grant is not_found, a grant that is no longer
// usable is invalid_credential.
func (s *Service) Claims(ctx context.Context, tenant domain.TenantID, grantID string) (gitv1.GrantClaims, error) {
	if tenant.Validate() != nil || !validID(grantID) {
		return gitv1.GrantClaims{}, notFound()
	}
	var g Grant
	err := s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) (err error) {
		g, err = tx.GrantByID(ctx, grantID, false)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return gitv1.GrantClaims{}, notFound()
	case err != nil:
		return gitv1.GrantClaims{}, err
	}
	return s.effective(ctx, tenant, g)
}

func (s *Service) effective(ctx context.Context, tenant domain.TenantID, g Grant) (gitv1.GrantClaims, error) {
	now := s.now()
	if g.Claims.TenantID != string(tenant) || g.Window(now) != nil {
		return gitv1.GrantClaims{}, invalidCredential()
	}
	claims := cloneClaims(g.Claims)
	scope := domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(claims.ProjectID)}
	if attempt, ok := g.AttemptID(); ok {
		if s.attempts == nil {
			return gitv1.GrantClaims{}, unavailable(errors.New("no attempt authority is configured"), "attempt")
		}
		lease, err := s.attempts.Lease(ctx, scope, attempt)
		switch {
		case errors.Is(err, ErrNotFound):
			return gitv1.GrantClaims{}, invalidCredential()
		case err != nil:
			return gitv1.GrantClaims{}, unavailable(err, "attempt")
		case !lease.Live, !lease.Deadline.IsZero() && !now.Before(lease.Deadline):
			return gitv1.GrantClaims{}, invalidCredential()
		}
	}
	if claims.Actor.Type != gitv1.ActorUser {
		return claims, nil
	}
	user := identity.UserID(claims.Actor.ID)
	switch claims.Kind {
	case gitv1.GrantHuman:
		read, err := s.allowed(ctx, user, scope, PermRead)
		if err != nil {
			return gitv1.GrantClaims{}, err
		}
		if !read {
			return gitv1.GrantClaims{}, invalidCredential()
		}
		push, err := s.allowed(ctx, user, scope, PermWrite)
		if err != nil {
			return gitv1.GrantClaims{}, err
		}
		if !push {
			for i := range claims.Repositories {
				claims.Repositories[i].Update = []string{}
			}
		}
	case gitv1.GrantSeed:
		engineer, err := s.allowed(ctx, user, scope, PermWrite)
		if err != nil {
			return gitv1.GrantClaims{}, err
		}
		if !engineer {
			return gitv1.GrantClaims{}, invalidCredential()
		}
	}
	return claims, nil
}

// ConsumeSeed marks the seed grant consumed by receiveID in its own transaction;
// see ConsumeSeedIn. The push authorizer calls ConsumeSeedIn in its
// authorization transaction so that consumption and the push record commit
// together.
func (s *Service) ConsumeSeed(ctx context.Context, tenant domain.TenantID, grantID, receiveID string) error {
	if tenant.Validate() != nil || !validID(grantID) {
		return notFound()
	}
	return s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) error {
		return ConsumeSeedIn(ctx, tx, grantID, receiveID, s.now())
	})
}

// ConsumeSeedIn consumes a seed grant for one receive inside a transaction the
// caller owns. The push authorizer (T-130) MUST call it in its push-authorization
// transaction, before inserting the push record, and commit both together: it
// takes the repository's seed lock, which is held until that transaction ends,
// so a concurrent seed issuance (which takes the same lock before it checks that
// the repository has no unsettled push) either runs first and revokes this
// unused seed (the receive is then refused as inactive) or waits and, once the
// push record is committed, is refused with seed_pending. Without that, a new
// seed could be issued while the consumed one's receive is unsettled.
//
// Lock order is the seed lock, then the grant row (FOR UPDATE), so concurrent
// seed receives authorize at most one: the first writes the consumption, later
// ones see it. It is idempotent for the same receive (a replayed authorization),
// ErrSeedConsumed for any other receive, ErrGrantInactive for a revoked or
// expired grant, and not_found for an unknown or foreign one.
func ConsumeSeedIn(ctx context.Context, tx GrantTx, grantID, receiveID string, now time.Time) error {
	g, err := tx.GrantByID(ctx, grantID, false)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return err
	}
	if g.Claims.Kind != gitv1.GrantSeed || len(g.Claims.Repositories) != 1 {
		return invalidRequest("only a seed grant is consumed")
	}
	if err := tx.LockSeed(ctx, g.Claims.Repositories[0].ID); err != nil {
		return err
	}
	// Re-read under the lock: the grant may have been revoked or consumed while
	// this transaction waited for it.
	g, err = tx.GrantByID(ctx, grantID, true)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return err
	}
	changed, err := g.Consume(receiveID, now)
	if err != nil || !changed {
		return err
	}
	return tx.Update(ctx, g)
}

// Revoke revokes one grant. It is idempotent; an unknown grant is not_found.
func (s *Service) Revoke(ctx context.Context, tenant domain.TenantID, grantID string) error {
	if tenant.Validate() != nil || !validID(grantID) {
		return notFound()
	}
	return s.store.Do(ctx, tenant, func(ctx context.Context, tx GrantTx) error {
		g, err := tx.GrantByID(ctx, grantID, true)
		if errors.Is(err, ErrNotFound) {
			return notFound()
		}
		if err != nil {
			return err
		}
		if !g.Revoke(s.now()) {
			return nil
		}
		return tx.Update(ctx, g)
	})
}
