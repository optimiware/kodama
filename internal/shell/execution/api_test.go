package execution_test

// API of internal/shell/execution pinned by these tests.
//
// The execution operation surface is task-free pipeline.start, inspect/list,
// cancel and container output staging, plus a scoped aggregate read so
// Cancel/StageOutputs can reduce through the pure Kernel flow. The names and
// signatures below are the exact API the behavioral tests in this directory
// exercise.
//
//	type Clock interface { Now() time.Time }
//
//	type IDSource interface {
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	    NewExecutionID() (id.ExecutionID, error)
//	}
//
//	// StateReader loads one scoped execution aggregate. The scope is the
//	// caller's verified scope: an execution owned by another tenant/project is
//	// reported as not_found, never returned.
//	type StateReader interface {
//	    Load(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)
//	}
//
//	type ListFilter struct {
//	    TaskID     id.TaskID
//	    PipelineID pipeline.PipelineID
//	    State      kernel.ExecutionState
//	    Limit      int
//	    Cursor     string
//	}
//
//	type Page struct {
//	    Items []View
//	    Next  string
//	}
//
//	type View struct {
//	    Owner         tenancy.Scope
//	    ExecutionID   id.ExecutionID
//	    TaskID        id.TaskID
//	    PipelineID    pipeline.PipelineID
//	    State         kernel.ExecutionState
//	    Version       uint64
//	    CurrentAction int
//	    WorkspaceID   id.WorkspaceID
//	    Actions       []ActionView
//	    Cleanup       kernel.Cleanup
//	    Failure       *kernel.Failure
//	}
//
//	type ActionView struct {
//	    ID               id.ActionID
//	    Index            int
//	    Name             id.ActionName
//	    Kind             definition.ActionKind
//	    State            kernel.ActionState
//	    Version          uint64
//	    Timeout          time.Duration
//	    ActivatedAt      time.Time
//	    Deadline         time.Time
//	    Outputs          kernel.Outputs
//	    StagedOutputs    kernel.Outputs
//	    StagedOutputsSet bool
//	    Stop             kernel.StopEvidence
//	    Decision         string
//	    Actor            id.PrincipalID
//	    AnsweredAt       time.Time
//	    Subject          *definition.Subject
//	}
//
//	type Reader interface {
//	    Inspect(context.Context, tenancy.Scope, id.ExecutionID) (View, error)
//	    List(context.Context, tenancy.Scope, ListFilter) (Page, error)
//	}
//
//	type StartUnit struct {
//	    Owner                   tenancy.Scope
//	    PipelineID              pipeline.PipelineID
//	    ExpectedPipelineVersion uint64
//	    Snapshot                pipeline.Snapshot
//	    Actions                 []pipeline.AdmittedAction
//	    Initiator               id.PrincipalID
//	    ExecutionPrincipal      id.PrincipalID
//	    NewExecutionID          id.ExecutionID
//	    At                      time.Time
//	    Receipt                 receipt.Record
//	    Audit                   []event.Audit
//	    Events                  []event.Envelope
//	    Authority               []authorize.AuthorityRead
//	    PostCommit              []event.PostCommitEffect
//	}
//
//	type StartResult struct {
//	    ExecutionID     id.ExecutionID
//	    PipelineVersion uint64
//	    Replayed        bool
//	}
//
//	type StartPort interface {
//	    Start(context.Context, StartUnit) (StartResult, error)
//	}
//
//	type StartCommand struct {
//	    Actor              authorize.Principal
//	    Scope              tenancy.Scope
//	    Admission          pipeline.AdmissionResult
//	    Initiator          id.PrincipalID
//	    ExecutionPrincipal id.PrincipalID
//	    CallerKey          string
//	    Content            receipt.Content
//	}
//
//	type InspectCommand struct {
//	    Actor       authorize.Principal
//	    Scope       tenancy.Scope
//	    ExecutionID id.ExecutionID
//	}
//
//	type ListCommand struct {
//	    Actor  authorize.Principal
//	    Scope  tenancy.Scope
//	    Filter ListFilter
//	}
//
//	type CancelCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    ExecutionID              id.ExecutionID
//	    ExpectedExecutionVersion uint64
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type OutputsCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    ExecutionID              id.ExecutionID
//	    ActionID                 id.ActionID
//	    ExpectedExecutionVersion uint64
//	    Outputs                  kernel.Outputs
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type Config struct {
//	    Clock       Clock
//	    IDs         IDSource
//	    Authorizer  authorize.Authorizer
//	    Authority   authorize.AuthorityReader
//	    Reader      Reader
//	    State       StateReader
//	    Transitions TransitionPort
//	    Starts      StartPort
//	}
//
//	type Service struct{ /* ... */ }
//	func NewService(Config) (*Service, error)
//	func (*Service) Start(context.Context, StartCommand) (StartResult, error)
//	func (*Service) Inspect(context.Context, InspectCommand) (View, error)
//	func (*Service) List(context.Context, ListCommand) (Page, error)
//	func (*Service) Cancel(context.Context, CancelCommand) (TransitionResult, error)
//	func (*Service) StageOutputs(context.Context, OutputsCommand) (TransitionResult, error)
//
// Operation mapping (operation registry):
//
//   - Start         -> pipeline.start, required receipt, authority reads
//                      membership + pipeline_version.
//   - Inspect       -> execution.inspect, no receipt, authority read membership.
//   - List          -> execution.list, no receipt, authority read membership.
//   - Cancel        -> execution.cancel, required receipt, authority reads
//                      membership + execution_version.
//   - StageOutputs  -> execution.action.outputs, required receipt, authority
//                      reads execution_running + action_deadline +
//                      execution_version.
//
// A reduced nil commit is a converged duplicate. Cancel and StageOutputs return
// TransitionResult{Replayed:true} reflecting the loaded state and never call
// TransitionPort again. A non-nil reduction error is propagated unchanged.
// TransitionUnit.Owner is always the caller's verified scope.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Compile-time pins for the added interfaces and method signatures.
var (
	_ execution.Clock          = fixedClock{}
	_ execution.IDSource       = (*sequentialIDs)(nil)
	_ execution.StateReader    = (*fakeStore)(nil)
	_ execution.Reader         = (*fakeReader)(nil)
	_ execution.TransitionPort = (*fakeTransitionPort)(nil)
	_ execution.StartPort      = (*fakeStartPort)(nil)

	_ func(execution.Config) (*execution.Service, error)                                                      = execution.NewService
	_ func(*execution.Service, context.Context, execution.StartCommand) (execution.StartResult, error)        = (*execution.Service).Start
	_ func(*execution.Service, context.Context, execution.InspectCommand) (execution.View, error)             = (*execution.Service).Inspect
	_ func(*execution.Service, context.Context, execution.ListCommand) (execution.Page, error)                = (*execution.Service).List
	_ func(*execution.Service, context.Context, execution.CancelCommand) (execution.TransitionResult, error)  = (*execution.Service).Cancel
	_ func(*execution.Service, context.Context, execution.OutputsCommand) (execution.TransitionResult, error) = (*execution.Service).StageOutputs
)

// Pinned struct shapes. Composite literals make a missing or renamed field a
// compile-time failure in this file rather than a silent behavioral gap.
var (
	_ = execution.ListFilter{
		TaskID:     id.TaskID(""),
		PipelineID: pipeline.PipelineID(""),
		State:      kernel.ExecutionQueued,
		Limit:      0,
		Cursor:     "",
	}
	_ = execution.Page{Items: []execution.View{}, Next: ""}
	_ = execution.View{
		Owner:         tenancy.Scope{},
		ExecutionID:   id.ExecutionID(""),
		TaskID:        id.TaskID(""),
		PipelineID:    pipeline.PipelineID(""),
		State:         kernel.ExecutionQueued,
		Version:       0,
		CurrentAction: -1,
		WorkspaceID:   id.WorkspaceID(""),
		Actions:       []execution.ActionView{},
		Cleanup:       kernel.Cleanup{},
		Failure:       (*kernel.Failure)(nil),
	}
	_ = execution.ActionView{
		ID:               id.ActionID(""),
		Index:            0,
		Name:             id.ActionName(""),
		Kind:             definition.ActionKindContainer,
		State:            kernel.ActionPending,
		Version:          0,
		Timeout:          time.Duration(0),
		ActivatedAt:      time.Time{},
		Deadline:         time.Time{},
		Outputs:          kernel.Outputs{},
		StagedOutputs:    kernel.Outputs{},
		StagedOutputsSet: false,
		Stop:             kernel.StopEvidence{},
		Decision:         "",
		Actor:            id.PrincipalID(""),
		AnsweredAt:       time.Time{},
		Subject:          (*definition.Subject)(nil),
	}
	_ = execution.StartUnit{
		Owner:                   tenancy.Scope{},
		PipelineID:              pipeline.PipelineID(""),
		ExpectedPipelineVersion: 0,
		Snapshot:                pipeline.Snapshot{},
		Actions:                 []pipeline.AdmittedAction{},
		Initiator:               id.PrincipalID(""),
		ExecutionPrincipal:      id.PrincipalID(""),
		NewExecutionID:          id.ExecutionID(""),
		At:                      time.Time{},
		Receipt:                 receipt.Record{},
		Audit:                   []event.Audit{},
		Events:                  []event.Envelope{},
		Authority:               []authorize.AuthorityRead{},
		PostCommit:              []event.PostCommitEffect{},
	}
	_ = execution.StartResult{ExecutionID: id.ExecutionID(""), PipelineVersion: 0, Replayed: false}
	_ = execution.StartCommand{
		Actor:              authorize.Principal{},
		Scope:              tenancy.Scope{},
		Admission:          pipeline.AdmissionResult{},
		Initiator:          id.PrincipalID(""),
		ExecutionPrincipal: id.PrincipalID(""),
		CallerKey:          "",
		Content:            receipt.Content{},
	}
	_ = execution.InspectCommand{Actor: authorize.Principal{}, Scope: tenancy.Scope{}, ExecutionID: id.ExecutionID("")}
	_ = execution.ListCommand{Actor: authorize.Principal{}, Scope: tenancy.Scope{}, Filter: execution.ListFilter{}}
	_ = execution.CancelCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		ExecutionID:              id.ExecutionID(""),
		ExpectedExecutionVersion: 0,
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = execution.OutputsCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		ExecutionID:              id.ExecutionID(""),
		ActionID:                 id.ActionID(""),
		ExpectedExecutionVersion: 0,
		Outputs:                  kernel.Outputs{},
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = execution.Config{
		Clock:       fixedClock{},
		IDs:         (*sequentialIDs)(nil),
		Authorizer:  (*fakeAuthorizer)(nil),
		Authority:   (*fakeAuthorityReader)(nil),
		Reader:      (*fakeReader)(nil),
		State:       (*fakeStore)(nil),
		Transitions: (*fakeTransitionPort)(nil),
		Starts:      (*fakeStartPort)(nil),
	}
)
