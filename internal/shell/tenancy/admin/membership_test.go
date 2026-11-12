package tenancyadmin_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

func TestGrantMembershipBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.MembershipGrantUnit
	ports.grantMembership = func(_ context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
		captured = unit
		return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"roles":["engineer","reviewer"]}`)}
	result, err := service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
		Actor: actor,
		Membership: tenancy.Membership{
			Scope:       projectScope("tenant-a", "project-a"),
			PrincipalID: "principal-b",
			Roles:       []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer},
		},
		ExpectedVersion: 2,
		CallerKey:       "caller-grant",
		Content:         content,
	})
	if err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	if result.Version != 3 || result.Replayed {
		t.Fatalf("result = %+v; want version 3 not replayed", result)
	}
	if captured.Owner != projectScope("tenant-a", "project-a") {
		t.Fatalf("owner = %+v; want project-a", captured.Owner)
	}
	if captured.Membership.Scope != projectScope("tenant-a", "project-a") {
		t.Fatalf("membership scope = %+v; want project-a", captured.Membership.Scope)
	}
	if captured.Membership.PrincipalID != "principal-b" {
		t.Fatalf("membership principal = %q; want principal-b", captured.Membership.PrincipalID)
	}
	if !reflect.DeepEqual(captured.Membership.Roles, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}) {
		t.Fatalf("membership roles = %v; want engineer and reviewer", captured.Membership.Roles)
	}
	if captured.Membership.Version != 3 || captured.ExpectedVersion != 2 {
		t.Fatalf("membership version = %d expected %d; want 3 expected 2", captured.Membership.Version, captured.ExpectedVersion)
	}
	if err := captured.Membership.Validate(); err != nil {
		t.Fatalf("membership is invalid: %v", err)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "project-a", "principal-admin", "membership.grant", "caller-grant", content, testNow)
	requireAudit(t, captured.Audit, projectScope("tenant-a", "project-a"), actor.ID, "membership.grant", testNow)
	requireConfigEvent(t, captured.Events, projectScope("tenant-a", "project-a"), "principal-b", 3, actor.ID, testNow)
	requireAuthority(t, captured.Authority, "membership.grant", authorize.CheckMembership, projectScope("tenant-a", "project-a"), actor.ID)
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestGrantMembershipFirstGrantStartsAtVersionOne(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.MembershipGrantUnit
	ports.grantMembership = func(_ context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
		captured = unit
		return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
		Actor: adminActor("tenant-a", "", "principal-admin"),
		Membership: tenancy.Membership{
			Scope:       tenantScope("tenant-a"),
			PrincipalID: "principal-b",
			Roles:       []tenancy.Role{tenancy.RoleViewer},
		},
		ExpectedVersion: 0,
		CallerKey:       "caller-grant",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	if err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	if captured.Membership.Version != 1 {
		t.Fatalf("first grant version = %d; want 1", captured.Membership.Version)
	}
	if captured.Events[0].SubjectVersion != 1 {
		t.Fatalf("event subject version = %d; want 1", captured.Events[0].SubjectVersion)
	}
}

func TestGrantMembershipRefusesCrossScopeReferences(t *testing.T) {
	tests := []struct {
		name   string
		actor  tenancy.Scope
		target tenancy.Scope
	}{
		{name: "cross tenant", actor: tenantScope("tenant-a"), target: projectScope("tenant-b", "project-a")},
		{name: "cross project", actor: projectScope("tenant-a", "project-a"), target: projectScope("tenant-a", "project-b")},
		{name: "project actor widening to tenant", actor: projectScope("tenant-a", "project-a"), target: tenantScope("tenant-a")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.grantMembership = func(context.Context, tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
				calls++
				return tenancyadmin.MembershipGrantResult{}, nil
			}
			service := newAdminService(t, ports)
			_, err := service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
				Actor: adminActor(string(test.actor.TenantID), string(test.actor.ProjectID), "principal-admin"),
				Membership: tenancy.Membership{
					Scope:       test.target,
					PrincipalID: "principal-b",
					Roles:       []tenancy.Role{tenancy.RoleViewer},
				},
				ExpectedVersion: 1,
				CallerKey:       "caller-grant",
				Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
			})
			requireFaultClass(t, err, fault.Forbidden)
			if calls != 0 {
				t.Fatalf("GrantMembership port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestGrantMembershipRefusesInvalidMembership(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		roles     []tenancy.Role
	}{
		{name: "invalid principal", principal: "bad/id", roles: []tenancy.Role{tenancy.RoleViewer}},
		{name: "unknown role", principal: "principal-b", roles: []tenancy.Role{tenancy.Role("owner")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.grantMembership = func(context.Context, tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
				calls++
				return tenancyadmin.MembershipGrantResult{}, nil
			}
			service := newAdminService(t, ports)
			_, err := service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
				Actor: adminActor("tenant-a", "", "principal-admin"),
				Membership: tenancy.Membership{
					Scope:       tenantScope("tenant-a"),
					PrincipalID: id.PrincipalID(test.principal),
					Roles:       test.roles,
				},
				ExpectedVersion: 1,
				CallerKey:       "caller-grant",
				Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if calls != 0 {
				t.Fatalf("GrantMembership port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestRevokeMembershipBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.MembershipRevokeUnit
	ports.revokeMembership = func(_ context.Context, unit tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
		captured = unit
		return tenancyadmin.MembershipRevokeResult{Version: unit.ExpectedVersion + 1}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"principal":"principal-b"}`)}
	result, err := service.RevokeMembership(context.Background(), tenancyadmin.MembershipRevokeCommand{
		Actor:           actor,
		Scope:           projectScope("tenant-a", "project-a"),
		PrincipalID:     "principal-b",
		ExpectedVersion: 3,
		CallerKey:       "caller-revoke",
		Content:         content,
	})
	if err != nil {
		t.Fatalf("RevokeMembership: %v", err)
	}
	if result.Version != 4 || result.Replayed {
		t.Fatalf("result = %+v; want version 4 not replayed", result)
	}
	if captured.Owner != projectScope("tenant-a", "project-a") {
		t.Fatalf("owner = %+v; want project-a", captured.Owner)
	}
	if captured.PrincipalID != "principal-b" || captured.ExpectedVersion != 3 {
		t.Fatalf("revoke target = %q expected %d; want principal-b expected 3", captured.PrincipalID, captured.ExpectedVersion)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "project-a", "principal-admin", "membership.revoke", "caller-revoke", content, testNow)
	requireAudit(t, captured.Audit, projectScope("tenant-a", "project-a"), actor.ID, "membership.revoke", testNow)
	requireConfigEvent(t, captured.Events, projectScope("tenant-a", "project-a"), "principal-b", 4, actor.ID, testNow)
	requireAuthority(t, captured.Authority, "membership.revoke", authorize.CheckMembership, projectScope("tenant-a", "project-a"), actor.ID)
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestRevokeMembershipRefusesZeroVersionAndCrossScope(t *testing.T) {
	tests := []struct {
		name            string
		expectedVersion uint64
		actor           tenancy.Scope
		target          tenancy.Scope
	}{
		{name: "zero expected version", expectedVersion: 0, actor: tenantScope("tenant-a"), target: tenantScope("tenant-a")},
		{name: "cross tenant", expectedVersion: 1, actor: tenantScope("tenant-a"), target: projectScope("tenant-b", "project-a")},
		{name: "cross project", expectedVersion: 1, actor: projectScope("tenant-a", "project-a"), target: projectScope("tenant-a", "project-b")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.revokeMembership = func(context.Context, tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
				calls++
				return tenancyadmin.MembershipRevokeResult{}, nil
			}
			service := newAdminService(t, ports)
			_, err := service.RevokeMembership(context.Background(), tenancyadmin.MembershipRevokeCommand{
				Actor:           adminActor(string(test.actor.TenantID), string(test.actor.ProjectID), "principal-admin"),
				Scope:           test.target,
				PrincipalID:     "principal-b",
				ExpectedVersion: test.expectedVersion,
				CallerKey:       "caller-revoke",
				Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
			})
			if test.expectedVersion == 0 {
				requireFaultClass(t, err, fault.InvalidInput)
			} else {
				requireFaultClass(t, err, fault.Forbidden)
			}
			if calls != 0 {
				t.Fatalf("RevokeMembership port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestListMembershipsBuildsScopedQuery(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.MembershipQuery
	page := tenancyadmin.MembershipPage{
		Memberships: []tenancy.Membership{{
			Scope:       projectScope("tenant-a", "project-a"),
			PrincipalID: "principal-b",
			Roles:       []tenancy.Role{tenancy.RoleViewer},
			Version:     2,
		}},
		NextCursor: "cursor-2",
	}
	ports.listMemberships = func(_ context.Context, query tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
		captured = query
		return page, nil
	}
	service := newAdminService(t, ports)

	result, err := service.ListMemberships(context.Background(), tenancyadmin.MembershipListCommand{
		Actor:       adminActor("tenant-a", "", "principal-admin"),
		Scope:       projectScope("tenant-a", "project-a"),
		PrincipalID: "principal-b",
		Cursor:      "cursor-1",
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if captured.Scope != projectScope("tenant-a", "project-a") {
		t.Fatalf("query scope = %+v; want project-a", captured.Scope)
	}
	if captured.PrincipalID != "principal-b" || captured.Cursor != "cursor-1" || captured.Limit != 50 {
		t.Fatalf("query = %+v; want principal-b cursor-1 limit 50", captured)
	}
	if result.NextCursor != "cursor-2" || len(result.Memberships) != 1 {
		t.Fatalf("page = %+v; want one membership and next cursor", result)
	}
}

func TestListMembershipsRefusesInvalidBoundsAndCrossScope(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		actor  tenancy.Scope
		target tenancy.Scope
		want   fault.Class
	}{
		{name: "zero limit", limit: 0, actor: tenantScope("tenant-a"), target: tenantScope("tenant-a"), want: fault.InvalidInput},
		{name: "negative limit", limit: -1, actor: tenantScope("tenant-a"), target: tenantScope("tenant-a"), want: fault.InvalidInput},
		{name: "oversized limit", limit: tenancyadmin.MaxMembershipPageSize + 1, actor: tenantScope("tenant-a"), target: tenantScope("tenant-a"), want: fault.InvalidInput},
		{name: "cross tenant", limit: 10, actor: tenantScope("tenant-a"), target: tenantScope("tenant-b"), want: fault.Forbidden},
		{name: "cross project", limit: 10, actor: projectScope("tenant-a", "project-a"), target: projectScope("tenant-a", "project-b"), want: fault.Forbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeAdminPorts{}
			calls := 0
			ports.listMemberships = func(context.Context, tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
				calls++
				return tenancyadmin.MembershipPage{}, nil
			}
			service := newAdminService(t, ports)
			_, err := service.ListMemberships(context.Background(), tenancyadmin.MembershipListCommand{
				Actor: adminActor(string(test.actor.TenantID), string(test.actor.ProjectID), "principal-admin"),
				Scope: test.target,
				Limit: test.limit,
			})
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("ListMemberships port calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestListMembershipsReturnsDefensiveCopy(t *testing.T) {
	ports := &fakeAdminPorts{}
	stored := tenancyadmin.MembershipPage{
		Memberships: []tenancy.Membership{{
			Scope:       tenantScope("tenant-a"),
			PrincipalID: "principal-b",
			Roles:       []tenancy.Role{tenancy.RoleViewer},
			Version:     1,
		}},
	}
	ports.listMemberships = func(context.Context, tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
		return stored, nil
	}
	service := newAdminService(t, ports)

	result, err := service.ListMemberships(context.Background(), tenancyadmin.MembershipListCommand{
		Actor: adminActor("tenant-a", "", "principal-admin"),
		Scope: tenantScope("tenant-a"),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	result.Memberships[0].Roles[0] = tenancy.RoleAdmin
	result.Memberships[0].PrincipalID = "principal-mutated"
	if stored.Memberships[0].Roles[0] != tenancy.RoleViewer || stored.Memberships[0].PrincipalID != "principal-b" {
		t.Fatal("returned membership page aliases the port-owned page")
	}
}

func TestGrantMembershipDefensiveCopiesRoles(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.MembershipGrantUnit
	ports.grantMembership = func(_ context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
		captured = unit
		return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
	}
	service := newAdminService(t, ports)

	roles := []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}
	command := tenancyadmin.MembershipGrantCommand{
		Actor: adminActor("tenant-a", "", "principal-admin"),
		Membership: tenancy.Membership{
			Scope:       tenantScope("tenant-a"),
			PrincipalID: "principal-b",
			Roles:       roles,
		},
		ExpectedVersion: 0,
		CallerKey:       "caller-grant",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	}
	if _, err := service.GrantMembership(context.Background(), command); err != nil {
		t.Fatalf("GrantMembership: %v", err)
	}
	roles[0] = tenancy.RoleAdmin
	command.Membership.Roles = append(command.Membership.Roles, tenancy.RoleReviewer)
	if !reflect.DeepEqual(captured.Membership.Roles, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer}) {
		t.Fatalf("committed roles = %v; want an independent copy of engineer and viewer", captured.Membership.Roles)
	}
}
