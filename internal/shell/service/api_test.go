// Pinned service-account creation API and value validation tests.
//
// The declared values live in types.go and ports.go; live administration
// operations commit through Admin and the pinned units:
//
//	func NewService(CreateRequest) (Service, error)
//
//	type CreateRequest struct {
//	    ID    ServiceID
//	    Scope tenancy.Scope
//	    State State
//	    Roles []tenancy.Role
//	}
//
// Rules pinned by these tests (S04 §1, S05 §1–3):
//   - A created service starts at version 1 with no credential pair.
//   - Services never hold Admin.
//   - The credential nonce/expiry pair is both present or both absent and a
//     present nonce is at least 32 bytes (declared Service.Validate rules).
package service_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func testScope() tenancy.Scope {
	return tenancy.Scope{
		TenantID:  id.TenantID("tenant-a"),
		ProjectID: id.ProjectID("project-1"),
	}
}

func testNonce() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func testExpiry() time.Time {
	return time.Unix(1_700_003_600, 0).UTC()
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func newEnabledService(t *testing.T, roles ...tenancy.Role) service.Service {
	t.Helper()
	created, err := service.NewService(service.CreateRequest{
		ID:    service.ServiceID("svc-1"),
		Scope: testScope(),
		State: service.StateEnabled,
		Roles: roles,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return created
}

// TestNewServiceStartsAtVersionOneWithoutCredential pins the create shape:
// version 1, explicit state, service-assignable roles and no token plaintext or
// pair invented by creation.
func TestNewServiceStartsAtVersionOneWithoutCredential(t *testing.T) {
	created := newEnabledService(t, tenancy.RoleEngineer, tenancy.RoleOperator)

	if created.ID != service.ServiceID("svc-1") {
		t.Fatalf("ID = %q; want svc-1", created.ID)
	}
	if created.Scope != testScope() {
		t.Fatalf("Scope = %+v; want %+v", created.Scope, testScope())
	}
	if created.State != service.StateEnabled {
		t.Fatalf("State = %q; want enabled", created.State)
	}
	if created.Version != 1 {
		t.Fatalf("Version = %d; want 1", created.Version)
	}
	if len(created.Roles) != 2 || created.Roles[0] != tenancy.RoleEngineer || created.Roles[1] != tenancy.RoleOperator {
		t.Fatalf("Roles = %v; want [engineer operator]", created.Roles)
	}
	if len(created.TokenNonce) != 0 || created.TokenExpiry != nil {
		t.Fatalf("created service carries a credential pair: nonce=%d expiry=%v", len(created.TokenNonce), created.TokenExpiry)
	}
	if err := created.Validate(); err != nil {
		t.Fatalf("created service is invalid: %v", err)
	}
}

// TestNewServiceRefusesAdminAndInvalidInput pins the refusal matrix for a
// service account: Admin is never service-assignable and malformed identity,
// scope or state is invalid input rather than a silently coerced value.
func TestNewServiceRefusesAdminAndInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		request service.CreateRequest
	}{
		{
			name: "admin role",
			request: service.CreateRequest{
				ID:    service.ServiceID("svc-1"),
				Scope: testScope(),
				State: service.StateEnabled,
				Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleAdmin},
			},
		},
		{
			name: "unknown role",
			request: service.CreateRequest{
				ID:    service.ServiceID("svc-1"),
				Scope: testScope(),
				State: service.StateEnabled,
				Roles: []tenancy.Role{tenancy.Role("owner")},
			},
		},
		{
			name: "empty state",
			request: service.CreateRequest{
				ID:    service.ServiceID("svc-1"),
				Scope: testScope(),
			},
		},
		{
			name: "unknown state",
			request: service.CreateRequest{
				ID:    service.ServiceID("svc-1"),
				Scope: testScope(),
				State: service.State("suspended"),
			},
		},
		{
			name: "invalid service id",
			request: service.CreateRequest{
				ID:    service.ServiceID("../svc"),
				Scope: testScope(),
				State: service.StateEnabled,
			},
		},
		{
			name: "invalid scope",
			request: service.CreateRequest{
				ID:    service.ServiceID("svc-1"),
				Scope: tenancy.Scope{},
				State: service.StateEnabled,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.NewService(testCase.request)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

// TestServiceValidateCredentialPairAndAdmin pins the declared value rules that
// guard persisted rows: a half credential pair, a short nonce and Admin on a
// service are all refused.
func TestServiceValidateCredentialPairAndAdmin(t *testing.T) {
	expiry := testExpiry()

	nonceOnly := newEnabledService(t)
	nonceOnly.TokenNonce = testNonce()
	if err := nonceOnly.Validate(); err == nil {
		t.Fatal("service with a nonce but no expiry accepted")
	}

	expiryOnly := newEnabledService(t)
	expiryOnly.TokenExpiry = &expiry
	if err := expiryOnly.Validate(); err == nil {
		t.Fatal("service with an expiry but no nonce accepted")
	}

	shortNonce := newEnabledService(t)
	shortNonce.TokenNonce = []byte("short")
	shortNonce.TokenExpiry = &expiry
	if err := shortNonce.Validate(); err == nil {
		t.Fatal("service with a short nonce accepted")
	}

	admin := newEnabledService(t)
	admin.Roles = []tenancy.Role{tenancy.RoleAdmin}
	if err := admin.Validate(); err == nil {
		t.Fatal("service with the Admin role accepted")
	}
}
