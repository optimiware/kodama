package resource_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
)

func validBinding() resource.Binding {
	return resource.Binding{
		ID:               "binding-a",
		ResourceID:       "resource-a",
		Scope:            projectScope("tenant-a", "project-a"),
		EnforcingService: service.ServiceID("git-service"),
		TrustedOrigins:   []string{"https://git.example.com/shop"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          3,
		Metadata:         map[string]string{"environment": "production"},
	}
}

func TestRegisterBindingBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingUnit
	ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
		captured = unit
		return resource.BindingResult{Version: unit.Binding.Version}, nil
	}
	service := newBindingService(t, ports)

	command := validRegisterCommand()
	result, err := service.RegisterBinding(context.Background(), command)
	if err != nil {
		t.Fatalf("RegisterBinding: %v", err)
	}
	if result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want version 1 not replayed", result)
	}
	if captured.Owner != projectScope("tenant-a", "project-a") {
		t.Fatalf("owner = %+v; want project-a", captured.Owner)
	}
	if captured.BindingID != "binding-a" || captured.ExpectedVersion != 0 {
		t.Fatalf("unit target = %q expected %d; want binding-a expected 0", captured.BindingID, captured.ExpectedVersion)
	}
	binding := captured.Binding
	if binding.ID != "binding-a" || binding.ResourceID != "resource-a" {
		t.Fatalf("binding identities = %q/%q; want binding-a/resource-a", binding.ID, binding.ResourceID)
	}
	if binding.Scope != projectScope("tenant-a", "project-a") {
		t.Fatalf("binding scope = %+v; want project-a", binding.Scope)
	}
	if binding.EnforcingService != "git-service" {
		t.Fatalf("enforcing service = %q; want git-service", binding.EnforcingService)
	}
	if !reflect.DeepEqual(binding.TrustedOrigins, []string{"https://git.example.com/shop"}) {
		t.Fatalf("trusted origins = %v; want the registered origin", binding.TrustedOrigins)
	}
	if !reflect.DeepEqual(binding.Operations, []resource.Operation{resource.OperationGitRefUpdate}) {
		t.Fatalf("operations = %v; want git.ref.update", binding.Operations)
	}
	if !reflect.DeepEqual(binding.ReportableEvents, []string{"git.ref.updated"}) {
		t.Fatalf("reportable events = %v; want git.ref.updated", binding.ReportableEvents)
	}
	if binding.Version != 1 {
		t.Fatalf("binding version = %d; want 1", binding.Version)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("binding is invalid: %v", err)
	}
	requireBindingReceipt(t, captured.Receipt, "tenant-a", "project-a", "principal-admin", "resource.binding.register", "caller-register", command.Content, testNow)
	requireBindingAudit(t, captured.Audit, projectScope("tenant-a", "project-a"), command.Actor.ID, "resource.binding.register", testNow)
	requireBindingConfigEvent(t, captured.Events, projectScope("tenant-a", "project-a"), "binding-a", 1, command.Actor.ID, testNow)
	requireBindingAuthority(t, captured.Authority, "resource.binding.register", authorize.CheckMembership, projectScope("tenant-a", "project-a"), command.Actor.ID)
	ownership := requireBindingAuthority(t, captured.Authority, "resource.binding.register", authorize.CheckResourceOwnership, projectScope("tenant-a", "project-a"), command.Actor.ID)
	if ownership.TargetID != "resource-a" {
		t.Fatalf("resource ownership target = %q; want resource-a", ownership.TargetID)
	}
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestRegisterBindingRefusesScopeMismatch(t *testing.T) {
	tests := []struct {
		name  string
		actor scopeFixture
		scope scopeFixture
		want  fault.Class
	}{
		{name: "tenant scope is not project scope", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a"}, want: fault.InvalidInput},
		{name: "cross tenant", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-b", project: "project-b"}, want: fault.Forbidden},
		{name: "cross project", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-b"}, want: fault.Forbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
				calls++
				return resource.BindingResult{}, nil
			}
			service := newBindingService(t, ports)
			command := validRegisterCommand()
			command.Actor = bindingActor(test.actor.tenant, test.actor.project, "principal-admin")
			command.Scope = tenancyScope(test.scope.tenant, test.scope.project)
			_, err := service.RegisterBinding(context.Background(), command)
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestRegisterBindingRequiresResourceIDLinkage(t *testing.T) {
	tests := []string{"", "bad/id", ".", ".."}
	for _, resourceID := range tests {
		t.Run(resourceID, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
				calls++
				return resource.BindingResult{}, nil
			}
			service := newBindingService(t, ports)
			command := validRegisterCommand()
			command.ResourceID = resource.ResourceID(resourceID)
			_, err := service.RegisterBinding(context.Background(), command)
			requireFaultClass(t, err, fault.InvalidInput)
			if calls != 0 {
				t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestRegisterBindingRefusesMalformedServiceOrOperation(t *testing.T) {
	tests := []struct {
		name       string
		service    service.ServiceID
		operations []resource.Operation
	}{
		{name: "empty service", service: "", operations: []resource.Operation{resource.OperationGitRefUpdate}},
		{name: "malformed service", service: "bad/service", operations: []resource.Operation{resource.OperationGitRefUpdate}},
		{name: "public operation claimed as resource operation", service: "git-service", operations: []resource.Operation{resource.Operation("task.create")}},
		{name: "empty operation", service: "git-service", operations: []resource.Operation{resource.Operation("")}},
		{name: "unknown resource operation", service: "git-service", operations: []resource.Operation{resource.Operation("git.ref.read")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
				calls++
				return resource.BindingResult{}, nil
			}
			service := newBindingService(t, ports)
			command := validRegisterCommand()
			command.EnforcingService = test.service
			command.Operations = test.operations
			_, err := service.RegisterBinding(context.Background(), command)
			requireFaultClass(t, err, fault.InvalidInput)
			if calls != 0 {
				t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestUpdateBindingBuildsVersionedCommitUnit(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingUnit
	ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
		captured = unit
		return resource.BindingResult{Version: unit.Binding.Version}, nil
	}
	bindingService := newBindingService(t, ports)

	command := resource.UpdateBindingCommand{
		Actor:            bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:            projectScope("tenant-a", "project-a"),
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: service.ServiceID("git-service"),
		TrustedOrigins:   []string{"https://git.example.com/shop"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"environment": "staging"},
		ExpectedVersion:  3,
		CallerKey:        "caller-update",
		Content:          receiptContent(`{"environment":"staging"}`),
	}
	result, err := bindingService.UpdateBinding(context.Background(), command)
	if err != nil {
		t.Fatalf("UpdateBinding: %v", err)
	}
	if result.Version != 4 || result.Replayed {
		t.Fatalf("result = %+v; want version 4 not replayed", result)
	}
	if captured.Owner != projectScope("tenant-a", "project-a") {
		t.Fatalf("owner = %+v; want project-a", captured.Owner)
	}
	if captured.BindingID != "binding-a" || captured.ExpectedVersion != 3 {
		t.Fatalf("unit target = %q expected %d; want binding-a expected 3", captured.BindingID, captured.ExpectedVersion)
	}
	if captured.Binding.Version != 4 {
		t.Fatalf("binding version = %d; want 4", captured.Binding.Version)
	}
	requireBindingReceipt(t, captured.Receipt, "tenant-a", "project-a", "principal-admin", "resource.binding.update", "caller-update", command.Content, testNow)
	requireBindingAudit(t, captured.Audit, projectScope("tenant-a", "project-a"), command.Actor.ID, "resource.binding.update", testNow)
	requireBindingConfigEvent(t, captured.Events, projectScope("tenant-a", "project-a"), "binding-a", 4, command.Actor.ID, testNow)
	requireBindingAuthority(t, captured.Authority, "resource.binding.update", authorize.CheckMembership, projectScope("tenant-a", "project-a"), command.Actor.ID)
	versionRead := requireBindingAuthority(t, captured.Authority, "resource.binding.update", authorize.CheckBindingVersion, projectScope("tenant-a", "project-a"), command.Actor.ID)
	if versionRead.TargetID != "binding-a" || versionRead.ExpectedVersion != 3 {
		t.Fatalf("binding version read = target %q expected %d; want binding-a expected 3", versionRead.TargetID, versionRead.ExpectedVersion)
	}
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestUpdateBindingRefusesZeroExpectedVersion(t *testing.T) {
	ports := &fakeBindingPorts{}
	calls := 0
	ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
		calls++
		return resource.BindingResult{}, nil
	}
	bindingService := newBindingService(t, ports)
	command := resource.UpdateBindingCommand{
		Actor:            bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:            projectScope("tenant-a", "project-a"),
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: service.ServiceID("git-service"),
		TrustedOrigins:   []string{"https://git.example.com/shop"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		ExpectedVersion:  0,
		CallerKey:        "caller-update",
		Content:          receiptContent(`{}`),
	}
	_, err := bindingService.UpdateBinding(context.Background(), command)
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("Apply calls = %d; want 0 before refusal", calls)
	}
}

func TestDeleteBindingBuildsAtomicCommitUnit(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingDeleteUnit
	ports.delete = func(_ context.Context, unit resource.BindingDeleteUnit) (resource.BindingDeleteResult, error) {
		captured = unit
		return resource.BindingDeleteResult{Version: unit.ExpectedVersion + 1}, nil
	}
	service := newBindingService(t, ports)

	command := resource.DeleteBindingCommand{
		Actor:           bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:           projectScope("tenant-a", "project-a"),
		BindingID:       "binding-a",
		ExpectedVersion: 2,
		CallerKey:       "caller-delete",
		Content:         receiptContent(`{"reason":"retired"}`),
	}
	result, err := service.DeleteBinding(context.Background(), command)
	if err != nil {
		t.Fatalf("DeleteBinding: %v", err)
	}
	if result.Version != 3 || result.Replayed {
		t.Fatalf("result = %+v; want version 3 not replayed", result)
	}
	if captured.Owner != projectScope("tenant-a", "project-a") {
		t.Fatalf("owner = %+v; want project-a", captured.Owner)
	}
	if captured.BindingID != "binding-a" || captured.ExpectedVersion != 2 {
		t.Fatalf("unit target = %q expected %d; want binding-a expected 2", captured.BindingID, captured.ExpectedVersion)
	}
	if !captured.At.Equal(testNow) {
		t.Fatalf("unit time = %v; want %v", captured.At, testNow)
	}
	requireBindingReceipt(t, captured.Receipt, "tenant-a", "project-a", "principal-admin", "resource.binding.delete", "caller-delete", command.Content, testNow)
	requireBindingAudit(t, captured.Audit, projectScope("tenant-a", "project-a"), command.Actor.ID, "resource.binding.delete", testNow)
	requireBindingConfigEvent(t, captured.Events, projectScope("tenant-a", "project-a"), "binding-a", 3, command.Actor.ID, testNow)
	requireBindingAuthority(t, captured.Authority, "resource.binding.delete", authorize.CheckMembership, projectScope("tenant-a", "project-a"), command.Actor.ID)
	versionRead := requireBindingAuthority(t, captured.Authority, "resource.binding.delete", authorize.CheckBindingVersion, projectScope("tenant-a", "project-a"), command.Actor.ID)
	if versionRead.TargetID != "binding-a" || versionRead.ExpectedVersion != 2 {
		t.Fatalf("binding version read = target %q expected %d; want binding-a expected 2", versionRead.TargetID, versionRead.ExpectedVersion)
	}
	requirePublishPostCommit(t, captured.PostCommit)
}

func TestDeleteBindingRefusesZeroExpectedVersion(t *testing.T) {
	ports := &fakeBindingPorts{}
	calls := 0
	ports.delete = func(context.Context, resource.BindingDeleteUnit) (resource.BindingDeleteResult, error) {
		calls++
		return resource.BindingDeleteResult{}, nil
	}
	service := newBindingService(t, ports)
	_, err := service.DeleteBinding(context.Background(), resource.DeleteBindingCommand{
		Actor:           bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:           projectScope("tenant-a", "project-a"),
		BindingID:       "binding-a",
		ExpectedVersion: 0,
		CallerKey:       "caller-delete",
		Content:         receiptContent(`{}`),
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if calls != 0 {
		t.Fatalf("DeleteBinding calls = %d; want 0 before refusal", calls)
	}
}

func TestReadBindingUsesScopedRead(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingReadRequest
	ports.read = func(_ context.Context, request resource.BindingReadRequest) (resource.Binding, error) {
		captured = request
		return validBinding(), nil
	}
	service := newBindingService(t, ports)

	result, err := service.ReadBinding(context.Background(), resource.ReadBindingCommand{
		Actor:     bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:     projectScope("tenant-a", "project-a"),
		BindingID: "binding-a",
	})
	if err != nil {
		t.Fatalf("ReadBinding: %v", err)
	}
	if captured.Scope != projectScope("tenant-a", "project-a") || captured.BindingID != "binding-a" {
		t.Fatalf("read request = %+v; want project-a binding-a", captured)
	}
	if result.ID != "binding-a" || result.ResourceID != "resource-a" || result.Version != 3 {
		t.Fatalf("binding = %+v; want binding-a resource-a version 3", result)
	}
}

func TestReadBindingRefusesCrossScope(t *testing.T) {
	tests := []struct {
		name  string
		actor scopeFixture
		scope scopeFixture
		want  fault.Class
	}{
		{name: "cross tenant", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-b", project: "project-b"}, want: fault.Forbidden},
		{name: "cross project", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-b"}, want: fault.Forbidden},
		{name: "tenant scope", actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a"}, want: fault.InvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.read = func(context.Context, resource.BindingReadRequest) (resource.Binding, error) {
				calls++
				return validBinding(), nil
			}
			service := newBindingService(t, ports)
			_, err := service.ReadBinding(context.Background(), resource.ReadBindingCommand{
				Actor:     bindingActor(test.actor.tenant, test.actor.project, "principal-admin"),
				Scope:     tenancyScope(test.scope.tenant, test.scope.project),
				BindingID: "binding-a",
			})
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("ReadBinding calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestReadBindingRefusesReturnedCrossScopeBinding(t *testing.T) {
	ports := &fakeBindingPorts{}
	ports.read = func(context.Context, resource.BindingReadRequest) (resource.Binding, error) {
		binding := validBinding()
		binding.Scope = projectScope("tenant-b", "project-b")
		return binding, nil
	}
	service := newBindingService(t, ports)
	_, err := service.ReadBinding(context.Background(), resource.ReadBindingCommand{
		Actor:     bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:     projectScope("tenant-a", "project-a"),
		BindingID: "binding-a",
	})
	requireFaultClass(t, err, fault.Forbidden)
}

func TestQueryBindingsBuildsScopedQuery(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingQuery
	page := resource.BindingPage{Bindings: []resource.Binding{validBinding()}, NextCursor: "cursor-2"}
	ports.query = func(_ context.Context, query resource.BindingQuery) (resource.BindingPage, error) {
		captured = query
		return page, nil
	}
	service := newBindingService(t, ports)

	result, err := service.QueryBindings(context.Background(), resource.QueryBindingsCommand{
		Actor:            bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:            projectScope("tenant-a", "project-a"),
		EnforcingService: "git-service",
		ResourceID:       "resource-a",
		Cursor:           "cursor-1",
		Limit:            25,
	})
	if err != nil {
		t.Fatalf("QueryBindings: %v", err)
	}
	if captured.Scope != projectScope("tenant-a", "project-a") {
		t.Fatalf("query scope = %+v; want project-a", captured.Scope)
	}
	if captured.EnforcingService != "git-service" || captured.ResourceID != "resource-a" {
		t.Fatalf("query filters = %q/%q; want git-service/resource-a", captured.EnforcingService, captured.ResourceID)
	}
	if captured.Cursor != "cursor-1" || captured.Limit != 25 {
		t.Fatalf("query page = cursor %q limit %d; want cursor-1 limit 25", captured.Cursor, captured.Limit)
	}
	if result.NextCursor != "cursor-2" || len(result.Bindings) != 1 {
		t.Fatalf("page = %+v; want one binding and next cursor", result)
	}
}

func TestQueryBindingsRefusesInvalidBoundsAndCrossScope(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		actor scopeFixture
		scope scopeFixture
		want  fault.Class
	}{
		{name: "zero limit", limit: 0, actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-a"}, want: fault.InvalidInput},
		{name: "negative limit", limit: -1, actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-a"}, want: fault.InvalidInput},
		{name: "oversized limit", limit: resource.MaxBindingPageSize + 1, actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-a"}, want: fault.InvalidInput},
		{name: "cross tenant", limit: 10, actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-b", project: "project-b"}, want: fault.Forbidden},
		{name: "cross project", limit: 10, actor: scopeFixture{tenant: "tenant-a", project: "project-a"}, scope: scopeFixture{tenant: "tenant-a", project: "project-b"}, want: fault.Forbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ports := &fakeBindingPorts{}
			calls := 0
			ports.query = func(context.Context, resource.BindingQuery) (resource.BindingPage, error) {
				calls++
				return resource.BindingPage{}, nil
			}
			service := newBindingService(t, ports)
			_, err := service.QueryBindings(context.Background(), resource.QueryBindingsCommand{
				Actor: bindingActor(test.actor.tenant, test.actor.project, "principal-admin"),
				Scope: tenancyScope(test.scope.tenant, test.scope.project),
				Limit: test.limit,
			})
			requireFaultClass(t, err, test.want)
			if calls != 0 {
				t.Fatalf("QueryBindings calls = %d; want 0 before refusal", calls)
			}
		})
	}
}

func TestQueryBindingsRefusesReturnedCrossScopeBinding(t *testing.T) {
	ports := &fakeBindingPorts{}
	ports.query = func(context.Context, resource.BindingQuery) (resource.BindingPage, error) {
		binding := validBinding()
		binding.Scope = projectScope("tenant-b", "project-b")
		return resource.BindingPage{Bindings: []resource.Binding{binding}}, nil
	}
	service := newBindingService(t, ports)
	_, err := service.QueryBindings(context.Background(), resource.QueryBindingsCommand{
		Actor: bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope: projectScope("tenant-a", "project-a"),
		Limit: 10,
	})
	requireFaultClass(t, err, fault.Forbidden)
}

func TestQueryBindingsReturnsDefensiveCopy(t *testing.T) {
	ports := &fakeBindingPorts{}
	stored := resource.BindingPage{Bindings: []resource.Binding{validBinding()}}
	ports.query = func(context.Context, resource.BindingQuery) (resource.BindingPage, error) {
		return stored, nil
	}
	service := newBindingService(t, ports)
	result, err := service.QueryBindings(context.Background(), resource.QueryBindingsCommand{
		Actor: bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope: projectScope("tenant-a", "project-a"),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("QueryBindings: %v", err)
	}
	result.Bindings[0].Metadata["environment"] = "mutated"
	result.Bindings[0].TrustedOrigins[0] = "https://evil.example.com"
	if stored.Bindings[0].Metadata["environment"] != "production" || stored.Bindings[0].TrustedOrigins[0] != "https://git.example.com/shop" {
		t.Fatal("returned binding page aliases the port-owned page")
	}
}
