package authorize

import (
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"slices"
)

// ConditionalGrantChecks returns the conditional current-authority checks the
// principal's grant for the operation depends on. The grant stage still answers
// "allow" for a listed role, but a conditional grant is not complete until
// every returned check has been read at the atomic commit boundary. A caller
// that builds a commit unit must therefore carry one AuthorityRead per returned
// check; an adapter must refuse a unit whose actor holds only the conditional
// grant and omitted the read.
//
// There is one such grant: task.start admits the operator role only as a
// failed-task retry. The condition is evaluated only when the grant comes from
// the operator role and not from the engineer grant, so an engineer (or an
// engineer-and-operator) principal returns no conditional check. The operator
// gets no pipeline-selection privilege and no other operation carries this
// condition.
func ConditionalGrantChecks(operation Operation, principal Principal) []AuthorityCheck {
	if principal.Kind != identity.KindHuman || !principal.Enabled {
		return nil
	}
	if principal.HasRole(tenancy.RoleEngineer) {
		return nil
	}
	if !principal.HasRole(tenancy.RoleOperator) {
		return nil
	}
	if operation.Name != "task.start" || !declaresAuthorityCheck(operation, CheckFailedTaskOperatorStart) {
		return nil
	}
	return []AuthorityCheck{CheckFailedTaskOperatorStart}
}

// declaresAuthorityCheck reports whether the registry row declares the check.
func declaresAuthorityCheck(operation Operation, want AuthorityCheck) bool {
	return slices.Contains(operation.Authority, want)
}
