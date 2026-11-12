// Package kernel defines the durable execution vocabulary shared by the pure
// transition logic, the reconciliation engine and the Shell adapters.
//
// Public states are deliberately small: preparation, launch uncertainty, stop
// observation and cleanup are separate evidence values, not extra states. An
// action is active only when it is the execution's current action, and terminal
// outcomes never change.
package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// ExecutionState is one of the five durable execution states.
type ExecutionState string

const (
	// ExecutionQueued is admitted but not activated.
	ExecutionQueued ExecutionState = "queued"
	// ExecutionRunning has an active or pending action.
	ExecutionRunning ExecutionState = "running"
	// ExecutionSucceeded is terminal after every action succeeded.
	ExecutionSucceeded ExecutionState = "succeeded"
	// ExecutionFailed is terminal after an action failed.
	ExecutionFailed ExecutionState = "failed"
	// ExecutionCancelled is terminal after explicit cancellation.
	ExecutionCancelled ExecutionState = "cancelled"
)

var executionStates = [...]ExecutionState{
	ExecutionQueued,
	ExecutionRunning,
	ExecutionSucceeded,
	ExecutionFailed,
	ExecutionCancelled,
}

// ExecutionStates returns every execution state in declaration order.
func ExecutionStates() []ExecutionState {
	return append([]ExecutionState(nil), executionStates[:]...)
}

// Valid reports whether the state is one of the declared states.
func (s ExecutionState) Valid() bool {
	for _, candidate := range executionStates {
		if s == candidate {
			return true
		}
	}
	return false
}

// Terminal reports whether the state is terminal and therefore immutable.
func (s ExecutionState) Terminal() bool {
	switch s {
	case ExecutionSucceeded, ExecutionFailed, ExecutionCancelled:
		return true
	default:
		return false
	}
}

// ActionState is one of the five durable action states.
type ActionState string

const (
	// ActionPending is admitted but not activated.
	ActionPending ActionState = "pending"
	// ActionRunning is the execution's single active action.
	ActionRunning ActionState = "running"
	// ActionSucceeded is terminal success.
	ActionSucceeded ActionState = "succeeded"
	// ActionFailed is terminal failure.
	ActionFailed ActionState = "failed"
	// ActionCancelled is terminal cancellation.
	ActionCancelled ActionState = "cancelled"
)

var actionStates = [...]ActionState{
	ActionPending,
	ActionRunning,
	ActionSucceeded,
	ActionFailed,
	ActionCancelled,
}

// ActionStates returns every action state in declaration order.
func ActionStates() []ActionState {
	return append([]ActionState(nil), actionStates[:]...)
}

// Valid reports whether the state is one of the declared states.
func (s ActionState) Valid() bool {
	for _, candidate := range actionStates {
		if s == candidate {
			return true
		}
	}
	return false
}

// Terminal reports whether the state is terminal and therefore immutable.
func (s ActionState) Terminal() bool {
	switch s {
	case ActionSucceeded, ActionFailed, ActionCancelled:
		return true
	default:
		return false
	}
}

// Decision is the authorized answer to a request action.
type Decision string

const (
	// DecisionPass succeeds the request action.
	DecisionPass Decision = "pass"
	// DecisionFail fails the request action.
	DecisionFail Decision = "fail"
)

// Valid reports whether the decision is one of the declared decisions.
func (d Decision) Valid() bool {
	return d == DecisionPass || d == DecisionFail
}

// StopState distinguishes confirmed termination from a still-pending barrier.
type StopState string

const (
	// StopStateConfirmed is affirmative evidence that the workload stopped.
	StopStateConfirmed StopState = "confirmed"
	// StopStateUnconfirmed means the stop barrier is still pending. It is not
	// evidence of execution, of nonexecution or of termination.
	StopStateUnconfirmed StopState = "unconfirmed"
)

// StopStates returns every stop state in declaration order.
func StopStates() []StopState {
	return []StopState{StopStateConfirmed, StopStateUnconfirmed}
}

// Valid reports whether the stop state is one of the declared states.
func (s StopState) Valid() bool {
	return s == StopStateConfirmed || s == StopStateUnconfirmed
}

// StopEvidence records the stop barrier for one workload. Acknowledging a stop
// request is not confirmation; only affirmative termination evidence is.
type StopEvidence struct {
	State  StopState `json:"state"`
	Detail string    `json:"detail,omitempty"`
}

// ConfirmedStop records affirmative termination evidence.
func ConfirmedStop(detail string) StopEvidence {
	return StopEvidence{State: StopStateConfirmed, Detail: detail}
}

// PendingStop records an unresolved stop barrier.
func PendingStop(detail string) StopEvidence {
	return StopEvidence{State: StopStateUnconfirmed, Detail: detail}
}

// IsConfirmed reports whether termination is affirmatively confirmed.
func (s StopEvidence) IsConfirmed() bool {
	return s.State == StopStateConfirmed
}

// ObservationKind is the runtime evidence vocabulary for one workload identity.
type ObservationKind string

const (
	// ObservationUnknown means the runtime could not report anything usable.
	ObservationUnknown ObservationKind = "unknown"
	// ObservationAbsent means no object was found. Absence alone is neither
	// proof of nonexecution nor proof of stop.
	ObservationAbsent ObservationKind = "absent"
	// ObservationNeverStarted is affirmative proof that the command never ran.
	ObservationNeverStarted ObservationKind = "never_started"
	// ObservationRunning means the workload is still executing.
	ObservationRunning ObservationKind = "running"
	// ObservationExited means the workload exited with a status. Stop is a
	// separate barrier reported by Stop.
	ObservationExited ObservationKind = "exited"
)

var observationKinds = [...]ObservationKind{
	ObservationUnknown,
	ObservationAbsent,
	ObservationNeverStarted,
	ObservationRunning,
	ObservationExited,
}

// ObservationKinds returns every observation kind in declaration order.
func ObservationKinds() []ObservationKind {
	return append([]ObservationKind(nil), observationKinds[:]...)
}

// Valid reports whether the kind is one of the declared kinds.
func (k ObservationKind) Valid() bool {
	for _, candidate := range observationKinds {
		if k == candidate {
			return true
		}
	}
	return false
}

// Observation is evidence about one stable workload identity. It never
// collapses absence, unknown outcome and affirmative proof into a boolean.
type Observation struct {
	Kind     ObservationKind
	Proof    string
	ExitCode *int
	Stop     *StopEvidence
}

// UnknownObservation reports that the runtime could not classify the workload.
func UnknownObservation() Observation {
	return Observation{Kind: ObservationUnknown}
}

// AbsentObservation reports that the runtime found no object. It is not proof
// of nonexecution or of stop.
func AbsentObservation() Observation {
	return Observation{Kind: ObservationAbsent}
}

// NeverStarted reports affirmative proof that the command never executed. The
// proof is required: an empty proof is not evidence.
func NeverStarted(proof string) Observation {
	return Observation{Kind: ObservationNeverStarted, Proof: proof}
}

// RunningObservation reports a workload that is still executing.
func RunningObservation() Observation {
	return Observation{Kind: ObservationRunning}
}

// ExitedObservation reports an exit status plus the separate stop barrier.
func ExitedObservation(exitCode int, stop StopEvidence) Observation {
	code := exitCode
	return Observation{Kind: ObservationExited, ExitCode: &code, Stop: &stop}
}

// ExitStatus returns the exit status only for an exited observation.
func (o Observation) ExitStatus() (int, bool) {
	if o.Kind != ObservationExited || o.ExitCode == nil {
		return 0, false
	}
	return *o.ExitCode, true
}

// ProofOfNonexecution reports affirmative proof that the command never ran.
// Only a never-started observation with a non-empty proof qualifies.
func (o Observation) ProofOfNonexecution() bool {
	return o.Kind == ObservationNeverStarted && strings.TrimSpace(o.Proof) != ""
}

// StopConfirmed reports affirmative termination evidence. An exited workload
// whose stop barrier is still pending is not confirmed.
func (o Observation) StopConfirmed() bool {
	return o.Kind == ObservationExited && o.Stop != nil && o.Stop.IsConfirmed()
}

// Valid reports whether the observation is internally consistent.
func (o Observation) Valid() bool {
	switch o.Kind {
	case ObservationUnknown, ObservationAbsent, ObservationRunning:
		return o.Proof == "" && o.ExitCode == nil && o.Stop == nil
	case ObservationNeverStarted:
		return strings.TrimSpace(o.Proof) != "" && o.ExitCode == nil && o.Stop == nil
	case ObservationExited:
		return o.ExitCode != nil && o.Stop != nil && o.Stop.State.Valid()
	default:
		return false
	}
}

// MarshalJSON emits the exact observation shape for the current kind.
func (o Observation) MarshalJSON() ([]byte, error) {
	switch o.Kind {
	case ObservationUnknown, ObservationAbsent, ObservationRunning:
		return json.Marshal(struct {
			Kind ObservationKind `json:"kind"`
		}{o.Kind})
	case ObservationNeverStarted:
		if strings.TrimSpace(o.Proof) == "" {
			return nil, fmt.Errorf("kernel: never-started observation requires proof")
		}
		return json.Marshal(struct {
			Kind  ObservationKind `json:"kind"`
			Proof string          `json:"proof"`
		}{o.Kind, o.Proof})
	case ObservationExited:
		if o.ExitCode == nil || o.Stop == nil || !o.Stop.State.Valid() {
			return nil, fmt.Errorf("kernel: exited observation requires exit status and valid stop evidence")
		}
		return json.Marshal(struct {
			Kind     ObservationKind `json:"kind"`
			ExitCode int             `json:"exit_code"`
			Stop     StopEvidence    `json:"stop"`
		}{o.Kind, *o.ExitCode, *o.Stop})
	default:
		return nil, fmt.Errorf("kernel: unknown observation kind %q", o.Kind)
	}
}

// UnmarshalJSON decodes one observation and rejects collapsed uncertainty.
func (o *Observation) UnmarshalJSON(data []byte) error {
	var raw struct {
		Kind     ObservationKind `json:"kind"`
		Proof    string          `json:"proof"`
		ExitCode *int            `json:"exit_code"`
		Stop     *StopEvidence   `json:"stop"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("kernel: decode observation: %w", err)
	}
	observation := Observation{
		Kind:     raw.Kind,
		Proof:    raw.Proof,
		ExitCode: raw.ExitCode,
		Stop:     raw.Stop,
	}
	if !observation.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("observation kind %q is inconsistent", raw.Kind))
	}
	*o = observation
	return nil
}

// Output bounds from the output contract.
const (
	// MaxOutputValues bounds one nullable output object.
	MaxOutputValues = 8
	// MaxOutputValueBytes bounds each declared UTF-8 output value in bytes.
	MaxOutputValueBytes = 4096
)

// Outputs is one nullable JSON object of declared string values. A nil map is
// JSON null; an empty non-nil map is JSON {}. Values are staged until a
// runtime-confirmed successful settlement promotes them.
type Outputs map[id.OutputKey]string

// Validate checks the output bounds against the declared keys.
func (o Outputs) Validate(declared []id.OutputKey) error {
	if len(o) == 0 {
		return nil
	}
	if len(o) > MaxOutputValues {
		return fault.New(fault.InvalidInput, fmt.Sprintf("outputs contain %d values; at most %d are allowed", len(o), MaxOutputValues))
	}
	declaredKeys := make(map[id.OutputKey]bool, len(declared))
	for _, key := range declared {
		declaredKeys[key] = true
	}
	keys := make([]string, 0, len(o))
	for key := range o {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, rawKey := range keys {
		key := id.OutputKey(rawKey)
		value := o[key]
		if !declaredKeys[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("output %q is not declared", key))
		}
		if !utf8.ValidString(value) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("output %q is not valid UTF-8", key))
		}
		if len(value) > MaxOutputValueBytes {
			return fault.New(fault.InvalidInput, fmt.Sprintf("output %q is %d bytes; at most %d are allowed", key, len(value), MaxOutputValueBytes))
		}
	}
	return nil
}

// UnmarshalJSON decodes a nullable object of string values only. JSON null
// decodes to a nil map; {} decodes to a non-nil empty map.
func (o *Outputs) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*o = nil
		return nil
	}
	var raw map[id.OutputKey]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("kernel: decode outputs: %w", err)
	}
	values := make(Outputs, len(raw))
	for key, message := range raw {
		if bytes.Equal(bytes.TrimSpace(message), []byte("null")) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("output %q is null; values must be strings", key))
		}
		var value string
		if err := json.Unmarshal(message, &value); err != nil {
			return fault.New(fault.InvalidInput, fmt.Sprintf("output %q is not a string", key))
		}
		values[key] = value
	}
	*o = values
	return nil
}

// Answer is the authorized settlement of one request action. Actor, time and
// provenance are recorded separately from workload-controlled output strings.
// A failed answer stays inspectable but never becomes downstream-eligible.
type Answer struct {
	Decision   Decision       `json:"decision"`
	Outputs    Outputs        `json:"outputs,omitempty"`
	Actor      id.PrincipalID `json:"actor"`
	At         time.Time      `json:"at"`
	Provenance []byte         `json:"provenance,omitempty"`
}

// DownstreamEligible reports whether the decision may feed later actions.
// Only a passing decision is eligible; the action state check belongs to the
// transition logic.
func (a Answer) DownstreamEligible() bool {
	return a.Decision == DecisionPass
}

// LaunchState is the persisted launch-attempt marker of one stable workload
// identity. It separates "no start was ever requested" from "a start was
// requested and may have happened", from "the start outcome is unknown", and
// from "the identity was affirmatively proven never started and is being
// retired before a replacement is committed".
type LaunchState string

const (
	// LaunchNotIssued means activation persisted the workload identity but no
	// start request has been recorded yet.
	LaunchNotIssued LaunchState = "not_issued"
	// LaunchIssued means a start request was issued. The identity is now
	// effect-owning and must be observed rather than relaunched.
	LaunchIssued LaunchState = "issued"
	// LaunchUnknown means a start was requested but its outcome cannot be
	// classified. It is never collapsed into proven-not-started.
	LaunchUnknown LaunchState = "unknown"
	// LaunchRetiring means the identity is affirmatively proven never started and
	// is being removed before a replacement is committed. The never-started
	// Observation carrying the non-empty proof is persisted in
	// ContainerAction.Observation in the same commit that records this state.
	// Its own workload resources are owed removal; the identity is never
	// prepared, started, issued or observed for a decision again, and no
	// replacement identity exists yet.
	LaunchRetiring LaunchState = "retiring"
)

var launchStates = [...]LaunchState{
	LaunchNotIssued,
	LaunchIssued,
	LaunchUnknown,
	LaunchRetiring,
}

// LaunchStates returns every launch state in declaration order.
func LaunchStates() []LaunchState {
	return append([]LaunchState(nil), launchStates[:]...)
}

// Valid reports whether the state is one of the declared launch states.
func (s LaunchState) Valid() bool {
	for _, candidate := range launchStates {
		if s == candidate {
			return true
		}
	}
	return false
}

// LaunchIntent is the durable launch intent of one container action. The
// workload identity and idempotency key are allocated once at activation and
// persisted before any start request, so every later consumer observes the same
// effect identity instead of deriving a different one. A fresh command attempt
// requires a new identity; the key keeps a retried start attributable.
// `not_issued` and `unknown` never prove nonexecution, and a retiring identity
// is replaced only after its removal is confirmed.
type LaunchIntent struct {
	WorkloadID     id.WorkloadID `json:"workload_id"`
	State          LaunchState   `json:"state"`
	IdempotencyKey string        `json:"idempotency_key,omitempty"`
}

// ContainerAction is the kind-specific durable state of a container action.
type ContainerAction struct {
	// Definition is the admitted declaration with a pinned image.
	Definition definition.Container
	// WorkloadID is the stable effect identity. It is never reused for a fresh
	// command attempt.
	WorkloadID id.WorkloadID
	// WorkspaceID is the execution's retained writable workspace.
	WorkspaceID id.WorkspaceID
	// Launch is the persisted launch intent, written before any start request.
	Launch LaunchIntent
	// Observation is the latest evidence about WorkloadID.
	Observation Observation
}

// RequestAction is the kind-specific durable state of a request action.
type RequestAction struct {
	// Definition is the admitted declaration.
	Definition definition.Request
	// Subject is the immutable resolved subject, set at activation. A nil
	// subject means the request is not answerable yet.
	Subject *definition.Subject
	// Answer is the authorized settlement, retained even when it failed.
	Answer *Answer
}

// Action is one durable action aggregate.
type Action struct {
	ID    id.ActionID
	Index int
	Name  id.ActionName
	Kind  definition.ActionKind
	State ActionState
	// Version is the optimistic concurrency version of this action row.
	Version uint64
	// Timeout is the resolved finite timeout. Deadline is persisted at
	// activation and never recalculated on restart.
	Timeout     time.Duration
	ActivatedAt time.Time
	Deadline    time.Time
	// Outputs are settled values visible to later actions.
	Outputs Outputs
	// StagedOutputs are submitted container values awaiting a
	// runtime-confirmed successful settlement. A nil map is a submitted JSON
	// null, not "never submitted"; StagedOutputsSet carries that distinction.
	StagedOutputs Outputs
	// StagedOutputsSet records whether an output submission happened. A nil
	// StagedOutputs map alone cannot distinguish "never submitted" from a
	// submitted null, so the presence marker is explicit.
	StagedOutputsSet bool `json:"staged_outputs_set"`
	// Stop is the stop barrier for this action's workload.
	Stop StopEvidence
	// Container and Request carry exactly one kind-specific value.
	Container *ContainerAction
	Request   *RequestAction
}

// CleanupState is the visible cleanup status, separate from the terminal
// execution outcome.
type CleanupState string

const (
	// CleanupNone means no external resource requires cleanup.
	CleanupNone CleanupState = "none"
	// CleanupPending means cleanup is still owed. The execution outcome is
	// already immutable.
	CleanupPending CleanupState = "pending"
	// CleanupComplete means the owned resources were removed.
	CleanupComplete CleanupState = "complete"
)

// Valid reports whether the cleanup state is one of the declared states.
func (s CleanupState) Valid() bool {
	return s == CleanupNone || s == CleanupPending || s == CleanupComplete
}

// Cleanup tracks owned resource identities, attempts and the last actionable
// error so pending cleanup is attributable after a crash.
type Cleanup struct {
	State       CleanupState   `json:"state"`
	WorkloadID  id.WorkloadID  `json:"workload_id,omitempty"`
	WorkspaceID id.WorkspaceID `json:"workspace_id,omitempty"`
	Attempts    int            `json:"attempts,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
}

// IsPending reports whether cleanup is still owed.
func (c Cleanup) IsPending() bool {
	return c.State == CleanupPending
}

// FailureKind is the typed cause of one terminal action/execution failure.
type FailureKind string

const (
	// FailureTimeout means the persisted deadline was reached.
	FailureTimeout FailureKind = "timeout"
	// FailureCancelled means an explicit cancellation settled the work.
	FailureCancelled FailureKind = "cancelled"
	// FailurePreparation means activation materialization failed before any
	// workload or request became usable.
	FailurePreparation FailureKind = "preparation"
	// FailureWorkspaceLost means the retained workspace disappeared; scratch
	// state is never reconstructed.
	FailureWorkspaceLost FailureKind = "workspace_lost"
	// FailureUpstreamFailed means an earlier action failed and this work was
	// cancelled rather than skipped.
	FailureUpstreamFailed FailureKind = "upstream_failed"
	// FailureRuntime means the runtime reported a terminal workload failure.
	FailureRuntime FailureKind = "runtime"
)

var failureKinds = [...]FailureKind{
	FailureTimeout,
	FailureCancelled,
	FailurePreparation,
	FailureWorkspaceLost,
	FailureUpstreamFailed,
	FailureRuntime,
}

// FailureKinds returns every failure kind in declaration order.
func FailureKinds() []FailureKind {
	return append([]FailureKind(nil), failureKinds[:]...)
}

// Valid reports whether the kind is one of the declared failure kinds.
func (k FailureKind) Valid() bool {
	for _, candidate := range failureKinds {
		if k == candidate {
			return true
		}
	}
	return false
}

// Failure is the typed terminal diagnostic of a failed action or execution.
// Reason is a bounded machine-readable code; Detail is operator-facing text.
// Neither replaces the retained stop/observation/cleanup evidence.
type Failure struct {
	Kind   FailureKind `json:"kind"`
	Reason string      `json:"reason,omitempty"`
	Detail string      `json:"detail,omitempty"`
}

// Execution is one durable execution aggregate.
type Execution struct {
	ID id.ExecutionID
	// Version is the optimistic concurrency version of this execution row.
	Version uint64
	State   ExecutionState
	// WorkspaceID is the retained disposable workspace identity.
	WorkspaceID id.WorkspaceID
	// Snapshot is the immutable admitted definition/input data.
	Snapshot *definition.Snapshot
	// Actions are in declaration order; at most one is active.
	Actions []Action
	// CurrentAction indexes Actions. It is -1 when no action is active.
	CurrentAction int
	// Failure is the typed terminal diagnostic. It is nil while the execution
	// has not failed; a failed execution always retains its cause.
	Failure *Failure
	// Cleanup is independent of State.
	Cleanup Cleanup
}
