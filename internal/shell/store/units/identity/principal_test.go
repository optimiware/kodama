package identity_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestReadPrincipalHumanFromMembership(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-human", 3, tenancy.RoleAdmin, tenancy.RoleReviewer)
	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "principal-human"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Kind != shellidentity.KindHuman || !principal.Enabled || principal.Version != 3 {
		t.Fatalf("principal = %+v; want enabled human version 3", principal)
	}
	if !reflect.DeepEqual(principal.Roles, []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleReviewer}) {
		t.Fatalf("roles = %v; want admin and reviewer", principal.Roles)
	}
}

// TestReadPrincipalTenantWideMembershipCoversProjectScope states the union
// truth: a tenant-wide row is a UNION contribution to a project scope, not a
// fallback that applies only when no project row exists. A human with no
// project row is still a project-scope human carrying the tenant-wide roles.
func TestReadPrincipalTenantWideMembershipCoversProjectScope(t *testing.T) {
	h := newHarness(t)
	h.seedMembership(tenantScope("tenant-a"), "membership-admin", "principal-human", 1, tenancy.RoleAdmin)
	scope := projectScope("tenant-a", "project-a")
	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "principal-human"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Kind != shellidentity.KindHuman || principal.Scope != scope {
		t.Fatalf("principal = %+v; want a human in the requested project scope", principal)
	}
	if !reflect.DeepEqual(principal.Roles, []tenancy.Role{tenancy.RoleAdmin}) {
		t.Fatalf("roles = %v; want the tenant-wide admin contribution", principal.Roles)
	}
	if principal.Version != 1 {
		t.Fatalf("principal version = %d; want the observed tenant-wide version 1", principal.Version)
	}
}

func TestReadPrincipalService(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedService(scope, "service-a", 4, string(serviceStateEnabledForTest), nil, nil)
	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "service-a"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Kind != shellidentity.KindService || !principal.Enabled || principal.Version != 4 {
		t.Fatalf("principal = %+v; want enabled service version 4", principal)
	}
	if len(principal.Roles) != 1 || principal.Roles[0] != tenancy.RoleEngineer {
		t.Fatalf("roles = %v; want engineer", principal.Roles)
	}
}

func TestReadPrincipalDisabledServiceIsNotEnabled(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedService(scope, "service-a", 2, "disabled", nil, nil)
	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "service-a"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Enabled {
		t.Fatalf("principal = %+v; want a disabled service fact", principal)
	}
}

func TestReadPrincipalExecution(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedExecution(scope, "execution-a", "principal-exec", 5, "running", nil, nil)
	principal, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "principal-exec"})
	if err != nil {
		t.Fatalf("ReadPrincipal: %v", err)
	}
	if principal.Kind != shellidentity.KindExecution || !principal.Enabled || principal.Version != 5 {
		t.Fatalf("principal = %+v; want a running execution principal version 5", principal)
	}
	if principal.ExecutionID != "execution-a" {
		t.Fatalf("execution id = %q; want execution-a", principal.ExecutionID)
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("execution roles = %v; want none", principal.Roles)
	}
}

func TestReadPrincipalMissingIsNotFound(t *testing.T) {
	h := newHarness(t)
	_, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: projectScope("tenant-a", "project-a"), PrincipalID: "principal-missing"})
	requireFaultClass(t, err, fault.NotFound)
}

func TestReadPrincipalCrossTenantInvisible(t *testing.T) {
	h := newHarness(t)
	h.seedMembership(tenantScope("tenant-b"), "membership-b", "principal-b", 1, tenancy.RoleViewer)
	_, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: tenantScope("tenant-a"), PrincipalID: "principal-b"})
	requireFaultClass(t, err, fault.NotFound)
}

func TestReadPrincipalAmbiguousIdentityRefused(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedService(scope, "principal-shared", 1, string(serviceStateEnabledForTest), nil, nil)
	h.seedMembership(scope, "membership-shared", "principal-shared", 1, tenancy.RoleViewer)
	_, err := h.adapter.ReadPrincipal(context.Background(), shellidentity.PrincipalRequest{Scope: scope, PrincipalID: "principal-shared"})
	requireFaultClass(t, err, fault.Forbidden)
}

// serviceStateEnabledForTest mirrors the declared enabled state without
// importing the service package into every fixture.
const serviceStateEnabledForTest = "enabled"
