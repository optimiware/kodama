package execution

import (
	"errors"
	"fmt"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Run-resource failures a transport maps to the runtime v2 error codes.
var (
	// ErrResourceUnbound: execution has not yet bound the resource's UID
	// (resource_unbound, 409; retry later).
	ErrResourceUnbound = fmt.Errorf("%w: resource_unbound", ErrConflict)
	// ErrResourceRevoked: the resource is stopping, stopped or past its
	// deadline (resource_revoked, 403).
	ErrResourceRevoked = fmt.Errorf("%w: resource_revoked", ErrDenied)
	// ErrReceiptConflict: a different receipt is already recorded
	// (receipt_conflict, 409).
	ErrReceiptConflict = fmt.Errorf("%w: receipt_conflict", ErrConflict)
	// ErrBeginConflict: a repeat of begin with a different body
	// (idempotency_conflict, 409).
	ErrBeginConflict = fmt.Errorf("%w: idempotency_conflict", ErrConflict)
)

// SubjectResource is the history/audit subject of a run resource.
const SubjectResource SubjectKind = "resource"

// RunResource is the durable record of one run resource: intent first (the
// canonical spec and its fingerprint exist before any runtime call), then the
// bound UID, the verified readiness, stop and removal evidence and safe reasons.
// It is not a business aggregate and has no status enum visible as a run, step
// or attempt state. It stores no scratch content and no secret plaintext.
type RunResource struct {
	Scope   domain.Scope
	RunID   domain.RunID
	Kind    ResourceKind
	Key     string
	Name    string
	Ordinal int

	Backend     string
	Spec        ResourceSpec
	Fingerprint string

	// EnsureRequestedAt is recorded (and committed) before each Ensure call, so
	// a crash between the call and its observation is a known "may exist" case.
	EnsureRequestedAt *time.Time
	UID               string
	BoundAt           *time.Time

	// ObserveGen is the server-side generation of the last observation (or
	// Ensure) REQUEST committed before its runtime call; AppliedGen the highest
	// generation whose answer was applied and ObserveRequestedAt when the latest
	// request was issued. A response is applied only if its generation is newer
	// than every applied one, so a delayed older answer can never overwrite a
	// newer unknown/pending observation: arrival order is never trusted.
	ObserveGen         int64
	AppliedGen         int64
	ObserveRequestedAt *time.Time

	Phase      ResourcePhase
	Reason     ResourceReason
	ObservedAt *time.Time
	ReadyAt    *time.Time
	ProbeKind  string
	// NotReadySince starts when a previously ready service stops being ready.
	NotReadySince *time.Time
	UnknownSince  *time.Time
	// Released is a workspace's last verified "no process or mount uses it".
	Released   bool
	ReleasedAt *time.Time

	// Receipt (preparation only): the recorded trusted checkout receipt, its
	// canonical digest (bound to the workspace UID) and when it was recorded.
	Receipt       *runtimev2.PreparationReceiptRequest
	ReceiptDigest string
	ReceiptAt     *time.Time

	StopRequestedAt *time.Time
	StopReason      ResourceReason
	StoppedAt       *time.Time
	StopLaunch      LaunchEvidence
	RemovedAt       *time.Time

	// Failures and NextActionAt drive restart-safe retry with backoff of
	// Ensure, Stop and Remove.
	Failures     int
	NextActionAt *time.Time

	Version   domain.Version
	CreatedAt time.Time
	UpdatedAt time.Time

	dirty bool
}

// NewRunResource creates the intent record of a spec. The spec must be valid.
func NewRunResource(spec ResourceSpec, ordinal int, now time.Time) (*RunResource, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return nil, err
	}
	return &RunResource{
		Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Key: spec.Key, Name: spec.Name, Ordinal: ordinal, Backend: spec.Backend,
		Spec: spec, Fingerprint: fp, Phase: PhasePending, Version: 1, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), dirty: true,
	}, nil
}

func (r *RunResource) touch(now time.Time) {
	if !r.dirty {
		r.Version++
		r.dirty = true
	}
	r.UpdatedAt = now.UTC()
}

// Dirty / MarkClean support persistence adapters. A resource created by a
// mutation is dirty until saved; Created tracks inserts separately.
func (r *RunResource) Dirty() bool { return r.dirty }
func (r *RunResource) MarkClean()  { r.dirty = false }

// Handle returns the resource's handle with its bound UID.
func (r *RunResource) Handle() ResourceHandle { return r.Spec.Handle(r.UID) }

// Stopping reports whether stop was requested.
func (r *RunResource) Stopping() bool { return r.StopRequestedAt != nil }

// Stopped reports verified stop (never reversed).
func (r *RunResource) Stopped() bool { return r.StoppedAt != nil }

// Removed reports verified removal (never reversed).
func (r *RunResource) Removed() bool { return r.RemovedAt != nil }

// Bound reports whether the provider's UID is bound.
func (r *RunResource) Bound() bool { return r.UID != "" }

// IsReady reports a verified usable resource that is not stopping.
func (r *RunResource) IsReady() bool {
	return r.Phase == PhaseReady && r.ReadyAt != nil && !r.Stopping() && !r.Removed()
}

// Due reports whether a retry of an effect is allowed now.
func (r *RunResource) Due(now time.Time) bool {
	return r.NextActionAt == nil || !r.NextActionAt.After(now)
}

// resState is the recorded position of a resource: the lifecycle name (phase,
// stopping, stopped, removed), the observed phase, the safe reason and whether
// a provider identity is bound. A transition is recorded when any of them moves.
type resState struct {
	name, phase string
	reason      ResourceReason
	uid         bool
	// receipt and ensured are the recorded evidence and intent: a same-phase
	// change of either is an observation transition too.
	receipt, ensured bool
}

func (r *RunResource) stateName() string {
	switch {
	case r.Removed():
		return "removed"
	case r.Stopped():
		return "stopped"
	case r.Stopping():
		return "stopping"
	}
	return string(r.Phase)
}

func (r *RunResource) state() resState {
	phase := string(r.Phase)
	if r.Removed() {
		phase = string(PhaseAbsent)
	}
	return resState{name: r.stateName(), phase: phase, reason: r.Reason, uid: r.Bound(), receipt: r.Receipt != nil, ensured: r.EnsureRequestedAt != nil}
}

// RequestObservation allocates the next request generation. It is committed
// before the runtime call it covers.
func (r *RunResource) RequestObservation(now time.Time) (int64, time.Time) {
	t := now.UTC()
	r.ObserveGen++
	r.ObserveRequestedAt = &t
	r.touch(now)
	return r.ObserveGen, t
}

// MarkEnsureRequested records, durably and before the runtime call, that an
// Ensure may create the object. Repeating it is allowed (an Ensure retry).
func (r *RunResource) MarkEnsureRequested(now time.Time) error {
	if r.Stopping() || r.Stopped() {
		return fmt.Errorf("%w: a stopped resource is never started again", ErrResourceFenced)
	}
	t := now.UTC()
	r.EnsureRequestedAt = &t
	r.touch(now)
	return nil
}

// Bind binds the provider's UID once. A different UID later is a collision.
func (r *RunResource) Bind(uid string, now time.Time) error {
	if !resourceUIDPattern.MatchString(uid) {
		return invalidf("the resource UID is malformed")
	}
	if r.UID != "" {
		if r.UID != uid {
			return ErrResourceCollision
		}
		return nil
	}
	t := now.UTC()
	r.UID, r.BoundAt = uid, &t
	r.touch(now)
	return nil
}

// observe records a verified observation. It never moves a stopped or removed
// resource back, and only a UID/fingerprint-matching observation is applied; a
// mismatch is returned as ErrResourceCollision for the caller to treat as loss.
func (r *RunResource) observe(o ResourceObservation, now time.Time) error {
	return r.observeStamped(o, now, now)
}

// observeAt applies the answer to request generation gen, issued at requestedAt.
// A generation that is not newer than an applied one is discarded (false). The
// credible observation time is the adapter's own ObservedAt (never in the
// future) or, absent that, the start of the request: never the arrival time of
// a delayed response.
func (r *RunResource) observeAt(gen int64, requestedAt time.Time, o ResourceObservation, now time.Time) (bool, error) {
	if gen <= r.AppliedGen {
		return false, nil
	}
	r.AppliedGen = gen
	at := requestedAt.UTC()
	if !o.ObservedAt.IsZero() {
		at = o.ObservedAt.UTC()
		if at.After(now) {
			at = now.UTC()
		}
	}
	r.touch(now)
	return true, r.observeStamped(o, at, now)
}

func (r *RunResource) observeStamped(o ResourceObservation, at, now time.Time) error {
	// Identity is verified BEFORE anything is bound or recorded: an observation
	// of another instance or another spec is a collision that commits nothing of
	// the foreign object (no binding, no phase).
	if o.Handle.UID != "" && r.UID != "" && o.Handle.UID != r.UID {
		return ErrResourceCollision
	}
	if o.Fingerprint != "" && o.Fingerprint != r.Fingerprint {
		return ErrResourceCollision
	}
	if r.Removed() {
		return nil
	}
	// An observation is positive evidence about OUR object only with an
	// affirmative, matching fingerprint. An empty one means "could not verify":
	// a claimed ready, pending, failed or stopped is then unknown, binds nothing and
	// is never fresh positive evidence. Absent and unknown carry no object.
	verified := o.Fingerprint != "" && o.Fingerprint == r.Fingerprint
	t := at.UTC()
	phase := o.Phase
	if r.Stopped() && (phase == PhaseReady || phase == PhasePending) {
		// A stopped resource is never started again: only the stop evidence counts.
		phase = PhaseStopped
	} else if !verified && (phase == PhaseReady || phase == PhasePending || phase == PhaseFailed || phase == PhaseStopped) {
		phase, o.Reason = PhaseUnknown, reasonResourceUnverified
	}
	if verified && phase != PhaseUnknown && o.Handle.UID != "" {
		if err := r.Bind(o.Handle.UID, now); err != nil {
			return err
		}
	}
	r.ObservedAt = &t
	if phase == PhaseReady && !r.Bound() {
		// Readiness is verified against the bound identity: a ready claim without
		// one is not evidence.
		phase, o.Reason = PhaseUnknown, reasonResourceUnobservable
	}
	if phase == PhaseUnknown && o.Released {
		o.Released = false // unknown never carries release evidence
	}
	switch phase {
	case PhaseReady:
		if r.ReadyAt == nil {
			ra := t
			if o.ReadyAt != nil {
				ra = o.ReadyAt.UTC()
			}
			r.ReadyAt = &ra
		}
		r.ProbeKind = o.ProbeKind
		r.NotReadySince, r.UnknownSince, r.Reason = nil, nil, ""
	case PhasePending:
		if r.ReadyAt != nil && r.NotReadySince == nil {
			r.NotReadySince = &t
		}
		r.UnknownSince = nil
	case PhaseUnknown:
		if r.UnknownSince == nil {
			r.UnknownSince = &t
		}
		if o.Reason != "" {
			r.Reason = o.Reason
		}
	case PhaseFailed, PhaseAbsent, PhaseStopped:
		r.UnknownSince = nil
		if o.Reason != "" {
			r.Reason = o.Reason
		}
	default:
		return invalidf("unknown resource phase")
	}
	r.Phase = phase
	if r.Kind == KindWorkspace {
		r.Released = o.Released
		if o.Released {
			ra := t
			r.ReleasedAt = &ra
		} else {
			r.ReleasedAt = nil
		}
	}
	r.touch(now)
	return nil
}

// Lost reports an affirmative loss of a resource that was in use: it failed or
// is absent after being ensured or ready, or a service readiness has been
// failing for its loss window. An unknown phase is never loss.
func (r *RunResource) Lost(now time.Time) (ResourceReason, bool) {
	if r.Stopping() || r.Removed() {
		return "", false
	}
	switch r.Phase {
	case PhaseFailed, PhaseAbsent:
		if r.ReadyAt != nil || r.Bound() {
			return r.lossReason(), true
		}
	case PhaseStopped:
		// A verified stop nobody asked for. A ready service, the scratch or the
		// network stopping on its own is loss; a preparation that stopped
		// without its receipt failed. A preparation that stopped after its
		// receipt is the intended end of its job.
		if r.Stopped() {
			return "", false
		}
		if r.Kind == KindPreparation && r.Receipt != nil {
			return "", false
		}
		return r.lossReason(), true
	case PhasePending:
		if r.Kind == KindService && r.ReadyAt != nil && r.NotReadySince != nil {
			if now.Sub(*r.NotReadySince) >= time.Duration(r.Spec.Service.Readiness.LossSecs)*time.Second {
				return ReasonServiceLost, true
			}
		}
	}
	return "", false
}

func (r *RunResource) lossReason() ResourceReason {
	switch r.Kind {
	case KindService:
		if r.ReadyAt == nil {
			return ReasonServiceNotReady
		}
		return ReasonServiceLost
	case KindPreparation:
		return ReasonPreparationFailed
	}
	return ReasonWorkspaceLost
}

// StartupExpired reports a resource that was ensured but is not ready within
// its startup budget (service readiness timeout or preparation timeout).
func (r *RunResource) StartupExpired(now time.Time) (ResourceReason, bool) {
	if r.Stopping() || r.EnsureRequestedAt == nil {
		return "", false
	}
	switch r.Kind {
	case KindService:
		if r.ReadyAt == nil && now.Sub(*r.EnsureRequestedAt) > time.Duration(r.Spec.Service.Readiness.TimeoutSecs)*time.Second {
			return ReasonServiceNotReady, true
		}
	case KindPreparation:
		// The preparation ends only with a recorded receipt; completing or
		// exiting without one, within the budget or not, is a failure at the
		// budget's end.
		if r.Receipt == nil && now.Sub(*r.EnsureRequestedAt) > time.Duration(r.Spec.Preparation.TimeoutSecs)*time.Second {
			return ReasonPreparationFailed, true
		}
	}
	return "", false
}

// requestStop records the stop intent once with its reason.
func (r *RunResource) requestStop(reason ResourceReason, now time.Time) bool {
	if r.StopRequestedAt != nil || r.Removed() {
		return false
	}
	t := now.UTC()
	r.StopRequestedAt, r.StopReason = &t, reason
	r.touch(now)
	return true
}

// recordStop records the runtime's stop evidence. A resource is stopped only
// on affirmative evidence; for the workspace the release of the volume is part
// of it. Evidence is recorded once and never reversed.
func (r *RunResource) recordStop(ev ResourceStopEvidence, now time.Time) bool {
	if r.Stopped() {
		return false
	}
	if !ev.Stopped || (r.Kind == KindWorkspace && !ev.Released) {
		if r.Reason != ReasonStopUnconfirmed {
			r.Reason = ReasonStopUnconfirmed
			r.touch(now)
			return true
		}
		return false
	}
	t := now.UTC()
	launch := ev.Launch
	if launch == "" {
		launch = LaunchUnknown
	}
	r.StoppedAt, r.StopLaunch, r.Phase, r.Reason = &t, launch, PhaseStopped, ""
	if r.Kind == KindWorkspace {
		r.Released, r.ReleasedAt = true, &t
	}
	r.touch(now)
	return true
}

// recordRemove records verified removal; it requires a prior verified stop. A
// resource whose removal is unconfirmed stays visibly cleanup_pending.
func (r *RunResource) recordRemove(ev ResourceRemoveEvidence, now time.Time) (bool, error) {
	if r.Removed() {
		return false, nil
	}
	if !r.Stopped() {
		return false, transitionf("resource %s is removed only after a verified stop", r.Key)
	}
	if !ev.Removed {
		if r.Reason != ReasonCleanupPending {
			r.Reason = ReasonCleanupPending
			r.touch(now)
			return true, nil
		}
		return false, nil
	}
	t := now.UTC()
	r.RemovedAt, r.Reason = &t, ""
	r.touch(now)
	return true, nil
}

// noteFailure records a failed or ambiguous runtime call: a restart-safe
// retry time with exponential backoff. The reason is safe and stable.
func (r *RunResource) noteFailure(reason ResourceReason, base, max time.Duration, now time.Time) {
	r.Failures++
	d := base
	for i := 1; i < r.Failures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	next := now.UTC().Add(d)
	r.NextActionAt = &next
	if reason != "" {
		r.Reason = reason
	}
	r.touch(now)
}

func (r *RunResource) clearFailure(now time.Time) {
	if r.Failures != 0 || r.NextActionAt != nil {
		r.Failures, r.NextActionAt = 0, nil
		r.touch(now)
	}
}

// ---- holder and begin ----

// RunHolder records which attempt holds the run's single workspace volume. It
// is separate from the logical workspace's writer reservation: a reader holds
// the volume, only a writer reserves the logical workspace. Exactly one
// holder is unreleased at a time, enforced by the transaction (and a partial
// unique index in PostgreSQL).
type RunHolder struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	StepID    string
	HeldAt    time.Time
	// ReleasedAt is when the workspace was observed released after the
	// attempt's workload was verified stopped and its effects settled.
	ReleasedAt *time.Time

	dirty bool
}

func (h *RunHolder) Dirty() bool { return h.dirty }
func (h *RunHolder) MarkClean()  { h.dirty = false }

// BeginRecord is the durable command-begin authorization of one attempt: taken
// immediately before the command starts, idempotent by body digest, and the
// proof that an attempt "may have run user code".
type BeginRecord struct {
	Scope          domain.Scope
	RunID          domain.RunID
	AttemptID      domain.AttemptID
	StepID         string
	Access         Access
	BeganAt        time.Time
	WorkspaceState runtimev2.WorkspaceState
	IgnoredPresent bool
	BodyDigest     string
}

// ---- aggregate: resources ----

// Resource returns the run's resource by key, or nil.
func (a *Aggregate) Resource(key string) *RunResource {
	for _, r := range a.Resources {
		if r.Key == key {
			return r
		}
	}
	return nil
}

// ResourcesRemoved reports that every resource of the run is verified removed
// (vacuously true for a run without resources).
func (a *Aggregate) ResourcesRemoved() bool {
	for _, r := range a.Resources {
		if !r.Removed() {
			return false
		}
	}
	return true
}

// UnfinishedResource returns the first resource not yet verified removed, in
// reverse provisioning order (the order teardown handles them), or nil.
func (a *Aggregate) UnfinishedResource() *RunResource {
	for i := len(a.Resources) - 1; i >= 0; i-- {
		if !a.Resources[i].Removed() {
			return a.Resources[i]
		}
	}
	return nil
}

// CleanupReason is the safe, visible reason a run is held nonterminal by its
// resources, or "" when none is. It is derived, never a business status.
func (a *Aggregate) CleanupReason() ResourceReason {
	if a.Run.Shared == nil || a.ResourcesRemoved() || len(a.Resources) == 0 {
		return ""
	}
	// A resource holds the run nonterminal once the outcome is decided: failure or
	// cancellation, successful completion of every step, or an interrupted older
	// controller's terminal run.
	if !a.Draining() && !a.completionDecided() && !a.Run.Status.Terminal() {
		return ""
	}
	for _, r := range a.Resources {
		if r.Stopping() && !r.Stopped() && r.Reason == ReasonStopUnconfirmed {
			return ReasonStopUnconfirmed
		}
	}
	return ReasonCleanupPending
}

// Draining reports whether the run is stopping, including a passed deadline.
func (a *Aggregate) Draining() bool { return a.Run.Draining() }

// ReceiptBound reports a recorded receipt whose digest still binds the bound
// workspace UID (recomputed, never trusted from the stored digest alone).
func (a *Aggregate) ReceiptBound() bool {
	prep, ws := a.Resource(KeyPreparation), a.Resource(KeyWorkspace)
	if prep == nil || ws == nil || prep.Receipt == nil || ws.UID == "" {
		return false
	}
	d, err := PreparationReceiptObject{Request: *prep.Receipt, RunID: a.Run.ID, ResourceKey: KeyPreparation, WorkspaceUID: ws.UID}.Digest()
	return err == nil && d == prep.ReceiptDigest
}

// WorkspaceReady is the workspace barrier: the scratch is bound and ready; and,
// for a source-backed run, the preparation receipt is recorded and bound to the
// workspace UID and the preparation workload is verified stopped (its
// credentials are gone). A source-free scratch is ready when bound and ready.
func (a *Aggregate) WorkspaceReady() bool {
	ws := a.Resource(KeyWorkspace)
	if ws == nil || !ws.IsReady() || !ws.Bound() {
		return false
	}
	if !a.Run.Shared.SourceBacked {
		return true
	}
	prep := a.Resource(KeyPreparation)
	return prep != nil && prep.Stopped() && a.ReceiptBound()
}

// ServicesReady reports that every declared service is verified ready.
func (a *Aggregate) ServicesReady() bool {
	for _, r := range a.Resources {
		if r.Kind == KindService && !r.IsReady() {
			return false
		}
	}
	return true
}

// BarrierOpen is the dispatch barrier over provisioning state: network,
// workspace and every service ready and fresh, nothing stopping.
func (a *Aggregate) BarrierOpen(now time.Time, fresh time.Duration) bool {
	if a.Run.Shared == nil || len(a.Resources) == 0 {
		return false
	}
	net := a.Resource(KeyNetwork)
	if net == nil || !net.IsReady() || !a.WorkspaceReady() || !a.ServicesReady() {
		return false
	}
	if ws := a.Resource(KeyWorkspace); a.ActiveHolder() == nil && (ws == nil || !ws.Released) {
		// With no attempt holding the volume, the freshest observation must also
		// report it released: a mount or process still using it blocks the next
		// handoff.
		return false
	}
	for _, r := range a.Resources {
		if r.Kind == KindPreparation {
			continue // verified through WorkspaceReady
		}
		if r.ObservedAt == nil || now.Sub(*r.ObservedAt) > fresh || r.UnknownSince != nil {
			return false
		}
	}
	return true
}

// ---- aggregate: holder and begin ----

// ActiveHolder returns the unreleased holder, or nil.
func (a *Aggregate) ActiveHolder() *RunHolder {
	for _, h := range a.Holders {
		if h.ReleasedAt == nil {
			return h
		}
	}
	return nil
}

// Began reports whether a begin was recorded for the attempt.
func (a *Aggregate) Began(id domain.AttemptID) bool {
	for _, b := range a.Begins {
		if b.AttemptID == id {
			return true
		}
	}
	return false
}

// StepBegan reports whether any attempt of the step recorded a begin: such a
// step is never retried automatically.
func (a *Aggregate) StepBegan(step string) bool {
	for _, b := range a.Begins {
		if b.StepID == step {
			return true
		}
	}
	return false
}

// attemptQuiescent reports that the attempt's workload is verified stopped.
func (a *Aggregate) attemptQuiescent(at *Attempt) bool {
	return at.Status.Terminal() && a.control(at.ID).TerminationConfirmedAt != nil
}

// HoldersQuiescent reports that no attempt of the run can still use the
// volume: every attempt is terminal with its workload verified stopped.
func (a *Aggregate) HoldersQuiescent() bool {
	for _, at := range a.Attempts {
		if !a.attemptQuiescent(at) {
			return false
		}
	}
	return true
}

var errNoHolder = errors.New("no holder")

// ReleaseHolder releases the unreleased holder after the attempt settled with
// its workload verified stopped AND the CURRENT state of the workspace carries
// fresh, UID-bound release evidence that postdates the verified stop: the
// workspace is bound, observed ready and released, not older than fresh and
// observed no earlier than the attempt's termination confirmation. The
// evidence is read from the state loaded under the run lock, never passed in by
// a caller that observed it earlier, so an intervening unreleased or unknown
// observation blocks the handoff. It is idempotent for an already released
// holder.
func (a *Aggregate) ReleaseHolder(id domain.AttemptID, fresh time.Duration, now time.Time) (bool, error) {
	for _, h := range a.Holders {
		if h.AttemptID != id {
			continue
		}
		if h.ReleasedAt != nil {
			return false, nil
		}
		at := a.Attempt(id)
		c := a.control(id)
		if at == nil || !c.Settled() || !a.attemptQuiescent(at) {
			return false, transitionf("the holder's attempt is not settled and stopped")
		}
		if !a.WorkspaceReleasedSince(*c.TerminationConfirmedAt, fresh, now) {
			return false, transitionf("the workspace is not observed released")
		}
		t := now.UTC()
		h.ReleasedAt, h.dirty = &t, true
		return true, nil
	}
	return false, errNoHolder
}

// WorkspaceReleasedSince reports current, fresh, UID-bound evidence that no
// process or mount uses the workspace volume, observed at or after since.
func (a *Aggregate) WorkspaceReleasedSince(since time.Time, fresh time.Duration, now time.Time) bool {
	ws := a.Resource(KeyWorkspace)
	if ws == nil || !ws.Bound() || ws.Phase != PhaseReady || !ws.Released || ws.UnknownSince != nil || ws.Stopping() || ws.Removed() ||
		ws.ObservedAt == nil || ws.ReleasedAt == nil {
		return false
	}
	return !ws.ObservedAt.Before(since) && now.Sub(*ws.ObservedAt) <= fresh
}

// acquireHolder records the attempt as the volume's holder in the claim's
// transaction. Exactly one unreleased holder may exist.
func (a *Aggregate) acquireHolder(at *Attempt, now time.Time) error {
	if a.ActiveHolder() != nil {
		return transitionf("the run workspace has an unreleased holder")
	}
	h := &RunHolder{Scope: a.Run.Scope, RunID: a.Run.ID, AttemptID: at.ID, StepID: at.StepID, HeldAt: now.UTC(), dirty: true}
	a.Holders = append(a.Holders, h)
	a.NewHolders = append(a.NewHolders, h)
	return nil
}

// completionDecided reports that every step ended successfully or was skipped:
// the success outcome is decided and only resource cleanup remains.
func (a *Aggregate) completionDecided() bool {
	for _, s := range a.Steps {
		if !s.Status.Terminal() || s.Status == StepFailed || s.Status == StepCancelled {
			return false
		}
	}
	return true
}

// stopResources records the stop intent on every resource, once, with the
// reason: it revokes admission and credentials of the resources (a stopping
// resource has no authority) and releases nothing yet. The reconciler performs
// the stops in order and records verified evidence. It never touches an
// attempt_private run.
func (a *Aggregate) stopResources(reason ResourceReason, now time.Time) {
	if a.Run.Shared == nil {
		return
	}
	for _, r := range a.Resources {
		from := r.state()
		if r.requestStop(reason, now) {
			a.recordResource(r, from, now)
		}
	}
}

func (a *Aggregate) recordResource(r *RunResource, from resState, now time.Time) {
	to := r.state()
	if from == to {
		return
	}
	reason := Reason(r.Reason)
	if reason == "" {
		switch {
		case !from.receipt && to.receipt:
			reason = ReasonReceiptRecorded
		case !from.ensured && to.ensured:
			reason = ReasonEnsureRequested
		case to.name == "removed":
			reason = ReasonResourceRemoved
		case to.name == "stopped":
			reason = ReasonResourceStopped
		case to.name == "ready":
			reason = ReasonResourceReady
		default:
			reason = ReasonResourceChanged
		}
	}
	a.Transitions = append(a.Transitions, Transition{
		Subject: SubjectResource, ID: r.Key, From: from.name, To: to.name, Reason: reason, At: now.UTC(),
		FromPhase: from.phase, ToPhase: to.phase,
	})
}

// ReasonResourceChanged is recorded on a resource transition that carries no
// other safe reason.
const ReasonResourceChanged Reason = "resource_changed"

// Reasons of resource transitions that carry no other safe reason.
const (
	ReasonResourceRemoved Reason = "resource_removed"
	ReasonResourceStopped Reason = "resource_stopped"
	ReasonResourceReady   Reason = "resource_ready"
	ReasonReceiptRecorded Reason = "receipt_recorded"
	ReasonEnsureRequested Reason = "ensure_requested"
)
