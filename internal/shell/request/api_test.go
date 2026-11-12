package request_test

// API of internal/shell/request pinned by these tests.
//
// types.go and ports.go declare Assignment, ResponseSchema, ActivationUnit,
// ActivationResult, ResolveUnit, ResolveResult, ActivationPort and ResolvePort.
// The two authorized Shell operations below build those units. The names and
// signatures are the exact API the behavioral tests in this directory exercise.
//
//	type Clock interface { Now() time.Time }
//
//	type IDSource interface {
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	}
//
//	// StateReader loads one scoped execution aggregate. An execution owned by
//	// another tenant/project is reported as not_found, never returned.
//	type StateReader interface {
//	    Load(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)
//	}
//
//	type EngineCaller struct{ /* non-principal internal capability */ }
//	func NewEngineCaller(id.PrincipalID) (EngineCaller, error)
//
//	type ActivateCommand struct {
//	    Caller                   EngineCaller
//	    Scope                    tenancy.Scope
//	    ExecutionID              id.ExecutionID
//	    ActionID                 id.ActionID
//	    ExpectedExecutionVersion uint64
//	    ExpectedActionVersion    uint64
//	    Subject                  *definition.Subject
//	    Assignment               Assignment
//	    Timeout                  time.Duration
//	    CallerKey                string
//	    Content                  receipt.Content
//	}
//
//	type ResolveCommand struct {
//	    Actor                    authorize.Principal
//	    Scope                    tenancy.Scope
//	    ExecutionID              id.ExecutionID
//	    ActionID                 id.ActionID
//	    ExpectedExecutionVersion uint64
//	    ExpectedActionVersion    uint64
//	    Decision                 Decision
//	    Response                 kernel.Outputs
//	}
//
//	type Config struct {
//	    Clock       Clock
//	    IDs         IDSource
//	    Authorizer  authorize.Authorizer
//	    Authority   authorize.AuthorityReader
//	    State       StateReader
//	    Activations ActivationPort
//	    Resolutions ResolvePort
//	}
//
//	type Service struct{ /* ... */ }
//	func NewService(Config) (*Service, error)
//	func (*Service) Activate(context.Context, ActivateCommand) (ActivationResult, error)
//	func (*Service) Resolve(context.Context, ResolveCommand) (ResolveResult, error)
//
// Operation mapping and authority:
//
//   - Activate is the internal request-activation transition. Only the
//     non-principal EngineCaller minted by NewEngineCaller is accepted; every
//     public principal kind and the zero caller are refused. It refuses an
//     unconfirmed stop barrier on any earlier container workload, a nil or
//     incomplete subject, an assignment that does not match the admitted
//     declaration, a non-pending action, stale versions or a foreign scope.
//     The ActivationUnit persists the reducer-derived immutable subject,
//     assignment and UTC deadline (activation time + admitted action timeout),
//     clears the execution credential, and carries receipt/audit/events plus
//     the current execution_running/execution_version authority reads. No
//     request becomes answerable with an incomplete subject.
//
//   - Resolve is execution.request.resolve (receipt policy request_scoped;
//     authority reads membership + execution_version + action_deadline). The
//     receipt caller key is the request identity (execution/action), never a
//     caller-supplied replacement key. Resolution re-reads the current
//     assignment authority at the commit boundary:
//     role/<role> requires a human currently holding that role, and
//     service/<id> requires exactly that enabled service. Cached principal
//     roles are never durable permission and a service's roles are irrelevant.
//     It refuses a missing/unverifiable actor, a wrong respondent, a stale
//     version, a non-request or non-running action, a response outside the
//     declared schema and an answer at or past the persisted deadline.
//     Equality expires; the authoritative clock decides, never a sender time.
//     An identical authorized retry returns the recorded result; changed
//     content is refused. Decision/actor/time/assignment stay separate from
//     workload-controlled response strings.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var (
	_ request.Clock          = (*fixedClock)(nil)
	_ request.IDSource       = (*sequentialIDs)(nil)
	_ request.StateReader    = (*fakeStore)(nil)
	_ request.ActivationPort = (*fakeActivationPort)(nil)
	_ request.ResolvePort    = (*fakeResolvePort)(nil)

	_ func(request.Config) (*request.Service, error)                                                     = request.NewService
	_ func(*request.Service, context.Context, request.ActivateCommand) (request.ActivationResult, error) = (*request.Service).Activate
	_ func(*request.Service, context.Context, request.ResolveCommand) (request.ResolveResult, error)     = (*request.Service).Resolve
)

var (
	_ = request.ActivateCommand{
		Caller:                   request.EngineCaller{},
		Scope:                    tenancy.Scope{},
		ExecutionID:              id.ExecutionID(""),
		ActionID:                 id.ActionID(""),
		ExpectedExecutionVersion: 0,
		ExpectedActionVersion:    0,
		Subject:                  (*definition.Subject)(nil),
		Assignment:               request.Assignment{},
		Timeout:                  time.Duration(0),
		CallerKey:                "",
		Content:                  receipt.Content{},
	}
	_ = request.ResolveCommand{
		Actor:                    authorize.Principal{},
		Scope:                    tenancy.Scope{},
		ExecutionID:              id.ExecutionID(""),
		ActionID:                 id.ActionID(""),
		ExpectedExecutionVersion: 0,
		ExpectedActionVersion:    0,
		Decision:                 request.DecisionPass,
		Response:                 kernel.Outputs{},
	}
	_ = request.Config{
		Clock:       (*fixedClock)(nil),
		IDs:         (*sequentialIDs)(nil),
		Authorizer:  (*fakeAuthorizer)(nil),
		Authority:   (*fakeAuthorityReader)(nil),
		State:       (*fakeStore)(nil),
		Activations: (*fakeActivationPort)(nil),
		Resolutions: (*fakeResolvePort)(nil),
	}
)
