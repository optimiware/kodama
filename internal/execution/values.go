package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Purpose distinguishes ordinary work runs from internal cleanup runs. A work
// run requires an actionable work item and a workspace source. A cleanup run is
// project-scoped, uses no workspace and is authorized only for one recorded
// deployment; it may outlive its work item and never consumes business WIP.
type Purpose string

const (
	PurposeWork    Purpose = "work"
	PurposeCleanup Purpose = "cleanup"
)

func (p Purpose) Valid() bool { return p == PurposeWork || p == PurposeCleanup }

// RunStatus, StepStatus and AttemptStatus are exactly the specified enums.
// Waiting for a gate, quota or backoff is derived from pending steps; there are
// no lost/paused/starting statuses.
type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled
}

type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	StepSkipped   StepStatus = "skipped"
	StepCancelled StepStatus = "cancelled"
)

func (s StepStatus) Terminal() bool {
	return s == StepSucceeded || s == StepFailed || s == StepSkipped || s == StepCancelled
}

type AttemptStatus string

const (
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptCancelled AttemptStatus = "cancelled"
)

func (s AttemptStatus) Terminal() bool { return s != AttemptRunning }

// Reason is a classified, content-free reason code (the event contract's
// ReasonCode syntax).
type Reason string

const (
	ReasonCreated       Reason = "created"
	ReasonStarting      Reason = "starting"
	ReasonRunning       Reason = "running"
	ReasonCompleted     Reason = "completed"
	ReasonStepFailed    Reason = "step_failed"
	ReasonCommandFailed Reason = "command_failed"
	ReasonNeedsInput    Reason = "needs_input"
	// ReasonOutputInvalid and ReasonOutputUploadFailed are the wrapper's output
	// failures, kept from a failed result instead of collapsing to command_failed.
	ReasonOutputInvalid      Reason = "output_invalid"
	ReasonOutputUploadFailed Reason = "output_upload_failed"
	ReasonStartupTimeout     Reason = "startup_timeout"
	ReasonTimeout            Reason = "execution_timeout"
	ReasonInfrastructure     Reason = "infrastructure_failure"
	ReasonTransient          Reason = "transient_failure"
	ReasonOutcomeUnknown     Reason = "outcome_unknown"
	ReasonCancelRequested    Reason = "cancel_requested"
	ReasonCancelled          Reason = "cancelled"
	ReasonWorkCancelled      Reason = "work_cancelled"
	ReasonRunFailed          Reason = "run_failed"
	ReasonSkipped            Reason = "skipped"
	ReasonCondition          Reason = "condition_false"
	ReasonSkippedAncestor    Reason = "skipped_ancestor"
	ReasonBackoff            Reason = "backoff"
	ReasonRetry              Reason = "retry_scheduled"
	ReasonInvalidRef         Reason = "invalid_reference"
	ReasonReconciled         Reason = "reconciled"
	ReasonRetained           Reason = "source_retained"
	ReasonNoOutputs          Reason = "missing_outputs"
	ReasonUnsettled          Reason = "unsettled"
	// ReasonWorkloadFailed: the workload ended without a result.
	ReasonWorkloadFailed Reason = "workload_failed"
	// ReasonWorkloadLost: a dispatched workload is affirmatively gone.
	ReasonWorkloadLost Reason = "workload_lost"
)

var reasonPattern = regexp.MustCompile(runtimev1.Patterns["ReasonCode"])

// Valid reports whether r is a well-formed reason code.
func (r Reason) Valid() bool { return reasonPattern.MatchString(string(r)) }

// ActorType identifies who caused a change. The type comes from the identity
// authority or the module itself, never from request content.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorService  ActorType = "service"
	ActorWorkload ActorType = "workload"
	ActorOperator ActorType = "operator"
)

// Actor is an attributable principal.
type Actor struct {
	Type ActorType `json:"type"`
	ID   string    `json:"id"`
}

// Validate checks the actor is usable for attribution.
func (a Actor) Validate() error {
	switch a.Type {
	case ActorUser, ActorService, ActorWorkload, ActorOperator:
	default:
		return invalidf("unknown actor type")
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return invalidf("actor id must be 1..256 bytes")
	}
	return nil
}

// System is the module's own actor for reconciliation and consumers.
var System = Actor{Type: ActorService, ID: "execution"}

// Permission names an authorization question. The values equal the identity
// matrix's permission names.
type Permission string

const (
	PermRunRead   Permission = "run:read"
	PermRunManage Permission = "run:manage"
)

// FailureClass classifies why an attempt failed. Classification alone never
// proves it is safe to replay an external effect.
type FailureClass string

const (
	ClassInfrastructure FailureClass = "infrastructure"
	ClassTransient      FailureClass = "transient"
	ClassCommand        FailureClass = "command"
	ClassValidation     FailureClass = "validation"
	ClassPolicy         FailureClass = "policy"
	ClassTimeout        FailureClass = "timeout"
	ClassCancelled      FailureClass = "cancelled"
)

// LaunchEvidence is the trusted executor's statement about whether the
// primary container's command could have started. It is the only thing that
// establishes replay safety of an ambiguous failure.
type LaunchEvidence string

const (
	// LaunchUnknown: the executor cannot tell (node loss, lost connection).
	LaunchUnknown LaunchEvidence = "unknown"
	// LaunchNotStarted: the runtime affirms the command never started (the
	// workload was never created or could not start).
	LaunchNotStarted LaunchEvidence = "not_started"
	// LaunchStarted: the command ran (or may have run to completion).
	LaunchStarted LaunchEvidence = "started"
)

// EffectState records what is known about an attempt's external effects.
type EffectState string

const (
	// EffectNone: no external effect is possible or all are settled.
	EffectNone EffectState = "none"
	// EffectPossible: the command may be running; termination is pending.
	EffectPossible EffectState = "possible"
	// EffectUnknown: an ambiguous failure; blocked until reconciled.
	EffectUnknown EffectState = "unknown"
	// EffectSettled: effects were verified settled.
	EffectSettled EffectState = "settled"
)

// Access mirrors the pipeline workspace access of a step.
type Access string

const (
	AccessNone  Access = "none"
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// PinnedInput is one resolved input with its provenance. It holds no secret
// plaintext.
type PinnedInput struct {
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	Value           runtimev1.Value `json:"value"`
	Source          string          `json:"source"`
	VariableID      string          `json:"variableId,omitempty"`
	VariableVersion int64           `json:"variableVersion,omitempty"`
}

// StepSecret is one pinned secret binding of one step. Binding names are only
// unique within a step, so the pins are kept per step.
type StepSecret struct {
	Step    string                  `json:"step"`
	Binding runtimev1.SecretBinding `json:"binding"`
}

// Specification is the pinned work-item snapshot (the work item's content at
// the pinned revision). Later edits never change it.
type Specification struct {
	Spec               string                          `json:"spec"`
	AcceptanceCriteria []runtimev1.AcceptanceCriterion `json:"acceptanceCriteria"`
}

// StepState is one step's pinned state bindings.
type StepState struct {
	Step     string                   `json:"step"`
	Bindings []runtimev1.StateBinding `json:"bindings"`
}

// StepArtifactFile is one verified artifact file binding.
type StepArtifactFile struct {
	Step  string                  `json:"step"`
	Input runtimev1.ArtifactInput `json:"input"`
}

// Snapshot is the immutable set of pinned inputs of a run.
type Snapshot struct {
	Inputs  []PinnedInput `json:"inputs"`
	Secrets []StepSecret  `json:"secrets"`
	// Omitted for historical/no-environment runs, preserving their stored shape.
	Environments []StepEnvironment `json:"environments,omitempty"`
	// State and artifact file bindings are kodama/v1alpha3 additions.
	States        []StepState        `json:"states,omitempty"`
	ArtifactFiles []StepArtifactFile `json:"artifactFiles,omitempty"`
	// Runtime is the runtime the run was admitted for; nil for a run admitted
	// while none was configured.
	Runtime *RuntimeBinding `json:"runtime,omitempty"`
}

// Values returns the input values keyed by name.
func (s Snapshot) Values() map[string]runtimev1.Value {
	out := make(map[string]runtimev1.Value, len(s.Inputs))
	for _, in := range s.Inputs {
		out[in.Name] = in.Value
	}
	return out
}

// SecretsOf returns a step's pinned bindings in declaration order.
func (s Snapshot) SecretsOf(step string) []runtimev1.SecretBinding {
	var out []runtimev1.SecretBinding
	for _, sec := range s.Secrets {
		if sec.Step == step {
			out = append(out, sec.Binding)
		}
	}
	return out
}

// StatesOf returns the step's pinned state bindings.
func (s Snapshot) StatesOf(step string) []runtimev1.StateBinding {
	for _, st := range s.States {
		if st.Step == step {
			return append([]runtimev1.StateBinding(nil), st.Bindings...)
		}
	}
	return nil
}

// ArtifactsOf returns the step's pinned file artifacts.
func (s Snapshot) ArtifactsOf(step string) []runtimev1.ArtifactInput {
	var out []runtimev1.ArtifactInput
	for _, a := range s.ArtifactFiles {
		if a.Step == step {
			out = append(out, a.Input)
		}
	}
	return out
}

// Digest is the stable fingerprint of the pinned input snapshot of a run: the
// definition, work item revision, source commit, inputs and secret versions.
func Digest(def domain.DefinitionID, defDigest string, item domain.WorkItemID, revision domain.ContentRevision, commit string, snap Snapshot) (string, error) {
	inputs := append([]PinnedInput(nil), snap.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Name < inputs[j].Name })
	environments := append([]StepEnvironment(nil), snap.Environments...)
	sort.Slice(environments, func(i, j int) bool { return environments[i].Step < environments[j].Step })
	states := append([]StepState(nil), snap.States...)
	sort.Slice(states, func(i, j int) bool { return states[i].Step < states[j].Step })
	artifacts := append([]StepArtifactFile(nil), snap.ArtifactFiles...)
	sort.Slice(artifacts, func(i, j int) bool {
		if artifacts[i].Step != artifacts[j].Step {
			return artifacts[i].Step < artifacts[j].Step
		}
		return artifacts[i].Input.Name < artifacts[j].Input.Name
	})
	doc := struct {
		Definition   string             `json:"definition"`
		Digest       string             `json:"definitionDigest"`
		WorkItem     string             `json:"workItem"`
		Revision     int64              `json:"contentRevision"`
		Commit       string             `json:"sourceCommit"`
		Inputs       []PinnedInput      `json:"inputs"`
		Secrets      []StepSecret       `json:"secrets"`
		Environments []StepEnvironment  `json:"environments,omitempty"`
		States       []StepState        `json:"states,omitempty"`
		Artifacts    []StepArtifactFile `json:"artifactFiles,omitempty"`
		Runtime      *RuntimeBinding    `json:"runtime,omitempty"`
	}{string(def), defDigest, string(item), int64(revision), commit, inputs, snap.Secrets, environments, states, artifacts, snap.Runtime}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	canonical, err := runtimev1.Canonicalize(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ActionKey returns the stable action key of a logical step. It is identical
// across the step's attempts, so external tools can recognize a replay.
func ActionKey(run domain.RunID, step string) string {
	sum := sha256.Sum256([]byte("kodama/action/" + string(run) + "/" + step))
	return "act:" + hex.EncodeToString(sum[:16])
}

// WorkloadName returns the deterministic workload name of an attempt. It is a
// pure function of the tenant and attempt identity, so a restarted controller
// derives the same name and adopts rather than duplicates the workload. It is
// a valid DNS label, usable as a Job, container or directory name.
func WorkloadName(tenant domain.TenantID, attempt domain.AttemptID) string {
	sum := sha256.Sum256([]byte("kodama/job/" + string(tenant) + "/" + string(attempt)))
	return "kdm-" + hex.EncodeToString(sum[:20])
}

// Limit is the typed execution policy of one scope. It is injected and
// validated; there is no default and a missing limit denies dispatch.
type Limit struct {
	// TenantConcurrency and ProjectConcurrency bound unsettled attempts.
	TenantConcurrency  int
	ProjectConcurrency int
	// StartupBudget bounds the time from dispatch intent to the observed start
	// of the command (distinct from the step's execution timeout).
	StartupBudget time.Duration
	// Output bounds carried in the runtime context.
	MaxOutputBytes     int64
	MaxOutputFiles     int64
	MaxDiagnosticBytes int64
	// DefaultTimeout applies when a step declares none.
	DefaultTimeout time.Duration
	// MaxTimeout bounds a step's declared timeout.
	MaxTimeout time.Duration
	// SharedTenant and SharedProject bound the run-level capacity reserved by
	// run_shared runs (services, preparation, the largest attempt and scratch).
	// The zero value reserves nothing: a run_shared run is never started
	// without explicit capacity.
	SharedTenant  CapacityUnits
	SharedProject CapacityUnits
}

// Validate rejects an unusable limit.
func (l Limit) Validate() error {
	switch {
	case l.TenantConcurrency < 1 || l.ProjectConcurrency < 1:
		return invalidf("concurrency limits must be positive")
	case l.StartupBudget < time.Second:
		return invalidf("the startup budget must be at least one second")
	case l.MaxOutputBytes < 1 || l.MaxOutputFiles < 1 || l.MaxDiagnosticBytes < 1:
		return invalidf("output bounds must be positive")
	case l.DefaultTimeout < time.Second || l.MaxTimeout < l.DefaultTimeout:
		return invalidf("timeouts are inconsistent")
	}
	return nil
}

// Caller is the verified principal and request of a public use case.
type Caller struct {
	PrincipalID string
	RequestID   string
}

func shortReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("execution: %v", err))
	}
	return v
}
