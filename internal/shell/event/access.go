package event

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// This file holds the shared authorization/confirmation sequence the authorized
// event read and subscribe-setup handlers use. It mirrors the capability
// service pattern: apply the shared matrix grant first, re-read the current
// principal, then confirm the registry's current-authority reads inside the
// read boundary. A previously calculated allow is never durable permission.

// registryOperation returns the declared registry row for one operation.
func registryOperation(name authorize.OperationName) (authorize.Operation, error) {
	operation, ok := authorize.LookupOperation(name)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, fmt.Sprintf("operation %q is not registered", name))
	}
	return operation, nil
}

// authorizeGrant applies the shared matrix to a human or service principal.
// An execution principal is refused here: event.read and event.subscribe
// declare no execution permission, so an execution credential can never read
// events or set up a subscription. A future row that admitted executions would
// need an explicit admitted execution context rather than an implicit allow.
func authorizeGrant(ctx context.Context, authorizer authorize.Authorizer, actor identity.Principal, operation authorize.Operation, scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := actor.Validate(); err != nil {
		return fault.Wrap(fault.Unauthenticated, "actor principal is invalid", err)
	}
	if actor.Kind == identity.KindExecution {
		if operation.Requires.Execution == authorize.ExecNone {
			return fault.New(fault.Forbidden, fmt.Sprintf("execution principal has no permission for %q", operation.Name))
		}
		return fault.New(fault.Forbidden, fmt.Sprintf("execution principal for %q requires an admitted execution context", operation.Name))
	}
	decision, err := authorizer.Authorize(ctx, actor, operation.Name, scope)
	if err != nil {
		return err
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", operation.Name))
	}
	return nil
}

// confirmCurrentPrincipal re-reads the actor's current principal so a stale
// snapshot can never substitute for current enablement, scope and membership.
func confirmCurrentPrincipal(ctx context.Context, principals identity.PrincipalReader, actor identity.Principal, operation authorize.Operation, scope tenancy.Scope) (identity.Principal, error) {
	principal, err := principals.ReadPrincipal(ctx, identity.PrincipalRequest{
		Scope:       actor.Scope,
		PrincipalID: actor.ID,
	})
	if err != nil {
		return identity.Principal{}, err
	}
	if principal.ID != actor.ID {
		return identity.Principal{}, fault.New(fault.InternalFailure, "principal reader returned another principal")
	}
	if principal.Scope != actor.Scope {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is not present in the actor scope")
	}
	if !principal.Enabled {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is disabled")
	}
	if principal.Kind == identity.KindExecution {
		if operation.Requires.Execution == authorize.ExecNone {
			return identity.Principal{}, fault.New(fault.Forbidden, fmt.Sprintf("execution principal has no permission for %q", operation.Name))
		}
		if principal.ExecutionID == "" || principal.ExecutionID != actor.ExecutionID {
			return identity.Principal{}, fault.New(fault.Forbidden, "execution principal identity does not match the authenticated actor")
		}
		return principal, nil
	}
	if !scopeCovers(principal.Scope, scope) {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal scope does not cover the requested scope")
	}
	if !grantHeld(principal, operation.Requires) {
		return identity.Principal{}, fault.New(fault.Forbidden, fmt.Sprintf("principal does not currently hold the grant for %q", operation.Name))
	}
	return principal, nil
}

// scopeCovers reports whether the owner scope covers the target scope. A
// tenant-scoped owner covers every project in its tenant; a project-scoped
// owner covers only that same project.
func scopeCovers(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// grantHeld reports whether a human/service principal currently holds one of
// the operation's declared roles for its kind.
func grantHeld(principal identity.Principal, requires authorize.Requires) bool {
	switch principal.Kind {
	case identity.KindHuman:
		if slices.ContainsFunc(requires.HumanRoles, principal.HasRole) {
			return true
		}
	case identity.KindService:
		if slices.ContainsFunc(requires.ServiceRoles, principal.HasRole) {
			return true
		}
	}
	return false
}

// operationAuthorityReads builds the registry's current-authority reads in
// declaration order for one authorized scope and principal.
func operationAuthorityReads(operation authorize.Operation, scope tenancy.Scope, principalID id.PrincipalID) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(operation.Authority))
	for _, check := range operation.Authority {
		reads = append(reads, authorize.AuthorityRead{
			Operation:   operation.Name,
			Required:    operation.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principalID,
		})
	}
	return reads
}

// confirmAuthority re-reads and confirms every current-authority fact before
// any read or setup effect. A missing or mismatched fact is a dependency
// contract failure, never an implicit allow.
func confirmAuthority(ctx context.Context, authority authorize.AuthorityReader, reads []authorize.AuthorityRead) error {
	if len(reads) == 0 {
		return nil
	}
	facts, err := authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}
