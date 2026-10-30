package execution

import (
	"fmt"
	"sort"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// SubjectKind names which entity a Transition describes.
type SubjectKind string

const (
	SubjectRun     SubjectKind = "run"
	SubjectStep    SubjectKind = "step"
	SubjectAttempt SubjectKind = "attempt"
)

// Transition is one recorded state change. The persistence adapter writes one
// immutable history row, audit row and (for runs and attempts) catalog event per
// transition, in the transaction that saves the aggregate.
type Transition struct {
	Subject   SubjectKind
	ID        string
	StepID    string
	AttemptID domain.AttemptID
	From, To  string
	Reason    Reason
	At        time.Time
	// Version is the subject's resource version after this transaction.
	Version domain.Version
	// FromPhase and ToPhase are the observed resource phases of a SubjectResource
	// transition (From is "none" at creation, To never is; "removed" is the phase
	// absent). They feed the run_resource.state_changed event; From/To keep the
	// finer lifecycle name (stopping, removed) for history and audit.
	FromPhase, ToPhase string
}

// PendingEvidence is evidence to append for an attempt.
type PendingEvidence struct {
	Attempt domain.AttemptID
	Evidence
}

// Aggregate is the run with its steps, attempts and controller bookkeeping, as
// loaded under the run row lock. Entity methods enforce each lifecycle; the
// aggregate enforces the rules that span them: dispatch, fail-fast draining,
// settlement-before-terminal completion and the unknown-effect barrier.
type Aggregate struct {
	Run      *Run
	Steps    []*StepRun
	Attempts []*Attempt
	Controls map[domain.AttemptID]*Control

	Transitions []Transition
	Evidence    []PendingEvidence
	// Created lists attempts inserted by this mutation.
	Created []*Attempt
	// Revoked lists attempts whose authority ended in this mutation; the
	// adapter terminates their workloads after commit.
	Revoked []*Attempt

	// run_shared state; all empty for an attempt_private run. Resources are
	// ordered by provisioning order; NewResources/NewHolders/NewBegins list the
	// rows inserted by this mutation.
	Resources    []*RunResource
	Holders      []*RunHolder
	Begins       []*BeginRecord
	NewResources []*RunResource
	NewHolders   []*RunHolder
	NewBegins    []*BeginRecord
}

// Step returns the run-local step, or nil.
func (a *Aggregate) Step(id string) *StepRun {
	for _, s := range a.Steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// Attempt returns an attempt by ID, or nil.
func (a *Aggregate) Attempt(id domain.AttemptID) *Attempt {
	for _, at := range a.Attempts {
		if at.ID == id {
			return at
		}
	}
	return nil
}

// AttemptsOf returns a step's attempts in ordinal order.
func (a *Aggregate) AttemptsOf(step string) []*Attempt {
	var out []*Attempt
	for _, at := range a.Attempts {
		if at.StepID == step {
			out = append(out, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ordinal < out[j].Ordinal })
	return out
}

func (a *Aggregate) control(id domain.AttemptID) *Control {
	if a.Controls == nil {
		a.Controls = map[domain.AttemptID]*Control{}
	}
	c, ok := a.Controls[id]
	if !ok {
		c = NewControl(id)
		a.Controls[id] = c
	}
	return c
}

// Control returns the controller bookkeeping of an attempt.
func (a *Aggregate) Control(id domain.AttemptID) *Control { return a.control(id) }

// Unsettled lists attempts whose effects are not yet accounted for.
func (a *Aggregate) Unsettled() []*Attempt {
	var out []*Attempt
	for _, at := range a.Attempts {
		if !a.control(at.ID).Settled() {
			out = append(out, at)
		}
	}
	return out
}

func (a *Aggregate) recordRun(from RunStatus, now time.Time, reason Reason) {
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectRun, ID: string(a.Run.ID), From: string(from), To: string(a.Run.Status), Reason: reason, At: now.UTC()})
}

func (a *Aggregate) recordStep(s *StepRun, from StepStatus, now time.Time) {
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectStep, ID: s.ID, StepID: s.ID, From: string(from), To: string(s.Status), Reason: s.Reason, At: now.UTC()})
}

func (a *Aggregate) recordAttempt(at *Attempt, from AttemptStatus, now time.Time) {
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectAttempt, ID: string(at.ID), StepID: at.StepID, AttemptID: at.ID, From: string(from), To: string(at.Status), Reason: at.Reason, At: now.UTC()})
}

func (a *Aggregate) addEvidence(id domain.AttemptID, kind EvidenceKind, detail map[string]any, now time.Time) {
	a.Evidence = append(a.Evidence, PendingEvidence{Attempt: id, Evidence: Evidence{Kind: kind, Detail: detail, OccurredAt: now.UTC()}})
}

// RecordEvidence appends late/reconciliation evidence without touching any
// outcome.
func (a *Aggregate) RecordEvidence(id domain.AttemptID, kind EvidenceKind, detail map[string]any, now time.Time) {
	a.addEvidence(id, kind, detail, now)
}

// ConfirmRetention applies run source-retention confirmation.
func (a *Aggregate) ConfirmRetention(refID string, now time.Time) error {
	changed, err := a.Run.ConfirmRetention(refID, now)
	if err != nil {
		return err
	}
	if changed {
		a.Transitions = append(a.Transitions, Transition{Subject: SubjectRun, ID: string(a.Run.ID), From: string(a.Run.Status), To: string(a.Run.Status), Reason: ReasonRetained, At: now.UTC()})
	}
	return nil
}

// ConfirmSourceSetRetention applies the confirmation that a source-set run's
// pinned commits are retained and its run branches exist.
func (a *Aggregate) ConfirmSourceSetRetention(now time.Time) error {
	changed, err := a.Run.ConfirmSourceSetRetention(now)
	if err != nil {
		return err
	}
	if changed {
		a.Transitions = append(a.Transitions, Transition{Subject: SubjectRun, ID: string(a.Run.ID), From: string(a.Run.Status), To: string(a.Run.Status), Reason: ReasonRetained, At: now.UTC()})
	}
	return nil
}

// ---- cancellation and fail-fast draining ----

// drain stops the run's remaining work: pending steps are cancelled and live
// attempts (except the excluded one) lose their authority, are marked
// cancelled and are queued for workload termination. Terminal completion still
// waits for their settlement.
func (a *Aggregate) drain(reason Reason, exclude domain.AttemptID, now time.Time) {
	for _, s := range a.Steps {
		if s.Status == StepPending {
			from := s.Status
			if err := s.cancel(reason, now); err == nil {
				a.recordStep(s, from, now)
			}
		}
	}
	for _, at := range a.Attempts {
		if at.ID == exclude || at.Status != AttemptRunning {
			continue
		}
		a.cancelAttempt(at, reason, now)
	}
}

func (a *Aggregate) cancelAttempt(at *Attempt, reason Reason, now time.Time) {
	from := at.Status
	if err := at.cancel(reason, now); err != nil {
		return
	}
	a.recordAttempt(at, from, now)
	c := a.control(at.ID)
	c.revoke(now)
	c.requestTermination(now)
	a.Revoked = append(a.Revoked, at)
}

// RequestCancel records the cancellation request. It is idempotent: a repeat or
// a terminal run changes nothing. Dispatch is refused from this moment, live
// attempts lose authority, and the run stays nonterminal until settlement.
func (a *Aggregate) RequestCancel(reason Reason, now time.Time) bool {
	if !a.Run.requestCancel(reason, now) {
		return false
	}
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectRun, ID: string(a.Run.ID), From: string(a.Run.Status), To: string(a.Run.Status), Reason: ReasonCancelRequested, At: now.UTC()})
	a.drain(reason, "", now)
	a.stopResources(ResourceReason(reason), now)
	return true
}

// RecordFailure records fail-fast intent: stop dispatch, drain siblings. The
// failed step's own attempt (exclude) keeps its outcome and settles normally.
func (a *Aggregate) RecordFailure(reason Reason, exclude domain.AttemptID, now time.Time) bool {
	if !a.Run.recordFailure(reason, now) {
		return false
	}
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectRun, ID: string(a.Run.ID), From: string(a.Run.Status), To: string(a.Run.Status), Reason: reason, At: now.UTC()})
	a.drain(ReasonRunFailed, exclude, now)
	a.stopResources(ResourceReason(reason), now)
	return true
}

// ---- dispatch ----

// ClaimParams are the facts a dispatch claim persists.
type ClaimParams struct {
	StepID    string
	AttemptID domain.AttemptID
	Plan      WorkloadPlan
	Access    Access
	Workspace string
	Input     string
	Tip       string
	Timeout   time.Duration
	Startup   time.Duration
	// Lease is how long the claiming driver owns the dispatch intent before a
	// redrive may take it over.
	Lease time.Duration
}

// Claim creates the attempt of a pending, runnable step: durable dispatch
// intent (running, "starting") before any workload exists. It enforces every
// dispatch precondition the entities know about; capacity, authority and
// workspace admission are enforced by the caller under the same lock.
func (a *Aggregate) Claim(p ClaimParams, now time.Time) (*Attempt, error) {
	if !a.Run.AcceptsDispatch() {
		return nil, transitionf("the run does not accept dispatch")
	}
	s := a.Step(p.StepID)
	if s == nil {
		return nil, ErrNotFound
	}
	if err := p.AttemptID.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	for _, at := range a.AttemptsOf(s.ID) {
		if !a.control(at.ID).Settled() {
			return nil, transitionf("step %s has an unsettled attempt", s.ID)
		}
	}
	if a.Run.Shared != nil {
		// A run_shared run executes one attempt at a time, whatever its step: the
		// one scratch is held by one attempt and the run's Git authority (its run
		// branch, its agent namespace) belongs to one executable attempt.
		for _, at := range a.Attempts {
			if !a.control(at.ID).Settled() {
				return nil, transitionf("run %s has an unsettled attempt", a.Run.ID)
			}
		}
	}
	fromStep := s.Status
	if err := s.start(now); err != nil {
		return nil, err
	}
	a.recordStep(s, fromStep, now)
	fromRun := a.Run.Status
	if changed, err := a.Run.markRunning(now); err != nil {
		return nil, err
	} else if changed {
		a.recordRun(fromRun, now, ReasonRunning)
	}
	tip := ""
	if p.Access == AccessWrite {
		tip = p.Tip
	}
	at := &Attempt{
		Scope: a.Run.Scope, ID: p.AttemptID, RunID: a.Run.ID, StepID: s.ID, Ordinal: s.AttemptsStarted,
		ActionKey: s.ActionKey, Status: AttemptRunning, Reason: ReasonStarting,
		WorkloadName: p.Plan.WorkloadName, SpecFingerprint: p.Plan.SpecFingerprint,
		WorkspaceID: p.Workspace, Access: p.Access, InputCommit: p.Input, ExpectedTip: tip,
		TimeoutSeconds: int64(p.Timeout / time.Second), StartupDeadline: now.UTC().Add(p.Startup),
		ContextDigest: p.Plan.ContextDigest, Context: p.Plan.Context,
		AuthorityRevision: 1, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	a.Attempts = append(a.Attempts, at)
	c := a.control(at.ID)
	until := now.UTC().Add(p.Lease)
	c.ClaimToken, c.ClaimUntil, c.dirty = "claim", &until, true
	a.Created = append(a.Created, at)
	a.Transitions = append(a.Transitions, Transition{Subject: SubjectAttempt, ID: string(at.ID), StepID: at.StepID, AttemptID: at.ID, From: "", To: string(at.Status), Reason: at.Reason, At: now.UTC()})
	return at, nil
}

// ---- observation ----

// Observation is trusted runtime evidence about an attempt's workload. The
// runtime verifies ownership and spec before reporting it.
type Observation struct {
	Kind ObservationKind
	// WorkloadName and SpecFingerprint must match the attempt's expected
	// identity; WorkloadUID, once bound, its instance.
	WorkloadName    string
	SpecFingerprint string
	WorkloadUID     string
	ObservedAt      time.Time
	// Class and Reason describe a failure; Launch is the runtime's launch
	// evidence for failed and terminated observations.
	Class  FailureClass
	Reason Reason
	Launch LaunchEvidence
}

// ObservationKind selects what the runtime saw.
type ObservationKind string

const (
	// ObservedDispatched: the runtime created or adopted the workload; its
	// instance UID, when reported, is bound to the attempt.
	ObservedDispatched ObservationKind = "dispatched"
	// ObservedStarted: the command started (instance UID known).
	ObservedStarted ObservationKind = "started"
	// ObservedFailed: the workload failed for an infrastructure/transient reason.
	ObservedFailed ObservationKind = "failed"
	// ObservedTerminated: the workload was stopped (or never launched).
	ObservedTerminated ObservationKind = "terminated"
)

// Validate checks the observation shape.
func (o Observation) Validate() error {
	switch o.Kind {
	case ObservedDispatched, ObservedStarted, ObservedFailed, ObservedTerminated:
	default:
		return invalidf("unknown observation kind")
	}
	switch o.Launch {
	case "", LaunchUnknown, LaunchNotStarted, LaunchStarted:
	default:
		return invalidf("unknown launch evidence")
	}
	if o.Kind == ObservedFailed {
		if o.Class != ClassInfrastructure && o.Class != ClassTransient {
			return invalidf("an observed failure is classified infrastructure or transient")
		}
		if !o.Reason.Valid() {
			return invalidf("a failure needs a reason code")
		}
	}
	if o.Kind == ObservedStarted && o.WorkloadUID == "" {
		return invalidf("a started observation needs the workload UID")
	}
	if o.WorkloadUID != "" && !uidPattern.MatchString(o.WorkloadUID) {
		return invalidf("the workload UID is malformed")
	}
	return nil
}

// ObserveResult tells the caller what an observation did.
type ObserveResult struct {
	// Late is true when the attempt was already terminal: the observation was
	// recorded as evidence and rewrote nothing.
	Late bool
}

// Observe applies one runtime observation. Observations of another workload,
// instance or spec are refused and recorded as mismatch evidence; they never
// change state.
func (a *Aggregate) Observe(at *Attempt, o Observation, now time.Time) (ObserveResult, error) {
	if err := o.Validate(); err != nil {
		return ObserveResult{}, err
	}
	c := a.control(at.ID)
	if (o.WorkloadName != "" && o.WorkloadName != at.WorkloadName) || (o.SpecFingerprint != "" && o.SpecFingerprint != at.SpecFingerprint) {
		a.addEvidence(at.ID, EvidenceMismatch, map[string]any{"kind": string(o.Kind)}, now)
		return ObserveResult{}, ErrConflict
	}
	if o.WorkloadUID != "" && at.WorkloadUID != "" && at.WorkloadUID != o.WorkloadUID {
		a.addEvidence(at.ID, EvidenceMismatch, map[string]any{"kind": string(o.Kind), "uid": "differs"}, now)
		return ObserveResult{}, ErrConflict
	}
	switch o.Kind {
	case ObservedTerminated:
		if at.Status == AttemptRunning {
			return ObserveResult{}, transitionf("attempt %s was not asked to terminate", at.ID)
		}
		if c.confirmTermination(o.Launch, now) {
			a.addEvidence(at.ID, EvidenceTermination, map[string]any{"launch": string(c.Launch)}, now)
		}
		return ObserveResult{Late: at.Status.Terminal()}, nil
	}
	if at.Status.Terminal() {
		a.addEvidence(at.ID, EvidenceLateObservation, map[string]any{"kind": string(o.Kind)}, now)
		return ObserveResult{Late: true}, nil
	}
	switch o.Kind {
	case ObservedDispatched:
		if c.DispatchConfirmedAt == nil {
			t := now.UTC()
			c.DispatchConfirmedAt, c.ClaimToken, c.ClaimUntil = &t, "", nil
			c.dirty = true
		}
		at.bindWorkload(o.WorkloadUID, now)
	case ObservedStarted:
		if c.DispatchConfirmedAt == nil {
			t := now.UTC()
			c.DispatchConfirmedAt = &t
			c.dirty = true
		}
		c.noteLaunch(LaunchStarted)
		changed, err := at.observeStart(o.WorkloadUID, now)
		if err != nil {
			return ObserveResult{}, err
		}
		if changed {
			a.Transitions = append(a.Transitions, Transition{Subject: SubjectAttempt, ID: string(at.ID), StepID: at.StepID, AttemptID: at.ID, From: string(at.Status), To: string(at.Status), Reason: at.Reason, At: now.UTC()})
		}
	case ObservedFailed:
		a.failAttempt(at, o.Reason, o.Class, o.Launch, now)
	}
	return ObserveResult{}, nil
}

// failAttempt ends a running attempt as failed. A nonretryable failure records
// fail-fast intent now; a retryable one waits for settlement, because only the
// settled launch evidence can show a replay is safe.
func (a *Aggregate) failAttempt(at *Attempt, reason Reason, class FailureClass, launch LaunchEvidence, now time.Time) {
	// A post-launch infrastructure failure whose launch is unknown is an
	// ambiguous outcome: the attempt is failed with reason outcome_unknown and
	// cannot be retried until its effects are accounted for.
	if (class == ClassInfrastructure || class == ClassTransient) && (launch == "" || launch == LaunchUnknown) && at.StartedAt == nil {
		reason = ReasonOutcomeUnknown
	}
	from := at.Status
	if err := at.fail(reason, class, now); err != nil {
		return
	}
	a.recordAttempt(at, from, now)
	c := a.control(at.ID)
	c.noteLaunch(launch)
	c.revoke(now)
	c.requestTermination(now)
	a.Revoked = append(a.Revoked, at)
	s := a.Step(at.StepID)
	// A retry reuses the run's scratch and services, so in a run_shared run a step
	// whose attempt (or an earlier one) recorded a command begin is never retried
	// automatically, whatever its retry class says: its failure is final now, the
	// services and preparation are stopped promptly and the uncertain attempt or
	// source effects settle independently.
	retryable := s != nil && s.Retryable(class) && !(a.Run.Shared != nil && a.StepBegan(s.ID))
	if retryable {
		return
	}
	a.RecordFailure(stepFailureReason(reason), at.ID, now)
}

func stepFailureReason(r Reason) Reason {
	switch r {
	case ReasonOutcomeUnknown, ReasonStartupTimeout, ReasonTimeout, ReasonCommandFailed, ReasonNeedsInput, ReasonOutputInvalid, ReasonOutputUploadFailed:
		return r
	}
	return ReasonStepFailed
}

// CheckDeadlines fails attempts that missed their distinct startup or
// execution deadline. Gate waits have neither: a gate step allocates no
// attempt. A timeout is a failure with a timeout reason, never retried.
func (a *Aggregate) CheckDeadlines(now time.Time) []*Attempt {
	var failed []*Attempt
	for _, at := range append([]*Attempt(nil), a.Attempts...) {
		if at.Status != AttemptRunning {
			continue
		}
		switch {
		case at.StartedAt == nil && now.After(at.StartupDeadline):
			a.failAttempt(at, ReasonStartupTimeout, ClassTimeout, LaunchUnknown, now)
			failed = append(failed, at)
		case at.ExecutionDeadline != nil && now.After(*at.ExecutionDeadline):
			a.failAttempt(at, ReasonTimeout, ClassTimeout, LaunchStarted, now)
			failed = append(failed, at)
		}
	}
	return failed
}

// ---- results ----

// CompleteAttempt applies an accepted, validated result to a running attempt.
// A successful result succeeds the attempt and its step and settles the attempt
// (the workload finished its work and its writer publication settled in the
// same transaction); a failed or needs-input result fails it (diagnostics only,
// no success outputs).
func (a *Aggregate) CompleteAttempt(at *Attempt, hash string, res runtimev1.Result, now time.Time) error {
	if at.Status != AttemptRunning || !a.control(at.ID).Live() {
		return ErrAttemptRevoked
	}
	s := a.Step(at.StepID)
	if s == nil {
		return ErrNotFound
	}
	from := at.Status
	switch res.Outcome {
	case runtimev1.OutcomeCompleted:
		if err := at.succeed(hash, res, now); err != nil {
			return err
		}
		a.recordAttempt(at, from, now)
		c := a.control(at.ID)
		c.revoke(now)
		c.Effect = EffectSettled
		fromStep := s.Status
		commit := ""
		if res.SourceCommit != nil {
			commit = string(*res.SourceCommit)
		}
		if err := s.succeed(commit, res.Outputs, now); err != nil {
			return err
		}
		a.recordStep(s, fromStep, now)
		return nil
	case runtimev1.OutcomeFailed, runtimev1.OutcomeNeedsInput:
		reason := ReasonCommandFailed
		if res.Outcome == runtimev1.OutcomeNeedsInput {
			reason = ReasonNeedsInput
		} else if res.Reason != nil {
			// The wrapper's output failures keep their specific reason; any other
			// reason a command reports stays the generic command failure. The
			// failure class is unchanged.
			switch r := Reason(*res.Reason); r {
			case ReasonOutputInvalid, ReasonOutputUploadFailed:
				reason = r
			}
		}
		if err := at.fail(reason, ClassCommand, now); err != nil {
			return err
		}
		r := res
		at.ResultHash, at.Result = hash, &r
		a.recordAttempt(at, from, now)
		c := a.control(at.ID)
		c.noteLaunch(LaunchStarted)
		c.revoke(now)
		c.requestTermination(now)
		a.Revoked = append(a.Revoked, at)
		a.RecordFailure(reason, at.ID, now)
		return nil
	}
	return invalidf("unknown result outcome")
}

// ---- settlement ----

// SettleOutcome reports what Settle decided.
type SettleOutcome string

const (
	SettledNow        SettleOutcome = "settled"
	SettleBlocked     SettleOutcome = "blocked"
	SettleNotNeeded   SettleOutcome = "already_settled"
	SettleAwaitingEnd SettleOutcome = "awaiting_termination"
)

// Settle accounts for an attempt's effects. It refuses while the workload is
// not confirmed gone (non-success) and while the launch evidence is unknown and
// unreconciled: an ambiguous outcome blocks the step, its writer slot and its
// capacity until trusted reconciliation supplies affirmative evidence. It then
// resolves the step: cancel, retry with persisted backoff when safe, or fail.
func (a *Aggregate) Settle(at *Attempt, now time.Time) (SettleOutcome, error) {
	c := a.control(at.ID)
	if c.Settled() {
		return SettleNotNeeded, nil
	}
	if !at.Status.Terminal() {
		return SettleBlocked, transitionf("attempt %s is still running", at.ID)
	}
	if at.Status != AttemptSucceeded {
		if c.TerminationConfirmedAt == nil {
			return SettleAwaitingEnd, nil
		}
		if c.Launch == LaunchUnknown && c.ReconciledAt == nil {
			c.Effect = EffectUnknown
			c.dirty = true
			return SettleBlocked, ErrOutcomeUnknown
		}
	}
	c.Effect = EffectSettled
	t := now.UTC()
	c.SettledAt = &t
	c.dirty = true

	s := a.Step(at.StepID)
	if s != nil && s.Status == StepRunning && s.AttemptsStarted == at.Ordinal {
		from := s.Status
		replaySafe := c.Launch == LaunchNotStarted || (c.ReconciledAt != nil && c.ReplaySafe)
		if a.Run.Shared != nil {
			// A run_shared retry reuses the same scratch, so it is allowed only
			// when no begin was recorded for this attempt or any earlier attempt of
			// the step and the workload is verified never started. Reconciled
			// replay-safety of an effect never overrides a recorded begin: the
			// scratch may hold partial mutations.
			replaySafe = c.Launch == LaunchNotStarted && !a.StepBegan(s.ID)
		}
		switch {
		case at.Status == AttemptCancelled:
			_ = s.cancel(at.Reason, now)
		case at.Status == AttemptFailed && s.Retryable(at.Class) && replaySafe && !a.Run.Draining():
			if err := s.scheduleRetry(now); err != nil {
				return SettleBlocked, err
			}
		case at.Status == AttemptFailed:
			_ = s.fail(at.Reason, now)
			a.RecordFailure(stepFailureReason(at.Reason), at.ID, now)
		}
		if s.Status != from {
			a.recordStep(s, from, now)
		}
	}
	c.StepResolvedAt = &t
	return SettledNow, nil
}

// Reconcile applies trusted reconciliation evidence to an unsettled attempt
// whose effects were unknown. The attempt's own outcome is never rewritten: a
// failed outcome_unknown attempt stays failed, whatever the evidence says.
func (a *Aggregate) Reconcile(at *Attempt, ev ReconciliationEvidence, now time.Time) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	c := a.control(at.ID)
	if c.Settled() {
		return nil
	}
	a.addEvidence(at.ID, EvidenceReconciliation, map[string]any{"verdict": string(ev.Verdict), "source": ev.Source, "replaySafe": ev.Verdict == VerdictReplaySafe}, now)
	switch ev.Verdict {
	case VerdictStillUnknown:
		return nil
	case VerdictReplaySafe, VerdictSettled:
		t := now.UTC()
		c.ReconciledAt = &t
		c.ReplaySafe = ev.Verdict == VerdictReplaySafe
		c.Effect = EffectNone
		c.dirty = true
	}
	return nil
}

// Verdict is the outcome of a reconciliation.
type Verdict string

const (
	// VerdictStillUnknown: no affirmative evidence; stay blocked.
	VerdictStillUnknown Verdict = "still_unknown"
	// VerdictReplaySafe: affirmative evidence that replaying is safe (the
	// effect did not happen, or the tool honors the action key).
	VerdictReplaySafe Verdict = "replay_safe"
	// VerdictSettled: the effect is accounted for but replay is not safe.
	VerdictSettled Verdict = "settled"
)

// ReconciliationEvidence is trusted adapter evidence about one attempt.
type ReconciliationEvidence struct {
	Verdict Verdict
	// Source names the trusted adapter (never free-form workload text).
	Source string
}

// Validate checks the evidence.
func (e ReconciliationEvidence) Validate() error {
	switch e.Verdict {
	case VerdictStillUnknown, VerdictReplaySafe, VerdictSettled:
	default:
		return invalidf("unknown reconciliation verdict")
	}
	if e.Source == "" || len(e.Source) > 128 {
		return invalidf("reconciliation evidence must name its source")
	}
	return nil
}

// ---- completion ----

// Finish ends the run when every attempt is settled and no step still awaits
// resolution. Terminal completion without settlement is impossible.
func (a *Aggregate) Finish(now time.Time) (bool, error) {
	if a.Run.Status.Terminal() {
		return false, nil
	}
	for _, at := range a.Attempts {
		if !a.control(at.ID).Settled() {
			return false, nil
		}
	}
	for _, s := range a.Steps {
		if s.Status == StepRunning {
			return false, nil
		}
	}
	if a.Run.Shared != nil && !a.ResourcesRemoved() {
		// Terminal completion waits for every resource to be verified stopped and
		// removed. The outcome intent is already recorded (or derivable); the run
		// stays nonterminal with a visible cleanup reason until then.
		if a.Run.CancelRequestedAt == nil && a.Run.FailureIntentAt == nil && !a.completionDecided() {
			return false, nil
		}
		a.stopResources(ResourceReason(ReasonCompleted), now)
		return false, nil
	}
	from := a.Run.Status
	var to RunStatus
	var reason Reason
	switch {
	case a.Run.CancelRequestedAt != nil:
		to, reason = RunCancelled, a.Run.CancelReason
		for _, s := range a.Steps {
			if s.Status == StepPending {
				f := s.Status
				_ = s.cancel(a.Run.CancelReason, now)
				a.recordStep(s, f, now)
			}
		}
	case a.Run.FailureIntentAt != nil:
		to, reason = RunFailed, a.Run.FailureReason
	default:
		for _, s := range a.Steps {
			if !s.Status.Terminal() {
				return false, nil
			}
			if s.Status == StepFailed || s.Status == StepCancelled {
				return false, nil
			}
		}
		to, reason = RunSucceeded, ReasonCompleted
	}
	if err := a.Run.finish(to, reason, now); err != nil {
		return false, err
	}
	a.recordRun(from, now, reason)
	return true, nil
}

// String describes the aggregate for diagnostics without sensitive content.
func (a *Aggregate) String() string {
	return fmt.Sprintf("run %s %s (%d steps, %d attempts)", a.Run.ID, a.Run.Status, len(a.Steps), len(a.Attempts))
}

// ClaimRedrive takes over an unconfirmed dispatch intent whose lease lapsed, so
// that exactly one controller redrives the same attempt and workload identity.
func (a *Aggregate) ClaimRedrive(at *Attempt, token string, lease time.Duration, now time.Time) bool {
	c := a.control(at.ID)
	if at.Status != AttemptRunning || c.DispatchConfirmedAt != nil || !c.Live() {
		return false
	}
	if c.ClaimUntil != nil && c.ClaimUntil.After(now) {
		return false
	}
	until := now.UTC().Add(lease)
	c.ClaimToken, c.ClaimUntil, c.dirty = token, &until, true
	return true
}
