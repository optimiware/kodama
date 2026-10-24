package workspace

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// PublicationStatus is the source-publication result recorded on a receipt.
// It is independent of how the writer attempt ended.
type PublicationStatus string

const (
	PublicationPending   PublicationStatus = "pending"
	PublicationPublished PublicationStatus = "published"
	PublicationConflict  PublicationStatus = "conflict"
)

// WriterOutcome is how the writer attempt ended. A published commit from an
// attempt that was cancelled or failed never becomes a successful downstream
// output.
type WriterOutcome string

const (
	OutcomeUnsettled WriterOutcome = "unsettled"
	OutcomeSucceeded WriterOutcome = "succeeded"
	OutcomeFailed    WriterOutcome = "failed"
	OutcomeCancelled WriterOutcome = "cancelled"
)

var digestRegexp = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Proposal is an attempt's request to publish a commit.
type Proposal struct {
	AttemptID    domain.AttemptID
	Commit       Commit
	ExpectedTip  Commit
	ArtifactID   domain.ArtifactID
	BundleDigest string
}

// Validate checks every field.
func (p Proposal) Validate() error {
	if err := firstErr(p.AttemptID.Validate(), p.Commit.Validate(), p.ExpectedTip.Validate(), p.ArtifactID.Validate()); err != nil {
		return err
	}
	if !digestRegexp.MatchString(p.BundleDigest) {
		return fmt.Errorf("%w: bundle digest", ErrInvalidValue)
	}
	if p.Commit == p.ExpectedTip {
		return fmt.Errorf("%w: proposed commit equals the expected tip", ErrInvalidValue)
	}
	return nil
}

// Publication is the immutable receipt of one proposal. Its identity
// (attempt, commit, expected tip, bundle) never changes; only its settlement
// fields do, and a receipt survives release of the writer.
type Publication struct {
	WorkspaceID    ID
	Scope          domain.Scope
	AttemptID      domain.AttemptID
	Commit         Commit
	ExpectedCommit Commit
	ArtifactID     domain.ArtifactID
	BundleDigest   string

	Status PublicationStatus
	// ObservedTip is the remote tip that defeated a conflicting proposal.
	ObservedTip Commit
	// ConflictReason is a safe code (tip_moved, not_fast_forward).
	ConflictReason string
	Outcome        WriterOutcome

	CreatedAt time.Time
	SettledAt *time.Time
}

// Matches reports whether p is the same proposal as the receipt's identity.
func (r *Publication) Matches(p Proposal) bool {
	return r.AttemptID == p.AttemptID && r.Commit == p.Commit && r.ExpectedCommit == p.ExpectedTip &&
		r.ArtifactID == p.ArtifactID && r.BundleDigest == p.BundleDigest
}

// SelectableOutput returns the commit a downstream reader may bind: only a
// published commit of a specific successful writer.
func (r *Publication) SelectableOutput() (Commit, error) {
	if r == nil || r.Status != PublicationPublished || r.Outcome != OutcomeSucceeded {
		return "", ErrOutputNotSelectable
	}
	return r.Commit, nil
}

// ErrPublisherBusy: another live driver owns the in-flight publication.
var ErrPublisherBusy = errors.New("workspace: publication is being driven by another publisher")

// Disposition tells the caller whether a proposal created a publication or
// observed an existing one.
type Disposition string

const (
	DispositionNew    Disposition = "new"
	DispositionReplay Disposition = "replay"
)

// BeginPublication validates the proposal against the live writer and records
// the durable pending intent. An identical repeated proposal observes the
// original receipt unchanged; a different proposal of the same attempt is
// rejected and can never replace an in-flight or settled publication.
func (w *Workspace) BeginPublication(p Proposal, existing *Publication, now time.Time) (*Publication, Disposition, error) {
	if err := p.Validate(); err != nil {
		return nil, "", err
	}
	if existing != nil {
		if !existing.Matches(p) {
			return nil, "", ErrProposalChanged
		}
		return existing, DispositionReplay, nil
	}
	switch {
	case w.Fence.Fenced:
		return nil, "", ErrFenced
	case w.Writer == nil || w.Writer.AttemptID != p.AttemptID:
		return nil, "", ErrNotWriter
	case w.State != StateAvailable || w.ArchiveRequestedAt != nil:
		return nil, "", ErrAdmissionClosed
	case w.Pending != nil:
		return nil, "", ErrPublicationPending
	case p.ExpectedTip != w.Writer.ExpectedTip:
		return nil, "", fmt.Errorf("%w: expected tip does not match the reservation", ErrInvalidValue)
	}
	w.Pending = &PendingPublication{
		AttemptID: p.AttemptID, Commit: p.Commit, ExpectedCommit: p.ExpectedTip,
		ArtifactID: p.ArtifactID, BundleDigest: p.BundleDigest, StartedAt: now.UTC(),
	}
	w.touch(now)
	return &Publication{
		WorkspaceID: w.ID, Scope: w.Scope, AttemptID: p.AttemptID, Commit: p.Commit, ExpectedCommit: p.ExpectedTip,
		ArtifactID: p.ArtifactID, BundleDigest: p.BundleDigest,
		Status: PublicationPending, Outcome: OutcomeUnsettled, CreatedAt: now.UTC(),
	}, DispositionNew, nil
}

// ClaimPublisher records that driverID is driving the pending push. A live
// claim of another driver in the current epoch is respected; a claim older than
// staleAfter, or from a fenced epoch, may be resumed. Resuming drives the same
// intent: it never creates or replaces a publication.
func (w *Workspace) ClaimPublisher(driverID string, now time.Time, staleAfter time.Duration) error {
	if w.Pending == nil {
		return fmt.Errorf("%w: no pending publication", ErrInvalidTransition)
	}
	if w.Fence.Fenced {
		return ErrFenced
	}
	p := w.Pending
	if p.DriverID != "" && p.DriverID != driverID && p.DriverEpoch == w.Fence.Epoch && now.Sub(p.DriverClaimed) < staleAfter {
		return ErrPublisherBusy
	}
	p.DriverID, p.DriverEpoch, p.DriverClaimed = driverID, w.Fence.Epoch, now.UTC()
	w.touch(now)
	return nil
}

// RecordPushAttempt notes, before the push starts, that the claimed driver is
// about to push. It is what RedriveDue measures.
func (w *Workspace) RecordPushAttempt(driverID string, now time.Time) error {
	if w.Pending == nil || w.Pending.DriverID != driverID {
		return ErrFenced
	}
	if err := w.checkDriver(driverID); err != nil {
		return err
	}
	w.Pending.Attempts++
	w.Pending.LastAttemptAt = now.UTC()
	w.touch(now)
	return nil
}

// ReleasePublisher gives up the claim after an uncertain push so the intent can
// be resumed. Ownership of the publication is kept.
func (w *Workspace) ReleasePublisher(driverID string, now time.Time) {
	if w.Pending == nil || w.Pending.DriverID != driverID {
		return
	}
	w.Pending.DriverID, w.Pending.DriverEpoch = "", 0
	w.touch(now)
}

// RedriveDue reports whether an uncertain push may be repeated: its last
// attempt is at least `after` old.
func (p *PendingPublication) RedriveDue(now time.Time, after time.Duration) bool {
	return p.LastAttemptAt.IsZero() || now.Sub(p.LastAttemptAt) >= after
}

func (w *Workspace) checkDriver(driverID string) error {
	p := w.Pending
	if p.DriverID == "" {
		if driverID != "" {
			return ErrFenced
		}
		if w.Fence.Fenced {
			return ErrFenced
		}
		return nil
	}
	if driverID != p.DriverID || p.DriverEpoch != w.Fence.Epoch || w.Fence.Fenced {
		return ErrFenced
	}
	return nil
}

func (w *Workspace) requireReceipt(r *Publication) error {
	if w.Pending == nil || r == nil || r.Status != PublicationPending ||
		r.AttemptID != w.Pending.AttemptID || r.Commit != w.Pending.Commit || r.WorkspaceID != w.ID {
		return fmt.Errorf("%w: no matching pending publication", ErrInvalidTransition)
	}
	return nil
}

// RecordPublished records that the remote branch contains the proposal: the
// accepted commit advances, the pending intent clears and the receipt settles.
// Only the driver that holds the current, unfenced claim may finalize. It
// returns the previous accepted commit.
func (w *Workspace) RecordPublished(driverID string, r *Publication, now time.Time) (Commit, error) {
	if err := w.requireReceipt(r); err != nil {
		return "", err
	}
	if err := w.checkDriver(driverID); err != nil {
		return "", err
	}
	prev := w.AcceptedCommit
	w.AcceptedCommit = r.Commit
	t := now.UTC()
	r.Status, r.SettledAt = PublicationPublished, &t
	w.Pending = nil
	w.touch(now)
	return prev, nil
}

// RecordConflict records a lost expected-tip race, preserving the proposal and
// the observed tip. The accepted commit is not changed.
func (w *Workspace) RecordConflict(driverID string, r *Publication, observed Commit, reason string, now time.Time) error {
	if err := w.requireReceipt(r); err != nil {
		return err
	}
	if err := w.checkDriver(driverID); err != nil {
		return err
	}
	if observed != "" {
		if err := observed.Validate(); err != nil {
			return err
		}
	}
	if err := validateReason(reason); err != nil {
		return err
	}
	t := now.UTC()
	r.Status, r.ObservedTip, r.ConflictReason, r.SettledAt = PublicationConflict, observed, reason, &t
	w.Pending = nil
	w.touch(now)
	return nil
}

// SettleWriter ends the writer reservation with the attempt's outcome after
// its publication settled. A writer whose proposal lost the race cannot be
// recorded as succeeded.
func (w *Workspace) SettleWriter(attempt domain.AttemptID, outcome WriterOutcome, r *Publication, now time.Time) error {
	switch outcome {
	case OutcomeSucceeded, OutcomeFailed, OutcomeCancelled:
	default:
		return fmt.Errorf("%w: writer outcome %q", ErrInvalidValue, outcome)
	}
	if w.Writer == nil || w.Writer.AttemptID != attempt {
		return ErrNotWriter
	}
	if w.Pending != nil {
		return ErrPublicationPending
	}
	if r != nil {
		if r.AttemptID != attempt || r.WorkspaceID != w.ID {
			return fmt.Errorf("%w: receipt belongs to another attempt", ErrInvalidValue)
		}
		if outcome == OutcomeSucceeded && r.Status != PublicationPublished {
			return ErrOutputNotSelectable
		}
		r.Outcome = outcome
	}
	w.Writer = nil
	w.touch(now)
	return nil
}

// VerifyWriterSource checks the commit a completed writer reports as its
// result: a published proposal's commit, or the expected tip when the writer
// changed nothing. Call it before SettleWriter.
func (w *Workspace) VerifyWriterSource(attempt domain.AttemptID, source Commit, r *Publication) error {
	if r != nil {
		if r.AttemptID != attempt || r.Status != PublicationPublished || r.Commit != source {
			return ErrOutputNotSelectable
		}
		return nil
	}
	if w.Writer == nil || w.Writer.AttemptID != attempt || w.Writer.ExpectedTip != source {
		return ErrOutputNotSelectable
	}
	return nil
}
