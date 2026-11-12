package domaincontract

// Declaration vector: tenancy.AssignableServiceRoles.
//
// It is the one definition of the roles a services row may grant. Admin is
// never service-assignable and an unknown value is not a role, so neither
// survives a read of a corrupt or mis-written row; the rest keep the observed
// order and the input is never modified.

import (
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestAssignableServiceRolesDropsAdminAndUnknownValues(t *testing.T) {
	cases := []struct {
		name     string
		observed []tenancy.Role
		want     []tenancy.Role
	}{
		{name: "empty", observed: nil, want: []tenancy.Role{}},
		{
			name:     "assignable roles keep the observed order",
			observed: []tenancy.Role{tenancy.RoleViewer, tenancy.RoleEngineer},
			want:     []tenancy.Role{tenancy.RoleViewer, tenancy.RoleEngineer},
		},
		{
			name:     "admin is dropped",
			observed: []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleOperator},
			want:     []tenancy.Role{tenancy.RoleOperator},
		},
		{
			name:     "unknown values are dropped",
			observed: []tenancy.Role{"owner", tenancy.RoleReviewer, ""},
			want:     []tenancy.Role{tenancy.RoleReviewer},
		},
		{name: "admin alone grants nothing", observed: []tenancy.Role{tenancy.RoleAdmin}, want: []tenancy.Role{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observed := slices.Clone(testCase.observed)
			got := tenancy.AssignableServiceRoles(observed)
			if !slices.Equal(got, testCase.want) {
				t.Errorf("AssignableServiceRoles(%v) = %v; want %v", testCase.observed, got, testCase.want)
			}
			if !slices.Equal(observed, testCase.observed) {
				t.Errorf("the observed roles were modified: %v", observed)
			}
		})
	}
	for _, role := range tenancy.AssignableServiceRoles(tenancy.Roles()) {
		if !slices.Contains(tenancy.ServiceRoles(), role) {
			t.Errorf("role %q survives the filter but is not a declared service role", role)
		}
	}
}
