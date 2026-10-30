package runtime

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RunRecord is the authoritative durable record of one run: everything an
// engine needs to decide the next step after a restart, with no transient
// backend configuration. Steps keep their attempt and result, commands keep
// their workload binding and outcome, Workloads keep the latest observation of
// each attempt, and Waits keep their recorded decisions, so a restarted engine can
// reconcile uncertain effects by stable identity and evaluate conditions over
// prior results and wait outcomes. It round-trips through encoding/json.
type RunRecord struct {
	Run  Run  `json:"run"`
	Plan Plan `json:"plan"`
	// Claim is the current lease; the zero claim means the run was never claimed.
	Claim    Claim     `json:"claim"`
	Waits    []Wait    `json:"waits,omitempty"`
	Commands []Command `json:"commands,omitempty"`
	// Workloads hold the latest observation of each workload attempt.
	Workloads []WorkloadObservation `json:"workloads,omitempty"`
	Resources []ResourceObservation `json:"resources,omitempty"`
}

// Validate checks that the record is coherent as read back from a store: the run
// was built from the plan, every wait agrees with its step, every command,
// workload and resource belongs to this run and tenant, and no workload attempt
// is newer than its step's attempt.
func (r RunRecord) Validate() error {
	if err := r.Run.Validate(); err != nil {
		return err
	}
	if err := r.Plan.Validate(); err != nil {
		return err
	}
	if len(r.Run.Steps) != len(r.Plan.Steps) {
		return invalid("the run has %d steps, the plan %d", len(r.Run.Steps), len(r.Plan.Steps))
	}
	for i, s := range r.Run.Steps {
		if s.Key != r.Plan.Steps[i].Key || s.Kind != r.Plan.Steps[i].Kind {
			return invalid("run step %s does not match plan step %s", s.Key, r.Plan.Steps[i].Key)
		}
	}
	if r.Claim != (Claim{}) {
		if err := r.Claim.Validate(); err != nil {
			return err
		}
		if r.Claim.Scope != r.Run.Scope || r.Claim.RunID != r.Run.ID {
			return invalid("claim belongs to another run")
		}
	}
	step := func(k domain.StepID) (Step, bool) {
		i := slices.IndexFunc(r.Run.Steps, func(s Step) bool { return s.Key == k })
		if i < 0 {
			return Step{}, false
		}
		return r.Run.Steps[i], true
	}
	waits := map[domain.StepID]bool{}
	for _, w := range r.Waits {
		if err := w.Validate(); err != nil {
			return err
		}
		if w.Scope != r.Run.Scope || w.RunID != r.Run.ID || waits[w.StepKey] {
			return invalid("wait %s does not belong to this run exactly once", w.ID)
		}
		waits[w.StepKey] = true
		s, ok := step(w.StepKey)
		if !ok {
			return invalid("wait %s names an unknown step", w.ID)
		}
		if err := CheckStepWait(s, w); err != nil {
			return err
		}
	}
	for _, s := range r.Run.Steps {
		if s.Kind == StepWait && !waits[s.Key] {
			return invalid("wait step %s has no wait", s.Key)
		}
	}
	ids := map[string]bool{}
	for _, c := range r.Commands {
		if err := c.Validate(); err != nil {
			return err
		}
		if c.Scope != r.Run.Scope || c.RunID != r.Run.ID || ids[c.ID] {
			return invalid("command %s does not belong to this run exactly once", c.ID)
		}
		ids[c.ID] = true
		if _, ok := step(c.StepKey); c.StepKey != "" && !ok {
			return invalid("command %s names an unknown step", c.ID)
		}
	}
	type attempt struct {
		step domain.StepID
		n    int
	}
	seen := map[attempt]bool{}
	for _, w := range r.Workloads {
		if err := w.Validate(); err != nil {
			return err
		}
		b := w.Binding
		s, ok := step(b.StepKey)
		if b.Scope != r.Run.Scope || b.RunID != r.Run.ID || !ok || s.Kind != StepContainer || b.Attempt > s.Attempt ||
			seen[attempt{b.StepKey, b.Attempt}] {
			return invalid("workload %s does not match one launched attempt of this run", b.WorkloadID)
		}
		seen[attempt{b.StepKey, b.Attempt}] = true
	}
	for _, o := range r.Resources {
		if err := o.Validate(); err != nil {
			return err
		}
		if o.Handle.Scope != r.Run.Scope || o.Handle.Reachable(r.Run.Scope) != nil {
			return invalid("resource %s is not available to this run", o.Handle.Name)
		}
	}
	return nil
}

// RequirementsMet checks, from recorded state alone, that a protected step may
// start: every Requires entry names a wait step that succeeded with a resolved
// decision whose outcome is that wait's approving outcome. A rejection, any other
// declared outcome, expired, and a skipped, failed, cancelled or still-open wait
// never satisfy it, whatever the step's when or join says. An unprotected step
// has no requirements. The engine calls this regardless of the format's own
// validation.
func (r RunRecord) RequirementsMet(key domain.StepID) error {
	i := slices.IndexFunc(r.Plan.Steps, func(p PlanStep) bool { return p.Key == key })
	if i < 0 {
		return invalid("the plan has no step %s", key)
	}
	for _, req := range r.Plan.Steps[i].Requires {
		pi := slices.IndexFunc(r.Plan.Steps, func(p PlanStep) bool { return p.Key == req.Wait && p.Kind == StepWait })
		si := slices.IndexFunc(r.Run.Steps, func(s Step) bool { return s.Key == req.Wait })
		wi := slices.IndexFunc(r.Waits, func(w Wait) bool { return w.StepKey == req.Wait })
		if pi < 0 || si < 0 || wi < 0 {
			return fmt.Errorf("%w: step %s: no wait %s", ErrRequiresUnmet, key, req.Wait)
		}
		approving, ok := r.Plan.Steps[pi].Wait.ApprovingOutcome()
		w := r.Waits[wi]
		if !ok || req.Outcome != approving || r.Run.Steps[si].State != StepSucceeded ||
			w.State != WaitResolved || w.Decision.Kind != DecisionResolved || w.Decision.Outcome != approving {
			return fmt.Errorf("%w: step %s needs wait %s decided %q", ErrRequiresUnmet, key, req.Wait, approving)
		}
	}
	return nil
}

// StepResult returns the retained workload result of a settled container step,
// for conditions over prior step results.
func (r RunRecord) StepResult(key domain.StepID) (Result, bool) {
	for _, s := range r.Run.Steps {
		if s.Key == key && s.Result != (Result{}) {
			return s.Result, true
		}
	}
	return Result{}, false
}

// WaitOutcome returns the recorded outcome of a decided wait step that has one:
// a declared outcome, or expired when the wait continued after its deadline. It
// reads recorded state only, for when predicates over wait outcomes.
func (r RunRecord) WaitOutcome(key domain.StepID) (string, bool) {
	for _, w := range r.Waits {
		if w.StepKey == key && w.Decision.Outcome != "" {
			return w.Decision.Outcome, true
		}
	}
	return "", false
}

// Unresolved returns the commands whose outcome is not final: intents with no
// recorded outcome and uncertain ones. A restarted engine reconciles each by its
// stable identity and binding; it never replays them.
func (r RunRecord) Unresolved() []Command {
	var out []Command
	for _, c := range r.Commands {
		if c.State == CommandIntent || c.State == CommandUncertain {
			out = append(out, c)
		}
	}
	return out
}

// RunView is the read model rebuilt from retained run, step and wait records.
// The shell derives its task projection from it; it is never authoritative.
type RunView struct {
	Run    Run       `json:"run"`
	Waits  []Wait    `json:"waits,omitempty"`
	Status RunStatus `json:"status"`
}

// RunRef names one run that has work for a claimant.
type RunRef struct {
	Scope   domain.Scope `json:"scope"`
	RunID   domain.RunID `json:"runId"`
	Purpose ClaimPurpose `json:"purpose"`
}

// RunnableQuery lists runs with executable, reconcilable or cleanable work:
// unclaimed runs and runs whose lease expired by Now.
type RunnableQuery struct {
	Now   time.Time `json:"now"`
	Limit int       `json:"limit"`
}

// Admission is a run to record: the run, its immutable plan, one pending wait
// per wait step and the run.created event.
type Admission struct {
	Run    Run     `json:"run"`
	Plan   Plan    `json:"plan"`
	Waits  []Wait  `json:"waits,omitempty"`
	Events []Event `json:"events,omitempty"`
}

// Validate checks that the run was built from the plan: same steps in the same
// order and kinds, and exactly one pending wait for each wait step.
func (a Admission) Validate() error {
	if err := a.Run.Validate(); err != nil {
		return err
	}
	if err := a.Plan.Validate(); err != nil {
		return err
	}
	if len(a.Run.Steps) != len(a.Plan.Steps) {
		return invalid("the run has %d steps, the plan %d", len(a.Run.Steps), len(a.Plan.Steps))
	}
	waits := map[domain.StepID]Wait{}
	for _, w := range a.Waits {
		if err := w.Validate(); err != nil {
			return err
		}
		if w.Scope != a.Run.Scope || w.RunID != a.Run.ID || w.State != WaitPending {
			return invalid("wait %s must be pending and belong to the run", w.ID)
		}
		if _, dup := waits[w.StepKey]; dup {
			return invalid("step %s has more than one wait", w.StepKey)
		}
		waits[w.StepKey] = w
	}
	nWait := 0
	for i, s := range a.Run.Steps {
		p := a.Plan.Steps[i]
		if s.Key != p.Key || s.Kind != p.Kind || s.State != StepPending {
			return invalid("run step %s does not match plan step %s", s.Key, p.Key)
		}
		if p.Kind == StepWait {
			nWait++
			w, ok := waits[p.Key]
			if !ok {
				return invalid("wait step %s has no wait", p.Key)
			}
			if !w.Spec.Equal(*p.Wait) {
				return invalid("wait %s does not match its plan step", w.ID)
			}
		}
	}
	if nWait != len(waits) {
		return invalid("a wait names a step that is not a wait step")
	}
	for _, e := range a.Events {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.Scope != a.Run.Scope || e.RunID != a.Run.ID {
			return invalid("event %s belongs to another run", e.ID)
		}
	}
	return nil
}

// StateStore is the durable run state the engine depends on. Every method
// takes scope explicitly; an implementation must never return or change another
// tenant's or project's run. Methods report domain.ErrConflict, ErrStaleFence,
// ErrClaimHeld, ErrClaimExpired or a *domain.Error with domain.CodeVersionConflict
// rather than overwriting silently.
//
// Three methods (Admit, RequestCancel, DecideSignal) are the writes that arrive
// from outside the claim holder: admission, cancellation and wait resolution.
// They are atomic and idempotent without a claim and must never wait for a lease.
type StateStore interface {
	// Admit records a run, its plan, its pending waits and its run.created
	// event atomically. It is idempotent on (scope, run id): a repeat with the
	// same plan digest returns the existing record; a different digest is
	// domain.ErrConflict.
	Admit(ctx context.Context, a Admission) (RunRecord, error)
	// Load returns the run record, or a *domain.Error with domain.CodeNotFound
	// for a run that does not exist in that scope.
	Load(ctx context.Context, scope domain.Scope, id domain.RunID) (RunRecord, error)
	// Claim grants a lease through AcquireClaim, refusing a purpose the run does
	// not admit (Run.Admits) and any unexpired lease, including the caller's own
	// (renew it instead). After cleanup is exhausted only a ClaimCleanup is
	// admitted.
	Claim(ctx context.Context, req ClaimRequest, now time.Time) (RunRecord, error)
	// Renew extends the caller's lease atomically through RenewClaim: scope,
	// owner, fence and expiry are checked against the stored claim, the fence is
	// unchanged, and the new expiry is persisted before the stored claim is
	// returned. A writer superseded by a takeover gets ErrStaleFence.
	Renew(ctx context.Context, held Claim, ttl time.Duration, now time.Time) (Claim, error)
	// Commit applies a Transition atomically with its outbox intent: it checks
	// the claim's fence and lease (Claim.Authorizes) and the expected versions,
	// then persists the run, waits, commands, workload observations, resources
	// and events together, and extends the lease when Transition.RenewTTL is
	// set. A stale writer is refused with ErrStaleFence and nothing is persisted.
	Commit(ctx context.Context, t Transition) (RunRecord, error)
	// Runnable lists runs with claimable work, bounded by q.Limit.
	Runnable(ctx context.Context, q RunnableQuery) ([]RunRef, error)
	// RequestCancel records a cancellation request without a claim. It blocks
	// new execution claims and is idempotent. In the same transaction it decides
	// every pending and open wait of the run as cancelled.
	RequestCancel(ctx context.Context, scope domain.Scope, id domain.RunID) (Run, error)
	// LookupSignal returns the durable record or tombstone of a signal identity,
	// or the zero lookup when there is none. It is the first step of routing and
	// is independent of any wait, step or run still existing: records have no
	// expiry, ordinary wait and run retention never removes them, and the only
	// removal is deleting the project. There is deliberately no per-record delete.
	LookupSignal(ctx context.Context, id SignalIdentity) (SignalLookup, error)
	// DecideSignal routes and decides one authenticated signal in a single
	// transaction by applying RouteSignal against the identity's lookup and the
	// project's waits. It commits, together and atomically, the updated wait (a
	// resolution, or an expiry found at the deadline), the new decision record or
	// tombstone, the continuation intent and the outbox events (wait.resolved,
	// wait.expired or signal.refused). A replay of a recorded identity returns the
	// recorded result and writes nothing; a changed request under a recorded key
	// returns ErrKeyReuse and writes nothing. Resolution, expiry and cancellation
	// race on one version-checked decision against the store's one clock; the
	// first commit wins and no second continuation is created.
	DecideSignal(ctx context.Context, req SignalRequest, now time.Time) (SignalResult, error)
	// View rebuilds the read model from retained run, step and wait records.
	View(ctx context.Context, scope domain.Scope, id domain.RunID) (RunView, error)
}

// Materialization is the private, declared secret material for one step. It
// names a directory outside /workspace and the references it holds; it never
// carries a value.
type Materialization struct {
	Scope     domain.Scope  `json:"scope"`
	RunID     domain.RunID  `json:"runId"`
	StepKey   domain.StepID `json:"stepKey"`
	Directory string        `json:"directory"`
	Refs      []SecretRef   `json:"refs,omitempty"`
	// Revision identifies the resolved versions for rotation and audit.
	Revision string `json:"revision,omitempty"`
}

// ResolveRequest asks for the declared secret references of one step.
type ResolveRequest struct {
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey"`
	// Refs are the references the plan declared for the step; see SelectSecrets.
	Refs []SecretRef `json:"refs"`
}

// SecretResolver resolves only declared references into a private
// materialization. It must not log, return or put a secret value in an event;
// an unavailable reference fails with ReasonSecretUnavailable.
type SecretResolver interface {
	// Resolve materializes the references for one step attempt.
	Resolve(ctx context.Context, req ResolveRequest) (Materialization, error)
	// Release removes the materialization and revokes access to it. It is
	// idempotent and runs when the step settles, not only when the run ends.
	Release(ctx context.Context, m Materialization) error
}

// StartRequest launches one workload attempt.
type StartRequest struct {
	Binding   RuntimeBinding `json:"binding"`
	Spec      WorkloadSpec   `json:"spec"`
	Workspace ResourceHandle `json:"workspace"`
	// Services are the run's ready services, reachable by name.
	Services []ResourceHandle `json:"services,omitempty"`
	Secrets  Materialization  `json:"secrets"`
}

// LogOptions selects how a log stream is read.
type LogOptions struct {
	Follow bool `json:"follow,omitempty"`
}

// Runtime runs one workload attempt per binding. It is the only port that
// launches containers.
//
// Start is idempotent on Binding.WorkloadID: an existing workload with the same
// SpecDigest is adopted, one with a different digest fails with ErrSpecMismatch,
// and a create carrying a fence older than the one that created the workload is
// rejected with ErrStaleFence. An effect that did not reach the backend returns
// an error wrapping ErrNotReached; any other error is treated as uncertain.
type Runtime interface {
	// Capabilities reports what the backend enforces. It must not over-claim.
	Capabilities(ctx context.Context) (CapabilitySet, error)
	// Start launches or adopts the workload and returns its first observation.
	Start(ctx context.Context, req StartRequest) (WorkloadObservation, error)
	// Observe reports the workload by its stable identity.
	Observe(ctx context.Context, b RuntimeBinding) (WorkloadObservation, error)
	// Logs streams the workload's output.
	Logs(ctx context.Context, b RuntimeBinding, opts LogOptions) (io.ReadCloser, error)
	// Stop stops the workload; a stop from a stale fence is refused.
	Stop(ctx context.Context, b RuntimeBinding) (WorkloadObservation, error)
	// Remove removes the workload and confirms with PhaseAbsent.
	Remove(ctx context.Context, b RuntimeBinding) (WorkloadObservation, error)
}

// ResourceManager owns the lifecycle of workspaces and services: ensure,
// observe, stop and remove. It is the only port that creates or adopts a
// workspace; a WorkspacePreparer only supplies its inputs.
//
// Ensure is idempotent on (ResourceSpec.Owner, Identity) with the same adoption
// and fence rules as Runtime.Start. Every method refuses with ErrForeignResource
// a spec or handle whose Owner is not available to its Scope (another tenant,
// or another project's project-owned resource); a tenant-owned service is shared
// by every project of that tenant. Removal is confirmed only by
// ResourceRemoved; ResourceUnknown never counts as removed.
type ResourceManager interface {
	// Capabilities reports what the backend enforces. It must not over-claim.
	Capabilities(ctx context.Context) (CapabilitySet, error)
	// Ensure creates or adopts the resource and returns its observation.
	Ensure(ctx context.Context, spec ResourceSpec) (ResourceObservation, error)
	// Observe reports the resource by its stable identity.
	Observe(ctx context.Context, h ResourceHandle) (ResourceObservation, error)
	// Logs streams the resource's output; a workspace has none.
	Logs(ctx context.Context, h ResourceHandle, opts LogOptions) (io.ReadCloser, error)
	// Stop stops the resource; a stop from a stale fence is refused.
	Stop(ctx context.Context, h ResourceHandle) (ResourceObservation, error)
	// Remove removes the resource and confirms with ResourceRemoved.
	Remove(ctx context.Context, h ResourceHandle) (ResourceObservation, error)
}

// PrepareRequest asks for the pinned preparation inputs of one run.
type PrepareRequest struct {
	Scope domain.Scope `json:"scope"`
	RunID domain.RunID `json:"runId"`
	// PlanDigest identifies the admitted plan the inputs are for.
	PlanDigest string `json:"planDigest"`
	// Inputs are the plan's declared non-secret inputs.
	Inputs map[string]string `json:"inputs,omitempty"`
}

// WorkspacePreparer supplies the pinned inputs for preparing a run's workspace:
// which sources at which exact revisions go where, and which credential
// references fetch them. It never creates, adopts, mutates or removes a
// workspace; the engine passes the inputs to ResourceManager.Ensure, which owns
// that lifecycle. It is deterministic for one (run, plan digest): repeated calls
// return the same Digest so a restarted engine adopts rather than re-prepares.
type WorkspacePreparer interface {
	// Inputs returns the pinned preparation inputs, or an empty source set for
	// a source-free run.
	Inputs(ctx context.Context, req PrepareRequest) (PreparationInputs, error)
}

// LogSink retains bounded output through a shell-owned store. Append is
// idempotent on (scope, run, step, source, sequence).
type LogSink interface {
	// Append stores one validated chunk. Redaction has already happened; the
	// sink must not re-expose a secret value.
	Append(ctx context.Context, c LogChunk) error
}

// Clock supplies time to the engine and, through request fields, to the state
// store, so tests and recovery are deterministic.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}
