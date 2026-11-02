package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Dependencies are the injected ports. Every required one must be non-nil:
// there is no default that grants access or reaches a worker.
type Dependencies struct {
	Store      Store
	Authorizer Authorizer
	Hosts      Hosts
	CloneURLs  CloneURLs

	// Tenants is required only by ReconcileAll.
	Tenants TenantSource
	// External is required only by MapConfigured.
	External ExternalSource

	Clock  func() time.Time
	NewID  func(prefix string) string
	Logger *slog.Logger
}

// Options are the explicit policies of a Service.
type Options struct {
	// HostsDeadline bounds each call to a tenant's worker made by a use case.
	HostsDeadline time.Duration
	// ReconcileBatch is the page size of the provisioning sweep.
	ReconcileBatch int
}

// DefaultOptions are conservative defaults a composition may override.
func DefaultOptions() Options {
	return Options{HostsDeadline: 30 * time.Second, ReconcileBatch: 50}
}

func (o Options) validate() error {
	if o.HostsDeadline <= 0 || o.ReconcileBatch < 1 || o.ReconcileBatch > gitv1.MaxPage {
		return fmt.Errorf("%w: the worker deadline and the sweep batch must be positive", ErrInvalidConfiguration)
	}
	return nil
}

// Service coordinates the repository entity through the injected ports.
type Service struct {
	d Dependencies
	o Options
}

// NewService validates the capabilities and options.
func NewService(d Dependencies, o Options) (*Service, error) {
	missing := func(name string) error { return fmt.Errorf("%w: %s", ErrNotConfigured, name) }
	switch {
	case d.Store == nil:
		return nil, missing("Store")
	case d.Authorizer == nil:
		return nil, missing("Authorizer")
	case d.Hosts == nil:
		return nil, missing("Hosts")
	case d.CloneURLs == nil:
		return nil, missing("CloneURLs")
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if d.Clock == nil {
		d.Clock = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = randomID
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d, o: o}, nil
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// system is the actor of provisioning confirmation and reconciliation.
var system = Actor{Type: ActorService, ID: "repository-reconciler"}

// now is the clock at the database's microsecond precision, so a value written
// and read back is equal to the one the use case holds.
func (s *Service) now() time.Time { return s.d.Clock().UTC().Truncate(time.Microsecond) }

func (s *Service) authorize(ctx context.Context, actor Actor, action Action, scope domain.Scope, res Resource) error {
	if err := firstErr(actor.Validate(), scope.Validate()); err != nil {
		return err
	}
	err := s.d.Authorizer.Authorize(ctx, actor, action, scope, res)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDenied), errors.Is(err, ErrNotFound):
		return err
	default:
		return fmt.Errorf("%w: authorization: %w", ErrUnavailable, err) // fail closed
	}
}

func (s *Service) project(scope domain.Scope) Resource {
	return Resource{Kind: "project", ID: string(scope.ProjectID)}
}

func repositoryResource(id ID) Resource { return Resource{Kind: "repository", ID: string(id)} }

func audit(action string, actor Actor, reason string, md map[string]any) Audit {
	return Audit{Action: action, Actor: actor, Reason: reason, Metadata: md}
}

// ---- create ----

// CreatePlan is a validated, authorized creation that has not touched storage.
// A transport that joins creation to an idempotency record inserts Repository
// with Effects in its own transaction and then calls Resolve; Create does both
// itself.
type CreatePlan struct {
	Repository *Repository
	Effects    Effects
}

// Prepare validates the request, authorizes the actor and builds the
// reservation: a provisioning repository with the effective default branch and
// the effective policy (an omitted policy is all-false, O-33).
func (s *Service) Prepare(ctx context.Context, actor Actor, scope domain.Scope, req gitv1.CreateRepositoryRequest) (*CreatePlan, error) {
	if err := s.authorize(ctx, actor, ActionCreate, scope, s.project(scope)); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	r, err := New(NewParams{
		ID: ID(s.d.NewID("repo")), Scope: scope, Name: req.Name, DefaultBranch: req.EffectiveDefaultBranch(),
		Policy: gitv1.EffectivePolicy(req.Policy), CreatedBy: actor,
	}, s.now())
	if err != nil {
		return nil, err
	}
	return &CreatePlan{Repository: r, Effects: Effects{Audit: []Audit{
		audit("repository.provisioning_requested", actor, "", map[string]any{
			"name": r.Name, "defaultBranch": r.DefaultBranch, "hosting": string(r.Hosting),
			"directPushToDefault": r.Policy.DirectPushToDefault,
		}),
	}}}, nil
}

// Resolve interprets the store's answer to Insert(plan.Repository). A created
// repository is the answer. An existing repository of the same name that is
// what the request asked for (same default branch and policy, internally
// hosted) is a replay and is returned unchanged with created false; any other
// is repository_name_taken, so retried and concurrent creates converge on one
// repository without a request silently changing another's settings.
func (p *CreatePlan) Resolve(persisted *Repository, created bool) (*Repository, bool, error) {
	if created {
		return persisted, true, nil
	}
	if persisted == nil || !persisted.Equivalent(p.Repository) {
		return nil, false, ErrNameTaken
	}
	return persisted, false, nil
}

// Created is the result of Create.
type Created struct {
	Repository *Repository
	// Created is false when the name already belonged to an equivalent repository.
	Created bool
}

// Create reserves the repository and provisions it. The reservation commits
// first (the row, its audit record); the worker is called afterwards, outside
// any transaction, so a failure leaves a provisioning repository with a reason
// that the reconciler finishes. The result may therefore still be
// provisioning.
func (s *Service) Create(ctx context.Context, actor Actor, scope domain.Scope, req gitv1.CreateRepositoryRequest) (Created, error) {
	plan, err := s.Prepare(ctx, actor, scope, req)
	if err != nil {
		return Created{}, err
	}
	persisted, created, err := s.d.Store.Insert(ctx, plan.Repository, plan.Effects)
	if err != nil {
		return Created{}, err
	}
	r, created, err := plan.Resolve(persisted, created)
	if err != nil {
		return Created{}, err
	}
	r, err = s.Provision(ctx, scope, r.ID)
	if err != nil {
		return Created{Repository: r, Created: created}, err
	}
	return Created{Repository: r, Created: created}, nil
}

// ---- provisioning ----

// Provision asks the tenant's worker to initialize the provisioning
// repository and, on confirmation, marks it ready and commits repository.created
// with the status change. It is idempotent: a repository that is no longer
// provisioning is returned unchanged, and the worker's initialization is
// idempotent on the repository ID, so it is safe after any interruption. No
// SQL transaction is open during the worker call. A worker failure is recorded
// as the repository's reason and is not an error here (the reconciler repeats
// it); an error is returned only for a storage failure or a cancelled ctx.
func (s *Service) Provision(ctx context.Context, scope domain.Scope, id ID) (*Repository, error) {
	r, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if err := r.requireInternal(); err != nil {
		return r, err
	}
	if r.Status != gitv1.StatusProvisioning {
		return r, nil
	}
	observed := s.now()
	hctx, cancel := context.WithTimeout(ctx, s.o.HostsDeadline)
	resp, err := s.d.Hosts.Initialize(hctx, scope.TenantID, gitv1.InitializeRepositoryRequest{RepositoryID: string(r.ID), DefaultBranch: r.DefaultBranch})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return r, ctx.Err() // the caller's cancellation is never an outcome
		}
		return s.noteProvisioning(ctx, scope, id, reasonFor(outcomeOf(err)))
	}
	if resp.Validate() != nil || resp.RepositoryID != string(r.ID) || resp.DefaultBranch != r.DefaultBranch {
		return s.noteProvisioning(ctx, scope, id, ReasonInitializationMismatch)
	}
	return s.completeProvisioning(ctx, scope, id, resp.Empty, observed)
}

func reasonFor(o Outcome) string {
	switch o {
	case Unavailable:
		return ReasonWorkerUnavailable
	case Refused:
		return ReasonInitializationRefused
	}
	return ReasonInitializationUnknown
}

func (s *Service) noteProvisioning(ctx context.Context, scope domain.Scope, id ID, reason string) (*Repository, error) {
	return s.d.Store.Update(ctx, scope, id, func(r *Repository) (Effects, error) {
		if r.Status != gitv1.StatusProvisioning {
			return Effects{}, nil // completed meanwhile
		}
		_, err := r.RecordProvisioningReason(reason, s.now())
		return Effects{}, err
	})
}

// completeProvisioning is the one transition out of provisioning: the status
// change, its audit record and repository.created (plus the state change) are
// one transaction. Repeating it for a repository that is already ready writes
// nothing.
func (s *Service) completeProvisioning(ctx context.Context, scope domain.Scope, id ID, empty bool, observed time.Time) (*Repository, error) {
	return s.d.Store.Update(ctx, scope, id, func(r *Repository) (Effects, error) {
		if r.Status != gitv1.StatusProvisioning {
			return Effects{}, nil
		}
		if err := r.CompleteProvisioning(empty, observed, s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{
			Audit: []Audit{audit("repository.ready", system, "", map[string]any{"empty": r.Empty})},
			Events: []Event{
				{Kind: EventCreated, Actor: r.CreatedBy},
				{Kind: EventStateChanged, Actor: system, From: string(gitv1.StatusProvisioning), To: string(gitv1.StatusReady), Reason: TransitionInitialized},
			},
		}, nil
	})
}

// ConfirmInitialized is the worker's confirmation of an initialization (POST
// .../repositories/{id}/initialized). The route has already applied the worker
// identity (gitv1.CheckWorkerCall), so tenant is the identity's and no user
// authorization applies. It completes provisioning like the reconciler does and
// is idempotent: a repository that is already ready or archived answers with
// its current status. A foreign or unknown repository is ErrNotFound.
func (s *Service) ConfirmInitialized(ctx context.Context, tenant domain.TenantID, req gitv1.InitializedRequest) (gitv1.InitializedResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.InitializedResponse{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	observed := s.now()
	r, err := s.d.Store.GetInTenant(ctx, tenant, ID(req.RepositoryID))
	if err != nil {
		return gitv1.InitializedResponse{}, err
	}
	if r.Hosting != gitv1.HostingInternal {
		return gitv1.InitializedResponse{}, ErrNotFound // a worker never initialized it
	}
	if r.Status == gitv1.StatusProvisioning {
		if req.DefaultBranch != r.DefaultBranch {
			if _, err := s.noteProvisioning(ctx, r.Scope, r.ID, ReasonInitializationMismatch); err != nil {
				return gitv1.InitializedResponse{}, err
			}
			return gitv1.InitializedResponse{}, fmt.Errorf("%w: the default branch differs from the reserved one", ErrInvalidValue)
		}
		if r, err = s.completeProvisioning(ctx, r.Scope, r.ID, req.Empty, observed); err != nil {
			return gitv1.InitializedResponse{}, err
		}
	}
	return gitv1.InitializedResponse{RepositoryID: string(r.ID), Status: r.Status, Version: int64(r.Version)}, nil
}

// ReportEmpty records the worker's report that the repository has no refs
// (empty true) or some (false), observed at observedAt (zero means now). A
// report older than the stored one is ignored, so a delayed report cannot make
// a seeded repository seedable again. The tenant is the worker identity's.
func (s *Service) ReportEmpty(ctx context.Context, tenant domain.TenantID, id ID, empty bool, observedAt time.Time) (*Repository, error) {
	if observedAt.IsZero() {
		observedAt = s.now()
	}
	r, err := s.d.Store.GetInTenant(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	return s.d.Store.Update(ctx, r.Scope, r.ID, func(r *Repository) (Effects, error) {
		_, err := r.ObserveEmpty(empty, observedAt, s.now())
		return Effects{}, err
	})
}

// ReconcileReport counts one provisioning sweep.
type ReconcileReport struct {
	Examined int
	Ready    int
	// Pending are repositories still provisioning (reason recorded).
	Pending int
}

// Reconcile finishes the interrupted provisioning of one tenant: every
// provisioning repository is initialized (idempotently) and marked ready, or
// its reason is refreshed. A failure of one repository never stops the sweep; a
// cancelled ctx or a storage failure does.
func (s *Service) Reconcile(ctx context.Context, tenant domain.TenantID) (ReconcileReport, error) {
	var rep ReconcileReport
	after := ID("")
	for {
		page, err := s.d.Store.Provisioning(ctx, tenant, after, s.o.ReconcileBatch)
		if err != nil {
			return rep, err
		}
		for _, c := range page {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			rep.Examined++
			r, err := s.Provision(ctx, c.Scope, c.ID)
			switch {
			case err != nil && ctx.Err() != nil:
				return rep, ctx.Err()
			case err != nil:
				s.d.Logger.Warn("repository provisioning sweep failed", "tenant", tenant, "repository", c.ID, "error", err)
				rep.Pending++
			case r.Status == gitv1.StatusProvisioning:
				rep.Pending++
			default:
				rep.Ready++
			}
			after = c.ID
		}
		if len(page) < s.o.ReconcileBatch {
			return rep, nil
		}
	}
}

// ReconcileAll sweeps every tenant the TenantSource lists. A tenant whose sweep
// fails does not hide the others; the first failure is returned at the end.
func (s *Service) ReconcileAll(ctx context.Context) (ReconcileReport, error) {
	if s.d.Tenants == nil {
		return ReconcileReport{}, fmt.Errorf("%w: Tenants", ErrNotConfigured)
	}
	tenants, err := s.d.Tenants.TenantIDs(ctx)
	if err != nil {
		return ReconcileReport{}, fmt.Errorf("%w: tenants: %w", ErrUnavailable, err)
	}
	var total ReconcileReport
	var first error
	for _, t := range tenants {
		rep, err := s.Reconcile(ctx, t)
		total.Examined += rep.Examined
		total.Ready += rep.Ready
		total.Pending += rep.Pending
		if err != nil {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			s.d.Logger.Warn("repository sweep failed", "tenant", t, "error", err)
			if first == nil {
				first = err
			}
		}
	}
	return total, first
}

// ---- read ----

// Get returns one repository of the scope.
func (s *Service) Get(ctx context.Context, actor Actor, scope domain.Scope, id ID) (*Repository, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionRead, scope, repositoryResource(id)); err != nil {
		return nil, err
	}
	return s.d.Store.Get(ctx, scope, id)
}

// List returns up to q.Limit repositories after the position, ordered by
// (created_at, id).
func (s *Service) List(ctx context.Context, actor Actor, scope domain.Scope, q ListQuery) ([]*Repository, error) {
	if q.Limit < 1 || q.Limit > gitv1.MaxPage+1 {
		return nil, fmt.Errorf("%w: the page size is 1 to %d", ErrInvalidValue, gitv1.MaxPage)
	}
	if q.After != nil {
		if err := q.After.ID.Validate(); err != nil {
			return nil, err
		}
	}
	if err := s.authorize(ctx, actor, ActionList, scope, s.project(scope)); err != nil {
		return nil, err
	}
	return s.d.Store.List(ctx, scope, q)
}

// CloneURL is the credential-free clone URL of the repository: an internal one
// is built by the CloneURLs port from the project and repository name (never
// from anything else), an external record keeps its recorded URL. It depends
// only on the scope and the name, so a caller may compute it before it opens a
// transaction.
func (s *Service) CloneURL(ctx context.Context, r *Repository) (string, error) {
	if r.Hosting != gitv1.HostingInternal {
		return r.ExternalURL, nil
	}
	url, err := s.d.CloneURLs.CloneURL(ctx, r.Scope, r.Name)
	if err != nil {
		return "", fmt.Errorf("%w: clone URL: %w", ErrUnavailable, err)
	}
	return url, nil
}

// Representation renders the repository as the contract's resource with the given
// clone URL and validates it against the contract.
func Representation(r *Repository, cloneURL string) (gitv1.Repository, error) {
	out := gitv1.Repository{
		ID: string(r.ID), Name: r.Name, DefaultBranch: r.DefaultBranch, Hosting: r.Hosting, Status: r.Status,
		CloneURL: cloneURL, Policy: r.Policy, Empty: r.Empty, Version: int64(r.Version), CreatedAt: gitv1.Timestamp(r.CreatedAt.UTC().Format(time.RFC3339Nano)),
	}
	if r.Reason != "" {
		reason := r.Reason
		out.Reason = &reason
	}
	if err := out.Validate(); err != nil {
		return gitv1.Repository{}, fmt.Errorf("%w: repository is not representable: %v", ErrInvalidValue, err)
	}
	return out, nil
}

// Describe renders the repository as the contract's resource (CloneURL then
// Representation).
func (s *Service) Describe(ctx context.Context, r *Repository) (gitv1.Repository, error) {
	url, err := s.CloneURL(ctx, r)
	if err != nil {
		return gitv1.Repository{}, err
	}
	return Representation(r, url)
}

// RefsQuery selects one page of a repository's refs.
type RefsQuery struct {
	// Prefix is refs/heads/ or refs/tags/; the server namespace is never public.
	Prefix string
	Cursor *string
	// Limit is 1 to gitv1.MaxPage; zero selects DefaultRefsPage.
	Limit int
}

// DefaultRefsPage is the page size of a refs read without an explicit limit.
const DefaultRefsPage = 100

// ReadRefs lists branch or tag tips through the tenant's worker. It needs an
// internal repository that is ready or archived (history is kept and stays
// readable). The worker's answer also refreshes the stored emptiness, best
// effort and never against a newer report.
func (s *Service) ReadRefs(ctx context.Context, actor Actor, scope domain.Scope, id ID, q RefsQuery) (gitv1.RefsResponse, error) {
	if q.Prefix != gitv1.HeadsPrefix && q.Prefix != gitv1.TagsPrefix {
		return gitv1.RefsResponse{}, fmt.Errorf("%w: prefix must be %s or %s", ErrInvalidValue, gitv1.HeadsPrefix, gitv1.TagsPrefix)
	}
	if q.Limit == 0 {
		q.Limit = DefaultRefsPage
	}
	req := gitv1.ReadRefsRequest{RepositoryID: string(id), Prefix: q.Prefix, Cursor: q.Cursor, Limit: int64(q.Limit)}
	if err := req.Validate(); err != nil {
		return gitv1.RefsResponse{}, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	r, err := s.Get(ctx, actor, scope, id)
	if err != nil {
		return gitv1.RefsResponse{}, err
	}
	if err := r.requireInternal(); err != nil {
		return gitv1.RefsResponse{}, err
	}
	if r.Status == gitv1.StatusProvisioning {
		return gitv1.RefsResponse{}, ErrNotReady
	}
	observed := s.now()
	hctx, cancel := context.WithTimeout(ctx, s.o.HostsDeadline)
	defer cancel()
	resp, err := s.d.Hosts.ReadRefs(hctx, scope.TenantID, req)
	if err != nil {
		return gitv1.RefsResponse{}, fmt.Errorf("%w: refs: %w", ErrUnavailable, err)
	}
	if err := resp.Validate(); err != nil {
		return gitv1.RefsResponse{}, fmt.Errorf("%w: refs: %v", ErrUnavailable, err)
	}
	// Every validated observation goes through the transactional observe path,
	// also an unchanged one: it advances the observation, so a delayed older
	// report that contradicts newer evidence is ignored (GR-18). The snapshot
	// read above decides nothing.
	if _, err := s.ReportEmpty(ctx, scope.TenantID, r.ID, resp.Empty, observed); err != nil {
		s.d.Logger.Warn("repository emptiness not refreshed", "repository", r.ID, "error", err)
	}
	return resp, nil
}

// ---- change ----

// UpdateCommand patches a repository's default branch and policy.
type UpdateCommand struct {
	Actor Actor
	Scope domain.Scope
	ID    ID
	// Expected is the If-Match version; zero compares nothing (an internal caller).
	Expected domain.Version
	Patch    gitv1.PatchRepositoryRequest
}

// Update changes the default branch and/or the policy under an expected
// version. Changing the branch needs repository.update and a policy change
// repository.set_policy; a patch naming both needs both. Only a ready internal
// repository is patched (an archived one is restored first, a provisioning one
// finishes first, an external record is read-only). A patch that changes
// nothing writes nothing.
func (s *Service) Update(ctx context.Context, c UpdateCommand) (*Repository, error) {
	if err := c.ID.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := c.Patch.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	if c.Patch.DefaultBranch != nil {
		if err := s.authorize(ctx, c.Actor, ActionUpdate, c.Scope, repositoryResource(c.ID)); err != nil {
			return nil, err
		}
	}
	if c.Patch.Policy != nil {
		if err := s.authorize(ctx, c.Actor, ActionSetPolicy, c.Scope, repositoryResource(c.ID)); err != nil {
			return nil, err
		}
	}
	return s.d.Store.Update(ctx, c.Scope, c.ID, func(r *Repository) (Effects, error) {
		if err := checkExpected(c.Expected, r); err != nil {
			return Effects{}, err
		}
		patch := Patch{DefaultBranch: c.Patch.DefaultBranch}
		if p := c.Patch.Policy; p != nil {
			patch.Policy = &gitv1.RepositoryPolicy{DirectPushToDefault: *p.DirectPushToDefault}
		}
		// One transition: both settings, one version step, one transaction.
		res, err := r.ApplyPatch(patch, s.now())
		if err != nil {
			return Effects{}, err
		}
		var fx Effects
		if res.BranchChanged {
			fx.Audit = append(fx.Audit, audit("repository.default_branch_changed", c.Actor, "", map[string]any{"from": res.BranchFrom, "to": r.DefaultBranch}))
		}
		if res.PolicyChanged {
			fx.Audit = append(fx.Audit, audit("repository.policy_changed", c.Actor, "", map[string]any{"directPushToDefault": r.Policy.DirectPushToDefault}))
		}
		return fx, nil
	})
}

func checkExpected(expected domain.Version, r *Repository) error {
	if expected != 0 && expected != r.Version {
		return ErrVersionConflict
	}
	return nil
}

// Archive closes admission: no new pushes and no new runs on the repository;
// history is never deleted. Archiving an archived repository is a no-op that
// still checks the expected version.
func (s *Service) Archive(ctx context.Context, actor Actor, scope domain.Scope, id ID, expected domain.Version) (*Repository, error) {
	return s.transition(ctx, actor, ActionArchive, scope, id, expected, func(r *Repository, now time.Time) (bool, error) { return r.Archive(now) },
		"repository.archived", TransitionArchived, gitv1.StatusReady, gitv1.StatusArchived)
}

// Restore reopens an archived repository. It never makes a non-empty
// repository seedable again.
func (s *Service) Restore(ctx context.Context, actor Actor, scope domain.Scope, id ID, expected domain.Version) (*Repository, error) {
	return s.transition(ctx, actor, ActionRestore, scope, id, expected, func(r *Repository, now time.Time) (bool, error) { return r.Restore(now) },
		"repository.restored", TransitionRestored, gitv1.StatusArchived, gitv1.StatusReady)
}

func (s *Service) transition(ctx context.Context, actor Actor, action Action, scope domain.Scope, id ID, expected domain.Version,
	do func(*Repository, time.Time) (bool, error), auditAction, reason string, from, to gitv1.RepositoryStatus) (*Repository, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, action, scope, repositoryResource(id)); err != nil {
		return nil, err
	}
	return s.d.Store.Update(ctx, scope, id, func(r *Repository) (Effects, error) {
		if err := checkExpected(expected, r); err != nil {
			return Effects{}, err
		}
		changed, err := do(r, s.now())
		if err != nil || !changed {
			return Effects{}, err
		}
		return Effects{
			Audit:  []Audit{audit(auditAction, actor, reason, nil)},
			Events: []Event{{Kind: EventStateChanged, Actor: actor, From: string(from), To: string(to), Reason: reason}},
		}, nil
	})
}
