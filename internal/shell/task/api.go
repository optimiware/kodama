// The task service API.
//
// types.go and ports.go declare the task conversation, its two optimistic
// versions and the start/ask/reply/cancel commit units. This file adds the
// creation, editing, progress and read seams.
//
// The declarations below are the exact API the behavioral tests exercise:
// one Service that authorizes, validates, admits and builds one complete
// commit unit per operation, and the consumer-owned read/clock/id ports it
// needs. No progress, start-intent or request entity is added: task progress
// is derived from the current execution.
package task

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock supplies the authoritative time for one task operation.
type Clock interface {
	Now() time.Time
}

// IDSource supplies every identity one task operation needs. Production uses
// secure randomness; tests inject deterministic sequences.
type IDSource interface {
	NewTaskID() (id.TaskID, error)
	NewQuestionID() (QuestionID, error)
	NewExecutionID() (id.ExecutionID, error)
	NewPrincipalID() (id.PrincipalID, error)
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// Admission is the immutable pipeline admission seam. It resolves the stored
// definition, validates inputs, pins images and returns the exact snapshot and
// ordered action rows committed with the queued execution.
type Admission interface {
	Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
}

// PipelineReader reads one scoped pipeline definition. It is used to validate
// an explicit selection and record its current version.
type PipelineReader interface {
	Get(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error)
}

// ExecutionView is the authoritative current execution fact a task operation
// needs: its version and whether its workloads are confirmed stopped. It is
// read evidence for one operation, never durable permission.
type ExecutionView struct {
	ID           id.ExecutionID
	Version      uint64
	State        kernel.ExecutionState
	StopEvidence kernel.StopEvidence
}

// ExecutionReader reads one scoped execution view.
type ExecutionReader interface {
	Get(context.Context, tenancy.Scope, id.ExecutionID) (ExecutionView, error)
}

// HistoryKind names one durable history entry category.
type HistoryKind string

const (
	// HistoryExecution is one execution of the task.
	HistoryExecution HistoryKind = "execution"
	// HistoryMessage is one conversation message.
	HistoryMessage HistoryKind = "message"
	// HistoryQuestion is one conversation question.
	HistoryQuestion HistoryKind = "question"
)

// HistoryQuery is one stable history page request.
type HistoryQuery struct {
	Limit  int
	Cursor string
}

// HistoryEntry is one authoritative history entry.
type HistoryEntry struct {
	Kind        HistoryKind
	ExecutionID id.ExecutionID
	State       kernel.ExecutionState
	Message     *Message
	Question    *Question
	At          time.Time
}

// HistoryPage is one stable history page.
type HistoryPage struct {
	Entries    []HistoryEntry
	NextCursor string
}

// Reader reads one scoped task and its history. The adapter applies scope; it
// exposes no mutation method.
type Reader interface {
	Get(context.Context, tenancy.Scope, id.TaskID) (Task, error)
	History(context.Context, tenancy.Scope, id.TaskID, HistoryQuery) (HistoryPage, error)
}

// CreateUnit is the complete atomic task-creation commit unit. Start is nil for
// a plain create; otherwise it carries the admission content committed in the
// same transaction. The single operation record (Receipt/Audit/Events/
// Authority/PostCommit) lives here; the nested StartUnit carries no second
// receipt, audit, event, authority or post-commit record.
type CreateUnit struct {
	Owner            tenancy.Scope             `json:"owner"`
	TaskID           id.TaskID                 `json:"task_id"`
	Title            string                    `json:"title"`
	Description      string                    `json:"description,omitempty"`
	Metadata         map[string]string         `json:"metadata,omitempty"`
	SelectedPipeline pipeline.PipelineID       `json:"selected_pipeline,omitempty"`
	Inputs           map[id.InputKey]string    `json:"inputs,omitempty"`
	Initiator        id.PrincipalID            `json:"initiator"`
	At               time.Time                 `json:"at"`
	Start            *StartUnit                `json:"start,omitempty"`
	Receipt          receipt.Record            `json:"receipt"`
	Audit            []event.Audit             `json:"audit"`
	Events           []event.Envelope          `json:"events"`
	Authority        []authorize.AuthorityRead `json:"authority"`
	PostCommit       []event.PostCommitEffect  `json:"post_commit"`
}

// CreateResult is the committed creation result. ExecutionID is empty for a
// plain create.
type CreateResult struct {
	TaskID           id.TaskID      `json:"task_id"`
	ExecutionID      id.ExecutionID `json:"execution_id,omitempty"`
	ContentVersion   uint64         `json:"content_version"`
	LifecycleVersion uint64         `json:"lifecycle_version"`
	Replayed         bool           `json:"replayed"`
}

// CreatePort applies one complete creation commit unit.
type CreatePort interface {
	Create(context.Context, CreateUnit) (CreateResult, error)
}

// EditKind names one task edit operation.
type EditKind string

const (
	// EditContent replaces title/description/metadata at the content version.
	EditContent EditKind = "content"
	// EditComment appends one conversation message at the content version.
	EditComment EditKind = "comment"
	// EditSelectPipeline records an explicit pipeline selection as a lifecycle
	// operation and records the selecting principal as the start authority.
	EditSelectPipeline EditKind = "select_pipeline"
)

// EditUnit is the complete atomic task-edit commit unit. A content or comment
// edit carries ExpectedContentVersion and no lifecycle change; a selection
// carries ExpectedLifecycleVersion, the pipeline identity/version and the
// start-authority principal. Every edit carries one receipt and one audit
// record and publishes no event.
type EditUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	TaskID                   id.TaskID                 `json:"task_id"`
	Kind                     EditKind                  `json:"kind"`
	ExpectedContentVersion   uint64                    `json:"expected_content_version,omitempty"`
	ExpectedLifecycleVersion uint64                    `json:"expected_lifecycle_version,omitempty"`
	Title                    string                    `json:"title,omitempty"`
	Description              string                    `json:"description,omitempty"`
	Metadata                 map[string]string         `json:"metadata,omitempty"`
	Message                  *Message                  `json:"message,omitempty"`
	PipelineID               pipeline.PipelineID       `json:"pipeline_id,omitempty"`
	Inputs                   map[id.InputKey]string    `json:"inputs,omitempty"`
	StartAuthority           id.PrincipalID            `json:"start_authority,omitempty"`
	ExpectedPipelineVersion  uint64                    `json:"expected_pipeline_version,omitempty"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// EditResult is the committed edit result.
type EditResult struct {
	ContentVersion   uint64 `json:"content_version,omitempty"`
	LifecycleVersion uint64 `json:"lifecycle_version,omitempty"`
	Replayed         bool   `json:"replayed"`
}

// EditPort applies one complete edit commit unit.
type EditPort interface {
	Edit(context.Context, EditUnit) (EditResult, error)
}

// ProgressOutcome is the terminal observation of one execution.
type ProgressOutcome string

const (
	// ProgressSucceeded reports that the execution succeeded.
	ProgressSucceeded ProgressOutcome = "succeeded"
	// ProgressFailed reports that the execution failed and the task stays open.
	ProgressFailed ProgressOutcome = "failed"
)

// ProgressCommand is one execution progress observation. It is derived from
// the execution, not from an independently mutable task progress entity.
type ProgressCommand struct {
	Scope       tenancy.Scope
	TaskID      id.TaskID
	ExecutionID id.ExecutionID
	Outcome     ProgressOutcome
}

// ProgressUnit is the atomic current-only completion commit unit.
type ProgressUnit struct {
	Owner                    tenancy.Scope  `json:"owner"`
	TaskID                   id.TaskID      `json:"task_id"`
	ExecutionID              id.ExecutionID `json:"execution_id"`
	ExpectedLifecycleVersion uint64         `json:"expected_lifecycle_version"`
	At                       time.Time      `json:"at"`
}

// ProgressResult is the committed progress result.
type ProgressResult struct {
	LifecycleVersion uint64 `json:"lifecycle_version"`
	Replayed         bool   `json:"replayed"`
}

// ProgressPort applies one current-only completion commit unit.
type ProgressPort interface {
	Apply(context.Context, ProgressUnit) (ProgressResult, error)
}

// ProgressStatus is the observed progress result. Applied reports whether the
// observation was current and committed; Replayed reports a port replay.
type ProgressStatus struct {
	State    State
	Applied  bool
	Replayed bool
}

// CreateCommand is one authorized task creation. Start asks for
// create-and-start: creation plus admission in one commit unit.
type CreateCommand struct {
	Actor       authorize.Principal
	Scope       tenancy.Scope
	Title       string
	Description string
	Metadata    []byte
	PipelineID  pipeline.PipelineID
	Inputs      map[id.InputKey]string
	Start       bool
	CallerKey   string
	Content     receipt.Content
}

// EditCommand is one authorized task edit.
type EditCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	TaskID                   id.TaskID
	Kind                     EditKind
	ExpectedContentVersion   uint64
	ExpectedLifecycleVersion uint64
	Title                    string
	Description              string
	Metadata                 []byte
	Body                     string
	References               []string
	PipelineID               pipeline.PipelineID
	Inputs                   map[id.InputKey]string
	CallerKey                string
	Content                  receipt.Content
}

// StartCommand is one authorized ordinary start.
type StartCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	TaskID                   id.TaskID
	ExpectedLifecycleVersion uint64
	CallerKey                string
	Content                  receipt.Content
}

// AskCommand is one authorized conversation-handoff command.
type AskCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	TaskID                   id.TaskID
	ExpectedLifecycleVersion uint64
	Question                 string
	Assignment               request.Assignment
	References               []string
	CallerKey                string
	Content                  receipt.Content
}

// ReplyCommand is one authorized assigned reply.
type ReplyCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	TaskID                   id.TaskID
	ExpectedLifecycleVersion uint64
	QuestionID               QuestionID
	ExpectedQuestionVersion  uint64
	Answer                   string
	CallerKey                string
	Content                  receipt.Content
}

// CancelCommand is one authorized task cancellation.
type CancelCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	TaskID                   id.TaskID
	ExpectedLifecycleVersion uint64
	CallerKey                string
	Content                  receipt.Content
}

// HistoryCommand is one authorized history read.
type HistoryCommand struct {
	Actor  authorize.Principal
	Scope  tenancy.Scope
	TaskID id.TaskID
	Limit  int
	Cursor string
}

// ServiceConfig supplies every task boundary. A nil dependency is refused
// rather than defaulted.
type ServiceConfig struct {
	Clock      Clock
	IDs        IDSource
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Principals identity.PrincipalReader
	Pipelines  PipelineReader
	Reader     Reader
	Executions ExecutionReader
	// Aggregates is the consumer-owned scoped aggregate reader. Both task
	// cancel paths load the version-guarded execution aggregate through it to
	// build the Kernel cancel reduction. It is required: a task cancellation
	// that cannot read the aggregate fails closed before the port.
	Aggregates     ExecutionAggregateReader
	Admission      Admission
	Creates        CreatePort
	Edits          EditPort
	Starts         StartPort
	Asks           AskPort
	Replies        ReplyPort
	Cancels        CancelPort
	Progress       ProgressPort
	DefaultTimeout time.Duration
}
