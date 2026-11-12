//go:build integration

package composition

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestBootstrapSeedsAdminAndAuthenticatesDevTokens proves the first-admin seed
// exists and that the configured development tokens authenticate.
func TestBootstrapSeedsAdminAndAuthenticatesDevTokens(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))

	// The first-admin seed created the tenant, project and tenant-wide admin
	// membership. Rotating the tenant key proves the tenant row exists; the
	// membership grant is authorized by the seeded tenant-wide admin membership
	// before any project grant is made.
	seedVersion := uint64(1)
	environment.mustInvoke(adminToken, "tenant.rotate_key", api.TenantRotateKeyRequest{
		TenantID: tenantID,
		Name:     "Tenant One",
	}, &seedVersion, "tenant-rotate-seed")
	environment.grantEngineer("admin-1", projectID)

	result := environment.mustInvoke(adminToken, "membership.list", api.MembershipListRequest{
		ProjectID: projectID,
	}, nil, "")
	var memberships api.MembershipListResult
	environment.decode(result, &memberships)
	found := false
	for _, membership := range memberships.Memberships {
		if membership.PrincipalID != "admin-1" {
			continue
		}
		found = true
		if len(membership.Roles) != 4 {
			t.Fatalf("seeded admin roles = %v; want the four granted roles", membership.Roles)
		}
	}
	if !found {
		t.Fatalf("the first-admin seed is missing from %+v", memberships.Memberships)
	}

	// Both the tenant-wide and project-scoped development tokens authenticate.
	environment.mustInvoke(adminToken, "membership.list", api.MembershipListRequest{ProjectID: projectID}, nil, "")
	environment.mustInvoke(projectToken, "task.inbox", api.TaskInboxRequest{ProjectID: projectID}, nil, "")
}

// TestUnauthenticatedForgedAndExpiredTokensAreRefused proves the transport
// boundary refuses a missing, unknown and expired credential on both edges.
func TestUnauthenticatedForgedAndExpiredTokensAreRefused(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	path := "/api/v1/projects/" + projectID + "/events"

	for _, testCase := range []struct {
		name  string
		token string
	}{
		{"missing", ""},
		{"forged", forgedToken},
		{"expired", expiredToken},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := environment.restRaw(http.MethodGet, path, testCase.token, api.Request{Input: json.RawMessage(`{}`)})
			if status != http.StatusUnauthorized {
				t.Fatalf("REST status = %d; want 401 (body=%s)", status, string(body))
			}
			var publicError api.Error
			if err := json.Unmarshal(body, &publicError); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if publicError.Code != api.CodeUnauthenticated {
				t.Fatalf("REST code = %q; want %q", publicError.Code, api.CodeUnauthenticated)
			}

			_, mcpError, err := environment.mcpCall(testCase.token, "task.inbox", api.TaskInboxRequest{ProjectID: projectID}, nil, "")
			if err != nil {
				t.Fatalf("MCP call: %v", err)
			}
			if mcpError.Code != api.CodeUnauthenticated {
				t.Fatalf("MCP code = %q; want %q", mcpError.Code, api.CodeUnauthenticated)
			}
		})
	}
}

// TestProjectScopeIsolationIsUniformNotFound proves a project-scoped principal
// cannot observe another project's row and that the refusal is not_found, never
// forbidden or a leak.
func TestProjectScopeIsolationIsUniformNotFound(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))

	// A second project in the same tenant, with its own project principal.
	environment.grantEngineer("admin-1", projectID)
	environment.mustInvoke(adminToken, "project.create", api.ProjectCreateRequest{
		TenantID:  tenantID,
		ProjectID: otherID,
		Name:      "Project Two",
	}, nil, "project-create-two")
	environment.grantEngineer("admin-2", otherID)

	created := environment.mustInvoke(otherToken, "task.create", api.TaskCreateRequest{
		ProjectID: otherID,
		Title:     "other project task",
	}, nil, "other-task-create")
	var task api.TaskCreateResult
	environment.decode(created, &task)

	// The project-1 principal reading the project-2 task id is uniformly
	// not_found over both transports.
	restError := environment.restError(projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	if restError.Code != api.CodeNotFound {
		t.Fatalf("REST foreign-project read code = %q; want %q", restError.Code, api.CodeNotFound)
	}
	_, mcpError, err := environment.mcpCall(projectToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	if err != nil {
		t.Fatalf("MCP foreign-project read: %v", err)
	}
	if mcpError.Code != api.CodeNotFound {
		t.Fatalf("MCP foreign-project read code = %q; want %q", mcpError.Code, api.CodeNotFound)
	}

	// A principal in another tenant is refused the same way; the existence of
	// the project is never disclosed.
	_, foreignError, err := environment.mcpCall(foreignToken, "task.read", api.TaskReadRequest{TaskID: task.TaskID}, nil, "")
	if err != nil {
		t.Fatalf("MCP foreign-tenant read: %v", err)
	}
	if foreignError.Code != api.CodeNotFound {
		t.Fatalf("MCP foreign-tenant read code = %q; want %q", foreignError.Code, api.CodeNotFound)
	}
}
