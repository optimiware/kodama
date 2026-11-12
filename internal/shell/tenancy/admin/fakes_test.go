package tenancyadmin_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
	"github.com/optimiware/kodama/internal/shell/token"
)

// testNow is the injected authoritative time for every administration command.
var testNow = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

// fixedClock is the deterministic Clock used by the tests.
type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

// sequentialIDs is a deterministic IDSource. Every audit, receipt and event
// identity is distinct and attributable to the call that requested it.
type sequentialIDs struct{ next int }

func (source *sequentialIDs) NewEventID() (id.EventID, error) {
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *sequentialIDs) NewReceiptID() (id.ReceiptID, error) {
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

// Compile-time pins for the injected boundaries the service must expose.
var (
	_ tenancyadmin.Clock    = fixedClock{}
	_ tenancyadmin.IDSource = (*sequentialIDs)(nil)
)

func unexpectedCall(name string) error {
	return fault.New(fault.InternalFailure, "unexpected port call "+name)
}

// fakeAdminPorts implements every tenancy administration port. Tests set only
// the callbacks they exercise; an unexpected call is a test failure rather than
// a silent zero result.
type fakeAdminPorts struct {
	createTenant     func(context.Context, tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error)
	rotateTenantKey  func(context.Context, tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error)
	createProject    func(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error)
	updateProject    func(context.Context, tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error)
	grantMembership  func(context.Context, tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error)
	revokeMembership func(context.Context, tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error)
	listMemberships  func(context.Context, tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error)
}

func (ports *fakeAdminPorts) CreateTenant(ctx context.Context, unit tenancyadmin.TenantCreateUnit) (tenancyadmin.TenantCreateResult, error) {
	if ports.createTenant == nil {
		return tenancyadmin.TenantCreateResult{}, unexpectedCall("CreateTenant")
	}
	return ports.createTenant(ctx, unit)
}

func (ports *fakeAdminPorts) RotateTenantKey(ctx context.Context, unit tenancyadmin.TenantKeyRotationUnit) (tenancyadmin.TenantKeyRotationResult, error) {
	if ports.rotateTenantKey == nil {
		return tenancyadmin.TenantKeyRotationResult{}, unexpectedCall("RotateTenantKey")
	}
	return ports.rotateTenantKey(ctx, unit)
}

func (ports *fakeAdminPorts) CreateProject(ctx context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
	if ports.createProject == nil {
		return tenancyadmin.ProjectCreateResult{}, unexpectedCall("CreateProject")
	}
	return ports.createProject(ctx, unit)
}

func (ports *fakeAdminPorts) UpdateProject(ctx context.Context, unit tenancyadmin.ProjectUpdateUnit) (tenancyadmin.ProjectUpdateResult, error) {
	if ports.updateProject == nil {
		return tenancyadmin.ProjectUpdateResult{}, unexpectedCall("UpdateProject")
	}
	return ports.updateProject(ctx, unit)
}

func (ports *fakeAdminPorts) GrantMembership(ctx context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
	if ports.grantMembership == nil {
		return tenancyadmin.MembershipGrantResult{}, unexpectedCall("GrantMembership")
	}
	return ports.grantMembership(ctx, unit)
}

func (ports *fakeAdminPorts) RevokeMembership(ctx context.Context, unit tenancyadmin.MembershipRevokeUnit) (tenancyadmin.MembershipRevokeResult, error) {
	if ports.revokeMembership == nil {
		return tenancyadmin.MembershipRevokeResult{}, unexpectedCall("RevokeMembership")
	}
	return ports.revokeMembership(ctx, unit)
}

func (ports *fakeAdminPorts) ListMemberships(ctx context.Context, query tenancyadmin.MembershipQuery) (tenancyadmin.MembershipPage, error) {
	if ports.listMemberships == nil {
		return tenancyadmin.MembershipPage{}, unexpectedCall("ListMemberships")
	}
	return ports.listMemberships(ctx, query)
}

func newAdminService(t *testing.T, ports *fakeAdminPorts) *tenancyadmin.Service {
	t.Helper()
	service, err := tenancyadmin.NewService(tenancyadmin.Config{
		Clock:             fixedClock{now: testNow},
		IDs:               &sequentialIDs{},
		TenantCreate:      ports,
		TenantKeyRotation: ports,
		ProjectCreate:     ports,
		ProjectUpdate:     ports,
		MembershipGrant:   ports,
		MembershipRevoke:  ports,
		MembershipQuery:   ports,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if service == nil {
		t.Fatal("NewService returned a nil service")
	}
	return service
}

func adminActor(tenant, project, principal string) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID(principal),
		Kind:    identity.KindHuman,
		Scope:   tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
}

func tenantScope(tenant string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant)}
}

func projectScope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

func tenantKey(tenant string, version uint64) token.TenantKey {
	return token.TenantKey{
		TenantID:   id.TenantID(tenant),
		Version:    version,
		Ciphertext: []byte("encrypted-derivation-key"),
		WrappedKey: []byte("wrapped-key"),
		WrapNonce:  []byte("wrap-nonce"),
		KeyRef:     "kms/tenant-key",
		KeyVersion: 1,
	}
}
