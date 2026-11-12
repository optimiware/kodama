// Service-account creation values.
//
// The package owns the pure initial service-account construction rules; live
// administration operations live in Admin, the atomic credential commit is
// token.MutationUnit, and the operation registry owns authorization decisions.
package service

import "github.com/optimiware/kodama/internal/shell/tenancy"

// CreateRequest is one service-account creation input. A created service starts
// at version 1 with no credential pair.
type CreateRequest struct {
	ID    ServiceID      `json:"id"`
	Scope tenancy.Scope  `json:"scope"`
	State State          `json:"state"`
	Roles []tenancy.Role `json:"roles,omitempty"`
}

// NewService builds a structurally valid service account at version 1 with no
// credential pair. Admin and unknown roles are refused.
func NewService(request CreateRequest) (Service, error) {
	created := Service{
		ID:      request.ID,
		Scope:   request.Scope,
		State:   request.State,
		Roles:   copyRoles(request.Roles),
		Version: 1,
	}
	if err := created.Validate(); err != nil {
		return Service{}, err
	}
	return created, nil
}

func copyRoles(roles []tenancy.Role) []tenancy.Role {
	if roles == nil {
		return nil
	}
	copied := make([]tenancy.Role, len(roles))
	copy(copied, roles)
	return copied
}
