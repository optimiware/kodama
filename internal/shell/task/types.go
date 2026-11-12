// Package task declares the task conversation, its two independent optimistic
// versions and the four atomic task commit units.
//
// Task content (title, description, metadata, messages) has its own version;
// lifecycle operations (select, start, ask, reply, cancel) advance the
// lifecycle version. A content edit can never start work, answer a question,
// replace the current execution or change the pipeline selection. A task
// conversation's execution progress is derived from the execution itself, so
// there is no divergent task "running" state.
package task

import (
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// State is the durable task state. Execution progress is never duplicated here.
type State string

const (
	// StateOpen means work can be started, is executing, or needs operator
	// follow-up after failure.
	StateOpen State = "open"
	// StateWaiting means a conversation question is assigned and the prior
	// execution was cancelled.
	StateWaiting State = "waiting"
	// StateDone means the current execution succeeded while the task was open.
	StateDone State = "done"
	// StateCancelled means the task was explicitly cancelled.
	StateCancelled State = "cancelled"
)

var states = [...]State{StateOpen, StateWaiting, StateDone, StateCancelled}

// States returns every task state in declaration order.
func States() []State {
	return append([]State(nil), states[:]...)
}

// Valid reports whether the state is one of the declared states.
func (state State) Valid() bool {
	for _, candidate := range states {
		if state == candidate {
			return true
		}
	}
	return false
}

// QuestionID identifies one conversation question.
type QuestionID string

// Message is one conversation message. References are durable restoration
// pointers, not proof of action success.
type Message struct {
	Author     id.PrincipalID `json:"author"`
	At         time.Time      `json:"at"`
	Body       string         `json:"body"`
	References []string       `json:"references,omitempty"`
}

// Question is the single outstanding conversation question of a waiting task.
type Question struct {
	ID         QuestionID         `json:"id"`
	Body       string             `json:"body"`
	Assignment request.Assignment `json:"assignment"`
	References []string           `json:"references,omitempty"`
	Version    uint64             `json:"version"`
	AskedBy    id.PrincipalID     `json:"asked_by"`
	At         time.Time          `json:"at"`
}

// AdmittedAction is one resolved action row admitted with an execution. It is
// the pipeline-owned admission value so the pipeline package need not import
// the task package; Index is the ordered position and Kind/Timeout are resolved
// at admission so the store persists them without re-deriving policy from YAML.
// Exactly one of Container or Request is present for the matching Kind.
type AdmittedAction = pipeline.AdmittedAction

// Task is one conversation and its lifecycle state.
type Task struct {
	ID               id.TaskID              `json:"id"`
	Scope            tenancy.Scope          `json:"scope"`
	Title            string                 `json:"title"`
	Description      string                 `json:"description,omitempty"`
	Metadata         map[string]string      `json:"metadata,omitempty"`
	Messages         []Message              `json:"messages,omitempty"`
	ContentVersion   uint64                 `json:"content_version"`
	LifecycleVersion uint64                 `json:"lifecycle_version"`
	State            State                  `json:"state"`
	SelectedPipeline pipeline.PipelineID    `json:"selected_pipeline,omitempty"`
	Inputs           map[id.InputKey]string `json:"inputs,omitempty"`
	Initiator        id.PrincipalID         `json:"initiator"`
	ExecutionHistory []id.ExecutionID       `json:"execution_history,omitempty"`
	CurrentExecution id.ExecutionID         `json:"current_execution,omitempty"`
	Question         *Question              `json:"question,omitempty"`
}

// StartUnit is the complete atomic start/admission commit unit. It carries the
// task and pipeline versions, the admitted snapshot, the resolved pipeline and
// ordered action rows, the initiating and execution principals, the queued
// execution identity and every write that must commit with it.
type StartUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	TaskID                   id.TaskID                 `json:"task_id"`
	PipelineID               pipeline.PipelineID       `json:"pipeline_id"`
	ExpectedLifecycleVersion uint64                    `json:"expected_lifecycle_version"`
	ExpectedPipelineVersion  uint64                    `json:"expected_pipeline_version"`
	Snapshot                 pipeline.Snapshot         `json:"snapshot"`
	Actions                  []AdmittedAction          `json:"actions"`
	Initiator                id.PrincipalID            `json:"initiator"`
	ExecutionPrincipal       id.PrincipalID            `json:"execution_principal"`
	NewExecutionID           id.ExecutionID            `json:"new_execution_id"`
	At                       time.Time                 `json:"at"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// StartResult is the committed start result.
type StartResult struct {
	ExecutionID      id.ExecutionID `json:"execution_id"`
	LifecycleVersion uint64         `json:"lifecycle_version"`
	Replayed         bool           `json:"replayed"`
}

// AskUnit is the complete atomic conversation-handoff commit unit. It appends
// the question, marks the task waiting, cancels the nonterminal execution,
// clears the credential and discards staged outputs in one transaction.
//
// Commit is the Kernel cancel reduction for the task's non-terminal current
// execution (`flow.CancelFact`). It is nil when the task has no execution or
// its current execution is already terminal: a terminal predecessor row is
// preserved, never rewritten.
type AskUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	TaskID                   id.TaskID                 `json:"task_id"`
	ExpectedLifecycleVersion uint64                    `json:"expected_lifecycle_version"`
	ExpectedExecutionVersion uint64                    `json:"expected_execution_version"`
	QuestionID               QuestionID                `json:"question_id"`
	Question                 string                    `json:"question"`
	Assignment               request.Assignment        `json:"assignment"`
	References               []string                  `json:"references,omitempty"`
	CredentialUpdate         *token.Credential         `json:"credential_update,omitempty"`
	Commit                   *flow.Commit              `json:"commit,omitempty"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// AskResult is the committed handoff result.
type AskResult struct {
	LifecycleVersion uint64 `json:"lifecycle_version"`
	QuestionVersion  uint64 `json:"question_version"`
	Replayed         bool   `json:"replayed"`
}

// ReplyUnit is the complete atomic reply commit unit. It appends the answer,
// reopens the task and queues exactly one fresh execution with an updated
// conversation snapshot and resolved action rows, after rechecking the
// initiator's current start authority.
type ReplyUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	TaskID                   id.TaskID                 `json:"task_id"`
	PipelineID               pipeline.PipelineID       `json:"pipeline_id"`
	ExpectedLifecycleVersion uint64                    `json:"expected_lifecycle_version"`
	QuestionID               QuestionID                `json:"question_id"`
	ExpectedQuestionVersion  uint64                    `json:"expected_question_version"`
	Assignment               request.Assignment        `json:"assignment"`
	Answer                   string                    `json:"answer"`
	Initiator                id.PrincipalID            `json:"initiator"`
	ExpectedPipelineVersion  uint64                    `json:"expected_pipeline_version"`
	Snapshot                 pipeline.Snapshot         `json:"snapshot"`
	Actions                  []AdmittedAction          `json:"actions"`
	ExecutionPrincipal       id.PrincipalID            `json:"execution_principal"`
	NewExecutionID           id.ExecutionID            `json:"new_execution_id"`
	At                       time.Time                 `json:"at"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// ReplyResult is the committed reply result.
type ReplyResult struct {
	ExecutionID      id.ExecutionID `json:"execution_id"`
	LifecycleVersion uint64         `json:"lifecycle_version"`
	QuestionVersion  uint64         `json:"question_version"`
	Replayed         bool           `json:"replayed"`
}

// CancelUnit is the complete atomic task-cancellation commit unit.
//
// Commit is the Kernel cancel reduction for the task's non-terminal current
// execution (`flow.CancelFact`). It is nil when the task has no execution or
// its current execution is already terminal: a terminal predecessor row is
// preserved, never rewritten.
type CancelUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	TaskID                   id.TaskID                 `json:"task_id"`
	ExpectedLifecycleVersion uint64                    `json:"expected_lifecycle_version"`
	ExpectedExecutionVersion uint64                    `json:"expected_execution_version"`
	CredentialUpdate         *token.Credential         `json:"credential_update,omitempty"`
	Commit                   *flow.Commit              `json:"commit,omitempty"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// CancelResult is the committed cancellation result.
type CancelResult struct {
	LifecycleVersion uint64 `json:"lifecycle_version"`
	Replayed         bool   `json:"replayed"`
}
