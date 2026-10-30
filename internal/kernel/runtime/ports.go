package runtime

import (
	"context"
	"io"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// RunRecord is the authoritative durable record of one run: everything an
// engine needs to decide the next step after a restart.
type RunRecord struct {
	Run  Run  `json:"run"`
	Plan Plan `json:"plan"`
	// Claim is the current lease; the zero claim means the run was never claimed.
	Claim     Claim                 `json:"claim"`
	Waits     []Wait                `json:"waits,omitempty"`
	Commands  []Command             `json:"commands,omitempty"`
	Resources []ResourceObservation `json:"resources,omitempty"`
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
			if w.Spec != *p.Wait {
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
// Three methods (Admit, RequestCancel, ConsumeWait) are the writes that arrive
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
	// not admit (Run.Admits) and an unexpired lease held by another owner.
	Claim(ctx context.Context, req ClaimRequest, now time.Time) (RunRecord, error)
	// Commit applies a Transition atomically with its outbox intent: it checks
	// the claim's fence and lease (Claim.Authorizes) and the expected versions,
	// then persists the run, waits, commands, resources and events together. A
	// stale writer is refused with ErrStaleFence and nothing is persisted.
	Commit(ctx context.Context, t Transition) (RunRecord, error)
	// Runnable lists runs with claimable work, bounded by q.Limit.
	Runnable(ctx context.Context, q RunnableQuery) ([]RunRef, error)
	// RequestCancel records a cancellation request without a claim. It blocks
	// new execution claims and is idempotent.
	RequestCancel(ctx context.Context, scope domain.Scope, id domain.RunID) (Run, error)
	// ConsumeWait applies a signal as one version-checked decision. A duplicate
	// of the consuming signal returns the recorded result; every other signal
	// for a closed wait is refused with the Wait.Consume errors. The consumption
	// and its wait.resolved event commit together.
	ConsumeWait(ctx context.Context, sig Signal, now time.Time) (Consumption, error)
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

// ResourceManager owns the lifecycle of run-owned workspaces and services:
// ensure, observe, stop and remove. It is the only port that creates or adopts
// a workspace; a WorkspacePreparer only supplies its inputs.
//
// Ensure is idempotent on ResourceSpec.Identity with the same adoption and
// fence rules as Runtime.Start. Removal is confirmed only by ResourceRemoved;
// ResourceUnknown never counts as removed.
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
