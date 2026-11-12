package api

import (
	"bytes"
	"encoding/json"
	"time"
)

// Execution, output, request-resolution and retained-log wire vocabulary.
//
// An execution is an immutable admitted start plus explicit state transitions.
// Outputs are submitted once per action; request resolution is the authorized
// answer; retained logs are bounded chunks. No result carries a secret or a
// Kernel transition payload.

// ExecutionInspectRequest is the execution.inspect input. The execution is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type ExecutionInspectRequest struct {
	ExecutionID string `json:"execution_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded execution identity.
func (request ExecutionInspectRequest) Validate() error {
	if request.ExecutionID != "" && !validID(request.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	return nil
}

// ExecutionStopEvidence is the credential-free stop-barrier summary of one
// action. Acknowledging a stop request is not confirmation; only affirmative
// termination evidence is.
type ExecutionStopEvidence struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Validate checks the declared stop state and bounded detail.
func (evidence ExecutionStopEvidence) Validate() error {
	if evidence.State != "confirmed" && evidence.State != "unconfirmed" {
		return invalid("unknown stop state %q", evidence.State)
	}
	if len(evidence.Detail) > MaxSubjectValueBytes {
		return invalid("stop detail is %d bytes; at most %d are allowed", len(evidence.Detail), MaxSubjectValueBytes)
	}
	return nil
}

// ExecutionAction is one admitted action observation. Recorded outputs,
// decision/actor/answered_at, timeout/deadline and the stop summary are
// credential-free inspection facts; staged outputs and workspace identities are
// never exposed. A request action also exposes the immutable typed Subject and
// the exact role/<role> or service/<id> Assignment, so a responder can decide
// without a second read.
type ExecutionAction struct {
	ActionID       string                 `json:"action_id"`
	Index          int                    `json:"index"`
	Name           string                 `json:"name"`
	Kind           string                 `json:"kind"`
	State          string                 `json:"state"`
	Version        uint64                 `json:"version" schema:"version"`
	TimeoutSeconds int64                  `json:"timeout_seconds,omitempty"`
	ActivatedAt    *time.Time             `json:"activated_at,omitempty"`
	Deadline       *time.Time             `json:"deadline,omitempty"`
	Outputs        map[string]string      `json:"outputs,omitempty"`
	Subject        *Subject               `json:"subject,omitempty"`
	Assignment     string                 `json:"assignment,omitempty"`
	Stop           *ExecutionStopEvidence `json:"stop,omitempty"`
	Decision       string                 `json:"decision,omitempty"`
	Actor          string                 `json:"actor,omitempty" schema:"id"`
	AnsweredAt     *time.Time             `json:"answered_at,omitempty"`
}

// UnmarshalJSON decodes one action strictly. The request subject is decoded
// with member presence exactly like an inbox item: a field missing its name,
// kind or value is refused instead of defaulting the absent value to an empty
// string. Every nested level still refuses unknown members.
func (action *ExecutionAction) UnmarshalJSON(data []byte) error {
	type alias ExecutionAction
	var wire struct {
		*alias
		Subject json.RawMessage `json:"subject"`
	}
	wire.alias = (*alias)(action)
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	action.Subject = nil
	raw := bytes.TrimSpace(wire.Subject)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var subjectWire struct {
		Fields []struct {
			Name  *string `json:"name"`
			Kind  *string `json:"kind"`
			Value *string `json:"value"`
		} `json:"fields"`
	}
	if err := decodeStrict(raw, &subjectWire); err != nil {
		return err
	}
	subject := Subject{Fields: make([]SubjectField, 0, len(subjectWire.Fields))}
	for _, field := range subjectWire.Fields {
		if field.Name == nil || field.Kind == nil || field.Value == nil {
			return invalid("subject field is missing a required member")
		}
		subject.Fields = append(subject.Fields, SubjectField{Name: *field.Name, Kind: *field.Kind, Value: *field.Value})
	}
	action.Subject = &subject
	return nil
}

// Validate checks the action identity, index, kind, state, version, outputs,
// timeout/deadline, stop summary and recorded decision facts.
func (action ExecutionAction) Validate() error {
	if !validID(action.ActionID) {
		return invalid("action_id is invalid")
	}
	if action.Index < 0 {
		return invalid("action index must not be negative")
	}
	if action.Name == "" {
		return invalid("action name is empty")
	}
	if !validActionKind(action.Kind) {
		return invalid("unknown action kind %q", action.Kind)
	}
	if !validActionState(action.State) {
		return invalid("unknown action state %q", action.State)
	}
	if action.Version == 0 {
		return invalid("action version must be positive")
	}
	if action.TimeoutSeconds < 0 {
		return invalid("action timeout must not be negative")
	}
	if action.ActivatedAt != nil && action.ActivatedAt.IsZero() {
		return invalid("action activated_at is zero")
	}
	if action.Deadline != nil && action.Deadline.IsZero() {
		return invalid("action deadline is zero")
	}
	if err := validateOutputs(action.Outputs); err != nil {
		return err
	}
	if action.Subject != nil {
		if err := action.Subject.Validate(); err != nil {
			return err
		}
		if action.Kind == "request" && len(action.Subject.Fields) == 0 {
			return invalid("request action subject is empty")
		}
	}
	if action.Assignment != "" && !validAssignment(action.Assignment) {
		return invalid("request assignment %q must be role/<role> or service/<id>", action.Assignment)
	}
	if action.Kind != "request" && (action.Subject != nil || action.Assignment != "") {
		return invalid("non-request action carries request facts")
	}
	if action.Stop != nil {
		if err := action.Stop.Validate(); err != nil {
			return err
		}
	}
	if action.Decision != "" {
		if !validDecision(action.Decision) {
			return invalid("unknown request decision %q", action.Decision)
		}
		if !validID(action.Actor) {
			return invalid("request decision actor is invalid")
		}
		if action.AnsweredAt == nil || action.AnsweredAt.IsZero() {
			return invalid("request decision answered_at is missing")
		}
	} else if action.Actor != "" || action.AnsweredAt != nil {
		return invalid("request decision actor/time is present without a decision")
	}
	return nil
}

// ExecutionFailure is the typed terminal diagnostic of a failed execution. It
// never replaces the retained stop/observation/cleanup evidence.
type ExecutionFailure struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Validate checks the declared failure kind and bounded reason/detail.
func (failure ExecutionFailure) Validate() error {
	if !validFailureKind(failure.Kind) {
		return invalid("unknown execution failure kind %q", failure.Kind)
	}
	if len(failure.Reason) > MaxSubjectValueBytes {
		return invalid("execution failure reason is %d bytes; at most %d are allowed", len(failure.Reason), MaxSubjectValueBytes)
	}
	if len(failure.Detail) > MaxSubjectValueBytes {
		return invalid("execution failure detail is %d bytes; at most %d are allowed", len(failure.Detail), MaxSubjectValueBytes)
	}
	return nil
}

// ExecutionCleanup is the visible pending-cleanup status of one execution,
// separate from the terminal execution outcome.
type ExecutionCleanup struct {
	State       string `json:"state"`
	WorkloadID  string `json:"workload_id,omitempty" schema:"id"`
	WorkspaceID string `json:"workspace_id,omitempty" schema:"id"`
	Attempts    int    `json:"attempts,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// Validate checks the declared cleanup state, identities, attempt count and
// bounded last error.
func (cleanup ExecutionCleanup) Validate() error {
	if !validCleanupState(cleanup.State) {
		return invalid("unknown cleanup state %q", cleanup.State)
	}
	if cleanup.WorkloadID != "" && !validID(cleanup.WorkloadID) {
		return invalid("cleanup workload_id is invalid")
	}
	if cleanup.WorkspaceID != "" && !validID(cleanup.WorkspaceID) {
		return invalid("cleanup workspace_id is invalid")
	}
	if cleanup.Attempts < 0 {
		return invalid("cleanup attempts must not be negative")
	}
	if len(cleanup.LastError) > MaxSubjectValueBytes {
		return invalid("cleanup last_error is %d bytes; at most %d are allowed", len(cleanup.LastError), MaxSubjectValueBytes)
	}
	return nil
}

// ExecutionInspectResult is the execution.inspect result. TaskID is empty for a
// task-free pipeline execution. PipelineID is the optional stored-pipeline
// identity: it is empty (and omitted from the wire) for a task-free inline
// execution, never fabricated. Failure is present only after a terminal
// failure; Cleanup is always present and is independent of the outcome.
type ExecutionInspectResult struct {
	ExecutionID   string            `json:"execution_id"`
	TaskID        string            `json:"task_id,omitempty" schema:"id"`
	PipelineID    string            `json:"pipeline_id,omitempty"`
	State         string            `json:"state"`
	Version       uint64            `json:"version" schema:"version"`
	CurrentAction int               `json:"current_action"`
	Actions       []ExecutionAction `json:"actions"`
	Failure       *ExecutionFailure `json:"failure,omitempty"`
	Cleanup       ExecutionCleanup  `json:"cleanup"`
}

// UnmarshalJSON decodes one inspection result strictly. The optional
// pipeline_id is presence-aware: an omitted member leaves the empty optional
// identity, while an explicit empty string is refused instead of being treated
// as absent.
func (result *ExecutionInspectResult) UnmarshalJSON(data []byte) error {
	type alias ExecutionInspectResult
	var wire struct {
		*alias
		PipelineID *string `json:"pipeline_id"`
	}
	wire.alias = (*alias)(result)
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	result.PipelineID = ""
	if wire.PipelineID != nil {
		if *wire.PipelineID == "" {
			return invalid("pipeline_id must not be empty when present")
		}
		result.PipelineID = *wire.PipelineID
	}
	return nil
}

// Validate checks the execution identity, state, versions, actions, failure and
// cleanup.
func (result ExecutionInspectResult) Validate() error {
	if !validID(result.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if result.TaskID != "" && !validID(result.TaskID) {
		return invalid("task_id is invalid")
	}
	if result.PipelineID != "" && !validID(result.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if !validExecutionState(result.State) {
		return invalid("unknown execution state %q", result.State)
	}
	if result.Version == 0 {
		return invalid("execution version must be positive")
	}
	if result.CurrentAction < -1 {
		return invalid("current_action must be -1 or a non-negative index")
	}
	for _, action := range result.Actions {
		if err := action.Validate(); err != nil {
			return err
		}
	}
	if result.Failure != nil {
		if err := result.Failure.Validate(); err != nil {
			return err
		}
	}
	return result.Cleanup.Validate()
}

// ExecutionListRequest is the execution.list input. The project is carried
// explicitly for MCP callers and is optional for REST callers whose route
// supplies it.
type ExecutionListRequest struct {
	ProjectID  string `json:"project_id,omitempty" schema:"id"`
	TaskID     string `json:"task_id,omitempty" schema:"id"`
	PipelineID string `json:"pipeline_id,omitempty" schema:"id"`
	State      string `json:"state,omitempty"`
	Page       *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity, filters and page bounds.
func (request ExecutionListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if request.PipelineID != "" && !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if request.State != "" && !validExecutionState(request.State) {
		return invalid("unknown execution state %q", request.State)
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// ExecutionSummary is one scoped execution observation. TaskID is empty for a
// task-free pipeline execution and PipelineID is the optional stored-pipeline
// identity: it is empty (and omitted from the wire) for a task-free inline
// execution, never fabricated.
type ExecutionSummary struct {
	ExecutionID string `json:"execution_id"`
	TaskID      string `json:"task_id,omitempty" schema:"id"`
	PipelineID  string `json:"pipeline_id,omitempty"`
	State       string `json:"state"`
	Version     uint64 `json:"version" schema:"version"`
}

// UnmarshalJSON decodes one summary strictly with the same presence-aware
// optional pipeline_id as execution.inspect: omitted stays empty, explicit
// empty is refused.
func (summary *ExecutionSummary) UnmarshalJSON(data []byte) error {
	type alias ExecutionSummary
	var wire struct {
		*alias
		PipelineID *string `json:"pipeline_id"`
	}
	wire.alias = (*alias)(summary)
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	summary.PipelineID = ""
	if wire.PipelineID != nil {
		if *wire.PipelineID == "" {
			return invalid("pipeline_id must not be empty when present")
		}
		summary.PipelineID = *wire.PipelineID
	}
	return nil
}

// Validate checks the execution identity, optional task identity, state and
// version.
func (summary ExecutionSummary) Validate() error {
	if !validID(summary.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if summary.TaskID != "" && !validID(summary.TaskID) {
		return invalid("task_id is invalid")
	}
	if summary.PipelineID != "" && !validID(summary.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if !validExecutionState(summary.State) {
		return invalid("unknown execution state %q", summary.State)
	}
	if summary.Version == 0 {
		return invalid("execution version must be positive")
	}
	return nil
}

// ExecutionListResult is the execution.list result.
type ExecutionListResult struct {
	Executions []ExecutionSummary `json:"executions"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// Validate checks every execution summary and the optional cursor bound.
func (result ExecutionListResult) Validate() error {
	for _, execution := range result.Executions {
		if err := execution.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// ExecutionCancelRequest is the execution.cancel input. The execution is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type ExecutionCancelRequest struct {
	ExecutionID string `json:"execution_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded execution identity.
func (request ExecutionCancelRequest) Validate() error {
	if request.ExecutionID != "" && !validID(request.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	return nil
}

// ExecutionCancelResult is the execution.cancel result.
type ExecutionCancelResult struct {
	ExecutionVersion uint64 `json:"execution_version" schema:"version"`
}

// Validate checks the committed execution version.
func (result ExecutionCancelResult) Validate() error {
	if result.ExecutionVersion == 0 {
		return invalid("execution version must be positive")
	}
	return nil
}

// ExecutionActionOutputsRequest is the execution.action.outputs input. The
// execution and action are carried explicitly for MCP callers and are optional
// for REST callers whose route supplies them.
type ExecutionActionOutputsRequest struct {
	ExecutionID string            `json:"execution_id,omitempty" schema:"id"`
	ActionID    string            `json:"action_id,omitempty" schema:"id"`
	Outputs     map[string]string `json:"outputs"`
}

// Validate checks the optional execution/action identities and the non-empty
// bounded outputs.
func (request ExecutionActionOutputsRequest) Validate() error {
	if request.ExecutionID != "" && !validID(request.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if request.ActionID != "" && !validID(request.ActionID) {
		return invalid("action_id is invalid")
	}
	if request.Outputs == nil {
		return invalid("action outputs are missing")
	}
	return validateOutputs(request.Outputs)
}

// ExecutionActionOutputsResult is the execution.action.outputs result.
type ExecutionActionOutputsResult struct {
	ExecutionVersion uint64 `json:"execution_version" schema:"version"`
	ActionVersion    uint64 `json:"action_version" schema:"version"`
}

// Validate checks both committed versions.
func (result ExecutionActionOutputsResult) Validate() error {
	if result.ExecutionVersion == 0 || result.ActionVersion == 0 {
		return invalid("execution and action versions must be positive")
	}
	return nil
}

// ExecutionRequestResolveRequest is the action-only execution.request.resolve
// input. The request names only the expected action version. The shared
// envelope expected_version remains the expected execution version for this
// operation, so the internal commit versions come from the observed state
// instead of a repeated body member. The execution and action are carried
// explicitly for MCP callers and are optional for REST callers whose route
// supplies them.
type ExecutionRequestResolveRequest struct {
	ExecutionID           string            `json:"execution_id,omitempty" schema:"id"`
	ActionID              string            `json:"action_id,omitempty" schema:"id"`
	ExpectedActionVersion uint64            `json:"expected_action_version" schema:"version"`
	Decision              string            `json:"decision"`
	Outputs               map[string]string `json:"outputs,omitempty"`
}

// Validate checks the optional execution/action identities, the expected action
// version, the declared decision and optional outputs.
func (request ExecutionRequestResolveRequest) Validate() error {
	if request.ExecutionID != "" && !validID(request.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if request.ActionID != "" && !validID(request.ActionID) {
		return invalid("action_id is invalid")
	}
	if request.ExpectedActionVersion == 0 {
		return invalid("expected_action_version must be positive")
	}
	if !validDecision(request.Decision) {
		return invalid("unknown request decision %q", request.Decision)
	}
	return validateOutputs(request.Outputs)
}

// ExecutionRequestResolveResult is the execution.request.resolve result.
type ExecutionRequestResolveResult struct {
	ExecutionVersion uint64 `json:"execution_version" schema:"version"`
	ActionVersion    uint64 `json:"action_version" schema:"version"`
}

// Validate checks both committed versions.
func (result ExecutionRequestResolveResult) Validate() error {
	if result.ExecutionVersion == 0 || result.ActionVersion == 0 {
		return invalid("execution and action versions must be positive")
	}
	return nil
}

// ExecutionLogsReadRequest is the execution.logs.read input. The execution is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it; the action and stream are body fields.
type ExecutionLogsReadRequest struct {
	ExecutionID string `json:"execution_id,omitempty" schema:"id"`
	ActionID    string `json:"action_id" schema:"id"`
	Stream      string `json:"stream"`
	Page        *Page  `json:"page,omitempty"`
}

// Validate checks the optional execution identity, action identity, declared
// stream and optional page bounds.
func (request ExecutionLogsReadRequest) Validate() error {
	if request.ExecutionID != "" && !validID(request.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if !validID(request.ActionID) {
		return invalid("action_id is invalid")
	}
	if !validLogStream(request.Stream) {
		return invalid("unknown log stream %q", request.Stream)
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// LogChunk is one retained log observation.
type LogChunk struct {
	ActionID string `json:"action_id"`
	Stream   string `json:"stream"`
	Offset   uint64 `json:"offset"`
	Sequence uint64 `json:"sequence"`
	Data     []byte `json:"data"`
}

// Validate checks the action identity, stream and bounded chunk body.
func (chunk LogChunk) Validate() error {
	if !validID(chunk.ActionID) {
		return invalid("action_id is invalid")
	}
	if !validLogStream(chunk.Stream) {
		return invalid("unknown log stream %q", chunk.Stream)
	}
	if len(chunk.Data) > MaxEventDataBytes {
		return invalid("log chunk is %d bytes; at most %d are allowed", len(chunk.Data), MaxEventDataBytes)
	}
	return nil
}

// ExecutionLogsReadResult is the execution.logs.read result.
type ExecutionLogsReadResult struct {
	Chunks     []LogChunk `json:"chunks"`
	NextCursor string     `json:"next_cursor,omitempty"`
	Truncated  bool       `json:"truncated"`
}

// Validate checks every chunk and the optional cursor bound.
func (result ExecutionLogsReadResult) Validate() error {
	for _, chunk := range result.Chunks {
		if err := chunk.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// executionStates is the durable execution state vocabulary.
var executionStates = []string{"queued", "running", "succeeded", "failed", "cancelled"}

// validExecutionState reports whether state is a declared execution state.
func validExecutionState(state string) bool {
	for _, candidate := range executionStates {
		if state == candidate {
			return true
		}
	}
	return false
}

// actionStates is the durable action state vocabulary.
var actionStates = []string{"pending", "running", "succeeded", "failed", "cancelled"}

// validActionState reports whether state is a declared action state.
func validActionState(state string) bool {
	for _, candidate := range actionStates {
		if state == candidate {
			return true
		}
	}
	return false
}

// actionKinds is the action kind vocabulary.
var actionKinds = []string{"container", "request"}

// validActionKind reports whether kind is a declared action kind.
func validActionKind(kind string) bool {
	for _, candidate := range actionKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// requestDecisions is the request decision vocabulary.
var requestDecisions = []string{"pass", "fail"}

// validDecision reports whether decision is a declared request decision.
func validDecision(decision string) bool {
	for _, candidate := range requestDecisions {
		if decision == candidate {
			return true
		}
	}
	return false
}

// logStreams is the retained-log stream vocabulary.
var logStreams = []string{"stdout", "stderr"}

// validLogStream reports whether stream is a declared log stream.
func validLogStream(stream string) bool {
	for _, candidate := range logStreams {
		if stream == candidate {
			return true
		}
	}
	return false
}

// failureKinds is the terminal failure-kind vocabulary.
var failureKinds = []string{"timeout", "cancelled", "preparation", "workspace_lost", "upstream_failed", "runtime"}

// validFailureKind reports whether kind is a declared failure kind.
func validFailureKind(kind string) bool {
	for _, candidate := range failureKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// cleanupStates is the visible cleanup-state vocabulary.
var cleanupStates = []string{"none", "pending", "complete"}

// validCleanupState reports whether state is a declared cleanup state.
func validCleanupState(state string) bool {
	for _, candidate := range cleanupStates {
		if state == candidate {
			return true
		}
	}
	return false
}

// validateOutputs checks an optional bounded declared-output map.
func validateOutputs(outputs map[string]string) error {
	for key, value := range outputs {
		if key == "" {
			return invalid("output name is empty")
		}
		if len(key) > maxIDLength {
			return invalid("output name is too long")
		}
		if len(value) > MaxSubjectValueBytes {
			return invalid("output %q value is too long", key)
		}
	}
	return nil
}
