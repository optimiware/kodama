// Package engine declares the consumer-owned ports and evidence values of the
// durable Kernel reconciliation engine.
//
// The engine loads an execution, observes external effects, reduces one
// explicit fact and compare-and-commits the complete transition payload before
// performing effects. It owns no persistence, identity or transport.
package engine

import (
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// ScanKind selects the due-work or unfinished-work recovery scan.
type ScanKind string

const (
	// ScanDue returns executions whose persisted deadline is due at Now.
	ScanDue ScanKind = "due"
	// ScanUnfinished returns executions with outstanding intents or cleanup.
	ScanUnfinished ScanKind = "unfinished"
)

// ScanKinds returns every scan kind in declaration order.
func ScanKinds() []ScanKind {
	return []ScanKind{ScanDue, ScanUnfinished}
}

// Valid reports whether the kind is one of the declared kinds.
func (k ScanKind) Valid() bool {
	return k == ScanDue || k == ScanUnfinished
}

// ScanRequest bounds one recovery scan. Now is the authoritative time.
type ScanRequest struct {
	Kind  ScanKind
	Now   time.Time
	Limit int
}

// EffectIdentity is the stable identity of one external effect. WorkloadID is
// never reused for a fresh command attempt; IdempotencyKey keeps retries
// attributable across a lost response. Both are allocated and persisted with
// kernel.LaunchIntent at activation, so adapters consume that identity instead
// of deriving a different one.
type EffectIdentity struct {
	ExecutionID    id.ExecutionID
	ActionID       id.ActionID
	WorkloadID     id.WorkloadID
	IdempotencyKey string
}

// Capabilities is the explicit capability set of one runtime adapter. Admitted
// requirements must be a subset before dispatch.
type Capabilities struct {
	Names []string
}

// Supports reports whether the adapter declares the named capability.
func (c Capabilities) Supports(name string) bool {
	for _, candidate := range c.Names {
		if candidate == name {
			return true
		}
	}
	return false
}

// SecretValue is one resolved activation-time secret. Values are materialized
// read-only and never logged, published or stored in task metadata.
type SecretValue struct {
	Name  id.SecretName
	Value string
}

// PrepareRequest materializes activation data once per workload and attaches
// the execution's existing workspace. Preparation failure prevents start.
type PrepareRequest struct {
	Identity    EffectIdentity
	WorkspaceID id.WorkspaceID
	// ActionName is the admitted action slug supplied as KODAMA_ACTION. It is
	// deliberately not the action id.
	ActionName id.ActionName
	// TaskID is the admitted task identity supplied as KODAMA_TASK. It is empty
	// when the execution is task-free, in which case the adapter omits it.
	TaskID id.TaskID
	// Container is the admitted declaration with a pinned image and the
	// pipeline-level environment defaults merged under the action environment
	// (action precedence, defaults first in declaration order).
	Container definition.Container
	// Secrets are resolved activation-time values for the declared names.
	Secrets []SecretValue
	// Inputs are the admitted input values.
	Inputs map[id.InputKey]string
	// Outputs are the settled outputs of earlier succeeded actions only.
	Outputs map[id.ActionName]kernel.Outputs
	// Task is the opaque admitted task snapshot; nil when task-free.
	Task []byte
	// Credential is the current execution credential material. It is never
	// logged.
	Credential string
}

// Preparation is the evidence that one workload was materialized.
type Preparation struct {
	WorkspaceID id.WorkspaceID
	Detail      string
}

// StartOutcome distinguishes an accepted start from affirmative nonexecution
// and from an unknown outcome.
type StartOutcome string

const (
	// StartAccepted means the runtime accepted the workload.
	StartAccepted StartOutcome = "accepted"
	// StartProvenNotStarted means affirmative evidence the command never ran.
	StartProvenNotStarted StartOutcome = "proven_not_started"
	// StartUnknown means the start outcome cannot be classified. It is never
	// collapsed into proven-not-started and never blindly replayed.
	StartUnknown StartOutcome = "unknown"
)

// StartOutcomes returns every start outcome in declaration order.
func StartOutcomes() []StartOutcome {
	return []StartOutcome{StartAccepted, StartProvenNotStarted, StartUnknown}
}

// Valid reports whether the outcome is one of the declared outcomes.
func (o StartOutcome) Valid() bool {
	return o == StartAccepted || o == StartProvenNotStarted || o == StartUnknown
}

// StartResult is the structured result of one start attempt.
type StartResult struct {
	Outcome    StartOutcome
	WorkloadID id.WorkloadID
	Detail     string
}

// StartRequest starts the workload prepared for one stable identity.
type StartRequest struct {
	Identity    EffectIdentity
	WorkspaceID id.WorkspaceID
}

// ObserveRequest asks for evidence about one stable workload identity.
type ObserveRequest struct {
	Identity    EffectIdentity
	WorkspaceID id.WorkspaceID
}

// LogStream identifies one ordered runtime log stream.
type LogStream string

const (
	// LogStdout is the workload's standard output.
	LogStdout LogStream = "stdout"
	// LogStderr is the workload's standard error.
	LogStderr LogStream = "stderr"
)

// LogChunk is one bounded, identified runtime log chunk.
type LogChunk struct {
	Stream    LogStream
	Offset    int64
	Sequence  int64
	Data      []byte
	Truncated bool
}

// LogBatch is one bounded ordered read. NextOffset continues the read.
type LogBatch struct {
	Chunks     []LogChunk
	NextOffset int64
}

// LogsRequest reads ordered chunks after AfterOffset. It carries no lifecycle
// authority.
type LogsRequest struct {
	Identity    EffectIdentity
	AfterOffset int64
	Limit       int
}

// StopRequest asks the runtime to terminate one stable workload identity.
type StopRequest struct {
	Identity    EffectIdentity
	WorkspaceID id.WorkspaceID
}

// RemoveRequest asks for idempotent cleanup of one workload's resources.
type RemoveRequest struct {
	Identity    EffectIdentity
	WorkspaceID id.WorkspaceID
}

// Removal is the evidence that owned resources were removed.
type Removal struct {
	Removed bool
	Detail  string
}

// CredentialRequest replaces token material without replaying or restarting
// the command.
type CredentialRequest struct {
	Identity   EffectIdentity
	Credential string
}

// CredentialRefresh is the evidence of one credential delivery attempt.
type CredentialRefresh struct {
	Delivered bool
	Detail    string
}

// WorkspaceObservation is the availability evidence for one retained
// workspace. A missing workspace is a failure, not permission to recreate it.
type WorkspaceObservation struct {
	WorkspaceID id.WorkspaceID
	Available   bool
	Detail      string
}
