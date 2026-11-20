package opsapp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	opsapp "github.com/optimiware/kodema/internal/shell/app/ops"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

type fakeRepositoryProvisioner struct {
	serviceID      string
	trustedOrigins []string
	publicGitURL   string
	calls          []opsapp.RepositoryProvisionRequest
	created        map[string]bool
	failErr        error
}

func (f *fakeRepositoryProvisioner) DefaultServiceID() string {
	return f.serviceID
}

func (f *fakeRepositoryProvisioner) DefaultTrustedOrigins() []string {
	return append([]string(nil), f.trustedOrigins...)
}

func (f *fakeRepositoryProvisioner) CloneURL(repository string) string {
	if f.publicGitURL == "" {
		return ""
	}
	return f.publicGitURL + "/" + repository + ".git"
}

func (f *fakeRepositoryProvisioner) ProvisionRepository(_ context.Context, req opsapp.RepositoryProvisionRequest) (opsapp.RepositoryProvisionResult, error) {
	f.calls = append(f.calls, req)
	if f.failErr != nil {
		return opsapp.RepositoryProvisionResult{}, f.failErr
	}
	if f.created == nil {
		f.created = make(map[string]bool)
	}
	wasCreated := !f.created[req.Repository]
	f.created[req.Repository] = true
	return opsapp.RepositoryProvisionResult{
		Repository:    req.Repository,
		DefaultBranch: req.DefaultBranch,
		CloneURL:      f.CloneURL(req.Repository),
		Created:       wasCreated,
	}, nil
}

func newHarnessWithProvisioner(t *testing.T, provisioner opsapp.RepositoryProvisioner) *harness {
	t.Helper()
	h := newHarness(t)
	h.deps.Repositories = provisioner
	bindings, err := opsapp.Bindings(h.deps)
	if err != nil {
		t.Fatalf("opsapp.Bindings: %v", err)
	}
	router, err := ops.NewRouter(ops.Config{
		Principals: h.principals,
		Scopes:     h.ownership,
		Executions: h.ownership,
		Bindings:   bindings,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}
	h.bindingsTable = bindings
	h.router = router
	return h
}

// TestProjectCreateInitialMembershipsAndDefaultTenant verifies project.create
// initializes creator membership, supports copy_members_from and creator_roles,
// defaults tenant_id when omitted (REST /api/v1/projects & MCP), and preserves
// strict admin-only authorization and tenant isolation.
func TestProjectCreateInitialMembershipsAndDefaultTenant(t *testing.T) {
	h := newHarness(t)

	if _, err := h.invokeKey(t, "token-admin", "tenant.create",
		`{"tenant_id":"`+testTenant+`","name":"Tenant A"}`, "t-create", nil); err != nil {
		t.Fatalf("tenant.create: %v", err)
	}

	// 1. Create project without tenant_id (MCP / alternate REST path): creator
	// automatically receives default project roles so they are not locked out.
	raw, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-auto","name":"Auto Project"}`, "p-auto", nil)
	if err != nil {
		t.Fatalf("project.create without tenant_id: %v", err)
	}
	var created api.ProjectCreateResult
	decodeResult(t, raw, &created)
	if created.ProjectID != "proj-auto" || created.Version != 1 {
		t.Fatalf("created = %+v; want proj-auto v1", created)
	}

	listedRaw, err := h.invoke(t, "token-admin", "membership.list", `{"project_id":"proj-auto"}`, nil)
	if err != nil {
		t.Fatalf("membership.list proj-auto: %v", err)
	}
	var listed api.MembershipListResult
	decodeResult(t, listedRaw, &listed)
	if len(listed.Memberships) != 1 || listed.Memberships[0].PrincipalID != testAdmin {
		t.Fatalf("proj-auto memberships = %+v; want creator %s", listed.Memberships, testAdmin)
	}
	wantDefaultRoles := []string{"admin", "engineer", "reviewer", "operator", "viewer"}
	if !reflect.DeepEqual(listed.Memberships[0].Roles, wantDefaultRoles) {
		t.Fatalf("creator roles = %v; want %v", listed.Memberships[0].Roles, wantDefaultRoles)
	}

	// Idempotent retry replays cleanly without failing on existing membership.
	replayedRaw, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-auto","name":"Auto Project"}`, "p-auto", nil)
	if err != nil {
		t.Fatalf("project.create replay: %v", err)
	}
	var replayed api.ProjectCreateResult
	decodeResult(t, replayedRaw, &replayed)
	if replayed != created {
		t.Fatalf("replayed = %+v; want %+v", replayed, created)
	}

	// 2. Grant an engineer into proj-auto, then create proj-copy with
	// copy_members_from="proj-auto" and verify both members are copied.
	if _, err := h.invokeKey(t, "token-admin", "membership.grant",
		`{"project_id":"proj-auto","principal_id":"engineer-9","roles":["engineer","reviewer"]}`, "grant-e9", nil); err != nil {
		t.Fatalf("membership.grant engineer-9: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-copy","name":"Copied","copy_members_from":"proj-auto"}`, "p-copy", nil); err != nil {
		t.Fatalf("project.create copy_members_from: %v", err)
	}
	copiedRaw, err := h.invoke(t, "token-admin", "membership.list", `{"project_id":"proj-copy"}`, nil)
	if err != nil {
		t.Fatalf("membership.list proj-copy: %v", err)
	}
	var copied api.MembershipListResult
	decodeResult(t, copiedRaw, &copied)
	byPrincipal := make(map[string][]string, len(copied.Memberships))
	for _, m := range copied.Memberships {
		byPrincipal[m.PrincipalID] = m.Roles
	}
	if !reflect.DeepEqual(byPrincipal["engineer-9"], []string{"engineer", "reviewer"}) {
		t.Fatalf("copied engineer-9 roles = %v; want [engineer reviewer]", byPrincipal["engineer-9"])
	}
	if !reflect.DeepEqual(byPrincipal[testAdmin], wantDefaultRoles) {
		t.Fatalf("copied admin roles = %v; want %v", byPrincipal[testAdmin], wantDefaultRoles)
	}

	// 3. Explicit creator_roles overrides creator's initial roles; empty slice
	// opts out of auto-granting the creator.
	if _, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-custom","name":"Custom","creator_roles":["admin","engineer"]}`, "p-custom", nil); err != nil {
		t.Fatalf("project.create custom creator_roles: %v", err)
	}
	customRaw, err := h.invoke(t, "token-admin", "membership.list", `{"project_id":"proj-custom"}`, nil)
	if err != nil {
		t.Fatalf("membership.list proj-custom: %v", err)
	}
	var custom api.MembershipListResult
	decodeResult(t, customRaw, &custom)
	if len(custom.Memberships) != 1 || !reflect.DeepEqual(custom.Memberships[0].Roles, []string{"admin", "engineer"}) {
		t.Fatalf("proj-custom memberships = %+v; want [admin engineer]", custom.Memberships)
	}

	if _, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-empty","name":"Empty","creator_roles":[]}`, "p-empty", nil); err != nil {
		t.Fatalf("project.create empty creator_roles: %v", err)
	}
	emptyRaw, err := h.invoke(t, "token-admin", "membership.list", `{"project_id":"proj-empty"}`, nil)
	if err != nil {
		t.Fatalf("membership.list proj-empty: %v", err)
	}
	var empty api.MembershipListResult
	decodeResult(t, emptyRaw, &empty)
	if len(empty.Memberships) != 0 {
		t.Fatalf("proj-empty memberships = %+v; want empty", empty.Memberships)
	}

	// 4. Nonexistent copy_members_from fails with not_found BEFORE creating project.
	_, err = h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"proj-missing-src","name":"Missing","copy_members_from":"does-not-exist"}`, "p-missing", nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("missing copy_members_from err = %v; want not_found", err)
	}
	if _, exists := h.store.Snapshot(tenancy.Scope{TenantID: testTenant}, "projects", "proj-missing-src"); exists {
		t.Fatal("project.create created project despite missing copy_members_from")
	}

	// 5. Security: non-admin / project-scoped caller cannot create project or
	// escalate roles.
	_, err = h.invokeKey(t, testToken, "project.create",
		`{"project_id":"proj-escalate","name":"Escalate","creator_roles":["admin"]}`, "p-esc", nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("non-admin project.create err = %v; want forbidden", err)
	}

	// 6. project.list without tenant_id defaults to caller's authenticated tenant.
	projListRaw, err := h.invoke(t, "token-admin", "project.list", `{}`, nil)
	if err != nil {
		t.Fatalf("project.list without tenant_id: %v", err)
	}
	var projList api.ProjectListResult
	decodeResult(t, projListRaw, &projList)
	if len(projList.Projects) < 4 {
		t.Fatalf("project.list returned %d projects; want at least 4: %+v", len(projList.Projects), projList.Projects)
	}

	// Foreign tenant_id is still uniformly not_found.
	_, err = h.invoke(t, "token-admin", "project.list", `{"tenant_id":"tenant-other"}`, nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("foreign tenant project.list err = %v; want not_found", err)
	}
}

// TestRepositoryCreateAndListHandlers verifies repository.create and
// repository.list coordination between resource bindings and the Git companion
// port, including defaults, idempotent replay, auth boundaries, and fail-closed
// behavior when unconfigured.
func TestRepositoryCreateAndListHandlers(t *testing.T) {
	// 1. Unconfigured Git companion fails closed without registering a binding.
	unconfigured := newHarness(t)
	_, err := unconfigured.invokeKey(t, "token-admin", "repository.create",
		`{"project_id":"`+testProject+`","name":"demo"}`, "repo-unconf", nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("unconfigured repository.create err = %v; want dependency_unavailable", err)
	}

	// 2. Configured Git companion coordinates binding + HTTP provisioning.
	provisioner := &fakeRepositoryProvisioner{
		serviceID:      testService,
		trustedOrigins: []string{"https://127.0.0.1:8443", "https://host.containers.internal:8443"},
		publicGitURL:   "https://127.0.0.1:8443/git",
	}
	h := newHarnessWithProvisioner(t, provisioner)
	if _, err := h.invokeKey(t, "token-admin", "tenant.create",
		`{"tenant_id":"`+testTenant+`","name":"Tenant A"}`, "repo-tenant", nil); err != nil {
		t.Fatalf("tenant.create: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "project.create",
		`{"project_id":"`+testProject+`","name":"Project A"}`, "repo-proj", nil); err != nil {
		t.Fatalf("project.create: %v", err)
	}
	if _, err := h.invokeKey(t, testToken, "service.create",
		`{"project_id":"`+testProject+`","service_id":"`+testService+`","roles":["viewer"]}`, "repo-svc", nil); err != nil {
		t.Fatalf("service.create: %v", err)
	}

	// Non-admin engineer is refused repository.create.
	h.principals.principals["token-engineer-only"] = identity.Principal{
		ID:      "engineer-only",
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
	_, err = h.invokeKey(t, "token-engineer-only", "repository.create",
		`{"project_id":"`+testProject+`","name":"denied"}`, "repo-denied", nil)
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("engineer repository.create err = %v; want forbidden", err)
	}
	if len(provisioner.calls) != 0 {
		t.Fatalf("provisioner called %d times on forbidden request; want 0", len(provisioner.calls))
	}

	// Admin creates repository with instance-derived defaults.
	createRaw, err := h.invokeKey(t, "token-admin", "repository.create",
		`{"project_id":"`+testProject+`","name":"shop/web"}`, "repo-create-1", nil)
	if err != nil {
		t.Fatalf("repository.create: %v", err)
	}
	var created api.RepositoryCreateResult
	decodeResult(t, createRaw, &created)
	if created.Name != "shop/web" ||
		created.ResourceID != "git-shop-web" ||
		created.BindingID != "git-shop-web" ||
		created.EnforcingService != testService ||
		created.DefaultBranch != "main" ||
		created.CloneURL != "https://127.0.0.1:8443/git/shop/web.git" ||
		created.Version != 1 ||
		!created.Created {
		t.Fatalf("unexpected repository.create result: %+v", created)
	}
	if !reflect.DeepEqual(created.TrustedOrigins, provisioner.trustedOrigins) {
		t.Fatalf("trusted_origins = %v; want %v", created.TrustedOrigins, provisioner.trustedOrigins)
	}
	if len(provisioner.calls) != 1 || provisioner.calls[0].ResourceID != "git-shop-web" || provisioner.calls[0].DefaultBranch != "main" {
		t.Fatalf("provisioner calls = %+v", provisioner.calls)
	}

	// Verify the underlying resource binding has git operations & reportable events.
	bindingRaw, err := h.invoke(t, testToken, "resource.binding.read",
		`{"binding_id":"git-shop-web"}`, map[string]string{"binding": "git-shop-web"})
	if err != nil {
		t.Fatalf("resource.binding.read: %v", err)
	}
	var binding api.ResourceBindingReadResult
	decodeResult(t, bindingRaw, &binding)
	if !reflect.DeepEqual(binding.Operations, []string{"git.read", "git.ref.update"}) {
		t.Fatalf("binding operations = %v", binding.Operations)
	}
	if !reflect.DeepEqual(binding.ReportableEvents, []string{"git.ref.updated"}) {
		t.Fatalf("binding reportable_events = %v", binding.ReportableEvents)
	}

	// Idempotent retry with same key converges without failing.
	retryRaw, err := h.invokeKey(t, "token-admin", "repository.create",
		`{"project_id":"`+testProject+`","name":"shop/web"}`, "repo-create-1", nil)
	if err != nil {
		t.Fatalf("repository.create retry: %v", err)
	}
	var retried api.RepositoryCreateResult
	decodeResult(t, retryRaw, &retried)
	if retried.Version != 1 || retried.Created {
		t.Fatalf("retried = %+v; want version=1 created=false", retried)
	}

	// Create a second repo with custom branch and explicit resource_id.
	customRaw, err := h.invokeKey(t, "token-admin", "repository.create",
		`{"project_id":"`+testProject+`","name":"infra","default_branch":"trunk","resource_id":"repo-infra"}`, "repo-create-2", nil)
	if err != nil {
		t.Fatalf("repository.create custom: %v", err)
	}
	var customRepo api.RepositoryCreateResult
	decodeResult(t, customRaw, &customRepo)
	if customRepo.DefaultBranch != "trunk" || customRepo.ResourceID != "repo-infra" || customRepo.BindingID != "repo-infra" {
		t.Fatalf("custom repo = %+v", customRepo)
	}

	// Register a non-git binding to verify repository.list filters to Git repos only.
	if _, err := h.invokeKey(t, "token-admin", "resource.binding.register",
		`{"binding_id":"webhook-1","resource_id":"wh-1","enforcing_service":"`+testService+`","trusted_origins":[],"operations":[],"reportable_events":[],"metadata":{"kind":"webhook"}}`,
		"reg-wh", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register webhook: %v", err)
	}

	// repository.list (callable by any project member, e.g. engineer) lists only Git repos.
	listRaw, err := h.invoke(t, testToken, "repository.list", `{"project_id":"`+testProject+`"}`, nil)
	if err != nil {
		t.Fatalf("repository.list: %v", err)
	}
	var listRes api.RepositoryListResult
	decodeResult(t, listRaw, &listRes)
	if len(listRes.Repositories) != 2 {
		b, _ := json.Marshal(listRes)
		t.Fatalf("repository.list = %s; want 2 repositories", b)
	}
	if listRes.Repositories[0].Name != "shop/web" || listRes.Repositories[0].CloneURL != "https://127.0.0.1:8443/git/shop/web.git" {
		t.Fatalf("repo[0] = %+v", listRes.Repositories[0])
	}
	if listRes.Repositories[1].Name != "infra" || listRes.Repositories[1].DefaultBranch != "trunk" {
		t.Fatalf("repo[1] = %+v", listRes.Repositories[1])
	}
}
