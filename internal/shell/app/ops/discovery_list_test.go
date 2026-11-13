package opsapp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

type fakeOIDCVerifierForWhoami struct {
	principal identity.Principal
}

func (v fakeOIDCVerifierForWhoami) Verify(_ context.Context, req identity.OIDCRequest) (identity.Principal, error) {
	if req.AccessToken != "oidc-fresh-user-jwt" {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "invalid token")
	}
	return v.principal, nil
}

type noopLookup struct{}

func (noopLookup) LookupCredential(context.Context, store.CredentialKind, string) (store.CandidateScope, error) {
	return store.CandidateScope{}, fault.New(fault.Unauthenticated, "missing")
}

type noopCredAuth struct{}

func (noopCredAuth) Authenticate(context.Context, token.AuthenticateRequest) (token.Authenticated, error) {
	return token.Authenticated{}, fault.New(fault.Unauthenticated, "missing")
}

// TestPrincipalWhoamiAcrossPrincipalKindsAndZeroMembershipOIDC proves
// principal.whoami works for tenant-wide humans, project-scoped services,
// running workloads, and a newly logged-in OIDC human who has 0 memberships yet
// (while other operations still refuse that 0-membership human).
func TestPrincipalWhoamiAcrossPrincipalKindsAndZeroMembershipOIDC(t *testing.T) {
	h := newHarness(t)

	if _, err := h.invokeKey(t, "token-admin", "tenant.create", `{"tenant_id":"`+testTenant+`","name":"Tenant A"}`, "seed-tenant", nil); err != nil {
		t.Fatalf("tenant.create: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "project.create", `{"project_id":"`+testProject+`","name":"Project A"}`, "seed-proj-a", map[string]string{"tenant": testTenant}); err != nil {
		t.Fatalf("project.create: %v", err)
	}

	// Grant admin-1 an explicit project membership in project-a so whoami
	// returns both tenant-wide roles and visible project memberships.
	if _, err := h.invokeKey(t, "token-admin", "membership.grant", `{"roles":["engineer","reviewer"]}`, "grant-admin-proj", map[string]string{
		"project":   testProject,
		"principal": testAdmin,
	}); err != nil {
		t.Fatalf("membership.grant admin project: %v", err)
	}

	adminRaw, err := h.invoke(t, "token-admin", "principal.whoami", `{}`, nil)
	if err != nil {
		t.Fatalf("admin principal.whoami: %v", err)
	}
	var adminWhoami api.PrincipalWhoamiResult
	decodeResult(t, adminRaw, &adminWhoami)
	if adminWhoami.PrincipalID != testAdmin || adminWhoami.Kind != "human" || adminWhoami.TenantID != testTenant || adminWhoami.ProjectID != "" {
		t.Fatalf("admin whoami = %+v", adminWhoami)
	}
	if !reflect.DeepEqual(adminWhoami.Roles, []string{"admin", "engineer", "operator", "reviewer", "viewer"}) &&
		!reflect.DeepEqual(adminWhoami.Roles, []string{"admin", "engineer", "reviewer", "operator", "viewer"}) {
		t.Fatalf("admin roles = %v", adminWhoami.Roles)
	}
	if len(adminWhoami.Memberships) != 1 || adminWhoami.Memberships[0].ProjectID != testProject ||
		!reflect.DeepEqual(adminWhoami.Memberships[0].Roles, []string{"engineer", "reviewer"}) {
		t.Fatalf("admin memberships = %+v; want project-a [engineer reviewer]", adminWhoami.Memberships)
	}

	// Service principal.
	h.principals.principals["token-service"] = identity.Principal{
		ID:      testService,
		Kind:    identity.KindService,
		Scope:   tenancy.Scope{TenantID: testTenant, ProjectID: testProject},
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
	svcRaw, err := h.invoke(t, "token-service", "principal.whoami", `{}`, nil)
	if err != nil {
		t.Fatalf("service principal.whoami: %v", err)
	}
	var svcWhoami api.PrincipalWhoamiResult
	decodeResult(t, svcRaw, &svcWhoami)
	if svcWhoami.PrincipalID != testService || svcWhoami.Kind != "service" ||
		svcWhoami.TenantID != testTenant || svcWhoami.ProjectID != testProject ||
		!reflect.DeepEqual(svcWhoami.Roles, []string{"viewer"}) {
		t.Fatalf("service whoami = %+v", svcWhoami)
	}

	// Running workload / execution principal.
	execID, _, _ := runningExecution(t, h)
	installExecutionPrincipal(t, h, execID, authorize.ExecOwnTaskContent, tenancy.Scope{
		TenantID:  testTenant,
		ProjectID: testProject,
	})
	execRaw, err := h.router.Invoke(executionRequest(), ops.Invocation{
		Operation:  "principal.whoami",
		Credential: "token-execution",
		Request:    api.Request{Input: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("workload principal.whoami: %v", err)
	}
	var execWhoami api.PrincipalWhoamiResult
	decodeResult(t, execRaw, &execWhoami)
	if execWhoami.Kind != "workload" || execWhoami.TenantID != testTenant || execWhoami.ProjectID != testProject || len(execWhoami.Roles) != 0 {
		t.Fatalf("workload whoami = %+v", execWhoami)
	}

	// Newly logged-in OIDC human with 0 memberships in the store, resolved via
	// the real token.Resolver + store PrincipalReader.
	oidcPrincipalID := id.PrincipalID("oidc-new-user-123")
	realResolver, err := token.NewResolver(token.ResolverConfig{
		Lookup:      noopLookup{},
		Credentials: noopCredAuth{},
		OIDC: fakeOIDCVerifierForWhoami{principal: identity.Principal{
			ID:      oidcPrincipalID,
			Kind:    identity.KindHuman,
			Scope:   tenancy.Scope{TenantID: testTenant},
			Enabled: true,
		}},
		Principals: h.adapter,
		Clock:      fixedClock{},
	})
	if err != nil {
		t.Fatalf("token.NewResolver: %v", err)
	}
	oidcRouter, err := ops.NewRouter(ops.Config{
		Principals: realResolver,
		Scopes:     h.ownership,
		Executions: h.ownership,
		Bindings:   h.bindingsTable,
	})
	if err != nil {
		t.Fatalf("ops.NewRouter: %v", err)
	}

	freshRaw, err := oidcRouter.Invoke(context.Background(), ops.Invocation{
		Operation:  "principal.whoami",
		Credential: "oidc-fresh-user-jwt",
		Request:    api.Request{Input: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("0-membership OIDC human principal.whoami refused: %v", err)
	}
	var freshWhoami api.PrincipalWhoamiResult
	decodeResult(t, freshRaw, &freshWhoami)
	if freshWhoami.PrincipalID != string(oidcPrincipalID) || freshWhoami.Kind != "human" ||
		freshWhoami.TenantID != testTenant || len(freshWhoami.Roles) != 0 || len(freshWhoami.Memberships) != 0 {
		t.Fatalf("0-membership OIDC whoami = %+v", freshWhoami)
	}

	// Every other operation still refuses the 0-membership OIDC user as
	// unauthenticated.
	_, err = oidcRouter.Invoke(context.Background(), ops.Invocation{
		Operation:  "project.list",
		Credential: "oidc-fresh-user-jwt",
		Request:    api.Request{Input: json.RawMessage(`{"tenant_id":"` + testTenant + `"}`)},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
		t.Fatalf("0-membership OIDC project.list err = %v; want unauthenticated", err)
	}
}

// TestDiscoveryAndListOperationsEndToEnd exercises project.list, pipeline.list,
// task.list, resource.binding.list, service.list and secret.metadata.list
// including filters, pagination and secret-freedom.
func TestDiscoveryAndListOperationsEndToEnd(t *testing.T) {
	h := newHarness(t)

	if _, err := h.invokeKey(t, "token-admin", "tenant.create", `{"tenant_id":"`+testTenant+`","name":"Tenant A"}`, "list-tenant", nil); err != nil {
		t.Fatalf("tenant.create: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "project.create", `{"project_id":"`+testProject+`","name":"Project A"}`, "list-proj-a", map[string]string{"tenant": testTenant}); err != nil {
		t.Fatalf("project.create a: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "project.create", `{"project_id":"project-b","name":"Project B"}`, "list-proj-b", map[string]string{"tenant": testTenant}); err != nil {
		t.Fatalf("project.create b: %v", err)
	}
	if _, err := h.invokeKey(t, testToken, "service.create",
		`{"service_id":"`+testService+`","roles":["viewer"]}`, "create-svc-base", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create base: %v", err)
	}

	// 1. project.list: tenant-wide admin sees both seeded projects (project-a
	// and project-b), paginated; project-scoped engineer sees only project-a.
	projPage1Raw, err := h.invoke(t, "token-admin", "project.list", `{"page":{"limit":1}}`, map[string]string{"tenant": testTenant})
	if err != nil {
		t.Fatalf("project.list page 1: %v", err)
	}
	var projPage1 api.ProjectListResult
	decodeResult(t, projPage1Raw, &projPage1)
	if len(projPage1.Projects) != 1 || projPage1.NextCursor == "" {
		t.Fatalf("project.list page 1 = %+v; want 1 project and next_cursor", projPage1)
	}
	projPage2Raw, err := h.invoke(t, "token-admin", "project.list", `{"page":{"limit":10,"cursor":"`+projPage1.NextCursor+`"}}`, map[string]string{"tenant": testTenant})
	if err != nil {
		t.Fatalf("project.list page 2: %v", err)
	}
	var projPage2 api.ProjectListResult
	decodeResult(t, projPage2Raw, &projPage2)
	if len(projPage2.Projects) != 1 || projPage2.NextCursor != "" {
		t.Fatalf("project.list page 2 = %+v; want 1 remaining project", projPage2)
	}

	scopedProjRaw, err := h.invoke(t, testToken, "project.list", `{}`, map[string]string{"tenant": testTenant})
	if err != nil {
		t.Fatalf("project-scoped project.list: %v", err)
	}
	var scopedProj api.ProjectListResult
	decodeResult(t, scopedProjRaw, &scopedProj)
	if len(scopedProj.Projects) != 1 || scopedProj.Projects[0].ProjectID != testProject {
		t.Fatalf("project-scoped project.list = %+v; want only %s", scopedProj, testProject)
	}

	// 2. pipeline.list.
	yamlB64 := base64.StdEncoding.EncodeToString([]byte("actions:\n  - name: build\n    image: registry.example.com/build:1\n    commands: [echo ok]\n"))
	for _, name := range []string{"alpha", "beta"} {
		if _, err := h.invokeKey(t, testToken, "pipeline.apply",
			`{"name":"`+name+`","yaml":"`+yamlB64+`"}`,
			"apply-"+name, map[string]string{"project": testProject, "pipeline": name}); err != nil {
			t.Fatalf("pipeline.apply %s: %v", name, err)
		}
	}
	pipesRaw, err := h.invoke(t, testToken, "pipeline.list", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("pipeline.list: %v", err)
	}
	var pipes api.PipelineListResult
	decodeResult(t, pipesRaw, &pipes)
	if len(pipes.Pipelines) != 2 {
		t.Fatalf("pipeline.list = %+v; want 2 pipelines", pipes)
	}

	// 3. task.list with optional status filter.
	t1Raw, err := h.invokeKey(t, testToken, "task.create", `{"title":"First task"}`, "create-t1", map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.create 1: %v", err)
	}
	var t1 api.TaskCreateResult
	decodeResult(t, t1Raw, &t1)
	if _, err := h.invokeKey(t, testToken, "task.create", `{"title":"Second task"}`, "create-t2", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("task.create 2: %v", err)
	}
	if _, err := h.invokeVersion(t, testToken, "task.cancel", `{}`, t1.LifecycleVersion, map[string]string{"task": t1.TaskID}); err != nil {
		t.Fatalf("task.cancel: %v", err)
	}

	allTasksRaw, err := h.invoke(t, testToken, "task.list", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.list all: %v", err)
	}
	var allTasks api.TaskListResult
	decodeResult(t, allTasksRaw, &allTasks)
	if len(allTasks.Tasks) != 2 {
		t.Fatalf("task.list all = %+v; want 2 tasks", allTasks)
	}

	openTasksRaw, err := h.invoke(t, testToken, "task.list", `{"status":"open"}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("task.list open: %v", err)
	}
	var openTasks api.TaskListResult
	decodeResult(t, openTasksRaw, &openTasks)
	if len(openTasks.Tasks) != 1 || openTasks.Tasks[0].Title != "Second task" || openTasks.Tasks[0].State != "open" {
		t.Fatalf("task.list open = %+v; want only Second task", openTasks)
	}

	// 4. resource.binding.list with optional kind filter.
	if _, err := h.invokeKey(t, "token-admin", "resource.binding.register",
		`{"binding_id":"bind-git","resource_id":"repo-1","enforcing_service":"`+testService+`","trusted_origins":["https://git.example.com"],"operations":["git.read","git.ref.update"],"reportable_events":["git.ref.updated"]}`,
		"reg-bind-git", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register git: %v", err)
	}
	if _, err := h.invokeKey(t, "token-admin", "resource.binding.register",
		`{"binding_id":"bind-custom","resource_id":"res-custom","enforcing_service":"`+testService+`","trusted_origins":[],"operations":[],"reportable_events":[],"metadata":{"kind":"webhook"}}`,
		"reg-bind-custom", map[string]string{"project": testProject}); err != nil {
		t.Fatalf("resource.binding.register custom: %v", err)
	}
	gitBindingsRaw, err := h.invoke(t, "token-admin", "resource.binding.list", `{"kind":"git"}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("resource.binding.list kind=git: %v", err)
	}
	var gitBindings api.ResourceBindingListResult
	decodeResult(t, gitBindingsRaw, &gitBindings)
	if len(gitBindings.Bindings) != 1 || gitBindings.Bindings[0].BindingID != "bind-git" {
		t.Fatalf("resource.binding.list kind=git = %+v; want [bind-git]", gitBindings)
	}

	// 5. service.list (must never leak credential tokens).
	svcCreateRaw, err := h.invokeKey(t, testToken, "service.create",
		`{"service_id":"svc-ci","roles":["engineer"]}`, "create-svc-ci", map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("service.create: %v", err)
	}
	var svcCreated api.ServiceCreateResult
	decodeResult(t, svcCreateRaw, &svcCreated)
	issuedRaw, err := h.invokeVersion(t, testToken, "service.credential.issue",
		`{"ttl_seconds":3600}`, svcCreated.Version, map[string]string{"service": "svc-ci"})
	if err != nil {
		t.Fatalf("service.credential.issue: %v", err)
	}
	var issued api.ServiceCredentialIssueResult
	decodeResult(t, issuedRaw, &issued)
	if issued.Token == nil || *issued.Token == "" {
		t.Fatal("service.credential.issue returned no token")
	}

	svcsRaw, err := h.invoke(t, "token-admin", "service.list", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("service.list: %v", err)
	}
	if bytes.Contains(svcsRaw.Body, []byte(*issued.Token)) {
		t.Fatal("service.list leaked the issued service credential token")
	}
	var svcs api.ServiceListResult
	decodeResult(t, svcsRaw, &svcs)
	foundCI := false
	for _, item := range svcs.Services {
		if item.ServiceID == "svc-ci" {
			foundCI = true
			if item.State != "enabled" || !reflect.DeepEqual(item.Roles, []string{"engineer"}) {
				t.Fatalf("svc-ci summary = %+v", item)
			}
		}
	}
	if !foundCI {
		t.Fatalf("service.list = %+v; missing svc-ci", svcs)
	}

	// 6. secret.metadata.list (must never leak secret values or ciphertext).
	const secretSentinel = "SUPER-SENSITIVE-SECRET-VALUE-999"
	if _, err := h.invokeKey(t, testToken, "secret.write",
		`{"value":"`+secretSentinel+`"}`, "write-sec-1", map[string]string{"project": testProject, "secret": "API_TOKEN"}); err != nil {
		t.Fatalf("secret.write: %v", err)
	}
	secsRaw, err := h.invoke(t, testToken, "secret.metadata.list", `{}`, map[string]string{"project": testProject})
	if err != nil {
		t.Fatalf("secret.metadata.list: %v", err)
	}
	if strings.Contains(string(secsRaw.Body), secretSentinel) {
		t.Fatal("secret.metadata.list leaked the secret value")
	}
	var secs api.SecretMetadataListResult
	decodeResult(t, secsRaw, &secs)
	if len(secs.Secrets) != 1 || secs.Secrets[0].Name != "API_TOKEN" || secs.Secrets[0].UpdatedAt.Before(time.Unix(1, 0)) {
		t.Fatalf("secret.metadata.list = %+v; want API_TOKEN metadata", secs)
	}
}
