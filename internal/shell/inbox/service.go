// The read-only inbox service.
//
// The inbox is a query over active request actions, waiting tasks and failed
// open tasks: it is not an independently mutable request entity, so
// authoritative state lives on the execution/action and task rows. types.go and
// ports.go declare Kind, Query, Item, Page and QueryPort; this file adds the
// read-only service seam.
package inbox

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// MaxPageSize bounds one inbox page. Zero asks the adapter for its default.
// It is the declared public page bound, so the served handler never accepts
// less than api.Page allows nor more than the public edge validated.
const MaxPageSize = api.MaxPageLimit

// ServiceConfig supplies the inbox boundaries. The inbox has no event, receipt
// or mutation dependency: it owns no mutable entity.
type ServiceConfig struct {
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Principals identity.PrincipalReader
	Queries    QueryPort
}

// QueryCommand is one scoped inbox query. Kinds is empty for every kind.
type QueryCommand struct {
	Actor  authorize.Principal
	Scope  tenancy.Scope
	Kinds  []Kind
	Cursor string
	Limit  int
}

// Service is the read-only inbox query seam.
type Service struct {
	authorizer authorize.Authorizer
	authority  authorize.AuthorityReader
	principals identity.PrincipalReader
	queries    QueryPort
}

// NewService validates the inbox boundary dependencies.
func NewService(config ServiceConfig) (*Service, error) {
	if config.Authorizer == nil || config.Authority == nil || config.Principals == nil || config.Queries == nil {
		return nil, fault.New(fault.InternalFailure, "inbox service requires an authorizer, an authority reader, a principal reader and a query port")
	}
	return &Service{
		authorizer: config.Authorizer,
		authority:  config.Authority,
		principals: config.Principals,
		queries:    config.Queries,
	}, nil
}

// Query authorizes one scoped inbox read and returns the adapter's
// authoritative page unchanged. It never interprets, rewrites, reorders or
// truncates a cursor or item, and it caches nothing: the next query reads
// current state again.
func (service *Service) Query(ctx context.Context, command QueryCommand) (Page, error) {
	if err := command.Scope.Validate(); err != nil {
		return Page{}, err
	}
	if !command.Scope.ProjectScoped() {
		return Page{}, fault.New(fault.InvalidInput, "inbox query requires a project scope")
	}
	if _, err := id.ParsePrincipalID(string(command.Actor.ID)); err != nil {
		return Page{}, fault.Wrap(fault.InvalidInput, "inbox principal id is invalid", err)
	}
	if !command.Actor.Kind.Valid() {
		return Page{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown inbox principal kind %q", command.Actor.Kind))
	}
	for _, kind := range command.Kinds {
		if !kind.Valid() {
			return Page{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown inbox kind %q", kind))
		}
	}
	if command.Limit < 0 || command.Limit > MaxPageSize {
		return Page{}, fault.New(fault.InvalidInput, fmt.Sprintf("inbox limit %d is outside 0..%d", command.Limit, MaxPageSize))
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return Page{}, fault.New(fault.Forbidden, "actor scope does not match command scope")
	}
	const operation authorize.OperationName = "task.inbox"
	decision, err := service.authorizer.Authorize(ctx, command.Actor, operation, command.Scope)
	if err != nil {
		return Page{}, err
	}
	if decision != authorize.DecisionAllow {
		return Page{}, fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", operation))
	}
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return Page{}, fault.New(fault.InternalFailure, "task.inbox is not registered")
	}
	reads := make([]authorize.AuthorityRead, 0, len(registered.Authority))
	for _, check := range registered.Authority {
		reads = append(reads, authorize.AuthorityRead{
			Operation:   operation,
			Required:    registered.Requires,
			Check:       check,
			Scope:       command.Scope,
			PrincipalID: command.Actor.ID,
		})
	}
	facts, err := service.authority.Read(ctx, reads)
	if err != nil {
		return Page{}, err
	}
	if err := authorize.Confirm(reads, facts); err != nil {
		return Page{}, err
	}
	current, err := service.principals.ReadPrincipal(ctx, identity.PrincipalRequest{
		Scope:       command.Scope,
		PrincipalID: command.Actor.ID,
	})
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return Page{}, fault.New(fault.Forbidden, "principal has no current authority in scope")
		}
		return Page{}, err
	}
	if current.ID != command.Actor.ID {
		return Page{}, fault.New(fault.InternalFailure, "principal reader returned another principal")
	}
	if !current.Enabled || current.Kind != command.Actor.Kind || current.Scope != command.Scope || !grantHeld(current, registered.Requires) {
		return Page{}, fault.New(fault.Forbidden, "principal does not currently hold the inbox grant in scope")
	}
	kinds := make([]Kind, len(command.Kinds))
	copy(kinds, command.Kinds)
	return service.queries.Query(ctx, Query{
		Scope:     command.Scope,
		Principal: current,
		Kinds:     kinds,
		Cursor:    command.Cursor,
		Limit:     command.Limit,
	})
}

func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	return owner.ProjectID == "" || owner.ProjectID == target.ProjectID
}

func grantHeld(principal identity.Principal, requires authorize.Requires) bool {
	switch principal.Kind {
	case identity.KindHuman:
		return slices.ContainsFunc(requires.HumanRoles, principal.HasRole)
	case identity.KindService:
		return slices.ContainsFunc(requires.ServiceRoles, principal.HasRole)
	default:
		return false
	}
}
