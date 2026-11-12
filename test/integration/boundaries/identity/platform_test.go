//go:build integration

package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/platform"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

type platformSource struct {
	mu       sync.Mutex
	snapshot platform.Snapshot
	err      error
	calls    int
}

func (source *platformSource) Snapshot(context.Context, authorize.HostedAuthorityRequest) (platform.Snapshot, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	return source.snapshot, source.err
}

func (source *platformSource) setError(err error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.err = err
}

func (source *platformSource) setSnapshot(snapshot platform.Snapshot) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.snapshot = snapshot
}

func platformClient() identity.OAuthClientConfig {
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

func platformClaims(provider *idfix.Provider) identity.OAuthClaims {
	return identity.OAuthClaims{
		TenantClaim: provider.TenantClaim(),
		Tenant:      fixtureTenant,
		RoleClaim:   provider.RoleClaim(),
		Role:        "operator",
		Subject:     "auth0|platform-human",
	}
}

func platformRequest(provider *idfix.Provider) authorize.HostedAuthorityRequest {
	return authorize.HostedAuthorityRequest{
		Protocol:       identity.OAuthProtocolOIDC,
		Scope:          tenancy.Scope{TenantID: fixtureTenant, ProjectID: fixtureProject},
		PrincipalID:    id.PrincipalID("oidc-0123456789abcdef0123456789abcdef"),
		Operation:      "model.invoke",
		Claims:         platformClaims(provider),
		RequiredAccess: []string{"platform.invoke"},
	}
}

func platformSnapshot(provider *idfix.Provider) platform.Snapshot {
	claims := platformClaims(provider)
	return platform.Snapshot{
		Applications: []platform.Application{{
			Name:   "app-a",
			Client: platformClient(),
			Claims: claims,
			RBAC: authorize.RBACTable{
				"operator": {"platform.read", "platform.invoke"},
			},
		}},
		ClaimValues: map[string][]string{
			claims.TenantClaim: {fixtureTenant},
			claims.RoleClaim:   {"viewer", "operator"},
		},
	}
}

func newPlatformAuthority(t *testing.T, source platform.Source) authorize.HostedAuthority {
	t.Helper()
	authority, err := platform.NewAuthority(platform.Config{Source: source})
	if err != nil {
		t.Fatalf("NewAuthority: %v", err)
	}
	return authority
}

func TestHostedAuthorityAllowsMatchingApplication(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	token, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|platform-human", Roles: []string{"operator"}})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token.Claims[provider.TenantClaim()] != fixtureTenant {
		t.Fatalf("minted tenant claim = %v", token.Claims[provider.TenantClaim()])
	}
	source := &platformSource{snapshot: platformSnapshot(provider)}
	authority := newPlatformAuthority(t, source)
	response, err := authority.Check(context.Background(), platformRequest(provider))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !response.Decision.Allowed || response.Decision.Role != "operator" {
		t.Fatalf("decision = %+v; want allowed operator", response.Decision)
	}
	if source.calls != 1 {
		t.Fatalf("source calls = %d; want 1", source.calls)
	}
}

func TestHostedAuthorityFailsClosedWhenUnreachable(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	source := &platformSource{snapshot: platformSnapshot(provider), err: errors.New("platform unavailable")}
	authority := newPlatformAuthority(t, source)
	response, err := authority.Check(context.Background(), platformRequest(provider))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if response.Decision.Allowed {
		t.Fatal("an unreachable hosted authority was allowed")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	response, err = authority.Check(canceled, platformRequest(provider))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, context.Canceled) || response.Decision.Allowed {
		t.Fatalf("canceled check = %+v / %v; want a dependency refusal", response, err)
	}

	// Recovery: the same authority allows once the source is reachable again.
	source.setError(nil)
	response, err = authority.Check(context.Background(), platformRequest(provider))
	if err != nil {
		t.Fatalf("recovery after an unreachable source: %v", err)
	}
	if !response.Decision.Allowed {
		t.Fatalf("recovered decision = %+v; want allowed", response.Decision)
	}
}

func TestHostedAuthorityFailsClosedWhenMalformed(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	valid := platformSnapshot(provider)
	cases := []struct {
		name     string
		snapshot platform.Snapshot
	}{
		{name: "no applications", snapshot: platform.Snapshot{ClaimValues: valid.ClaimValues}},
		{name: "no claim values", snapshot: platform.Snapshot{Applications: valid.Applications}},
		{
			name: "invalid client",
			snapshot: func() platform.Snapshot {
				malformed := valid
				malformed.Applications = append([]platform.Application(nil), valid.Applications...)
				malformed.Applications[0].Client = identity.OAuthClientConfig{}
				return malformed
			}(),
		},
		{
			name: "invalid rbac",
			snapshot: func() platform.Snapshot {
				malformed := valid
				malformed.Applications = append([]platform.Application(nil), valid.Applications...)
				malformed.Applications[0].RBAC = authorize.RBACTable{}
				return malformed
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &platformSource{snapshot: testCase.snapshot}
			authority := newPlatformAuthority(t, source)
			response, err := authority.Check(context.Background(), platformRequest(provider))
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if response.Decision.Allowed {
				t.Fatal("a malformed hosted-authority snapshot was allowed")
			}
			// Recovery: the same authority allows once the snapshot is valid.
			source.setSnapshot(valid)
			response, err = authority.Check(context.Background(), platformRequest(provider))
			if err != nil {
				t.Fatalf("recovery after a malformed snapshot: %v", err)
			}
			if !response.Decision.Allowed {
				t.Fatalf("recovered decision = %+v; want allowed", response.Decision)
			}
		})
	}
}

func TestHostedAuthorityRefusesMissingOrMismatchedClaims(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	cases := []struct {
		name   string
		mutate func(*platform.Snapshot)
	}{
		{"missing tenant claim", func(snapshot *platform.Snapshot) { delete(snapshot.ClaimValues, provider.TenantClaim()) }},
		{"wrong tenant", func(snapshot *platform.Snapshot) {
			snapshot.ClaimValues[provider.TenantClaim()] = []string{"tenant-b"}
		}},
		{"ambiguous tenant", func(snapshot *platform.Snapshot) {
			snapshot.ClaimValues[provider.TenantClaim()] = []string{fixtureTenant, fixtureTenant}
		}},
		{"missing role", func(snapshot *platform.Snapshot) { delete(snapshot.ClaimValues, provider.RoleClaim()) }},
		{"role not granted", func(snapshot *platform.Snapshot) {
			snapshot.ClaimValues[provider.RoleClaim()] = []string{"viewer"}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := platformSnapshot(provider)
			testCase.mutate(&snapshot)
			source := &platformSource{snapshot: snapshot}
			authority := newPlatformAuthority(t, source)
			response, err := authority.Check(context.Background(), platformRequest(provider))
			if err != nil {
				t.Fatalf("policy refusal returned an error: %v", err)
			}
			if response.Decision.Allowed {
				t.Fatalf("decision = %+v; want a refusal", response.Decision)
			}
		})
	}

	// Positive control: the unmutated snapshot is allowed, so each refusal
	// above is attributable to the mutated claim value.
	source := &platformSource{snapshot: platformSnapshot(provider)}
	authority := newPlatformAuthority(t, source)
	response, err := authority.Check(context.Background(), platformRequest(provider))
	if err != nil {
		t.Fatalf("baseline Check: %v", err)
	}
	if !response.Decision.Allowed {
		t.Fatalf("baseline decision = %+v; want allowed", response.Decision)
	}
}
