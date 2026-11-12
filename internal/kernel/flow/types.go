// Package flow declares the pure Kernel transition vocabulary: explicit facts
// in, one transition commit out.
//
// Nothing here performs I/O. The reducer that consumes these declarations is
// the pure lifecycle implementation; this file declares the facts, the
// output/stop/cleanup intents and the cancel reasons they use.
package flow

import (
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// FactKind names one external observation or command fed to the pure reducer.
type FactKind string

const (
	// FactActivate asks to activate the named action.
	FactActivate FactKind = "activate"
	// FactObserve reports runtime evidence about the current workload.
	FactObserve FactKind = "observe"
	// FactOutput reports submitted container output values.
	FactOutput FactKind = "output"
	// FactAnswer reports an authorized request decision.
	FactAnswer FactKind = "answer"
	// FactCancel reports an explicit cancellation.
	FactCancel FactKind = "cancel"
	// FactExpire reports that the authoritative time reached the deadline.
	FactExpire FactKind = "expire"
	// FactWorkspaceLost reports that the retained workspace disappeared.
	FactWorkspaceLost FactKind = "workspace_lost"
	// FactRetired reports that removal of the retiring workload identity was
	// confirmed, so the reducer may allocate its replacement identity.
	FactRetired FactKind = "retired"
)

// FactKinds returns every fact kind in declaration order.
func FactKinds() []FactKind {
	return []FactKind{
		FactActivate,
		FactObserve,
		FactOutput,
		FactAnswer,
		FactCancel,
		FactExpire,
		FactWorkspaceLost,
		FactRetired,
	}
}

// Valid reports whether the kind is one of the declared kinds.
func (k FactKind) Valid() bool {
	for _, candidate := range FactKinds() {
		if k == candidate {
			return true
		}
	}
	return false
}

// Fact is one explicit reducer input. Time is always carried explicitly; the
// reducer never reads a wall clock.
type Fact interface {
	Kind() FactKind
}

// ActivateFact asks to activate one pending action.
type ActivateFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	At          time.Time
}

// Kind implements Fact.
func (ActivateFact) Kind() FactKind { return FactActivate }

// ObserveFact reports runtime evidence about the current workload.
type ObserveFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	Observation kernel.Observation
	At          time.Time
}

// Kind implements Fact.
func (ObserveFact) Kind() FactKind { return FactObserve }

// OutputFact reports submitted container output values for the current action.
type OutputFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	Outputs     kernel.Outputs
	At          time.Time
}

// Kind implements Fact.
func (OutputFact) Kind() FactKind { return FactOutput }

// AnswerFact reports an authorized request decision. The Kernel does not
// authenticate the answerer; a Shell transaction validates the actor first.
type AnswerFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	Answer      kernel.Answer
	At          time.Time
}

// Kind implements Fact.
func (AnswerFact) Kind() FactKind { return FactAnswer }

// CancelReason names why an execution or pending action is cancelled.
type CancelReason string

const (
	// CancelRequested is an explicit operator or client cancellation.
	CancelRequested CancelReason = "requested"
	// CancelUpstreamFailed cancels unstarted actions after an earlier failure.
	CancelUpstreamFailed CancelReason = "upstream_failed"
)

// Valid reports whether the reason is one of the declared reasons.
func (r CancelReason) Valid() bool {
	return r == CancelRequested || r == CancelUpstreamFailed
}

// CancelFact requests cancellation. Pending actions are cancelled rather than
// skipped, and no new work starts after this transition commits.
type CancelFact struct {
	ExecutionID id.ExecutionID
	Reason      CancelReason
	At          time.Time
}

// Kind implements Fact.
func (CancelFact) Kind() FactKind { return FactCancel }

// ExpireFact reports that the authoritative time is at or past the persisted
// deadline. Equality expires; sender timestamps never decide timeliness.
type ExpireFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	At          time.Time
}

// Kind implements Fact.
func (ExpireFact) Kind() FactKind { return FactExpire }

// WorkspaceLostFact reports that the retained workspace disappeared. The
// execution fails visibly; scratch state is never reconstructed.
type WorkspaceLostFact struct {
	ExecutionID id.ExecutionID
	WorkspaceID id.WorkspaceID
	At          time.Time
}

// Kind implements Fact.
func (WorkspaceLostFact) Kind() FactKind { return FactWorkspaceLost }

// RetiredFact reports that removal of the retiring workload identity was
// confirmed. It is valid only for the running action whose launch state is
// `retiring` for exactly WorkloadID; the reducer then allocates the replacement
// identity. A retired fact is never produced from `absent`.
type RetiredFact struct {
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	WorkloadID  id.WorkloadID
	At          time.Time
}

// Kind implements Fact.
func (RetiredFact) Kind() FactKind { return FactRetired }

// OutputAction names the output change carried by one commit.
type OutputAction string

const (
	// OutputNone leaves outputs unchanged.
	OutputNone OutputAction = "none"
	// OutputStage stores submitted values as immutable staging. A nil Values
	// map is a submitted JSON null, distinct from never submitting.
	OutputStage OutputAction = "stage"
	// OutputPromote moves staged values into settled downstream outputs.
	OutputPromote OutputAction = "promote"
	// OutputDiscard clears staged values after failure, cancellation or timeout.
	OutputDiscard OutputAction = "discard"
)

// OutputActions returns every output action in declaration order.
func OutputActions() []OutputAction {
	return []OutputAction{OutputNone, OutputStage, OutputPromote, OutputDiscard}
}

// Valid reports whether the action is one of the declared actions.
func (a OutputAction) Valid() bool {
	return a == OutputNone || a == OutputStage || a == OutputPromote || a == OutputDiscard
}

// OutputChange is the output part of one transition commit. Replacements are
// refused; only staging, promotion or discard are representable.
type OutputChange struct {
	Action OutputAction   `json:"action"`
	Values kernel.Outputs `json:"values,omitempty"`
}

// StopIntent asks the runtime to terminate one stable workload identity. A stop
// acknowledgment is not confirmation; the reducer records evidence separately.
type StopIntent struct {
	WorkloadID  id.WorkloadID  `json:"workload_id"`
	WorkspaceID id.WorkspaceID `json:"workspace_id"`
}

// CleanupIntent asks to remove owned resources after the appropriate stop. The
// reason stays actionable for a bounded retry policy.
type CleanupIntent struct {
	WorkloadID  id.WorkloadID  `json:"workload_id"`
	WorkspaceID id.WorkspaceID `json:"workspace_id"`
	Reason      string         `json:"reason"`
}

// CleanupUpdate replaces the visible cleanup progress. It records the state,
// the owned resource identities, the attempt count and the last actionable
// error, so a crash between attempts stays attributable. It is progress, not a
// new cleanup intent.
type CleanupUpdate struct {
	State       kernel.CleanupState `json:"state"`
	WorkloadID  id.WorkloadID       `json:"workload_id,omitempty"`
	WorkspaceID id.WorkspaceID      `json:"workspace_id,omitempty"`
	Attempts    int                 `json:"attempts,omitempty"`
	LastError   string              `json:"last_error,omitempty"`
}

// Failure is the typed terminal diagnostic carried by a commit. It is the same
// durable value stored on the execution, so no evidence is dropped between the
// reducer and the state port.
type Failure = kernel.Failure

// ActionUpdate is one additional action transition applied atomically with the
// primary action update in the same commit. Pending successors are cancelled
// through it, so a failure or cancellation never leaves them pending.
type ActionUpdate struct {
	ActionID        id.ActionID        `json:"action_id"`
	ExpectedVersion uint64             `json:"expected_version"`
	NextState       kernel.ActionState `json:"next_state"`
}

// Commit is one complete compare-and-commit transition payload. The state port
// applies it atomically: expected versions, next states, output change,
// credential invalidation, stop/cleanup intents, immutable subject, persisted
// deadline, authoritative time, kind-specific evidence (answer, observation,
// stop evidence, launch intent), cleanup progress and the typed terminal
// failure travel together. The primary action transition may be accompanied by
// additional pending-action updates and a current-action index change, so a
// failure or cancellation settles the current action and cancels every pending
// successor in one commit. There is no split save, so every writable durable
// field is reachable in this one payload.
type Commit struct {
	ExecutionID              id.ExecutionID        `json:"execution_id"`
	ExpectedExecutionVersion uint64                `json:"expected_execution_version"`
	NextExecutionState       kernel.ExecutionState `json:"next_execution_state"`
	ActionID                 id.ActionID           `json:"action_id"`
	ExpectedActionVersion    uint64                `json:"expected_action_version"`
	NextActionState          kernel.ActionState    `json:"next_action_state"`
	// PendingCancellations are additional action updates applied atomically
	// with the primary action update.
	PendingCancellations []ActionUpdate `json:"pending_cancellations,omitempty"`
	// NextCurrentAction is the execution's active action index after this
	// commit. Nil leaves Execution.CurrentAction unchanged; -1 means no action
	// is active.
	NextCurrentAction    *int         `json:"next_current_action,omitempty"`
	Output               OutputChange `json:"output"`
	InvalidateCredential bool         `json:"invalidate_credential"`
	Stop                 *StopIntent  `json:"stop,omitempty"`
	// StopEvidence is the observed stop barrier. Stop remains the request; a
	// request is never treated as confirmation.
	StopEvidence *kernel.StopEvidence `json:"stop_evidence,omitempty"`
	Cleanup      *CleanupIntent       `json:"cleanup,omitempty"`
	// CleanupUpdate records pending/complete progress and retry attribution.
	CleanupUpdate *CleanupUpdate      `json:"cleanup_update,omitempty"`
	Subject       *definition.Subject `json:"subject,omitempty"`
	// Answer is the authorized request settlement, retained even when failed.
	Answer *kernel.Answer `json:"answer,omitempty"`
	// Observation is the latest runtime evidence about the container workload.
	Observation *kernel.Observation `json:"observation,omitempty"`
	// Launch is the persisted launch intent written at activation and updated
	// as the start attempt progresses. It also carries the `retiring` marker
	// (with the never-started proof in Observation) committed before removal,
	// and the replacement identity committed only after removal is confirmed.
	Launch   *kernel.LaunchIntent `json:"launch,omitempty"`
	Failure  *Failure             `json:"failure,omitempty"`
	Deadline time.Time            `json:"deadline"`
	At       time.Time            `json:"at"`
}
