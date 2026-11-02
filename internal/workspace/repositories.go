package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// This file is the internally hosted part of the model (ADR 0011, WS-17 to
// WS-19). An internally hosted workspace keeps the one-workspace-per-work-item
// and one-writer-reservation rules; what changes is where source lives: for each
// repository the work item touches the workspace records its task branch and the
// commit last accepted there (RepositoryAssociation), and a writer's pushed
// commits are accepted from applied push records (InternalCheckpoint) instead of
// through the publisher's bundle protocol.

// NoPublisherRef is the PublisherSecretRef of an internally hosted workspace.
// It is not a secret reference and is never resolved: no publisher exists for
// internal repositories, the attempt's own Git grant pushes. It only satisfies
// the "distinct, non-empty" rule the shared record keeps for both hostings.
const NoPublisherRef = "kodama:internal-hosting:no-publisher"

// MaxRepositories bounds the repositories one workspace associates.
const MaxRepositories = 32

// Safe reason codes of the internal hosting path.
const (
	ReasonRepositoryEmpty = "repository_empty"
	ReasonSecretInSource  = "secret_in_source"
	ReasonNotFastForward  = "not_fast_forward"
	// ReasonUnacceptedPush: the task branch moved by a push that is not an
	// accepted checkpoint and not provably human work. Nothing is adopted.
	ReasonUnacceptedPush = "unaccepted_push"
)

// Internal reports whether the workspace is hosted by the internal Git service.
func (w *Workspace) Internal() bool { return w.Hosting == HostingInternal }

// NewInternalParams creates an internally hosted workspace in provisioning
// state. The primary repository fills the workspace's own repository fields.
type NewInternalParams struct {
	ID         ID
	Scope      domain.Scope
	WorkItemID domain.WorkItemID
	Primary    RepositoryID
	// URL is the primary repository's Git endpoint; CheckoutSecretRef is the
	// opaque reference the composition's read-credential resolver understands.
	URL               string
	CheckoutSecretRef string
}

// NewInternal validates the parameters and returns a provisioning workspace at
// version 1 whose source is hosted internally.
func NewInternal(p NewInternalParams, now time.Time) (*Workspace, error) {
	w, err := New(NewParams{ID: p.ID, Scope: p.Scope, WorkItemID: p.WorkItemID, RepositoryID: p.Primary,
		RepositoryURL: p.URL, CheckoutSecretRef: p.CheckoutSecretRef, PublisherSecretRef: NoPublisherRef}, now)
	if err != nil {
		return nil, err
	}
	w.Hosting = HostingInternal
	return w, nil
}

// requireInternal refuses an operation of the internal model on another hosting.
func (w *Workspace) requireInternal(op string) error {
	if !w.Internal() {
		return fmt.Errorf("%w: %s needs an internally hosted workspace", ErrWrongHosting, op)
	}
	return nil
}

// requireExternal refuses an operation of the publisher model on an internally
// hosted workspace: its source is pushed by attempts, never published.
func (w *Workspace) requireExternal(op string) error {
	if w.Internal() {
		return fmt.Errorf("%w: %s needs an externally hosted workspace", ErrWrongHosting, op)
	}
	return nil
}

// ---- repository associations ----

// AssociationState is the lifecycle of one task branch. It is independent of
// the workspace's own state.
type AssociationState string

const (
	// AssociationIntent: the base commit is recorded; the task branch may or
	// may not exist yet. Nothing can be pushed or accepted.
	AssociationIntent AssociationState = "intent"
	// AssociationCreated: the task branch exists at (or beyond) the base commit
	// and AcceptedCommit is recorded.
	AssociationCreated AssociationState = "created"
)

// RepositoryAssociation records, for one repository a work item touches, its
// task branch and the commit last accepted there (WS-17).
type RepositoryAssociation struct {
	WorkspaceID  ID
	Scope        domain.Scope
	RepositoryID RepositoryID
	// TaskBranch is refs/heads/kodama/work/<work-item-id>.
	TaskBranch string
	// BaseCommit is the default branch's tip the task branch is created at,
	// recorded before the branch is created. The first recorded value wins.
	BaseCommit Commit
	// AcceptedCommit is the commit last accepted on the task branch; empty
	// until the association is created.
	AcceptedCommit Commit
	State          AssociationState
	// LastError is a safe reason code, never free text.
	LastError string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAssociation records the intent to create the workspace's task branch in a
// repository at base.
func NewAssociation(w *Workspace, repo RepositoryID, base Commit, now time.Time) (*RepositoryAssociation, error) {
	if err := w.requireInternal("a repository association"); err != nil {
		return nil, err
	}
	if err := firstErr(repo.Validate(), base.Validate()); err != nil {
		return nil, err
	}
	branch, err := gitv1.TaskBranch(string(w.WorkItemID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return &RepositoryAssociation{
		WorkspaceID: w.ID, Scope: w.Scope, RepositoryID: repo, TaskBranch: branch, BaseCommit: base,
		State: AssociationIntent, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}, nil
}

// BranchOperationID is the Hosts operation identifier of the creation of this
// workspace's task branch in a repository: stable across retries, restarts and
// concurrent callers, so the worker's journal attributes every attempt to one
// operation (WS-17).
func BranchOperationID(w *Workspace, repo RepositoryID) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "workspace-task-branch/%s/%s/%s/%s", w.Scope.TenantID, w.Scope.ProjectID, w.ID, repo))
	return "wsbr_" + hex.EncodeToString(sum[:20])
}

// MarkCreated records that the task branch exists at the base commit (the
// worker created it, or an observation proved it did). It is idempotent.
func (a *RepositoryAssociation) MarkCreated(now time.Time) (bool, error) {
	if a.State == AssociationCreated {
		return false, nil
	}
	a.State, a.AcceptedCommit, a.LastError, a.UpdatedAt = AssociationCreated, a.BaseCommit, "", now.UTC()
	return true, nil
}

// RecordError keeps the association in intent with a safe reason. It reports
// whether anything changed.
func (a *RepositoryAssociation) RecordError(reason string, now time.Time) (bool, error) {
	if a.State != AssociationIntent {
		return false, fmt.Errorf("%w: record error on a %s association", ErrInvalidTransition, a.State)
	}
	if err := validateReason(reason); err != nil {
		return false, err
	}
	if a.LastError == reason {
		return false, nil
	}
	a.LastError, a.UpdatedAt = reason, now.UTC()
	return true, nil
}

// Advance records a new accepted commit. from must be the accepted commit the
// caller compared against; a concurrent change is ErrStale. Only a created
// association advances.
func (a *RepositoryAssociation) Advance(from, to Commit, now time.Time) error {
	if a.State != AssociationCreated {
		return fmt.Errorf("%w: advance a %s association", ErrInvalidTransition, a.State)
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if a.AcceptedCommit != from {
		return ErrStale
	}
	a.AcceptedCommit, a.LastError, a.UpdatedAt = to, "", now.UTC()
	return nil
}

// RecordObservation notes that the task branch is missing, no longer descends
// from the accepted commit, or moved by a push that is not an accepted
// checkpoint (reason is ReasonBranchMissing, ReasonBranchDiverged or
// ReasonUnacceptedPush). Nothing is adopted or recreated: admission keeps
// refusing until the branch is restored. It reports whether anything
// changed; from is the accepted commit the observation was made against.
func (a *RepositoryAssociation) RecordObservation(from Commit, reason string, now time.Time) (bool, error) {
	switch {
	case a.State != AssociationCreated:
		return false, fmt.Errorf("%w: observe a %s association", ErrInvalidTransition, a.State)
	case a.AcceptedCommit != from:
		return false, ErrStale
	case reason != ReasonBranchDiverged && reason != ReasonBranchMissing && reason != ReasonUnacceptedPush:
		return false, fmt.Errorf("%w: observation reason %q", ErrInvalidValue, reason)
	case a.LastError == reason:
		return false, nil
	}
	a.LastError, a.UpdatedAt = reason, now.UTC()
	return true, nil
}

// ClearObservation drops a recorded divergence once the branch matches again.
func (a *RepositoryAssociation) ClearObservation(now time.Time) bool {
	if a.State != AssociationCreated || (a.LastError != ReasonBranchDiverged && a.LastError != ReasonBranchMissing && a.LastError != ReasonUnacceptedPush) {
		return false
	}
	a.LastError, a.UpdatedAt = "", now.UTC()
	return true
}

func findAssociation(list []*RepositoryAssociation, repo RepositoryID) *RepositoryAssociation {
	for _, a := range list {
		if a.RepositoryID == repo {
			return a
		}
	}
	return nil
}

// FindAssociation returns the association of a repository, or nil.
func FindAssociation(list []*RepositoryAssociation, repo RepositoryID) *RepositoryAssociation {
	return findAssociation(list, repo)
}

// AddAssociation records the intent for one more repository. A repository that
// is already associated returns its association unchanged (the first recorded
// base wins), so repeated and concurrent provisioning create nothing more. New
// associations are refused once admission is closed.
func (w *Workspace) AddAssociation(existing []*RepositoryAssociation, repo RepositoryID, base Commit, now time.Time) (*RepositoryAssociation, bool, error) {
	if err := w.requireInternal("a repository association"); err != nil {
		return nil, false, err
	}
	if a := findAssociation(existing, repo); a != nil {
		return a, false, nil
	}
	switch {
	case w.State == StateArchived || w.ArchiveRequestedAt != nil:
		return nil, false, ErrAdmissionClosed
	case len(existing) >= MaxRepositories:
		return nil, false, fmt.Errorf("%w: a workspace associates at most %d repositories", ErrInvalidValue, MaxRepositories)
	}
	a, err := NewAssociation(w, repo, base, now)
	if err != nil {
		return nil, false, err
	}
	w.touch(now)
	return a, true, nil
}

// TouchAssociations advances the workspace version for a change to its
// associations, so If-Match preconditions and the deterministic event
// identifiers see it.
func (w *Workspace) TouchAssociations(now time.Time) { w.touch(now) }

// CompleteInternalProvisioning makes a provisioning workspace available once
// every association is created, at the primary repository's accepted commit.
func (w *Workspace) CompleteInternalProvisioning(assocs []*RepositoryAssociation, now time.Time) error {
	if err := w.requireInternal("provisioning"); err != nil {
		return err
	}
	if w.State != StateProvisioning {
		return w.transitionErr("complete provisioning")
	}
	primary := findAssociation(assocs, w.RepositoryID)
	if primary == nil {
		return fmt.Errorf("%w: the primary repository has no association", ErrInvalidTransition)
	}
	for _, a := range assocs {
		if a.State != AssociationCreated {
			return fmt.Errorf("%w: association of %s is %s", ErrInvalidTransition, a.RepositoryID, a.State)
		}
	}
	if w.InitialCommit == "" {
		w.InitialCommit = primary.BaseCommit
	}
	return w.CompleteProvisioning(primary.AcceptedCommit, now)
}

// ---- writer admission ----

// ReserveInternalWriter reserves the single writer slot of an internally hosted
// workspace: the one-writer rule is the same as for external hosting. Both
// commit pins are the primary repository's accepted commit; per-repository
// expected tips travel in the run's source set. gate decisions about unknown
// pushes are the caller's, made before this transition (Service.CheckWriterGateAt),
// because they read another module's records.
func (w *Workspace) ReserveInternalWriter(attempt domain.AttemptID, now time.Time) error {
	if err := w.requireInternal("an internal writer reservation"); err != nil {
		return err
	}
	return w.ReserveWriter(attempt, w.AcceptedCommit, w.AcceptedCommit, now)
}

// ---- checkpoints ----

// CheckpointPinStatus is the lifecycle of the retained ref of a checkpoint.
type CheckpointPinStatus string

const (
	// CheckpointPinIntent: the retained ref name is recorded; the host has not
	// confirmed it.
	CheckpointPinIntent CheckpointPinStatus = "intent"
	// CheckpointPinConfirmed: the host holds refs/kodama/runs/<run>/<n> at the
	// commit.
	CheckpointPinConfirmed CheckpointPinStatus = "confirmed"
)

// InternalCheckpoint is the durable acceptance of one applied push entry of the
// reserved writer (WS-19). It is keyed by (repository, push record, ref), so a
// repeated acceptance returns the same checkpoint and never repeats its effects.
type InternalCheckpoint struct {
	WorkspaceID  ID
	Scope        domain.Scope
	RepositoryID RepositoryID
	PushID       string
	Ref          string

	// Commit is the entry's new value; PreviousCommit the association's accepted
	// commit when it was accepted. Advanced is false when the commit was
	// already contained in the accepted history (a late acceptance of an
	// earlier push of the same writer): nothing moves backwards.
	Commit         Commit
	PreviousCommit Commit
	Advanced       bool
	AttemptID      domain.AttemptID
	RunID          domain.RunID

	// PinRef is refs/kodama/runs/<run>/<n> on the internal host.
	PinRef    string
	PinStatus CheckpointPinStatus

	CreatedAt time.Time
	PinnedAt  *time.Time
}

// CheckpointKey identifies a checkpoint within its workspace.
func CheckpointKey(repo RepositoryID, pushID, ref string) string {
	return string(repo) + "|" + pushID + "|" + ref
}

// Key is the checkpoint's identity within its workspace.
func (c *InternalCheckpoint) Key() string { return CheckpointKey(c.RepositoryID, c.PushID, c.Ref) }

// NewCheckpoint records an accepted push entry with its retained-ref intent.
func NewCheckpoint(w *Workspace, repo RepositoryID, pushID, ref string, commit, previous Commit, advanced bool,
	attempt domain.AttemptID, run domain.RunID, pinRef string, now time.Time) (*InternalCheckpoint, error) {
	if err := w.requireInternal("a checkpoint"); err != nil {
		return nil, err
	}
	if err := firstErr(repo.Validate(), validateID("push id", pushID), commit.Validate(), previous.Validate(), attempt.Validate(), run.Validate(), gitv1.ValidateRefName(ref)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	if _, _, err := gitv1.ParseRetainedRef(pinRef); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return &InternalCheckpoint{
		WorkspaceID: w.ID, Scope: w.Scope, RepositoryID: repo, PushID: pushID, Ref: ref, Commit: commit, PreviousCommit: previous,
		Advanced: advanced, AttemptID: attempt, RunID: run, PinRef: pinRef, PinStatus: CheckpointPinIntent, CreatedAt: now.UTC(),
	}, nil
}

// ConfirmPin records that the host holds the retained ref. Idempotent.
func (c *InternalCheckpoint) ConfirmPin(now time.Time) bool {
	if c.PinStatus == CheckpointPinConfirmed {
		return false
	}
	t := now.UTC()
	c.PinStatus, c.PinnedAt = CheckpointPinConfirmed, &t
	return true
}

// RepointPin replaces the retained ref name of an unconfirmed pin, after the
// host showed the earlier name taken by another commit of the run.
func (c *InternalCheckpoint) RepointPin(ref string) error {
	if c.PinStatus != CheckpointPinIntent {
		return fmt.Errorf("%w: repoint a %s pin", ErrInvalidTransition, c.PinStatus)
	}
	if _, _, err := gitv1.ParseRetainedRef(ref); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	c.PinRef = ref
	return nil
}

// FindCheckpoint returns the checkpoint with the key, or nil.
func FindCheckpoint(list []*InternalCheckpoint, repo RepositoryID, pushID, ref string) *InternalCheckpoint {
	for _, c := range list {
		if c.RepositoryID == repo && c.PushID == pushID && c.Ref == ref {
			return c
		}
	}
	return nil
}

// NextPinRef returns the lowest retained ref of the run in the repository that
// is above every number the workspace already used for it and not below
// atLeast. The host arbitrates collisions with pins other modules made for the
// same run (a retained ref at another commit is retained_ref_exists, and the
// caller then asks again with a higher floor).
func NextPinRef(list []*InternalCheckpoint, repo RepositoryID, run domain.RunID, atLeast int64) (string, error) {
	n := max(atLeast, 1)
	for _, c := range list {
		if c.RepositoryID != repo || c.RunID != run {
			continue
		}
		if _, k, err := gitv1.ParseRetainedRef(c.PinRef); err == nil && k >= n {
			n = k + 1
		}
	}
	ref, err := gitv1.RetainedRef(string(run), n)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return ref, nil
}

// PinOperationID is the Hosts operation identifier of one retained-ref request
// of a checkpoint: stable for the same (checkpoint, ref), so a repeated request
// after an unknown outcome is the same operation.
func PinOperationID(c *InternalCheckpoint) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "workspace-checkpoint-pin/%s/%s/%s/%s/%s/%s", c.Scope.TenantID, c.Scope.ProjectID, c.WorkspaceID, c.Key(), c.PinRef, c.Commit))
	return "wspin_" + hex.EncodeToString(sum[:20])
}
