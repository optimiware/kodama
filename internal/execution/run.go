package execution

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// SourceRetention is the lifecycle of the run-bound retained source reference.
// A work run may not dispatch until its reference is confirmed.
type SourceRetention string

const (
	// RetentionNotRequired: cleanup runs use no workspace.
	RetentionNotRequired SourceRetention = "not_required"
	// RetentionPending: the intent is durable, the remote reference is not
	// confirmed yet; no dispatch.
	RetentionPending   SourceRetention = "pending"
	RetentionConfirmed SourceRetention = "confirmed"
)

// Run is one execution of a pinned pipeline definition. Its pins are immutable
// after admission; only status, cancellation/failure intent, retention
// confirmation and timestamps change. A rerun is another Run.
type Run struct {
	Scope domain.Scope
	ID    domain.RunID

	Purpose      Purpose
	WorkItemID   domain.WorkItemID // empty for cleanup
	WorkspaceID  string            // empty for cleanup
	DeploymentID string            // cleanup only: the recorded deployment

	DefinitionID     domain.DefinitionID
	DefinitionDigest string
	ContentRevision  domain.ContentRevision // zero for cleanup
	Specification    *Specification         // nil for cleanup
	SourceCommit     string                 // empty for cleanup
	Snapshot         Snapshot
	SnapshotDigest   string

	SourceRetention SourceRetention
	RetainedRefID   string

	RequestedBy Actor

	// Shared is the immutable run_shared pin set; nil for every
	// attempt_private run, which keeps its stored shape unchanged.
	Shared *RunShared
	// DeadlineAt is the pinned total run deadline (covering preparation, service
	// startup, attempts, backoff and gate waits), set once when resource holding
	// starts. Nil until then and for attempt_private runs.
	DeadlineAt *time.Time

	Status RunStatus
	Reason Reason
	// CancelRequestedAt is the durable cancellation request; the run stays
	// nonterminal until admitted work is settled.
	CancelRequestedAt *time.Time
	CancelReason      Reason
	// FailureIntentAt records the fail-fast decision: stop dispatch, drain
	// siblings, then fail after settlement.
	FailureIntentAt *time.Time
	FailureReason   Reason

	Version    domain.Version
	CreatedAt  time.Time
	UpdatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time

	dirty bool
}

func (r *Run) touch(now time.Time) {
	if !r.dirty {
		r.Version++
		r.dirty = true
	}
	r.UpdatedAt = now.UTC()
}

// Dirty reports whether the run changed since it was loaded or created.
func (r *Run) Dirty() bool { return r.dirty }

// MarkClean is called by persistence adapters after saving.
func (r *Run) MarkClean() { r.dirty = false }

// AcceptsDispatch reports whether new attempts may be claimed. Cancellation
// and failure intent prevent dispatch immediately, before any settlement.
func (r *Run) AcceptsDispatch() bool {
	return !r.Status.Terminal() && r.CancelRequestedAt == nil && r.FailureIntentAt == nil &&
		(r.SourceRetention == RetentionConfirmed || r.SourceRetention == RetentionNotRequired)
}

// Draining reports whether the run is stopping (cancel or failure intent).
func (r *Run) Draining() bool { return r.CancelRequestedAt != nil || r.FailureIntentAt != nil }

// ConfirmRetention records that the run-bound source reference exists. It is
// idempotent for the same reference.
func (r *Run) ConfirmRetention(refID string, now time.Time) (bool, error) {
	switch r.SourceRetention {
	case RetentionConfirmed:
		if r.RetainedRefID == refID {
			return false, nil
		}
		return false, transitionf("the run is bound to another retained reference")
	case RetentionNotRequired:
		return false, transitionf("the run requires no retained source")
	}
	if refID == "" {
		return false, invalidf("a retained reference is required")
	}
	if r.RetainedRefID != "" && r.RetainedRefID != refID {
		return false, transitionf("the run is bound to another retained reference")
	}
	r.SourceRetention, r.RetainedRefID = RetentionConfirmed, refID
	r.touch(now)
	return true, nil
}

// markRunning moves a queued run to running when its first attempt is created.
func (r *Run) markRunning(now time.Time) (bool, error) {
	switch r.Status {
	case RunRunning:
		return false, nil
	case RunQueued:
		r.Status, r.Reason = RunRunning, ReasonRunning
		t := now.UTC()
		r.StartedAt = &t
		r.touch(now)
		return true, nil
	}
	return false, transitionf("a %s run cannot start", r.Status)
}

// requestCancel records the cancellation request. It is idempotent and never
// changes a terminal run.
func (r *Run) requestCancel(reason Reason, now time.Time) bool {
	if r.Status.Terminal() || r.CancelRequestedAt != nil {
		return false
	}
	t := now.UTC()
	r.CancelRequestedAt, r.CancelReason = &t, reason
	r.touch(now)
	return true
}

// recordFailure records the fail-fast intent (first reason wins).
func (r *Run) recordFailure(reason Reason, now time.Time) bool {
	if r.Status.Terminal() || r.FailureIntentAt != nil {
		return false
	}
	t := now.UTC()
	r.FailureIntentAt, r.FailureReason = &t, reason
	r.touch(now)
	return true
}

// finish ends the run. Terminal completion requires that the caller verified
// settlement; the entity only enforces the legal edges.
func (r *Run) finish(to RunStatus, reason Reason, now time.Time) error {
	if !to.Terminal() {
		return transitionf("%s is not a terminal run status", to)
	}
	if r.Status.Terminal() {
		return transitionf("a %s run is immutable", r.Status)
	}
	switch to {
	case RunSucceeded:
		if r.Status != RunRunning && r.Status != RunQueued {
			return transitionf("cannot succeed from %s", r.Status)
		}
		if r.Draining() {
			return transitionf("a draining run cannot succeed")
		}
	case RunFailed:
		if r.FailureIntentAt == nil {
			return transitionf("a run fails only after recording failure intent")
		}
	case RunCancelled:
		if r.CancelRequestedAt == nil {
			return transitionf("a run is cancelled only after a cancellation request")
		}
	}
	r.Status, r.Reason = to, reason
	t := now.UTC()
	r.FinishedAt = &t
	r.touch(now)
	return nil
}
