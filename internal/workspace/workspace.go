// Package workspace implements Kodama's Git-backed source continuity: one
// durable workspace per actionable work item, writer reservation, controlled
// publication under expected-tip compare-and-swap, human synchronization,
// archive/restore and retained source references.
//
// The package holds entities, value objects, use cases and the small
// consumer-owned ports they need. It contains no pgx, HTTP or process
// dependencies: PostgreSQL (postgres/), Git (git/), HTTP (http/) and runtime
// contract translation (runtime/) are adapters wired by the composition root.
//
// A workspace, a writer reservation, a publication and a retained reference
// have separate lifecycles; each transition is an explicit entity method.
package workspace

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ID identifies a workspace. It follows the opaque identifier syntax shared by
// every module.
type ID string

func (id ID) Validate() error { return validateID("workspace id", string(id)) }

// RepositoryID identifies the project's configured repository.
type RepositoryID string

func (id RepositoryID) Validate() error { return validateID("repository id", string(id)) }

// PinID identifies a retained-reference pin.
type PinID string

func (id PinID) Validate() error { return validateID("pin id", string(id)) }

var idRegexp = regexp.MustCompile(domain.IDPattern)

func validateID(kind, v string) error {
	if !idRegexp.MatchString(v) {
		return fmt.Errorf("%w: %s must match %s", ErrInvalidValue, kind, domain.IDPattern)
	}
	return nil
}

// Commit is a full Git object name (SHA-1 or SHA-256, lowercase hex).
type Commit string

var commitRegexp = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

func (c Commit) Validate() error {
	if !commitRegexp.MatchString(string(c)) {
		return fmt.Errorf("%w: commit must be a full lowercase object name", ErrInvalidValue)
	}
	return nil
}

func (c Commit) String() string { return string(c) }

// State is the workspace's availability. Errors are reasons on the resource,
// never states.
type State string

const (
	StateProvisioning State = "provisioning"
	StateAvailable    State = "available"
	StateArchived     State = "archived"
)

// BranchPrefix is the default portable work branch namespace.
const BranchPrefix = "refs/heads/kodama/work/"

// WorkBranch returns the default work branch of a work item.
func WorkBranch(id domain.WorkItemID) string { return BranchPrefix + string(id) }

// ValidateBranch accepts only ordinary branch references with a conservative
// character set, so a branch can never smuggle options or traversal into Git.
func ValidateBranch(ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/") || len(ref) > 255 || !refRegexp.MatchString(ref) ||
		strings.Contains(ref, "..") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") || strings.Contains(ref, "//") {
		return fmt.Errorf("%w: %q is not a valid branch reference", ErrInvalidValue, ref)
	}
	return nil
}

var refRegexp = regexp.MustCompile(`^refs/heads/[A-Za-z0-9][A-Za-z0-9_./-]*$`)

// ActorType identifies who caused a change.
type ActorType string

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

// WriterReservation is the sole publication slot of a workspace.
type WriterReservation struct {
	AttemptID domain.AttemptID
	// InputCommit is the commit the attempt checks out; ExpectedTip is the
	// branch tip observed at admission and the CAS value of its publication.
	InputCommit Commit
	ExpectedTip Commit
	ReservedAt  time.Time
}

// PendingPublication is the durable intent of the one in-flight publication.
// It exists from before the Git push until the result is known and recorded.
type PendingPublication struct {
	AttemptID      domain.AttemptID
	Commit         Commit // publish_commit
	ExpectedCommit Commit // publish_expected_commit
	ArtifactID     domain.ArtifactID
	BundleDigest   string
	StartedAt      time.Time
	// Driver identifies one invocation driving the push, including within a
	// single process. Its claim lapsing never starts a different publication;
	// a later driver resumes this same expected-old-ref CAS intent.
	DriverID      string
	DriverEpoch   int64
	DriverClaimed time.Time
	Attempts      int
	LastAttemptAt time.Time
}

// Fence is the installation-recovery fence on publication authority.
type Fence struct {
	Epoch  int64
	Fenced bool
}

// Workspace is the aggregate. Fields are exported for adapters; mutate it only
// through its methods so every invariant and transition stays in one place.
type Workspace struct {
	ID         ID
	Scope      domain.Scope
	WorkItemID domain.WorkItemID

	RepositoryID  RepositoryID
	RepositoryURL string
	// CheckoutSecretRef and PublisherSecretRef are distinct references; the
	// publisher reference is only ever resolved by the publisher adapter.
	CheckoutSecretRef  string
	PublisherSecretRef string
	WorkBranch         string

	// InitialCommit is the recorded intent/result of branch creation.
	InitialCommit  Commit
	AcceptedCommit Commit
	State          State
	Version        domain.Version

	Writer             *WriterReservation
	Pending            *PendingPublication
	ArchiveRequestedAt *time.Time
	// LastError is a safe reason code, never free text.
	LastError string
	Fence     Fence

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewParams creates a workspace in provisioning state.
type NewParams struct {
	ID                 ID
	Scope              domain.Scope
	WorkItemID         domain.WorkItemID
	RepositoryID       RepositoryID
	RepositoryURL      string
	CheckoutSecretRef  string
	PublisherSecretRef string
}

// New validates the parameters and returns a provisioning workspace at
// version 1 on the default work branch.
func New(p NewParams, now time.Time) (*Workspace, error) {
	if err := firstErr(p.ID.Validate(), p.Scope.Validate(), p.WorkItemID.Validate(), p.RepositoryID.Validate()); err != nil {
		return nil, err
	}
	if p.RepositoryURL == "" || len(p.RepositoryURL) > 2048 {
		return nil, fmt.Errorf("%w: repository url", ErrInvalidValue)
	}
	if p.CheckoutSecretRef == "" || p.PublisherSecretRef == "" || p.CheckoutSecretRef == p.PublisherSecretRef {
		return nil, fmt.Errorf("%w: checkout and publisher secret references must both be set and distinct", ErrInvalidValue)
	}
	branch := WorkBranch(p.WorkItemID)
	if err := ValidateBranch(branch); err != nil {
		return nil, err
	}
	return &Workspace{
		ID: p.ID, Scope: p.Scope, WorkItemID: p.WorkItemID,
		RepositoryID: p.RepositoryID, RepositoryURL: p.RepositoryURL,
		CheckoutSecretRef: p.CheckoutSecretRef, PublisherSecretRef: p.PublisherSecretRef,
		WorkBranch: branch, State: StateProvisioning, Version: 1,
		Fence:     Fence{Epoch: 1},
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (w *Workspace) touch(now time.Time) {
	w.Version++
	w.UpdatedAt = now.UTC()
}

func (w *Workspace) transitionErr(op string) error {
	return fmt.Errorf("%w: %s from state %s", ErrInvalidTransition, op, w.State)
}

// CheckVersion compares an expected version (If-Match); zero means no
// precondition. It must run inside the transaction that holds the row.
func (w *Workspace) CheckVersion(expected domain.Version) error {
	if expected != 0 && expected != w.Version {
		return ErrVersionConflict
	}
	return nil
}

// AdmissionOpen reports whether new source may be admitted.
func (w *Workspace) AdmissionOpen() bool {
	return w.State == StateAvailable && w.ArchiveRequestedAt == nil && !w.Fence.Fenced
}

// ---- provisioning ----

// RecordInitialIntent persists the commit the branch will be created at,
// before the branch is created. The first recorded intent wins; it reports
// whether this call recorded it.
func (w *Workspace) RecordInitialIntent(c Commit, now time.Time) (bool, error) {
	if w.State != StateProvisioning {
		return false, w.transitionErr("record initial intent")
	}
	if err := c.Validate(); err != nil {
		return false, err
	}
	if w.InitialCommit != "" {
		return false, nil
	}
	w.InitialCommit = c
	w.touch(now)
	return true, nil
}

// RecordProvisioningError keeps the workspace provisioning and records a safe
// reason for the retry. It reports whether anything changed.
func (w *Workspace) RecordProvisioningError(reason string, now time.Time) (bool, error) {
	if w.State != StateProvisioning {
		return false, w.transitionErr("record provisioning error")
	}
	if err := validateReason(reason); err != nil {
		return false, err
	}
	if w.LastError == reason {
		return false, nil
	}
	w.LastError = reason
	w.touch(now)
	return true, nil
}

// CompleteProvisioning makes the workspace available at the confirmed initial
// commit.
func (w *Workspace) CompleteProvisioning(commit Commit, now time.Time) error {
	if w.State != StateProvisioning {
		return w.transitionErr("complete provisioning")
	}
	if err := commit.Validate(); err != nil {
		return err
	}
	w.AcceptedCommit, w.State, w.LastError = commit, StateAvailable, ""
	if w.InitialCommit == "" {
		w.InitialCommit = commit
	}
	w.touch(now)
	return nil
}

var reasonRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func validateReason(r string) error {
	if !reasonRegexp.MatchString(r) {
		return fmt.Errorf("%w: reason must be a lowercase code", ErrInvalidValue)
	}
	return nil
}

// ---- writer reservation ----

// ReserveWriter reserves the single publication slot. tip must equal the
// accepted commit the service just refreshed from the remote, so a stale
// observation cannot be reserved. A repeated reservation by the same attempt
// with the same pins is idempotent.
func (w *Workspace) ReserveWriter(attempt domain.AttemptID, input, tip Commit, now time.Time) error {
	if err := firstErr(attempt.Validate(), input.Validate(), tip.Validate()); err != nil {
		return err
	}
	if w.Writer != nil && w.Writer.AttemptID == attempt {
		if w.Writer.InputCommit == input && w.Writer.ExpectedTip == tip {
			return nil
		}
		return fmt.Errorf("%w: attempt already reserved with different pins", ErrInvalidTransition)
	}
	switch {
	case w.Fence.Fenced:
		return ErrFenced
	case w.State != StateAvailable:
		return w.transitionErr("reserve writer")
	case w.ArchiveRequestedAt != nil:
		return ErrAdmissionClosed
	case w.Writer != nil:
		return ErrWriterBusy
	case tip != w.AcceptedCommit:
		return ErrStale
	}
	w.Writer = &WriterReservation{AttemptID: attempt, InputCommit: input, ExpectedTip: tip, ReservedAt: now.UTC()}
	w.touch(now)
	return nil
}

// ReleaseWriter frees the slot after publication and workload/result
// settlement. A publication that is still unsettled blocks release; a writer
// that was already released is reported as ErrNotWriter.
func (w *Workspace) ReleaseWriter(attempt domain.AttemptID, now time.Time) error {
	if w.Writer == nil || w.Writer.AttemptID != attempt {
		return ErrNotWriter
	}
	if w.Pending != nil {
		return ErrPublicationPending
	}
	w.Writer = nil
	w.touch(now)
	return nil
}

// ---- human synchronization ----

// ObserveHumanTip records an authorized fast-forward of the work branch made
// outside Kodama. from is the accepted commit the caller compared against; a
// concurrent change yields ErrStale. A tip that is not a fast-forward is never
// adopted (no merge, no rewind): the reason is recorded and ErrDiverged
// returned by the caller after persisting it.
func (w *Workspace) ObserveHumanTip(from, tip Commit, fastForward bool, now time.Time) (changed bool, err error) {
	if err := tip.Validate(); err != nil {
		return false, err
	}
	switch {
	case w.State != StateAvailable:
		return false, w.transitionErr("synchronize")
	case w.Pending != nil:
		return false, ErrPublicationPending
	case w.AcceptedCommit != from:
		return false, ErrStale
	case tip == w.AcceptedCommit:
		if w.LastError == ReasonBranchDiverged || w.LastError == ReasonBranchMissing {
			w.LastError = ""
			w.touch(now)
			return true, nil
		}
		return false, nil
	case !fastForward:
		if w.LastError != ReasonBranchDiverged {
			w.LastError = ReasonBranchDiverged
			w.touch(now)
			return true, ErrDiverged
		}
		return false, ErrDiverged
	}
	w.AcceptedCommit, w.LastError = tip, ""
	w.touch(now)
	return true, nil
}

// RecordBranchMissing notes that the remote work branch no longer exists.
// Nothing is recreated or adopted; the safe reason blocks writer admission until
// the branch is restored. It always returns ErrDiverged (the observation is not
// a usable tip) together with whether anything changed.
func (w *Workspace) RecordBranchMissing(from Commit, now time.Time) (bool, error) {
	switch {
	case w.State != StateAvailable:
		return false, w.transitionErr("synchronize")
	case w.Pending != nil:
		return false, ErrPublicationPending
	case w.AcceptedCommit != from:
		return false, ErrStale
	case w.LastError == ReasonBranchMissing:
		return false, ErrDiverged
	}
	w.LastError = ReasonBranchMissing
	w.touch(now)
	return true, ErrDiverged
}

// Safe reason codes recorded on workspaces.
const (
	ReasonBranchMissing     = "branch_missing"
	ReasonBranchDiverged    = "branch_diverged"
	ReasonBranchExists      = "branch_exists"
	ReasonProvisionFailed   = "provision_unavailable"
	ReasonRepositoryUnready = "repository_unavailable"
	ReasonPublicationFailed = "publication_unavailable"
)

// ---- archive and restore ----

// RequestArchive durably closes source admission. It is idempotent.
func (w *Workspace) RequestArchive(now time.Time) (changed bool, err error) {
	switch w.State {
	case StateArchived:
		return false, nil
	case StateAvailable:
	default:
		return false, w.transitionErr("archive")
	}
	if w.ArchiveRequestedAt != nil {
		return false, nil
	}
	t := now.UTC()
	w.ArchiveRequestedAt = &t
	w.touch(now)
	return true, nil
}

// CompleteArchive archives a workspace whose admission is closed and whose
// publication is settled.
func (w *Workspace) CompleteArchive(now time.Time) (changed bool, err error) {
	switch {
	case w.State == StateArchived:
		return false, nil
	case w.State != StateAvailable || w.ArchiveRequestedAt == nil:
		return false, w.transitionErr("complete archive")
	case w.Pending != nil:
		return false, ErrPublicationPending
	}
	w.State = StateArchived
	w.touch(now)
	return true, nil
}

// Restore reopens the same workspace and branch. An archive that was requested
// but has not settled can be cancelled only once its publication settled.
func (w *Workspace) Restore(now time.Time) error {
	switch {
	case w.Pending != nil:
		return ErrPublicationPending
	case w.State == StateArchived:
		w.State = StateAvailable
	case w.State == StateAvailable && w.ArchiveRequestedAt != nil:
	default:
		return w.transitionErr("restore")
	}
	w.ArchiveRequestedAt = nil
	w.touch(now)
	return nil
}

// ---- installation-recovery fence ----

// FencePublishing revokes every in-flight publisher: claims made under an
// earlier epoch can no longer finalize, and no new writer or proposal is
// admitted until publishing is resumed.
func (w *Workspace) FencePublishing(now time.Time) {
	w.Fence.Epoch++
	w.Fence.Fenced = true
	w.touch(now)
}

// ResumePublishing lifts the fence after the caller verified that publisher
// credentials were rotated.
func (w *Workspace) ResumePublishing(now time.Time) bool {
	if !w.Fence.Fenced {
		return false
	}
	w.Fence.Fenced = false
	w.touch(now)
	return true
}
