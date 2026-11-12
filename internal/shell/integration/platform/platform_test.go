package platform

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func hostedClaimValues(tenants, roles []string) map[string][]string {
	claims := testOAuthClaims()
	values := make(map[string][]string)
	if tenants != nil {
		values[claims.TenantClaim] = tenants
	}
	if roles != nil {
		values[claims.RoleClaim] = roles
	}
	return values
}

func requestForTenant(tenant string, access ...string) authorize.HostedAuthorityRequest {
	request := testHostedRequest()
	request.Claims.Tenant = tenant
	request.Scope = tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: "project-1"}
	request.RequiredAccess = access
	return request
}

func requireAllowed(t *testing.T, response authorize.HostedAuthorityResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !response.Decision.Allowed {
		t.Fatalf("decision denied: %+v", response.Decision)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("allowed response is invalid: %v", err)
	}
}

func requireDenied(t *testing.T, response authorize.HostedAuthorityResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("policy refusal returned error: %v", err)
	}
	if response.Decision.Allowed {
		t.Fatalf("decision allowed: %+v", response.Decision)
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("deny response is invalid: %v", err)
	}
}

func TestCheckAllowsMatchingTenantRoleAndAccess(t *testing.T) {
	source := newFakeSource(
		[]testApplication{defaultTestApplication()},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)

	response, err := authority.Check(context.Background(), testHostedRequest())
	requireAllowed(t, response, err)
	if response.Protocol != identity.OAuthProtocolOIDC {
		t.Fatalf("response protocol = %q; want %q", response.Protocol, identity.OAuthProtocolOIDC)
	}
	if response.Decision.Role != "operator" {
		t.Fatalf("decision role = %q; want operator", response.Decision.Role)
	}
	wantAccess := []string{"platform.read", "platform.invoke"}
	if !reflect.DeepEqual(response.Decision.Access, wantAccess) {
		t.Fatalf("decision access = %v; want %v", response.Decision.Access, wantAccess)
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("source calls = %d; want 1", got)
	}
}

func TestCheckHonorsMultipleLegitimateRoles(t *testing.T) {
	source := newFakeSource(
		[]testApplication{defaultTestApplication()},
		hostedClaimValues([]string{"tenant-a"}, []string{"viewer", "operator", "auditor"}),
	)
	authority := newAuthority(t, source)
	response, err := authority.Check(context.Background(), testHostedRequest())
	requireAllowed(t, response, err)

	source.setClaimValues(hostedClaimValues([]string{"tenant-a"}, []string{"viewer", "auditor"}))
	response, err = authority.Check(context.Background(), testHostedRequest())
	requireDenied(t, response, err)
}

func TestCheckRefusesMissingAmbiguousOrDuplicateTenantClaim(t *testing.T) {
	cases := []struct {
		name   string
		values map[string][]string
	}{
		{"missing_tenant_key", map[string][]string{testOAuthClaims().RoleClaim: {"operator"}}},
		{"empty_tenant_values", hostedClaimValues([]string{}, []string{"operator"})},
		{"duplicate_same_tenant", hostedClaimValues([]string{"tenant-a", "tenant-a"}, []string{"operator"})},
		{"ambiguous_tenant_values", hostedClaimValues([]string{"tenant-a", "tenant-b"}, []string{"operator"})},
		{"wrong_tenant", hostedClaimValues([]string{"tenant-b"}, []string{"operator"})},
		{"missing_role_key", hostedClaimValues([]string{"tenant-a"}, nil)},
		{"empty_role_values", hostedClaimValues([]string{"tenant-a"}, []string{})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := newFakeSource([]testApplication{defaultTestApplication()}, testCase.values)
			authority := newAuthority(t, source)
			response, err := authority.Check(context.Background(), testHostedRequest())
			requireDenied(t, response, err)
		})
	}
}

func TestCheckRefusesUnknownApplication(t *testing.T) {
	source := newFakeSource(
		[]testApplication{defaultTestApplication()},
		hostedClaimValues([]string{"tenant-z"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)
	response, err := authority.Check(context.Background(), requestForTenant("tenant-z", "platform.invoke"))
	requireDenied(t, response, err)
	if response.Protocol != identity.OAuthProtocolOIDC {
		t.Fatalf("deny response protocol = %q; want the request protocol", response.Protocol)
	}
}

func TestCheckRefusesRoleGrantedOnlyByAnotherApplication(t *testing.T) {
	applicationA := defaultTestApplication()
	applicationA.rbac = authorize.RBACTable{"viewer": {"platform.read"}}
	applicationB := defaultTestApplication()
	applicationB.name = "app-b"
	applicationB.claims.Tenant = "tenant-b"
	applicationB.rbac = authorize.RBACTable{"operator": {"platform.invoke"}}

	source := newFakeSource(
		[]testApplication{applicationA, applicationB},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)
	response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.invoke"))
	requireDenied(t, response, err)
}

func TestCheckEnforcesApplicationIsolation(t *testing.T) {
	applicationA := defaultTestApplication()
	applicationA.rbac = authorize.RBACTable{"operator": {"platform.read"}}
	applicationB := defaultTestApplication()
	applicationB.name = "app-b"
	applicationB.claims.Tenant = "tenant-b"
	applicationB.rbac = authorize.RBACTable{"operator": {"platform.write"}}

	source := newFakeSource(
		[]testApplication{applicationA, applicationB},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)

	// App A's read grant never satisfies app B's write access.
	response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.write"))
	requireDenied(t, response, err)

	// App B is selected only by its own tenant claim and only grants its own
	// access strings.
	source.setClaimValues(hostedClaimValues([]string{"tenant-b"}, []string{"operator"}))
	response, err = authority.Check(context.Background(), requestForTenant("tenant-b", "platform.write"))
	requireAllowed(t, response, err)

	response, err = authority.Check(context.Background(), requestForTenant("tenant-b", "platform.read"))
	requireDenied(t, response, err)
}

func TestCheckRequiresRequestAndApplicationClaimMatch(t *testing.T) {
	application := defaultTestApplication()
	application.claims.Tenant = "tenant-b"
	source := newFakeSource(
		[]testApplication{application},
		hostedClaimValues([]string{"tenant-b"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)

	// The operation's configured claim match says tenant-a, but the only
	// application is configured for tenant-b.
	response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.invoke"))
	requireDenied(t, response, err)

	// The token values and application match tenant-b, but the operation's
	// configured role is viewer rather than the application's operator.
	request := requestForTenant("tenant-b", "platform.invoke")
	request.Claims.Role = "viewer"
	response, err = authority.Check(context.Background(), request)
	requireDenied(t, response, err)

	// Both the request and the application agree on tenant-b/operator.
	request = requestForTenant("tenant-b", "platform.invoke")
	response, err = authority.Check(context.Background(), request)
	requireAllowed(t, response, err)
}

func TestCheckRefusesMissingRequiredAccess(t *testing.T) {
	application := defaultTestApplication()
	application.rbac = authorize.RBACTable{"operator": {"platform.read"}}
	source := newFakeSource(
		[]testApplication{application},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)

	response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.invoke"))
	requireDenied(t, response, err)

	response, err = authority.Check(context.Background(), requestForTenant("tenant-a", "platform.read", "platform.invoke"))
	requireDenied(t, response, err)

	response, err = authority.Check(context.Background(), requestForTenant("tenant-a", "platform.read"))
	requireAllowed(t, response, err)
}

func TestCheckRefusesAmbiguousApplicationMatch(t *testing.T) {
	applicationA := defaultTestApplication()
	applicationA.rbac = authorize.RBACTable{"operator": {"platform.read"}}
	applicationB := defaultTestApplication()
	applicationB.name = "app-b"
	applicationB.rbac = authorize.RBACTable{"operator": {"platform.write"}}

	source := newFakeSource(
		[]testApplication{applicationA, applicationB},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)
	response, err := authority.Check(context.Background(), requestForTenant("tenant-a", "platform.read"))
	requireDenied(t, response, err)
}

func TestCheckReEvaluatesCurrentState(t *testing.T) {
	application := defaultTestApplication()
	application.rbac = authorize.RBACTable{"operator": {"platform.read"}}
	source := newFakeSource(
		[]testApplication{application},
		hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
	)
	authority := newAuthority(t, source)
	request := requestForTenant("tenant-a", "platform.invoke")

	response, err := authority.Check(context.Background(), request)
	requireDenied(t, response, err)

	source.setRBAC("app-a", authorize.RBACTable{"operator": {"platform.read", "platform.invoke"}})
	response, err = authority.Check(context.Background(), request)
	requireAllowed(t, response, err)

	source.setClaimValues(hostedClaimValues([]string{"tenant-a"}, []string{"viewer"}))
	response, err = authority.Check(context.Background(), request)
	requireDenied(t, response, err)

	source.setClaimValues(hostedClaimValues([]string{"tenant-a"}, []string{"operator"}))
	source.setRBAC("app-a", authorize.RBACTable{"operator": {"platform.read"}})
	response, err = authority.Check(context.Background(), request)
	requireDenied(t, response, err)

	if got := source.callCount(); got != 4 {
		t.Fatalf("source calls = %d; want 4", got)
	}
}

func TestCheckReusesRequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest
	}{
		{"unknown_protocol", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.Protocol = identity.OAuthProtocol("saml")
			return request
		}},
		{"invalid_scope", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.Scope = tenancy.Scope{}
			return request
		}},
		{"invalid_principal", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.PrincipalID = id.PrincipalID("bad id!")
			return request
		}},
		{"empty_operation", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.Operation = ""
			return request
		}},
		{"invalid_claims", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.Claims = identity.OAuthClaims{}
			return request
		}},
		{"empty_required_access", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.RequiredAccess = nil
			return request
		}},
		{"duplicate_required_access", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.RequiredAccess = []string{"platform.invoke", "platform.invoke"}
			return request
		}},
		{"invalid_required_access", func(request authorize.HostedAuthorityRequest) authorize.HostedAuthorityRequest {
			request.RequiredAccess = []string{"bad access"}
			return request
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := newFakeSource(
				[]testApplication{defaultTestApplication()},
				hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
			)
			authority := newAuthority(t, source)
			response, err := authority.Check(context.Background(), testCase.mutate(testHostedRequest()))
			requireFaultClass(t, err, fault.InvalidInput)
			if response.Decision.Allowed {
				t.Fatal("invalid request was allowed")
			}
			if got := source.callCount(); got != 0 {
				t.Fatalf("source was called %d times for an invalid request; want 0", got)
			}
		})
	}
}

func TestCheckValidatesProtocol(t *testing.T) {
	for _, protocol := range []identity.OAuthProtocol{identity.OAuthProtocolOAuth2, identity.OAuthProtocolOIDC} {
		t.Run(string(protocol), func(t *testing.T) {
			application := defaultTestApplication()
			application.client.Protocol = protocol
			source := newFakeSource(
				[]testApplication{application},
				hostedClaimValues([]string{"tenant-a"}, []string{"operator"}),
			)
			authority := newAuthority(t, source)
			request := testHostedRequest()
			request.Protocol = protocol
			response, err := authority.Check(context.Background(), request)
			requireAllowed(t, response, err)
			if response.Protocol != protocol {
				t.Fatalf("response protocol = %q; want %q", response.Protocol, protocol)
			}
		})
	}
}
