package resource_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type sequentialIDs struct{ next int }

func (source *sequentialIDs) NewEventID() (id.EventID, error) {
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *sequentialIDs) NewReceiptID() (id.ReceiptID, error) {
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

// Compile-time pins for the injected boundaries the binding service must expose.
var (
	_ resource.Clock    = fixedClock{}
	_ resource.IDSource = (*sequentialIDs)(nil)
)

func unexpectedCall(name string) error {
	return fault.New(fault.InternalFailure, "unexpected port call "+name)
}

type fakeBindingPorts struct {
	apply  func(context.Context, resource.BindingUnit) (resource.BindingResult, error)
	delete func(context.Context, resource.BindingDeleteUnit) (resource.BindingDeleteResult, error)
	read   func(context.Context, resource.BindingReadRequest) (resource.Binding, error)
	query  func(context.Context, resource.BindingQuery) (resource.BindingPage, error)
}

func (ports *fakeBindingPorts) Apply(ctx context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
	if ports.apply == nil {
		return resource.BindingResult{}, unexpectedCall("Apply")
	}
	return ports.apply(ctx, unit)
}

func (ports *fakeBindingPorts) DeleteBinding(ctx context.Context, unit resource.BindingDeleteUnit) (resource.BindingDeleteResult, error) {
	if ports.delete == nil {
		return resource.BindingDeleteResult{}, unexpectedCall("DeleteBinding")
	}
	return ports.delete(ctx, unit)
}

func (ports *fakeBindingPorts) ReadBinding(ctx context.Context, request resource.BindingReadRequest) (resource.Binding, error) {
	if ports.read == nil {
		return resource.Binding{}, unexpectedCall("ReadBinding")
	}
	return ports.read(ctx, request)
}

func (ports *fakeBindingPorts) QueryBindings(ctx context.Context, query resource.BindingQuery) (resource.BindingPage, error) {
	if ports.query == nil {
		return resource.BindingPage{}, unexpectedCall("QueryBindings")
	}
	return ports.query(ctx, query)
}

func newBindingService(t *testing.T, ports *fakeBindingPorts) *resource.Service {
	t.Helper()
	service, err := resource.NewService(resource.Config{
		Clock:    fixedClock{now: testNow},
		IDs:      &sequentialIDs{},
		Bindings: ports,
		Deletes:  ports,
		Reads:    ports,
		Queries:  ports,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if service == nil {
		t.Fatal("NewService returned a nil service")
	}
	return service
}

func bindingActor(tenant, project, principal string) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
}

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func validRegisterCommand() resource.RegisterBindingCommand {
	return resource.RegisterBindingCommand{
		Actor:            bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:            projectScope("tenant-a", "project-a"),
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: service.ServiceID("git-service"),
		TrustedOrigins:   []string{"https://git.example.com/shop"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         map[string]string{"environment": "production"},
		CallerKey:        "caller-register",
		Content:          receiptContent(`{"resource":"resource-a"}`),
	}
}
