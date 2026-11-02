// Package repository implements the lifecycle and persistence of project-owned
// Git repositories (docs/specs/git-repositories.md, GR-01, GR-02, GR-14,
// GR-19): the entity with its explicit status transitions, the create, read,
// list, patch, archive and restore use cases with current authorization,
// provisioning through the consumer-owned Hosts port outside any SQL
// transaction, an idempotent reconciler for interrupted provisioning, and the
// read-only mapping of a project's existing configured repository to a
// hosting: external record.
//
// PostgreSQL owns a repository's metadata; the tenant's Git worker owns refs
// and objects. The package holds the entity, the use cases and the small ports
// they consume. It contains no pgx, HTTP or process dependency: PostgreSQL
// (postgres/), memory (repositorytest/) and HTTP (http/) are adapters wired by
// the composition root. The wire shapes, validation and codes are the
// contracts/git/v1 ones; nothing is redefined here.
//
// A repository, a workspace, a run and a gate have independent lifecycles.
// Transitions of this entity are provisioning to ready, ready to archived and
// archived to ready. A provisioning failure is a Reason on a provisioning
// repository, retried by reconciliation, never another status.
package repository

import (
	"fmt"
	"regexp"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ID identifies a repository: server-assigned, opaque and immutable. Storage
// paths on the worker derive only from it, never from the name.
type ID string

var idRegexp = regexp.MustCompile(domain.IDPattern)

// Validate checks the shared opaque identifier syntax.
func (id ID) Validate() error {
	if !idRegexp.MatchString(string(id)) {
		return fmt.Errorf("%w: repository id must match %s", ErrInvalidValue, domain.IDPattern)
	}
	return nil
}

// Stable reason codes stored on a repository (gitv1.Repository.Reason). A
// reason is non-null exactly while the repository is not ready.
const (
	// ReasonProvisioningPending: reserved; initialization has not been confirmed.
	ReasonProvisioningPending = "provisioning_pending"
	// ReasonWorkerUnavailable: the worker was provably not reached; retried.
	ReasonWorkerUnavailable = "worker_unavailable"
	// ReasonInitializationUnknown: the request may have been applied; the same
	// request is repeated (it is idempotent on the repository ID).
	ReasonInitializationUnknown = "initialization_unknown"
	// ReasonInitializationRefused: the worker refused the request; still retried
	// by reconciliation, the code stays visible until it succeeds.
	ReasonInitializationRefused = "initialization_refused"
	// ReasonInitializationMismatch: the worker reported a repository that does
	// not match the reserved record (another default branch or ID).
	ReasonInitializationMismatch = "initialization_mismatch"
	// ReasonArchived: the repository was archived on request.
	ReasonArchived = "archived"
)

// Reasons that move a repository's status (the event reason of state_changed).
const (
	TransitionInitialized = "initialized"
	TransitionArchived    = "archived"
	TransitionRestored    = "restored"
)

// ActorType identifies who caused a change.
type ActorType string

// Actor types (the audit and event vocabularies' names).
const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is the principal behind a use case call.
type Actor struct {
	Type ActorType
	ID   string
}

// Validate checks the actor type and identifier.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidValue, a.Type)
	}
	if a.ID == "" || len(a.ID) > 256 {
		return fmt.Errorf("%w: actor id must be 1..256 bytes", ErrInvalidValue)
	}
	return nil
}

// Repository is the aggregate. Its fields are exported for the adapters; every
// business change goes through the methods below, which keep the invariants.
type Repository struct {
	ID    ID
	Scope domain.Scope
	// Name is project-unique and immutable.
	Name          string
	DefaultBranch string
	Hosting       gitv1.Hosting
	Status        gitv1.RepositoryStatus
	// Reason is the stable code explaining a status other than ready.
	Reason string
	Policy gitv1.RepositoryPolicy
	// Empty is the tenant worker's last report that the repository has no refs;
	// it decides whether a seed can be issued. Provisioning repositories and
	// external records are never empty.
	Empty bool
	// EmptyObservedAt is when the worker's last report was observed; an older
	// report never overwrites a newer one.
	EmptyObservedAt time.Time
	// ExternalURL is the credential-free URL of a hosting: external record; it
	// is empty for an internal repository.
	ExternalURL string
	// CreatedBy is who requested the repository (the actor of repository.created).
	CreatedBy Actor
	Version   domain.Version
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams describe an internal repository to reserve.
type NewParams struct {
	ID            ID
	Scope         domain.Scope
	Name          string
	DefaultBranch string // "" selects gitv1.DefaultBranchName
	Policy        gitv1.RepositoryPolicy
	CreatedBy     Actor
}

func checkSpec(name, branch string) error {
	req := gitv1.CreateRepositoryRequest{Name: name, DefaultBranch: &branch}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return nil
}

// New reserves an internal repository in the provisioning status with the
// pending reason. Nothing is created in Git yet; the use case provisions it.
func New(p NewParams, now time.Time) (*Repository, error) {
	if err := firstErr(p.ID.Validate(), p.Scope.Validate(), p.CreatedBy.Validate()); err != nil {
		return nil, err
	}
	if p.DefaultBranch == "" {
		p.DefaultBranch = gitv1.DefaultBranchName
	}
	if err := checkSpec(p.Name, p.DefaultBranch); err != nil {
		return nil, err
	}
	now = now.UTC()
	return &Repository{
		ID: p.ID, Scope: p.Scope, Name: p.Name, DefaultBranch: p.DefaultBranch,
		Hosting: gitv1.HostingInternal, Status: gitv1.StatusProvisioning, Reason: ReasonProvisioningPending,
		Policy: p.Policy, CreatedBy: p.CreatedBy, Version: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// NewExternalParams describe the read-only record of an externally hosted
// repository.
type NewExternalParams struct {
	ID            ID
	Scope         domain.Scope
	Name          string
	DefaultBranch string
	// URL is the credential-free HTTPS URL of the externally hosted repository.
	URL       string
	CreatedBy Actor
}

// NewExternal records an externally hosted repository: ready from the start
// (nothing is provisioned), never empty, with the all-false policy. It is a
// read-only mapping: it has no worker, so every later change is refused with
// ErrHostingMismatch.
func NewExternal(p NewExternalParams, now time.Time) (*Repository, error) {
	if err := firstErr(p.ID.Validate(), p.Scope.Validate(), p.CreatedBy.Validate()); err != nil {
		return nil, err
	}
	if p.DefaultBranch == "" {
		p.DefaultBranch = gitv1.DefaultBranchName
	}
	if err := checkSpec(p.Name, p.DefaultBranch); err != nil {
		return nil, err
	}
	if !urlPattern.MatchString(p.URL) || len(p.URL) > 2048 {
		return nil, fmt.Errorf("%w: an external repository URL is an https URL without credentials", ErrInvalidValue)
	}
	now = now.UTC()
	return &Repository{
		ID: p.ID, Scope: p.Scope, Name: p.Name, DefaultBranch: p.DefaultBranch,
		Hosting: gitv1.HostingExternal, Status: gitv1.StatusReady, ExternalURL: p.URL,
		CreatedBy: p.CreatedBy, Version: 1, CreatedAt: now, UpdatedAt: now,
	}, nil
}

var urlPattern = regexp.MustCompile(gitv1.Patterns["RepositoryURL"])

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) touch(now time.Time) {
	r.Version++
	r.UpdatedAt = now.UTC()
}

// requireInternal is the guard of every change that needs a Git worker.
func (r *Repository) requireInternal() error {
	if r.Hosting != gitv1.HostingInternal {
		return ErrHostingMismatch
	}
	return nil
}

// requireReady refuses a repository that is provisioning or archived.
func (r *Repository) requireReady() error {
	switch r.Status {
	case gitv1.StatusReady:
		return nil
	case gitv1.StatusArchived:
		return ErrArchived
	}
	return ErrNotReady
}

// CompleteProvisioning moves provisioning to ready after the worker confirmed
// the initialization, recording whether it has no refs. It is the only way out
// of provisioning.
func (r *Repository) CompleteProvisioning(empty bool, observedAt, now time.Time) error {
	if err := r.requireInternal(); err != nil {
		return err
	}
	if r.Status != gitv1.StatusProvisioning {
		return fmt.Errorf("%w: %s repository cannot complete provisioning", ErrInvalidValue, r.Status)
	}
	r.Status, r.Reason = gitv1.StatusReady, ""
	r.Empty, r.EmptyObservedAt = empty, observedAt.UTC()
	r.touch(now)
	return nil
}

// RecordProvisioningReason sets the visible reason of a provisioning repository.
// A reason already recorded changes nothing (no version bump), so a reconciler
// that keeps failing does not rewrite the row.
func (r *Repository) RecordProvisioningReason(reason string, now time.Time) (changed bool, err error) {
	if err := r.requireInternal(); err != nil {
		return false, err
	}
	if r.Status != gitv1.StatusProvisioning {
		return false, ErrInvalidValue
	}
	if r.Reason == reason {
		return false, nil
	}
	r.Reason = reason
	r.touch(now)
	return true, nil
}

// Archive moves a ready repository to archived: it refuses new pushes and new
// runs; it never deletes history. Archiving an archived repository is a no-op.
func (r *Repository) Archive(now time.Time) (changed bool, err error) {
	if err := r.requireInternal(); err != nil {
		return false, err
	}
	switch r.Status {
	case gitv1.StatusArchived:
		return false, nil
	case gitv1.StatusProvisioning:
		return false, ErrNotReady
	}
	r.Status, r.Reason = gitv1.StatusArchived, ReasonArchived
	r.touch(now)
	return true, nil
}

// Restore reopens an archived repository. It never makes a non-empty
// repository seedable again: Empty is not touched. Restoring a ready
// repository is a no-op.
func (r *Repository) Restore(now time.Time) (changed bool, err error) {
	if err := r.requireInternal(); err != nil {
		return false, err
	}
	switch r.Status {
	case gitv1.StatusReady:
		return false, nil
	case gitv1.StatusProvisioning:
		return false, ErrNotReady
	}
	r.Status, r.Reason = gitv1.StatusReady, ""
	r.touch(now)
	return true, nil
}

// Patch is a set of settings changed together: one transition, one version.
type Patch struct {
	DefaultBranch *string
	Policy        *gitv1.RepositoryPolicy
}

// PatchResult says what a Patch changed.
type PatchResult struct {
	BranchFrom    string
	BranchChanged bool
	PolicyChanged bool
}

// Changed reports whether the patch changed anything.
func (p PatchResult) Changed() bool { return p.BranchChanged || p.PolicyChanged }

// ApplyPatch applies every named setting as one transition: all of it is
// validated first, nothing is applied on an error, and a patch that changes
// something advances the version exactly once (the storage guard requires a
// single step), however many settings it names.
//
// The default branch may change at any time while the repository is ready
// (before seeding it changes the ref the seed must create); it renames nothing
// in Git and applies to the next push. directPushToDefault never changes by
// itself and defaults to false (O-33); enabling it grants no one a push:
// authority is checked at every push. A provisioning repository's branch is
// fixed by the initialization already requested; an archived one is reopened
// first; an external record is read-only.
func (r *Repository) ApplyPatch(p Patch, now time.Time) (PatchResult, error) {
	var res PatchResult
	if err := r.requireInternal(); err != nil {
		return res, err
	}
	if err := r.requireReady(); err != nil {
		return res, err
	}
	if p.DefaultBranch != nil {
		if err := checkSpec(r.Name, *p.DefaultBranch); err != nil {
			return res, err
		}
	}
	res.BranchFrom = r.DefaultBranch
	res.BranchChanged = p.DefaultBranch != nil && *p.DefaultBranch != r.DefaultBranch
	res.PolicyChanged = p.Policy != nil && *p.Policy != r.Policy
	if res.BranchChanged {
		r.DefaultBranch = *p.DefaultBranch
	}
	if res.PolicyChanged {
		r.Policy = *p.Policy
	}
	if res.Changed() {
		r.touch(now)
	}
	return res, nil
}

// ChangeDefaultBranch is ApplyPatch of the branch alone.
func (r *Repository) ChangeDefaultBranch(branch string, now time.Time) (changed bool, err error) {
	res, err := r.ApplyPatch(Patch{DefaultBranch: &branch}, now)
	return res.BranchChanged, err
}

// SetPolicy is ApplyPatch of the policy alone.
func (r *Repository) SetPolicy(p gitv1.RepositoryPolicy, now time.Time) (changed bool, err error) {
	res, err := r.ApplyPatch(Patch{Policy: &p}, now)
	return res.PolicyChanged, err
}

// CheckChange is the storage guard's rule as pure code, so an adapter without a
// trigger (memory) enforces it too: identity, name, hosting, URL and creator
// never change; a change advances the version by exactly one; the only change
// without a version bump is a newer emptiness observation; a status moves only
// provisioning to ready, ready to archived, archived to ready; an observation
// never moves back.
func CheckChange(before, after *Repository) error {
	if before.ID != after.ID || before.Scope != after.Scope || before.Name != after.Name || before.Hosting != after.Hosting ||
		before.ExternalURL != after.ExternalURL || before.CreatedBy != after.CreatedBy || !before.CreatedAt.Equal(after.CreatedAt) {
		return fmt.Errorf("%w: repository identity, name, hosting, URL and creator are immutable", ErrInvalidValue)
	}
	if !before.EmptyObservedAt.IsZero() && after.EmptyObservedAt.Before(before.EmptyObservedAt) {
		return fmt.Errorf("%w: an emptiness observation never moves back", ErrInvalidValue)
	}
	if after.Version == before.Version {
		b, a := *before, *after
		b.EmptyObservedAt, a.EmptyObservedAt = time.Time{}, time.Time{}
		if a != b {
			return fmt.Errorf("%w: a repository change advances its version", ErrInvalidValue)
		}
		return nil
	}
	if after.Version != before.Version+1 {
		return fmt.Errorf("%w: repository version advances by one", ErrInvalidValue)
	}
	if after.Status != before.Status && !gitv1.ValidRepositoryTransition(before.Status, after.Status) {
		return fmt.Errorf("%w: repository status %s cannot become %s", ErrInvalidValue, before.Status, after.Status)
	}
	return nil
}

// ObserveEmpty records the worker's report that the repository has (or has no)
// refs, observed at observedAt. A report older than the recorded one is ignored
// (a delayed report must never reopen a seeded repository, GR-18). It applies
// to a ready or archived internal repository; provisioning completes with its
// own report.
func (r *Repository) ObserveEmpty(empty bool, observedAt, now time.Time) (changed bool, err error) {
	if err := r.requireInternal(); err != nil {
		return false, err
	}
	if r.Status == gitv1.StatusProvisioning {
		return false, ErrNotReady
	}
	observedAt = observedAt.UTC()
	if observedAt.Before(r.EmptyObservedAt) {
		return false, nil // stale: a newer report already decided
	}
	if r.Empty == empty {
		// Only the observation moves forward; the resource did not change, so the
		// version stays (the store persists the row because it differs).
		r.EmptyObservedAt = observedAt
		return false, nil
	}
	r.Empty, r.EmptyObservedAt = empty, observedAt
	r.touch(now)
	return true, nil
}

// Equivalent reports whether a create request for the same name asks for what
// this repository already is, so a retry converges instead of conflicting.
func (r *Repository) Equivalent(o *Repository) bool {
	return r.Hosting == gitv1.HostingInternal && o.Hosting == gitv1.HostingInternal &&
		r.DefaultBranch == o.DefaultBranch && r.Policy == o.Policy
}

// Validate checks the invariants the adapters also enforce in storage.
func (r *Repository) Validate() error {
	if err := firstErr(r.ID.Validate(), r.Scope.Validate(), r.CreatedBy.Validate(), r.Version.Validate()); err != nil {
		return err
	}
	if err := checkSpec(r.Name, r.DefaultBranch); err != nil {
		return err
	}
	switch r.Hosting {
	case gitv1.HostingInternal:
		if r.ExternalURL != "" {
			return fmt.Errorf("%w: an internal repository has no external URL", ErrInvalidValue)
		}
	case gitv1.HostingExternal:
		if r.ExternalURL == "" || r.Status != gitv1.StatusReady || r.Empty {
			return fmt.Errorf("%w: an external record is a ready, non-empty mapping with a URL", ErrInvalidValue)
		}
	default:
		return fmt.Errorf("%w: unknown hosting %q", ErrInvalidValue, r.Hosting)
	}
	switch r.Status {
	case gitv1.StatusReady:
		if r.Reason != "" {
			return fmt.Errorf("%w: a ready repository has no reason", ErrInvalidValue)
		}
	case gitv1.StatusProvisioning, gitv1.StatusArchived:
		if r.Reason == "" {
			return fmt.Errorf("%w: a %s repository states its reason", ErrInvalidValue, r.Status)
		}
		if r.Status == gitv1.StatusProvisioning && r.Empty {
			return fmt.Errorf("%w: a provisioning repository is not reported empty", ErrInvalidValue)
		}
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalidValue, r.Status)
	}
	return nil
}

// Clone copies the aggregate so a caller never shares it with a stored row.
func (r *Repository) Clone() *Repository {
	c := *r
	return &c
}
