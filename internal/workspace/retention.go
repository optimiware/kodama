package workspace

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// PinPurpose says why a commit is retained.
type PinPurpose string

const (
	// PinReader retains the commit a run/reader checks out.
	PinReader PinPurpose = "reader"
	// PinWriterOutput retains the published output of one specific successful
	// writer attempt.
	PinWriterOutput PinPurpose = "writer_output"
)

// PinStatus is the lifecycle of a retained reference.
type PinStatus string

const (
	// PinIntent: recorded, remote ref not yet confirmed; not yet usable.
	PinIntent PinStatus = "intent"
	// PinConfirmed: the remote ref exists; the pin is usable and live.
	PinConfirmed PinStatus = "confirmed"
	// PinReleased: no reader depends on it; removal may be requested.
	PinReleased PinStatus = "released"
	// PinRemoving: removal accepted; the remote ref deletion may be in flight.
	PinRemoving PinStatus = "removing"
	// PinRemoved: the remote ref is confirmed deleted.
	PinRemoved PinStatus = "removed"
)

// DefaultRetainedRefPrefix is the proposed portable ordinary-branch namespace.
const DefaultRetainedRefPrefix = "refs/heads/kodama/retained/"

// RetainedRef protects one commit through one remote reference. The reference
// name is persisted, never re-derived after configuration changes.
type RetainedRef struct {
	ID          PinID
	WorkspaceID ID
	Scope       domain.Scope
	Commit      Commit
	RefName     string
	Purpose     PinPurpose
	RunID       domain.RunID
	// AttemptID names the specific writer for PinWriterOutput.
	AttemptID domain.AttemptID
	Status    PinStatus

	CreatedAt          time.Time
	ConfirmedAt        *time.Time
	ReleasedAt         *time.Time
	RemovalRequestedAt *time.Time
	RemovedAt          *time.Time
}

// NewPin creates a pin in intent status. The reference is collision-safe: it
// embeds the workspace, run and pin identifiers.
func NewPin(id PinID, w *Workspace, commit Commit, purpose PinPurpose, run domain.RunID, attempt domain.AttemptID, prefix string, now time.Time) (*RetainedRef, error) {
	if err := firstErr(id.Validate(), commit.Validate(), run.Validate()); err != nil {
		return nil, err
	}
	switch purpose {
	case PinReader:
	case PinWriterOutput:
		if err := attempt.Validate(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: pin purpose %q", ErrInvalidValue, purpose)
	}
	if !strings.HasPrefix(prefix, "refs/heads/") || !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("%w: retained ref prefix", ErrInvalidValue)
	}
	name := fmt.Sprintf("%s%s/%s-%s", prefix, w.ID, run, id)
	if err := ValidateBranch(name); err != nil {
		return nil, err
	}
	return &RetainedRef{
		ID: id, WorkspaceID: w.ID, Scope: w.Scope, Commit: commit, RefName: name,
		Purpose: purpose, RunID: run, AttemptID: attempt, Status: PinIntent, CreatedAt: now.UTC(),
	}, nil
}

// Live reports whether something may still depend on the reference.
func (p *RetainedRef) Live() bool { return p.Status == PinIntent || p.Status == PinConfirmed }

// Confirm records that the remote reference exists.
func (p *RetainedRef) Confirm(now time.Time) error {
	switch p.Status {
	case PinConfirmed:
		return nil
	case PinIntent:
		t := now.UTC()
		p.Status, p.ConfirmedAt = PinConfirmed, &t
		return nil
	}
	return fmt.Errorf("%w: confirm pin in status %s", ErrInvalidTransition, p.Status)
}

// Release marks a confirmed pin as no longer referenced.
func (p *RetainedRef) Release(now time.Time) error {
	switch p.Status {
	case PinReleased, PinRemoving, PinRemoved:
		return nil
	case PinConfirmed:
		t := now.UTC()
		p.Status, p.ReleasedAt = PinReleased, &t
		return nil
	}
	return fmt.Errorf("%w: release pin in status %s", ErrInvalidTransition, p.Status)
}

// BeginRemoval accepts a removal of a released pin.
func (p *RetainedRef) BeginRemoval(now time.Time) error {
	switch p.Status {
	case PinRemoving, PinRemoved:
		return nil
	case PinReleased:
		t := now.UTC()
		p.Status, p.RemovalRequestedAt = PinRemoving, &t
		return nil
	}
	return fmt.Errorf("%w: remove pin in status %s", ErrInvalidTransition, p.Status)
}

// CompleteRemoval records the confirmed remote deletion.
func (p *RetainedRef) CompleteRemoval(now time.Time) error {
	switch p.Status {
	case PinRemoved:
		return nil
	case PinRemoving:
		t := now.UTC()
		p.Status, p.RemovedAt = PinRemoved, &t
		return nil
	}
	return fmt.Errorf("%w: complete removal in status %s", ErrInvalidTransition, p.Status)
}

// Verdict is the outcome of a removal request.
type Verdict string

const (
	VerdictAllow Verdict = "allow"
	VerdictDeny  Verdict = "deny"
	VerdictDefer Verdict = "defer"
)

// RemovalDecision is a verdict and its safe reason code.
type RemovalDecision struct {
	Verdict Verdict
	Reason  string
}

// EvaluateRemoval decides a removal request for pin among the workspace's pins.
// Deletion is never automatic: without an explicit policy grant it is denied;
// a live reference or an unsettled publication always blocks it.
func EvaluateRemoval(w *Workspace, pin *RetainedRef, all []*RetainedRef, policyAllows bool) RemovalDecision {
	if pin.Status == PinRemoving || pin.Status == PinRemoved {
		return RemovalDecision{VerdictAllow, "resume"}
	}
	switch {
	case !policyAllows:
		return RemovalDecision{VerdictDeny, "deletion_policy"}
	case pin.Live():
		return RemovalDecision{VerdictDeny, "referenced"}
	case w.Pending != nil:
		return RemovalDecision{VerdictDefer, "publication_unsettled"}
	}
	for _, o := range all {
		if o.ID != pin.ID && o.Commit == pin.Commit && o.Status == PinIntent {
			return RemovalDecision{VerdictDefer, "pin_in_progress"}
		}
	}
	return RemovalDecision{VerdictAllow, "released"}
}

// CheckPinAdmission refuses a new pin of a commit whose retention is being
// removed, so a pin and a removal of the same commit serialize under the
// workspace lock.
func CheckPinAdmission(commit Commit, all []*RetainedRef) error {
	for _, o := range all {
		if o.Commit == commit && o.Status == PinRemoving {
			return ErrRetentionBusy
		}
	}
	return nil
}
