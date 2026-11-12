package execution

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock is the injected authoritative time source for execution operations.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable identities one execution operation commits.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
	NewExecutionID() (id.ExecutionID, error)
}

// StateReader loads one scoped execution aggregate. An execution owned by
// another tenant/project is reported as not_found, never returned.
type StateReader interface {
	Load(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)
}

// ListFilter selects one scoped execution page.
type ListFilter struct {
	TaskID     id.TaskID
	PipelineID pipeline.PipelineID
	State      kernel.ExecutionState
	Limit      int
	Cursor     string
}

// Page is one stable execution page.
type Page struct {
	Items []View
	Next  string
}

// View is the credential-free inspection projection of one execution.
type View struct {
	Owner         tenancy.Scope
	ExecutionID   id.ExecutionID
	TaskID        id.TaskID
	PipelineID    pipeline.PipelineID
	State         kernel.ExecutionState
	Version       uint64
	CurrentAction int
	WorkspaceID   id.WorkspaceID
	Actions       []ActionView
	Cleanup       kernel.Cleanup
	Failure       *kernel.Failure
}

// ActionView is the credential-free inspection projection of one action.
//
// Subject and Assignment are the immutable request facts a responder needs to
// decide without a second read: the resolved typed subject frozen at
// activation and the exact role/<role> or service/<id> assignee. They are empty
// for a container action and for a request action that is not answerable yet.
type ActionView struct {
	ID               id.ActionID
	Index            int
	Name             id.ActionName
	Kind             definition.ActionKind
	State            kernel.ActionState
	Version          uint64
	Timeout          time.Duration
	ActivatedAt      time.Time
	Deadline         time.Time
	Outputs          kernel.Outputs
	StagedOutputs    kernel.Outputs
	StagedOutputsSet bool
	Stop             kernel.StopEvidence
	Decision         string
	Actor            id.PrincipalID
	AnsweredAt       time.Time
	Subject          *definition.Subject
	Assignment       request.Assignment
}

// Reader reads scoped execution views. Implementations must never return an
// execution outside the caller's verified scope.
type Reader interface {
	Inspect(context.Context, tenancy.Scope, id.ExecutionID) (View, error)
	List(context.Context, tenancy.Scope, ListFilter) (Page, error)
}

// StartUnit is the complete atomic task-free execution-start commit unit.
type StartUnit struct {
	Owner                   tenancy.Scope
	PipelineID              pipeline.PipelineID
	ExpectedPipelineVersion uint64
	Snapshot                pipeline.Snapshot
	Actions                 []pipeline.AdmittedAction
	Initiator               id.PrincipalID
	ExecutionPrincipal      id.PrincipalID
	NewExecutionID          id.ExecutionID
	At                      time.Time
	Receipt                 receipt.Record
	Audit                   []event.Audit
	Events                  []event.Envelope
	Authority               []authorize.AuthorityRead
	PostCommit              []event.PostCommitEffect
}

// StartResult is the committed execution-start result. ExecutionVersion is the
// committed execution revision (1 on the first commit) and is independent of any
// stored pipeline revision; PipelineVersion stays zero for an inline admission.
type StartResult struct {
	ExecutionID      id.ExecutionID
	ExecutionVersion uint64
	PipelineVersion  uint64
	Replayed         bool
}

// StartPort applies one complete execution-start commit unit.
type StartPort interface {
	Start(context.Context, StartUnit) (StartResult, error)
}

// StartCommand is one authorized task-free pipeline start.
type StartCommand struct {
	Actor     authorize.Principal
	Scope     tenancy.Scope
	Admission pipeline.AdmissionResult
	// ExpectedPipelineVersion is the caller's envelope expectation for a
	// stored selection. It is nil for an inline admission and for callers that
	// admitted the definition themselves. When present it must equal the
	// admitted stored version: a fresh start at a stale envelope version is a
	// version conflict, never an execution of the newer definition.
	ExpectedPipelineVersion *uint64
	Initiator               id.PrincipalID
	ExecutionPrincipal      id.PrincipalID
	CallerKey               string
	Content                 receipt.Content
}

// InspectCommand reads one scoped execution.
type InspectCommand struct {
	Actor       authorize.Principal
	Scope       tenancy.Scope
	ExecutionID id.ExecutionID
}

// ListCommand reads one scoped execution page.
type ListCommand struct {
	Actor  authorize.Principal
	Scope  tenancy.Scope
	Filter ListFilter
}

// CancelCommand cancels one execution at an expected version.
type CancelCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	ExecutionID              id.ExecutionID
	ExpectedExecutionVersion uint64
	CallerKey                string
	Content                  receipt.Content
}

// OutputsCommand stages container outputs for one exact execution action.
type OutputsCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	ExecutionID              id.ExecutionID
	ActionID                 id.ActionID
	ExpectedExecutionVersion uint64
	Outputs                  kernel.Outputs
	CallerKey                string
	Content                  receipt.Content
}

// Config is the complete set of injected execution-operation boundaries.
type Config struct {
	Clock       Clock
	IDs         IDSource
	Authorizer  authorize.Authorizer
	Authority   authorize.AuthorityReader
	Reader      Reader
	State       StateReader
	Transitions TransitionPort
	Starts      StartPort
}

// Service builds complete execution-operation commit units and performs
// scoped reads. It owns no scheduler and no second state machine.
type Service struct {
	clock       Clock
	ids         IDSource
	authorizer  authorize.Authorizer
	authority   authorize.AuthorityReader
	reader      Reader
	state       StateReader
	transitions TransitionPort
	starts      StartPort
}
