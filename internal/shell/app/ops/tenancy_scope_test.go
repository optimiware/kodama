package opsapp_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// membershipRows returns the committed membership rows of one principal in one
// scope. A tenant scope sees only tenant-wide rows (project_id IS NULL), so a
// project-addressed grant that leaked into tenant scope is observable here.
func membershipRows(t *testing.T, h *harness, scope tenancy.Scope, principal string) []store.Row {
	t.Helper()
	page, err := h.store.Query(context.Background(), scope, store.ScanQuery{
		Table:  "memberships",
		Equals: map[string]any{"principal_id": principal},
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("query memberships in %+v: %v", scope, err)
	}
	return page.Rows
}

// listMembershipPrincipals returns the principal ids of one membership.list
// page addressed by the project route.
func listMembershipPrincipals(t *testing.T, h *harness, project string) map[string]bool {
	t.Helper()
	listed, err := h.invoke(t, "token-admin", "membership.list", `{}`, map[string]string{"project": project})
	if err != nil {
		t.Fatalf("membership.list %s: %v", project, err)
	}
	var result api.MembershipListResult
	decodeResult(t, listed, &result)
	principals := make(map[string]bool, len(result.Memberships))
	for _, membership := range result.Memberships {
		principals[membership.PrincipalID] = true
	}
	return principals
}

// TestMembershipScopeUsesRouteProject pins that membership.grant/revoke/list
// address the project named by the route (or project_id for MCP), never the
// tenant-only scope the registry assigns to the operation family. A tenant-wide
// admin granting into project-b must create a project-b row, and a project-scoped
// principal may never address another project.
func TestMembershipScopeUsesRouteProject(t *testing.T) {
	h := newHarness(t)

	granted, err := h.invoke(t, "token-admin", "membership.grant", `{"roles":["engineer"]}`, map[string]string{
		"project":   "project-b",
		"principal": "engineer-2",
	})
	if err != nil {
		t.Fatalf("membership.grant project-b: %v", err)
	}
	var grant api.MembershipGrantResult
	decodeResult(t, granted, &grant)
	if grant.Version != 1 {
		t.Fatalf("membership.grant version = %d; want 1", grant.Version)
	}

	rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}, "engineer-2")
	if len(rows) != 1 {
		t.Fatalf("project-b membership rows = %d; want 1", len(rows))
	}
	if got, _ := rows[0].Values["project_id"].(string); got != "project-b" {
		t.Fatalf("stored membership project_id = %q; want project-b", got)
	}
	if rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant}, "engineer-2"); len(rows) != 0 {
		t.Fatalf("grant leaked into tenant-wide scope: %+v", rows)
	}

	projectB := listMembershipPrincipals(t, h, "project-b")
	if !projectB["engineer-2"] || !projectB["engineer-b"] {
		t.Fatalf("project-b memberships = %+v; want engineer-2 and engineer-b", projectB)
	}
	projectA := listMembershipPrincipals(t, h, testProject)
	if projectA["engineer-2"] {
		t.Fatalf("project-a memberships leaked engineer-2: %+v", projectA)
	}

	// A second project-b grant lets the revoke prove it removes only its own row.
	if _, err := h.invokeKey(t, "token-admin", "membership.grant", `{"roles":["viewer"]}`, "grant-engineer-3", map[string]string{
		"project":   "project-b",
		"principal": "engineer-3",
	}); err != nil {
		t.Fatalf("membership.grant engineer-3: %v", err)
	}
	revoked, err := h.invokeVersion(t, "token-admin", "membership.revoke", `{}`, 1, map[string]string{
		"project":   "project-b",
		"principal": "engineer-2",
	})
	if err != nil {
		t.Fatalf("membership.revoke project-b: %v", err)
	}
	var revocation api.MembershipRevokeResult
	decodeResult(t, revoked, &revocation)
	if revocation.Version != 2 {
		t.Fatalf("membership.revoke version = %d; want 2", revocation.Version)
	}
	if rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}, "engineer-2"); len(rows) != 0 {
		t.Fatalf("revoked membership row remains: %+v", rows)
	}
	if rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}, "engineer-3"); len(rows) != 1 {
		t.Fatalf("revoke removed another principal's row: %+v", rows)
	}
	if rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant}, "engineer-2"); len(rows) != 0 {
		t.Fatalf("revoke left a tenant-wide row: %+v", rows)
	}

	// A project-scoped principal naming another project is uniformly not_found:
	// no lookup may reveal that the foreign project exists.
	for _, operation := range []string{"membership.grant", "membership.revoke", "membership.list"} {
		input := `{}`
		if operation == "membership.grant" {
			input = `{"roles":["engineer"]}`
		}
		_, err := h.invokeVersion(t, testToken, operation, input, 1, map[string]string{
			"project":   "project-b",
			"principal": "engineer-2",
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("%s naming a foreign project = %v; want not_found", operation, err)
		}
	}

	// Membership administration is tenant-scoped in the registry, so a
	// project-scoped principal cannot hold the grant at all. The invariant that
	// matters is that it never widens into a tenant-wide row.
	_, err = h.invoke(t, testToken, "membership.grant", `{"roles":["engineer"]}`, map[string]string{
		"project":   testProject,
		"principal": "engineer-2",
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("project-scoped membership.grant = %v; want forbidden", err)
	}
	if rows := membershipRows(t, h, tenancy.Scope{TenantID: testTenant}, "engineer-2"); len(rows) != 0 {
		t.Fatalf("project-scoped grant widened to tenant scope: %+v", rows)
	}
}

// TestEventPublishUsesResolvedProjectScope pins that a tenant-wide publisher
// naming a project publishes the observation in that project's scope instead of
// silently widening it to a tenant-wide event.
func TestEventPublishUsesResolvedProjectScope(t *testing.T) {
	h := newHarness(t)

	if _, err := h.invokeKey(t, "token-admin", "event.publish", jsonBody(t, map[string]any{
		"type":       "observation.custom",
		"subject_id": "subject-project-b",
	}), "publish-project-b", map[string]string{"project": "project-b"}); err != nil {
		t.Fatalf("event.publish project-b: %v", err)
	}

	projectEvents := h.store.Events(tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"})
	if len(projectEvents) != 1 {
		t.Fatalf("project-b events = %d; want 1", len(projectEvents))
	}
	if got := projectEvents[0].Envelope.Scope; got != (tenancy.Scope{TenantID: testTenant, ProjectID: "project-b"}) {
		t.Fatalf("event scope = %+v; want project-b", got)
	}
	if events := h.store.Events(tenancy.Scope{TenantID: testTenant}); len(events) != 0 {
		t.Fatalf("event leaked into tenant-wide scope: %+v", events)
	}
}
