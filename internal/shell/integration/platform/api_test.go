// Platform hosted-authority API pinned by these tests.
//
// The declared seam is authorize.HostedAuthority / HostedAuthorityRequest /
// HostedAuthorityResponse. The concrete adapter is configured with one
// application's OAuth client, claim match and RBAC table, but it must read the
// verified token's current claim values separately from the declared request:
// HostedAuthorityRequest.Claims is the configured claim match, not a
// map[string][]string of token values. The adapter therefore takes a Source:
//
//	type Application struct {
//	    Name   string
//	    Client identity.OAuthClientConfig
//	    Claims identity.OAuthClaims
//	    RBAC   authorize.RBACTable
//	}
//	type Snapshot struct {
//	    Applications []Application
//	    ClaimValues  map[string][]string
//	}
//	type Source interface {
//	    Snapshot(context.Context, authorize.HostedAuthorityRequest) (Snapshot, error)
//	}
//	type Config struct {
//	    Source Source
//	    Logf   func(format string, args ...any)
//	}
//	func NewAuthority(Config) (*Authority, error)
//	func (*Authority) Check(context.Context, authorize.HostedAuthorityRequest) (authorize.HostedAuthorityResponse, error)
//
// The Source seam exists because the declared request alone cannot carry
// missing/duplicate/ambiguous tenant claim values or multiple legitimate roles.
//
// Semantics pinned by the tests:
//   - Every Check validates the declared request first and calls
//     Source.Snapshot once. A policy refusal is a normal response with
//     Allowed=false and no error; only invalid input and dependency failure
//     return errors.
//   - The tenant claim must have exactly one value equal to the configured
//     tenant. Missing, empty, duplicate or ambiguous tenant values are refused.
//   - The configured role string must be one of the role claim's values. Extra
//     legitimate roles do not invalidate the match.
//   - Exactly one application may match; zero is unknown application and more
//     than one is ambiguous. Both are refusals.
//   - The matched application's own RBAC table must grant every RequiredAccess
//     string. RBAC is never merged across applications and never cached.
//   - Any Source error, malformed snapshot or invalid application config is
//     dependency_unavailable and never an allowed decision. There is no
//     standalone-grant fallback.
package platform

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type testApplication struct {
	name   string
	client identity.OAuthClientConfig
	claims identity.OAuthClaims
	rbac   authorize.RBACTable
}

func testOAuthClient() identity.OAuthClientConfig {
	return identity.OAuthClientConfig{
		Protocol:        identity.OAuthProtocolOIDC,
		Issuer:          "https://auth.example/",
		ClientID:        "client-1",
		ClientSecretRef: "secret/platform-client",
		Audience:        "platform-api",
		Scopes:          []string{"openid", "profile"},
		RedirectURL:     "https://kodama.example/oauth/callback",
	}
}

func testOAuthClaims() identity.OAuthClaims {
	return identity.OAuthClaims{
		TenantClaim: "https://kodama/tenant",
		Tenant:      "tenant-a",
		RoleClaim:   "https://kodama/roles",
		Role:        "operator",
		Subject:     "auth0|abc123",
	}
}

func defaultTestApplication() testApplication {
	return testApplication{
		name:   "app-a",
		client: testOAuthClient(),
		claims: testOAuthClaims(),
		rbac: authorize.RBACTable{
			"operator": {"platform.read", "platform.invoke"},
		},
	}
}

func testHostedRequest() authorize.HostedAuthorityRequest {
	return authorize.HostedAuthorityRequest{
		Protocol:       identity.OAuthProtocolOIDC,
		Scope:          tenancyScope(),
		PrincipalID:    "oidc-0123456789abcdef0123456789abcdef",
		Operation:      "model.invoke",
		Claims:         testOAuthClaims(),
		RequiredAccess: []string{"platform.invoke"},
	}
}

func tenancyScope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-1"}
}

// authority is the declared authorize.HostedAuthority shape. The production
// *Authority must satisfy it; see the compile-time assertion below.
type authority interface {
	Check(context.Context, authorize.HostedAuthorityRequest) (authorize.HostedAuthorityResponse, error)
}

var _ authorize.HostedAuthority = (*Authority)(nil)

type source interface {
	Snapshot(context.Context, authorize.HostedAuthorityRequest) (Snapshot, error)
}

type fakeSource struct {
	mu           sync.Mutex
	applications []Application
	claimValues  map[string][]string
	err          error
	calls        int
}

func (fake *fakeSource) Snapshot(_ context.Context, request authorize.HostedAuthorityRequest) (Snapshot, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if fake.err != nil {
		return Snapshot{}, fake.err
	}
	applications := make([]Application, len(fake.applications))
	copy(applications, fake.applications)
	var values map[string][]string
	if fake.claimValues != nil {
		values = make(map[string][]string, len(fake.claimValues))
		for name, entries := range fake.claimValues {
			values[name] = append([]string(nil), entries...)
		}
	}
	return Snapshot{Applications: applications, ClaimValues: values}, nil
}

func newFakeSource(applications []testApplication, claimValues map[string][]string) *fakeSource {
	fake := &fakeSource{}
	fake.setApplications(applications)
	fake.setClaimValues(claimValues)
	return fake
}

func (fake *fakeSource) setApplications(applications []testApplication) {
	converted := make([]Application, len(applications))
	for index, application := range applications {
		converted[index] = Application{
			Name:   application.name,
			Client: application.client,
			Claims: application.claims,
			RBAC:   application.rbac,
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.applications = converted
}

func (fake *fakeSource) setClaimValues(values map[string][]string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.claimValues = values
}

func (fake *fakeSource) setRBAC(name string, table authorize.RBACTable) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for index := range fake.applications {
		if fake.applications[index].Name == name {
			fake.applications[index].RBAC = table
		}
	}
}

func (fake *fakeSource) setError(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.err = err
}

func (fake *fakeSource) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

type testAuthorityConfig struct {
	source source
	logf   func(format string, args ...any)
}

// buildAuthority is the only place that names the production Config and
// Authority types.
func buildAuthority(config testAuthorityConfig) (authority, error) {
	return NewAuthority(Config{Source: config.source, Logf: config.logf})
}

func newAuthority(t *testing.T, source source) authority {
	t.Helper()
	instance, err := buildAuthority(testAuthorityConfig{source: source})
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	if instance == nil {
		t.Fatal("NewAuthority returned a nil authority")
	}
	return instance
}

func newAuthorityWithLog(t *testing.T, source source, logf func(format string, args ...any)) authority {
	t.Helper()
	instance, err := buildAuthority(testAuthorityConfig{source: source, logf: logf})
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	if instance == nil {
		t.Fatal("NewAuthority returned a nil authority")
	}
	return instance
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func requireNoMarker(t *testing.T, text string, markers ...string) {
	t.Helper()
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			t.Fatalf("rendered text leaked marker %q: %s", marker, text)
		}
	}
}
