package request

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Clock is the injected authoritative time source for request operations.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable identities one request commit unit carries.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
}

// StateReader loads one scoped execution aggregate. An execution owned by
// another tenant/project is reported as not_found, never returned.
type StateReader interface {
	Load(context.Context, tenancy.Scope, id.ExecutionID) (kernel.Execution, error)
}

// EngineCaller is the non-principal capability the Shell composition holds to
// run the internal request-activation step. It cannot be built from a public
// authorize.Principal; its zero value is invalid and every Activate call with
// one is refused. The attribution identity is for audit/event provenance only
// and grants no public authority.
type EngineCaller struct {
	marker *engineCallerMarker
	actor  id.PrincipalID
}

type engineCallerMarker struct{}

// NewEngineCaller mints the internal activation capability. The actor is the
// Shell engine identity used only for audit/event attribution.
func NewEngineCaller(actor id.PrincipalID) (EngineCaller, error) {
	if _, err := id.ParsePrincipalID(string(actor)); err != nil {
		return EngineCaller{}, fault.Wrap(fault.InvalidInput, "request engine caller identity is invalid", err)
	}
	return EngineCaller{marker: &engineCallerMarker{}, actor: actor}, nil
}

func (caller EngineCaller) valid() bool {
	return caller.marker != nil && caller.actor != ""
}

func (caller EngineCaller) principal() id.PrincipalID {
	return caller.actor
}

// ActivateCommand activates one admitted request action after every earlier
// container workload is confirmed stopped. It is an internal engine step, not
// a public operation: Caller is the only accepted authority. Subject and
// Timeout are never authoritative; when supplied they must equal the values
// the reducer derives from the admitted action or activation is refused.
type ActivateCommand struct {
	Caller                   EngineCaller
	Scope                    tenancy.Scope
	ExecutionID              id.ExecutionID
	ActionID                 id.ActionID
	ExpectedExecutionVersion uint64
	ExpectedActionVersion    uint64
	Subject                  *definition.Subject
	Assignment               Assignment
	Timeout                  time.Duration
	CallerKey                string
	Content                  receipt.Content
}

// ResolveCommand is one authorized answer to an activated request action. The
// public body is action-only; ExpectedExecutionVersion is the optional shared
// envelope guard (zero means omitted). The service derives the internal commit
// versions from the state it observes at the commit boundary.
type ResolveCommand struct {
	Actor                    authorize.Principal
	Scope                    tenancy.Scope
	ExecutionID              id.ExecutionID
	ActionID                 id.ActionID
	ExpectedExecutionVersion uint64
	ExpectedActionVersion    uint64
	Decision                 Decision
	Response                 kernel.Outputs
}

// Config is the complete set of injected request-operation boundaries.
type Config struct {
	Clock       Clock
	IDs         IDSource
	Authorizer  authorize.Authorizer
	Authority   authorize.AuthorityReader
	State       StateReader
	Activations ActivationPort
	Resolutions ResolvePort
}

// Service builds complete request activation and resolution commit units. It
// owns no request entity and no second lifecycle: the Kernel reducer settles
// the action, and every authority fact is re-read at the commit boundary.
type Service struct {
	clock       Clock
	ids         IDSource
	authorizer  authorize.Authorizer
	authority   authorize.AuthorityReader
	state       StateReader
	activations ActivationPort
	resolutions ResolvePort
}
