package api

import (
	"encoding/json"
	"strings"
	"time"
)

// Task and conversation-handoff wire vocabulary.
//
// A task has a content version and a lifecycle version; every result reports
// the versions the commit advanced. Conversation questions, answers and inbox
// items are typed observations, never durable authority.

// MaxTaskMetadataBytes bounds one opaque task metadata object. The Shell
// versions and snapshots the object without interpreting it; the same bound
// applies to every create, edit and read so an oversized registration is
// refused before any effect.
const MaxTaskMetadataBytes = 16 << 10

// Task reference bounds. A reference is a durable restoration pointer (a pushed
// commit, an artifact digest or an external restoration URL), not a Kodama
// identity: the domain clones arbitrary strings and applies no character
// grammar. The public DTO therefore bounds only the byte length and list size.
const (
	// MaxReferenceBytes bounds one opaque durable reference.
	MaxReferenceBytes = 4096
	// MaxReferences bounds one message or question reference list.
	MaxReferences = 256
)

// TaskCreateRequest is the task.create input. The description and pipeline are
// optional: a task may be created open with neither, and only task.start
// requires a selected pipeline. The project is carried explicitly for MCP
// callers and is optional for REST callers whose route supplies it.
type TaskCreateRequest struct {
	ProjectID   string            `json:"project_id,omitempty" schema:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	PipelineID  string            `json:"pipeline_id,omitempty" schema:"id"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Validate checks the title, optional description, optional pipeline identity,
// inputs and bounded metadata.
func (request TaskCreateRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Title == "" {
		return invalid("task title is empty")
	}
	if request.PipelineID != "" && !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if err := validateInputs(request.Inputs); err != nil {
		return err
	}
	return validateTaskMetadata(request.Metadata)
}

// TaskCreateResult is the task.create result.
type TaskCreateResult struct {
	TaskID           string `json:"task_id"`
	ContentVersion   uint64 `json:"content_version" schema:"version"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks the task identity and both committed versions.
func (result TaskCreateResult) Validate() error {
	if !validID(result.TaskID) {
		return invalid("task_id is invalid")
	}
	if result.ContentVersion == 0 || result.LifecycleVersion == 0 {
		return invalid("task content and lifecycle versions must be positive")
	}
	return nil
}

// TaskReadRequest is the task.read input. The task is carried explicitly for
// MCP callers and is optional for REST callers whose route supplies it.
type TaskReadRequest struct {
	TaskID string `json:"task_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded task identity.
func (request TaskReadRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	return nil
}

// TaskMessage is one conversation message observation. References are durable
// restoration pointers, not proof of action success.
type TaskMessage struct {
	Author     string    `json:"author" schema:"id"`
	At         time.Time `json:"at"`
	Body       string    `json:"body"`
	References []string  `json:"references,omitempty"`
}

// Validate checks the author, time, body and optional references.
func (message TaskMessage) Validate() error {
	if !validID(message.Author) {
		return invalid("message author is invalid")
	}
	if message.At.IsZero() {
		return invalid("message time is zero")
	}
	if message.Body == "" {
		return invalid("message body is empty")
	}
	return validateReferences(message.References)
}

// TaskQuestion is the single outstanding conversation question of a waiting
// task. Assignment uses the `role/<role>` or `service/<id>` grammar so a
// task.reply can take both question_id and expected_question_version from a
// task.read.
type TaskQuestion struct {
	QuestionID string   `json:"question_id" schema:"id"`
	Body       string   `json:"body"`
	Assignment string   `json:"assignment"`
	References []string `json:"references,omitempty"`
	Version    uint64   `json:"version" schema:"version"`
}

// Validate checks the question identity, body, assignment, references and
// question version.
func (question TaskQuestion) Validate() error {
	if !validID(question.QuestionID) {
		return invalid("question_id is invalid")
	}
	if question.Body == "" {
		return invalid("question body is empty")
	}
	if !validAssignment(question.Assignment) {
		return invalid("assignment %q must be role/<role> or service/<id>", question.Assignment)
	}
	if err := validateReferences(question.References); err != nil {
		return err
	}
	if question.Version == 0 {
		return invalid("question version must be positive")
	}
	return nil
}

// TaskReadResult is the task.read result. It carries the complete durable task
// content: messages, inputs, metadata, initiator, current execution, execution
// history and the outstanding question, so task.reply needs no second read.
type TaskReadResult struct {
	TaskID           string            `json:"task_id"`
	Title            string            `json:"title"`
	Description      string            `json:"description,omitempty"`
	State            string            `json:"state"`
	ContentVersion   uint64            `json:"content_version" schema:"version"`
	LifecycleVersion uint64            `json:"lifecycle_version" schema:"version"`
	SelectedPipeline string            `json:"selected_pipeline,omitempty" schema:"id"`
	Inputs           map[string]string `json:"inputs,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Initiator        string            `json:"initiator" schema:"id"`
	Messages         []TaskMessage     `json:"messages,omitempty"`
	ExecutionHistory []string          `json:"execution_history,omitempty" schema:"ids"`
	CurrentExecution string            `json:"current_execution,omitempty" schema:"id"`
	Question         *TaskQuestion     `json:"question,omitempty"`
}

// Validate checks the task identity, content, state, versions, conversation,
// metadata, initiator and execution pointers.
func (result TaskReadResult) Validate() error {
	if !validID(result.TaskID) {
		return invalid("task_id is invalid")
	}
	if result.Title == "" {
		return invalid("task title is empty")
	}
	if !validTaskState(result.State) {
		return invalid("unknown task state %q", result.State)
	}
	if result.ContentVersion == 0 || result.LifecycleVersion == 0 {
		return invalid("task content and lifecycle versions must be positive")
	}
	if result.SelectedPipeline != "" && !validID(result.SelectedPipeline) {
		return invalid("selected_pipeline is invalid")
	}
	if err := validateInputs(result.Inputs); err != nil {
		return err
	}
	if err := validateTaskMetadata(result.Metadata); err != nil {
		return err
	}
	if !validID(result.Initiator) {
		return invalid("initiator is invalid")
	}
	for _, message := range result.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
	}
	if err := validateOptionalIDs(result.ExecutionHistory); err != nil {
		return err
	}
	if result.CurrentExecution != "" && !validID(result.CurrentExecution) {
		return invalid("current_execution is invalid")
	}
	if result.Question != nil {
		return result.Question.Validate()
	}
	return nil
}

// TaskEditRequest is the task.edit input. The task is carried explicitly for
// MCP callers and is optional for REST callers whose route supplies it.
type TaskEditRequest struct {
	TaskID      string            `json:"task_id,omitempty" schema:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Validate checks the optional task identity, replacement title, optional
// description and bounded metadata.
func (request TaskEditRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if request.Title == "" {
		return invalid("task title is empty")
	}
	return validateTaskMetadata(request.Metadata)
}

// TaskEditResult is the task.edit result.
type TaskEditResult struct {
	ContentVersion   uint64 `json:"content_version" schema:"version"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks both committed versions.
func (result TaskEditResult) Validate() error {
	if result.ContentVersion == 0 || result.LifecycleVersion == 0 {
		return invalid("task content and lifecycle versions must be positive")
	}
	return nil
}

// TaskCommentRequest is the task.comment input. The task is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it.
type TaskCommentRequest struct {
	TaskID     string   `json:"task_id,omitempty" schema:"id"`
	Body       string   `json:"body"`
	References []string `json:"references,omitempty"`
}

// Validate checks the optional task identity, body and optional bounded
// references.
func (request TaskCommentRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if request.Body == "" {
		return invalid("task comment body is empty")
	}
	return validateReferences(request.References)
}

// TaskCommentResult is the task.comment result.
type TaskCommentResult struct {
	ContentVersion   uint64 `json:"content_version" schema:"version"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks both committed versions.
func (result TaskCommentResult) Validate() error {
	if result.ContentVersion == 0 || result.LifecycleVersion == 0 {
		return invalid("task content and lifecycle versions must be positive")
	}
	return nil
}

// TaskSelectPipelineRequest is the task.select_pipeline input. The task is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type TaskSelectPipelineRequest struct {
	TaskID     string            `json:"task_id,omitempty" schema:"id"`
	PipelineID string            `json:"pipeline_id" schema:"id"`
	Inputs     map[string]string `json:"inputs,omitempty"`
}

// Validate checks the optional task identity, pipeline identity and optional
// inputs.
func (request TaskSelectPipelineRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	return validateInputs(request.Inputs)
}

// TaskSelectPipelineResult is the task.select_pipeline result.
type TaskSelectPipelineResult struct {
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks the committed lifecycle version.
func (result TaskSelectPipelineResult) Validate() error {
	if result.LifecycleVersion == 0 {
		return invalid("task lifecycle version must be positive")
	}
	return nil
}

// TaskStartRequest is the task.start input. The task is carried explicitly for
// MCP callers and is optional for REST callers whose route supplies it.
type TaskStartRequest struct {
	TaskID string `json:"task_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded task identity.
func (request TaskStartRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	return nil
}

// TaskStartResult is the task.start result.
type TaskStartResult struct {
	ExecutionID      string `json:"execution_id"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks the admitted execution identity and lifecycle version.
func (result TaskStartResult) Validate() error {
	if !validID(result.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if result.LifecycleVersion == 0 {
		return invalid("task lifecycle version must be positive")
	}
	return nil
}

// TaskCreateAndStartRequest is the task.create_and_start input. The project is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type TaskCreateAndStartRequest struct {
	ProjectID   string            `json:"project_id,omitempty" schema:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	PipelineID  string            `json:"pipeline_id" schema:"id"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Validate checks the optional project identity, title, optional description,
// pipeline identity, inputs and bounded metadata.
func (request TaskCreateAndStartRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Title == "" {
		return invalid("task title is empty")
	}
	if !validID(request.PipelineID) {
		return invalid("pipeline_id is invalid")
	}
	if err := validateInputs(request.Inputs); err != nil {
		return err
	}
	return validateTaskMetadata(request.Metadata)
}

// TaskCreateAndStartResult is the task.create_and_start result.
type TaskCreateAndStartResult struct {
	TaskID           string `json:"task_id"`
	ExecutionID      string `json:"execution_id"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks both identities and the committed lifecycle version.
func (result TaskCreateAndStartResult) Validate() error {
	if !validID(result.TaskID) {
		return invalid("task_id is invalid")
	}
	if !validID(result.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if result.LifecycleVersion == 0 {
		return invalid("task lifecycle version must be positive")
	}
	return nil
}

// TaskAskRequest is the task.ask input. Assignment is the
// `role/<role>` human approval grammar or an exact service identity. The task
// is carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type TaskAskRequest struct {
	TaskID     string   `json:"task_id,omitempty" schema:"id"`
	Question   string   `json:"question"`
	Assignment string   `json:"assignment"`
	References []string `json:"references,omitempty"`
}

// Validate checks the optional task identity, question, assignment grammar and
// optional references.
func (request TaskAskRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if request.Question == "" {
		return invalid("task question is empty")
	}
	if !validAssignment(request.Assignment) {
		return invalid("assignment %q must be role/<role> or service/<id>", request.Assignment)
	}
	return validateReferences(request.References)
}

// TaskAskResult is the task.ask result. QuestionID is optional: the capability's
// AskResult has no question id, so a handler may fill it when it can read the
// committed question and omits it on replay after the question is answered.
// Clients learn the outstanding question identity and version from task.read.
type TaskAskResult struct {
	QuestionID       string `json:"question_id,omitempty" schema:"id"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
	QuestionVersion  uint64 `json:"question_version" schema:"version"`
}

// Validate checks the optional question identity and both committed versions.
func (result TaskAskResult) Validate() error {
	if result.QuestionID != "" && !validID(result.QuestionID) {
		return invalid("question_id is invalid")
	}
	if result.LifecycleVersion == 0 || result.QuestionVersion == 0 {
		return invalid("task lifecycle and question versions must be positive")
	}
	return nil
}

// TaskReplyRequest is the task.reply input. The task is carried explicitly for
// MCP callers and is optional for REST callers whose route supplies it.
type TaskReplyRequest struct {
	TaskID                  string `json:"task_id,omitempty" schema:"id"`
	QuestionID              string `json:"question_id" schema:"id"`
	ExpectedQuestionVersion uint64 `json:"expected_question_version" schema:"version"`
	Answer                  string `json:"answer"`
}

// Validate checks the optional task identity, question identity, expected
// version and answer.
func (request TaskReplyRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if !validID(request.QuestionID) {
		return invalid("question_id is invalid")
	}
	if request.ExpectedQuestionVersion == 0 {
		return invalid("expected_question_version must be positive")
	}
	if request.Answer == "" {
		return invalid("task answer is empty")
	}
	return nil
}

// TaskReplyResult is the task.reply result.
type TaskReplyResult struct {
	ExecutionID      string `json:"execution_id"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
	QuestionVersion  uint64 `json:"question_version" schema:"version"`
}

// Validate checks the admitted execution identity and both versions.
func (result TaskReplyResult) Validate() error {
	if !validID(result.ExecutionID) {
		return invalid("execution_id is invalid")
	}
	if result.LifecycleVersion == 0 || result.QuestionVersion == 0 {
		return invalid("task lifecycle and question versions must be positive")
	}
	return nil
}

// TaskCancelRequest is the task.cancel input. The task is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it.
type TaskCancelRequest struct {
	TaskID string `json:"task_id,omitempty" schema:"id"`
}

// Validate checks an optional bounded task identity.
func (request TaskCancelRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	return nil
}

// TaskCancelResult is the task.cancel result.
type TaskCancelResult struct {
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks the committed lifecycle version.
func (result TaskCancelResult) Validate() error {
	if result.LifecycleVersion == 0 {
		return invalid("task lifecycle version must be positive")
	}
	return nil
}

// TaskHistoryRequest is the task.history input. The task is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it.
type TaskHistoryRequest struct {
	TaskID string `json:"task_id,omitempty" schema:"id"`
	Page   *Page  `json:"page,omitempty"`
}

// Validate checks the optional task identity and page bounds.
func (request TaskHistoryRequest) Validate() error {
	if request.TaskID != "" && !validID(request.TaskID) {
		return invalid("task_id is invalid")
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// TaskHistoryEntry is one kind-discriminated authoritative history
// observation. An execution entry carries its execution identity and state; a
// message or question entry carries its conversation payload instead.
type TaskHistoryEntry struct {
	Kind        string        `json:"kind"`
	At          time.Time     `json:"at"`
	ExecutionID string        `json:"execution_id,omitempty" schema:"id"`
	State       string        `json:"state,omitempty"`
	Message     *TaskMessage  `json:"message,omitempty"`
	Question    *TaskQuestion `json:"question,omitempty"`
}

// Validate checks the history kind and time, then the kind-specific payload.
func (entry TaskHistoryEntry) Validate() error {
	if !validHistoryKind(entry.Kind) {
		return invalid("unknown history kind %q", entry.Kind)
	}
	if entry.At.IsZero() {
		return invalid("history entry time is zero")
	}
	switch entry.Kind {
	case "execution":
		if !validID(entry.ExecutionID) {
			return invalid("history execution_id is invalid")
		}
		if !validExecutionState(entry.State) {
			return invalid("unknown history execution state %q", entry.State)
		}
		if entry.Message != nil || entry.Question != nil {
			return invalid("execution history entry carries a conversation payload")
		}
	case "message":
		if entry.ExecutionID != "" || entry.State != "" {
			return invalid("message history entry carries execution fields")
		}
		if entry.Message == nil {
			return invalid("message history entry has no message")
		}
		if err := entry.Message.Validate(); err != nil {
			return err
		}
		if entry.Question != nil {
			return invalid("message history entry carries a question")
		}
	case "question":
		if entry.ExecutionID != "" || entry.State != "" {
			return invalid("question history entry carries execution fields")
		}
		if entry.Question == nil {
			return invalid("question history entry has no question")
		}
		if err := entry.Question.Validate(); err != nil {
			return err
		}
		if entry.Message != nil {
			return invalid("question history entry carries a message")
		}
	}
	return nil
}

// TaskHistoryResult is the task.history result.
type TaskHistoryResult struct {
	Entries    []TaskHistoryEntry `json:"entries"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// Validate checks every history entry and the optional cursor bound.
func (result TaskHistoryResult) Validate() error {
	for _, entry := range result.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// TaskListRequest is the task.list input. The project is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it.
type TaskListRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Status    string `json:"status,omitempty"`
	Page      *Page  `json:"page,omitempty"`
}

// Validate checks the optional project identity, optional task state filter
// and page bounds.
func (request TaskListRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Status != "" && !validTaskState(request.Status) {
		return invalid("unknown task status %q", request.Status)
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// TaskSummaryView is one scoped task summary observation in task.list.
type TaskSummaryView struct {
	TaskID           string `json:"task_id"`
	Title            string `json:"title"`
	State            string `json:"state"`
	SelectedPipeline string `json:"selected_pipeline,omitempty" schema:"id"`
	CurrentExecution string `json:"current_execution,omitempty" schema:"id"`
	ContentVersion   uint64 `json:"content_version" schema:"version"`
	LifecycleVersion uint64 `json:"lifecycle_version" schema:"version"`
}

// Validate checks the task identity, title, state, optional pipeline/execution
// identities and both versions.
func (view TaskSummaryView) Validate() error {
	if !validID(view.TaskID) {
		return invalid("task_id is invalid")
	}
	if view.Title == "" {
		return invalid("task title is empty")
	}
	if !validTaskState(view.State) {
		return invalid("unknown task state %q", view.State)
	}
	if view.SelectedPipeline != "" && !validID(view.SelectedPipeline) {
		return invalid("selected_pipeline is invalid")
	}
	if view.CurrentExecution != "" && !validID(view.CurrentExecution) {
		return invalid("current_execution is invalid")
	}
	if view.ContentVersion == 0 || view.LifecycleVersion == 0 {
		return invalid("task content and lifecycle versions must be positive")
	}
	return nil
}

// TaskListResult is the task.list result.
type TaskListResult struct {
	Tasks      []TaskSummaryView `json:"tasks"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// Validate checks every task summary and the optional cursor bound.
func (result TaskListResult) Validate() error {
	for _, task := range result.Tasks {
		if err := task.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// taskStates is the durable task state vocabulary.
var taskStates = []string{"open", "waiting", "done", "cancelled"}

// validTaskState reports whether state is a declared task state.
func validTaskState(state string) bool {
	for _, candidate := range taskStates {
		if state == candidate {
			return true
		}
	}
	return false
}

// historyKinds is the durable history kind vocabulary.
var historyKinds = []string{"execution", "message", "question"}

// validHistoryKind reports whether kind is a declared history kind.
func validHistoryKind(kind string) bool {
	for _, candidate := range historyKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// validateOptionalIDs checks an optional bounded identity list.
func validateOptionalIDs(ids []string) error {
	for _, value := range ids {
		if !validID(value) {
			return invalid("identity %q is invalid", value)
		}
	}
	return nil
}

// validateReferences checks the opaque durable-reference rule: at most
// MaxReferences non-empty strings, each at most MaxReferenceBytes. There is no
// character grammar, so commits, digests and restoration URLs all pass.
func validateReferences(references []string) error {
	if len(references) > MaxReferences {
		return invalid("references has %d entries; at most %d are allowed", len(references), MaxReferences)
	}
	for _, reference := range references {
		if reference == "" {
			return invalid("reference is empty")
		}
		if len(reference) > MaxReferenceBytes {
			return invalid("reference is %d bytes; at most %d are allowed", len(reference), MaxReferenceBytes)
		}
	}
	return nil
}

// validateTaskMetadata checks the opaque task metadata object bound. Metadata
// is a JSON object of string values; the Shell versions and snapshots it
// without interpreting labels or workflow stages.
func validateTaskMetadata(metadata map[string]string) error {
	if len(metadata) == 0 {
		return nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return invalid("task metadata cannot be encoded")
	}
	if len(encoded) > MaxTaskMetadataBytes {
		return invalid("task metadata is %d bytes; at most %d are allowed", len(encoded), MaxTaskMetadataBytes)
	}
	return nil
}

// validAssignment checks the `role/<role>` or `service/<id>` assignment
// grammar. It accepts no whitespace, empty segment or extra segment.
func validAssignment(value string) bool {
	kind, rest, found := strings.Cut(value, "/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return false
	}
	switch kind {
	case "role":
		return validHumanRole(rest)
	case "service":
		return validID(rest)
	default:
		return false
	}
}
