package execution

import (
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Attempt is one attempt to execute a step. Creating it is the durable dispatch
// intent: it exists, running with reason "starting", before any workload does. Once
// terminal its outcome is immutable; late observations and reconciliation are
// stored as separate evidence, never by rewriting the outcome.
type Attempt struct {
	Scope   domain.Scope
	ID      domain.AttemptID
	RunID   domain.RunID
	StepID  string
	Ordinal int

	// ActionKey is the logical step's key, identical across attempts.
	ActionKey string

	Status AttemptStatus
	Reason Reason
	Class  FailureClass

	// WorkloadName and SpecFingerprint are the expected workload identity.
	// They are derived before any workload exists; an observed workload must
	// match both. WorkloadUID is the instance the runtime reported, bound once.
	WorkloadName    string
	SpecFingerprint string
	WorkloadUID     string

	// Workspace binding: the source of this attempt.
	WorkspaceID string
	Access      Access
	InputCommit string
	ExpectedTip string

	TimeoutSeconds    int64
	StartupDeadline   time.Time
	ExecutionDeadline *time.Time

	// ContextDigest and Context are the immutable pinned runtime context
	// (canonical document) the entrypoint receives.
	ContextDigest string
	Context       []byte

	StartedAt  *time.Time
	FinishedAt *time.Time

	// ResultHash and Result are the accepted result, if any.
	ResultHash string
	Result     *runtimev1.Result

	// AuthorityRevision changes when the workload binding, pinned context or
	// allowed actions change.
	AuthorityRevision int64

	Version   domain.Version
	CreatedAt time.Time
	UpdatedAt time.Time

	dirty bool
}

// Writer reports whether the attempt holds the workspace writer slot.
func (a *Attempt) Writer() bool { return a.Access == AccessWrite }

func (a *Attempt) touch(now time.Time) {
	if !a.dirty {
		a.Version++
		a.dirty = true
	}
	a.UpdatedAt = now.UTC()
}

// Dirty / MarkClean support persistence adapters.
func (a *Attempt) Dirty() bool { return a.dirty }
func (a *Attempt) MarkClean()  { a.dirty = false }

// bindWorkload binds the workload instance the runtime reported at dispatch.
// The first reported UID is kept; the caller refuses a different one.
func (a *Attempt) bindWorkload(uid string, now time.Time) {
	if uid == "" || a.WorkloadUID != "" {
		return
	}
	a.WorkloadUID = uid
	a.touch(now)
}

// observeStart records the observed workload UID and the actual start.
// Observing the same UID again is idempotent; a different UID is another
// instance and is refused.
func (a *Attempt) observeStart(uid string, now time.Time) (bool, error) {
	if a.Status != AttemptRunning {
		return false, transitionf("attempt %s is %s", a.ID, a.Status)
	}
	if uid == "" {
		return false, invalidf("an observed workload UID is required")
	}
	if a.WorkloadUID != "" && a.WorkloadUID != uid {
		return false, ErrConflict
	}
	if a.WorkloadUID == uid && a.StartedAt != nil {
		return false, nil
	}
	a.WorkloadUID, a.Reason = uid, ReasonRunning
	t := now.UTC()
	a.StartedAt = &t
	if a.TimeoutSeconds > 0 {
		d := t.Add(time.Duration(a.TimeoutSeconds) * time.Second)
		a.ExecutionDeadline = &d
	}
	a.touch(now)
	return true, nil
}

func (a *Attempt) finish(to AttemptStatus, reason Reason, class FailureClass, now time.Time) error {
	if a.Status.Terminal() {
		return transitionf("attempt %s is %s and immutable", a.ID, a.Status)
	}
	if !to.Terminal() {
		return transitionf("%s is not terminal", to)
	}
	a.Status, a.Reason, a.Class = to, reason, class
	t := now.UTC()
	a.FinishedAt = &t
	a.touch(now)
	return nil
}

func (a *Attempt) succeed(hash string, res runtimev1.Result, now time.Time) error {
	if err := a.finish(AttemptSucceeded, ReasonCompleted, "", now); err != nil {
		return err
	}
	a.ResultHash = hash
	r := res
	a.Result = &r
	return nil
}

func (a *Attempt) fail(reason Reason, class FailureClass, now time.Time) error {
	return a.finish(AttemptFailed, reason, class, now)
}

func (a *Attempt) cancel(reason Reason, now time.Time) error {
	return a.finish(AttemptCancelled, reason, ClassCancelled, now)
}

// Control is the mutable controller bookkeeping of an attempt, separate from the
// immutable outcome: dispatch confirmation, termination, authority revocation,
// launch evidence and settlement.
type Control struct {
	AttemptID domain.AttemptID

	// DispatchConfirmedAt is set once the runtime confirmed it created or
	// adopted the workload. An unconfirmed intent is redriven with the same
	// identity after ClaimUntil lapses.
	DispatchConfirmedAt *time.Time
	ClaimToken          string
	ClaimUntil          *time.Time

	// RevokedAt is the moment the attempt's authority ended: every runtime
	// grant of the attempt is denied from then on.
	RevokedAt *time.Time

	TerminationRequestedAt *time.Time
	TerminationConfirmedAt *time.Time

	Launch LaunchEvidence
	Effect EffectState

	// ReconciledAt/Reconciliation record trusted evidence that resolved an
	// unknown effect.
	ReconciledAt   *time.Time
	ReplaySafe     bool
	SettledAt      *time.Time
	StepResolvedAt *time.Time

	dirty bool
}

// NewControl creates the bookkeeping of a fresh attempt.
func NewControl(id domain.AttemptID) *Control {
	return &Control{AttemptID: id, Launch: LaunchUnknown, Effect: EffectPossible}
}

// Dirty / MarkClean support persistence adapters.
func (c *Control) Dirty() bool { return c.dirty }
func (c *Control) MarkClean()  { c.dirty = false }

// Live reports whether the attempt still holds authority.
func (c *Control) Live() bool { return c.RevokedAt == nil }

// Settled reports whether every effect of the attempt is accounted for.
func (c *Control) Settled() bool { return c.SettledAt != nil }

func (c *Control) revoke(now time.Time) bool {
	if c.RevokedAt != nil {
		return false
	}
	t := now.UTC()
	c.RevokedAt = &t
	c.dirty = true
	return true
}

func (c *Control) requestTermination(now time.Time) bool {
	if c.TerminationRequestedAt != nil {
		return false
	}
	t := now.UTC()
	c.TerminationRequestedAt = &t
	c.dirty = true
	return true
}

// confirmTermination records the executor's statement that the workload is gone
// together with its launch evidence. Evidence only ever becomes more
// informative: "unknown" yields to a definite statement, never the reverse.
func (c *Control) confirmTermination(launch LaunchEvidence, now time.Time) bool {
	changed := false
	if c.TerminationConfirmedAt == nil {
		t := now.UTC()
		c.TerminationConfirmedAt = &t
		changed = true
	}
	if c.Launch == LaunchUnknown && (launch == LaunchNotStarted || launch == LaunchStarted) {
		c.Launch = launch
		changed = true
	}
	if changed {
		c.dirty = true
	}
	return changed
}

func (c *Control) noteLaunch(l LaunchEvidence) {
	if c.Launch == LaunchUnknown && (l == LaunchNotStarted || l == LaunchStarted) {
		c.Launch = l
		c.dirty = true
	}
}

// Evidence is an append-only record about an attempt that must not rewrite its
// outcome: a late observation, a late result or a reconciliation.
type Evidence struct {
	Kind       EvidenceKind
	Detail     map[string]any
	OccurredAt time.Time
}

type EvidenceKind string

const (
	EvidenceLateObservation EvidenceKind = "late_observation"
	EvidenceLateResult      EvidenceKind = "late_result"
	EvidenceMismatch        EvidenceKind = "observation_mismatch"
	EvidenceReconciliation  EvidenceKind = "reconciliation"
	EvidenceTermination     EvidenceKind = "termination"
)
