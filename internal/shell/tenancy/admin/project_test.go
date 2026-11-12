package tenancyadmin_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

func TestCreateProjectDerivesTenantFromActorScope(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.ProjectCreateUnit
	ports.createProject = func(_ context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		captured = unit
		return tenancyadmin.ProjectCreateResult{ProjectID: unit.Project.ID, Version: unit.Project.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Project A"}`)}
	result, err := service.CreateProject(context.Background(), tenancyadmin.ProjectCreateCommand{
		Actor:     actor,
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   content,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if result.ProjectID != "project-a" || result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want project-a version 1 not replayed", result)
	}
	if captured.Project.ID != "project-a" || captured.Project.Name != "Project A" {
		t.Fatalf("project = %+v; want project-a", captured.Project)
	}
	if captured.Project.TenantID != "tenant-a" {
		t.Fatalf("project tenant = %q; want actor tenant tenant-a", captured.Project.TenantID)
	}
	if captured.Project.Version != 1 {
		t.Fatalf("project version = %d; want 1", captured.Project.Version)
	}
	if err := captured.Project.Validate(); err != nil {
		t.Fatalf("project is invalid: %v", err)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "", "principal-admin", "project.create", "caller-1", content, testNow)
	requireAudit(t, captured.Audit, tenantScope("tenant-a"), actor.ID, "project.create", testNow)
	requireConfigEvent(t, captured.Events, tenantScope("tenant-a"), "project-a", 1, actor.ID, testNow)
	requireAuthority(t, captured.Authority, "project.create", authorize.CheckMembership, tenantScope("tenant-a"), actor.ID)
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestCreateProjectRefusesProjectScopedActor(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.createProject = func(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		calls++
		return tenancyadmin.ProjectCreateResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.CreateProject(context.Background(), tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "project-a", "principal-admin"),
		ProjectID: "project-b",
		Name:      "Project B",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.Forbidden)
	if calls != 0 {
		t.Fatalf("CreateProject port calls = %d; want 0 before refusal", calls)
	}
}

func TestCreateProjectRefusesInvalidProject(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.createProject = func(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		calls++
		return tenancyadmin.ProjectCreateResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.CreateProject(context.Background(), tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "", "principal-admin"),
		ProjectID: "project-a",
		Name:      "",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("CreateProject port calls = %d; want 0 before refusal", calls)
	}
}

func TestUpdateProjectBuildsVersionedCommitUnit(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.ProjectUpdateUnit
	ports.updateProject = func(_ context.Context, unit tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
		captured = unit
		return tenancyadmin.ProjectUpdateResult{Version: unit.Project.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Renamed"}`)}
	result, err := service.UpdateProject(context.Background(), tenancyadmin.ProjectUpdateCommand{
		Actor:           actor,
		ProjectID:       "project-a",
		ExpectedVersion: 4,
		Name:            "Renamed",
		CallerKey:       "caller-2",
		Content:         content,
	})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if result.Version != 5 || result.Replayed {
		t.Fatalf("result = %+v; want version 5 not replayed", result)
	}
	if captured.Owner != tenantScope("tenant-a") {
		t.Fatalf("owner = %+v; want tenant-a", captured.Owner)
	}
	if captured.ProjectID != "project-a" || captured.ExpectedVersion != 4 {
		t.Fatalf("update target = %q expected %d; want project-a expected 4", captured.ProjectID, captured.ExpectedVersion)
	}
	if captured.Project.ID != "project-a" || captured.Project.TenantID != "tenant-a" || captured.Project.Name != "Renamed" {
		t.Fatalf("project = %+v; want project-a tenant-a Renamed", captured.Project)
	}
	if captured.Project.Version != 5 {
		t.Fatalf("project version = %d; want 5", captured.Project.Version)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	requireReceipt(t, captured.Receipt, "tenant-a", "", "principal-admin", "project.update", "caller-2", content, testNow)
	requireAudit(t, captured.Audit, tenantScope("tenant-a"), actor.ID, "project.update", testNow)
	requireConfigEvent(t, captured.Events, tenantScope("tenant-a"), "project-a", 5, actor.ID, testNow)
	requireAuthority(t, captured.Authority, "project.update", authorize.CheckMembership, tenantScope("tenant-a"), actor.ID)
	requireAuthority(t, captured.Authority, "project.update", authorize.CheckPrincipalEnabled, tenantScope("tenant-a"), actor.ID)
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestUpdateProjectRefusesZeroExpectedVersion(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	ports.updateProject = func(context.Context, tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
		calls++
		return tenancyadmin.ProjectUpdateResult{}, nil
	}
	service := newAdminService(t, ports)

	_, err := service.UpdateProject(context.Background(), tenancyadmin.ProjectUpdateCommand{
		Actor:           adminActor("tenant-a", "", "principal-admin"),
		ProjectID:       "project-a",
		ExpectedVersion: 0,
		Name:            "Renamed",
		CallerKey:       "caller-2",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("UpdateProject port calls = %d; want 0 before refusal", calls)
	}
}
