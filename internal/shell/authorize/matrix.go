package authorize

import (
	"context"
	"fmt"
	"slices"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// matrix is the single explicit operation/grant evaluator shared by every
// Shell capability and public edge. It holds no per-call state: each call
// answers only from the registry row and the principal presented by the
// caller. It never caches a grant, so a previously calculated allow cannot
// survive a role removal, disablement or scope change.
type matrix struct{}

// NewMatrix returns the shared explicit operation/grant authorizer. It answers
// only the grant question for one operation in one scope; the commit boundary
// must still read the operation's Authority facts.
func NewMatrix() Authorizer {
	return matrix{}
}

// Authorize answers whether a verified human or service principal currently
// holds the grant for one operation in one scope.
//
// The operation must be registered, the scope must have the operation's
// declared shape and the principal must be structurally valid. A principal
// whose scope does not contain the target scope is denied without an error. An
// execution principal carries its permission separately and is refused here;
// use AuthorizeExecution for admitted execution authority.
func (matrix) Authorize(_ context.Context, principal Principal, name OperationName, scope tenancy.Scope) (Decision, error) {
	operation, ok := LookupOperation(name)
	if !ok {
		return DecisionDeny, fault.New(fault.InvalidInput, fmt.Sprintf("unknown operation %q", name))
	}
	if err := scope.Validate(); err != nil {
		return DecisionDeny, err
	}
	if err := principal.Validate(); err != nil {
		return DecisionDeny, err
	}
	if principal.Kind == identity.KindExecution {
		return DecisionDeny, fault.New(fault.InvalidInput, "execution principal requires its admitted permission")
	}
	if !scopeContains(principal.Scope, scope) {
		return DecisionDeny, nil
	}
	if err := operationScopeMatches(operation, scope); err != nil {
		return DecisionDeny, err
	}
	if !principal.Enabled {
		return DecisionDeny, nil
	}
	switch principal.Kind {
	case identity.KindHuman:
		if anyRoleGranted(principal.Roles, operation.Requires.HumanRoles) {
			return DecisionAllow, nil
		}
	case identity.KindService:
		if anyRoleGranted(principal.Roles, operation.Requires.ServiceRoles) {
			return DecisionAllow, nil
		}
	}
	return DecisionDeny, nil
}

// AuthorizeExecution answers whether an admitted execution currently holds the
// execution permission one operation declares. The admitted context is
// validated first, so a mismatched execution identity or unknown permission is
// an invalid input rather than a silent deny.
//
// The answer is a grant fact for one attempt only. The commit boundary must
// still re-read the current execution/action state and deadline.
func AuthorizeExecution(admitted AdmittedExecution, name OperationName, scope tenancy.Scope) (Decision, error) {
	operation, ok := LookupOperation(name)
	if !ok {
		return DecisionDeny, fault.New(fault.InvalidInput, fmt.Sprintf("unknown operation %q", name))
	}
	if err := scope.Validate(); err != nil {
		return DecisionDeny, err
	}
	if err := admitted.Validate(); err != nil {
		return DecisionDeny, err
	}
	if !scopeContains(admitted.Principal.Scope, scope) {
		return DecisionDeny, nil
	}
	if err := operationScopeMatches(operation, scope); err != nil {
		return DecisionDeny, err
	}
	if !admitted.Principal.Enabled {
		return DecisionDeny, nil
	}
	if admitted.Permission != ExecNone && operation.Requires.Execution == admitted.Permission {
		return DecisionAllow, nil
	}
	return DecisionDeny, nil
}

// scopeContains reports whether the owner scope covers the target scope. A
// tenant-scoped owner covers every project in its tenant; a project-scoped
// owner covers only that same project and never the whole tenant.
func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// operationScopeMatches refuses a target scope whose shape differs from the
// operation's declared scope. A tenant operation has no project; a project
// operation always names one.
func operationScopeMatches(operation Operation, scope tenancy.Scope) error {
	switch operation.Scope {
	case ScopeTenant:
		if scope.ProjectScoped() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("operation %q is tenant-scoped; project scope %q is not valid", operation.Name, scope.ProjectID))
		}
	case ScopeProject:
		if !scope.ProjectScoped() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("operation %q is project-scoped; a project is required", operation.Name))
		}
	default:
		return fault.New(fault.InternalFailure, fmt.Sprintf("operation %q has an unknown scope kind %q", operation.Name, operation.Scope))
	}
	return nil
}

// anyRoleGranted reports whether the principal holds at least one of the
// operation's declared roles for its kind.
func anyRoleGranted(current, declared []tenancy.Role) bool {
	for _, role := range current {
		if slices.Contains(declared, role) {
			return true
		}
	}
	return false
}
