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

	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
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

// TestRolelessEnabledServiceProjectReadsAndAssignedResolutions pins that an
// enabled service with roles: [] is admitted to project-scoped operational
// reads and assigned resolutions, while still denied mutations and Admin.
func TestRolelessEnabledServiceProjectReadsAndAssignedResolutions(t *testing.T) {
	roleless := identity.Principal{
		ID:      "svc-roleless",
		Kind:    identity.KindService,
		Scope:   projectScope(),
		Roles:   nil,
		Enabled: true,
		Version: 1,
	}
	disabled := roleless
	disabled.Enabled = false

	allowedOps := []authorize.OperationName{
		"resource.binding.read", "resource.binding.list",
		"pipeline.read", "pipeline.list",
		"secret.metadata.read", "secret.metadata.list",
		"service.list",
		"task.read", "task.list", "task.history", "task.inbox",
		"execution.inspect", "execution.list", "execution.logs.read",
		"event.read", "event.subscribe",
		"task.reply", "execution.request.resolve",
	}
	for _, name := range allowedOps {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("operation %q missing", name)
		}
		if !op.Requires.Allows(roleless) {
			t.Errorf("roleless enabled service was denied %s", name)
		}
		if op.Requires.Allows(disabled) {
			t.Errorf("disabled roleless service was allowed %s", name)
		}
	}

	deniedOps := []authorize.OperationName{
		"pipeline.apply", "pipeline.update", "pipeline.delete", "pipeline.start",
		"task.create", "task.edit", "task.comment", "task.select_pipeline",
		"task.start", "task.create_and_start", "task.ask", "task.cancel",
		"execution.cancel", "secret.write", "secret.delete",
		"service.create", "service.update", "service.disable",
		"resource.binding.register", "event.publish",
		"membership.grant", "membership.revoke", "project.create",
	}
	for _, name := range deniedOps {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("operation %q missing", name)
		}
		if op.Requires.Allows(roleless) {
			t.Errorf("roleless service was allowed mutation/admin operation %s", name)
		}
	}

	// A malformed service principal carrying Admin is always refused.
	corruptAdmin := roleless
	corruptAdmin.Roles = []tenancy.Role{tenancy.RoleAdmin}
	for _, name := range append(allowedOps, deniedOps...) {
		op, ok := authorize.LookupOperation(name)
		if !ok {
			t.Fatalf("operation %q missing", name)
		}
		if op.Requires.Allows(corruptAdmin) {
			t.Errorf("service carrying Admin was allowed %s", name)
		}
	}
}
