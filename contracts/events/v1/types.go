package eventsv1

import (
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Contract constants.
const (
	// SchemaVersion is the only envelope version this package supports.
	SchemaVersion = 1

	// MaxEnvelopeBytes bounds a whole encoded envelope; MaxDataBytes bounds its
	// data member. Events carry references, not content.
	MaxEnvelopeBytes = 64 << 10
	MaxDataBytes     = 32 << 10

	// MaxDepth bounds JSON nesting.
	MaxDepth = 16

	// MaxSafeInteger is the largest integer accepted anywhere in an event
	// (2^53-1, so every consumer language represents it exactly).
	MaxSafeInteger = 1<<53 - 1
)

// ActorType identifies who caused an event.
type ActorType string

// Supported actor types. Automated actors keep the initiating reference in the
// causation/correlation members where applicable.
const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is the principal that caused the committed transition.
type Actor struct {
	Type ActorType `json:"type"`
	ID   string    `json:"id"`
}

// Resource is the aggregate observation an event describes. Version is the
// committed aggregate version; several events may share one version.
type Resource struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Envelope is the catalog envelope. ID is globally unique and stable across
// publication retries. ProjectID is empty for tenant-wide events and omitted on
// the wire. CausationID is nullable and always written. Data holds the
// per-type payload exactly as the producer committed it.
type Envelope struct {
	ID            string
	SchemaVersion int
	Type          string
	OccurredAt    time.Time
	TenantID      domain.TenantID
	ProjectID     domain.ProjectID
	Resource      Resource
	Actor         Actor
	CorrelationID string
	CausationID   *string
	Data          json.RawMessage
}

// wireEnvelope is the JSON shape of Envelope; OccurredAt travels as text so the
// UTC "Z" form is checked rather than normalized away.
type wireEnvelope struct {
	ID            string          `json:"id"`
	SchemaVersion int             `json:"schemaVersion"`
	Type          string          `json:"type"`
	OccurredAt    string          `json:"occurredAt"`
	TenantID      string          `json:"tenantId"`
	ProjectID     string          `json:"projectId,omitempty"`
	Resource      Resource        `json:"resource"`
	Actor         Actor           `json:"actor"`
	CorrelationID string          `json:"correlationId"`
	CausationID   *string         `json:"causationId"`
	Data          json.RawMessage `json:"data"`
}

// MarshalJSON writes the catalog wire form with a UTC timestamp.
func (e Envelope) MarshalJSON() ([]byte, error) {
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return json.Marshal(wireEnvelope{
		ID: e.ID, SchemaVersion: e.SchemaVersion, Type: e.Type,
		OccurredAt: e.OccurredAt.UTC().Format(timestampLayout),
		TenantID:   string(e.TenantID), ProjectID: string(e.ProjectID),
		Resource: e.Resource, Actor: e.Actor, CorrelationID: e.CorrelationID,
		CausationID: e.CausationID, Data: data,
	})
}

const timestampLayout = "2006-01-02T15:04:05.999999999Z"

// Payloads of the initial event types. Unknown additional members are tolerated
// on decode (compatible optional additions within a schema version).

type WorkItemCreated struct {
	Kind  string `json:"kind"`
	State string `json:"state"`
}

type WorkItemUpdated struct {
	ChangedFields   []string `json:"changedFields"`
	ContentRevision *int64   `json:"contentRevision,omitempty"`
}

type WorkItemTransitioned struct {
	From         string `json:"from"`
	To           string `json:"to"`
	TransitionID string `json:"transitionId"`
}

type WorkspaceStateChanged struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// WorkspaceCheckpointCreated records a committed source revision. Exactly one
// of PublishingAttemptID and HumanSynchronization identifies its origin.
type WorkspaceCheckpointCreated struct {
	Repository           string  `json:"repository"`
	PreviousCommit       *string `json:"previousCommit"`
	NewCommit            string  `json:"newCommit"`
	PublishingAttemptID  *string `json:"publishingAttemptId,omitempty"`
	HumanSynchronization bool    `json:"humanSynchronization,omitempty"`
}

type RunCreated struct {
	WorkItemID          string `json:"workItemId"`
	DefinitionID        string `json:"definitionId"`
	InputSnapshotDigest string `json:"inputSnapshotDigest"`
}

type RunStateChanged struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type StepAttemptStateChanged struct {
	RunID  string `json:"runId"`
	StepID string `json:"stepId"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

type GateRequested struct {
	SubjectDigest   string `json:"subjectDigest"`
	ContentRevision int64  `json:"contentRevision"`
	ReviewerScope   string `json:"reviewerScope"`
	Deadline        string `json:"deadline"`
}

type GateDecided struct {
	DecidedBy     Actor  `json:"decidedBy"`
	Decision      string `json:"decision"`
	State         string `json:"state"`
	SubjectDigest string `json:"subjectDigest"`
}

type GateInvalidated struct {
	SubjectDigest       string  `json:"subjectDigest"`
	Reason              string  `json:"reason"`
	SupersedingCommit   *string `json:"supersedingCommit,omitempty"`
	SupersedingRevision *int64  `json:"supersedingRevision,omitempty"`
}

// DeploymentStateChanged must carry at least one source/artifact reference.
type DeploymentStateChanged struct {
	EnvironmentID string   `json:"environmentId"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	SourceCommit  *string  `json:"sourceCommit,omitempty"`
	ArtifactIDs   []string `json:"artifactIds,omitempty"`
	ImageDigest   *string  `json:"imageDigest,omitempty"`
}

type MembershipChanged struct {
	UserID           string   `json:"userId"`
	BindingIDs       []string `json:"bindingIds"`
	EffectiveVersion int64    `json:"effectiveVersion"`
}

type ProjectSettingsChanged struct {
	ChangedSettings []string `json:"changedSettings"`
	Version         int64    `json:"version"`
}

// ConfigurationChanged never carries a value; Version is the value or wrapping
// version.
type ConfigurationChanged struct {
	Kind    string `json:"kind"`
	Version int64  `json:"version"`
	Change  string `json:"change"`
	Scope   string `json:"scope"`
}

type UsageRecorded struct {
	RunID        string `json:"runId"`
	AttemptID    string `json:"attemptId"`
	Model        string `json:"model"`
	Basis        string `json:"basis"`
	Uncertainty  string `json:"uncertainty"`
	InputTokens  *int64 `json:"inputTokens,omitempty"`
	OutputTokens *int64 `json:"outputTokens,omitempty"`
}

// Initial event type names.
const (
	TypeWorkItemCreated        = "work_item.created"
	TypeWorkItemUpdated        = "work_item.updated"
	TypeWorkItemTransitioned   = "work_item.transitioned"
	TypeWorkspaceStateChanged  = "workspace.state_changed"
	TypeWorkspaceCheckpoint    = "workspace.checkpoint_created"
	TypeRunCreated             = "run.created"
	TypeRunStateChanged        = "run.state_changed"
	TypeStepAttemptStateChange = "step_attempt.state_changed"
	TypeGateRequested          = "gate.requested"
	TypeGateDecided            = "gate.decided"
	TypeGateInvalidated        = "gate.invalidated"
	TypeDeploymentStateChanged = "deployment.state_changed"
	TypeMembershipChanged      = "membership.changed"
	TypeProjectSettingsChanged = "project.settings_changed"
	TypeConfigurationChanged   = "configuration.changed"
	TypeUsageRecorded          = "usage.recorded"
)
