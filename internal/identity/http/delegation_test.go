package identityhttp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
)

type delegatedVerifier struct{}

func (delegatedVerifier) Verify(context.Context, string) (identity.Principal, error) {
	return identity.Principal{Issuer: "https://issuer.example", Subject: "user|1"}, nil
}

func (delegatedVerifier) VerifyAccessToken(context.Context, string) (identity.AccessToken, error) {
	return identity.AccessToken{
		Principal:  identity.Principal{Issuer: "https://issuer.example", Subject: "user|1"},
		Delegation: &identity.Delegation{Actor: "kodama-mcp", Scopes: []string{"kodama.api"}},
	}, nil
}

type fixedMapper struct{}

func (fixedMapper) MapPrincipal(context.Context, identity.Principal) (identity.UserID, error) {
	return identity.UserID("usr_delegated"), nil
}

type recordingAuthority struct{ calls int }

func (a *recordingAuthority) Authorize(context.Context, identity.Actor, identity.Target, identity.Permission) error {
	a.calls++
	return nil
}

func TestAuthenticatorCarriesDelegation(t *testing.T) {
	a, err := NewAuthenticator(delegatedVerifier{}, fixedMapper{})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer token")
	p, err := a.Authenticate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "usr_delegated" || p.DelegatedBy != "kodama-mcp" || !p.Delegated() {
		t.Fatalf("principal = %+v", p)
	}
	if p.DelegatedScopes != "kodama.api" {
		t.Fatalf("scopes = %q", p.DelegatedScopes)
	}
}

func TestDelegatedPrincipalCannotUseHumanOnlyRoute(t *testing.T) {
	auth := &recordingAuthority{}
	z, err := NewAuthorizer(auth, map[httpapi.Action]Rule{
		"gate.decide": {Level: LevelProject, Any: []identity.Permission{identity.PermGateDecide}},
	})
	if err != nil {
		t.Fatal(err)
	}
	target := httpapi.Target{TenantID: domain.TenantID("t_acme"), ProjectID: domain.ProjectID("p_one")}
	err = z.Authorize(context.Background(), httpapi.Principal{ID: "usr_delegated", DelegatedBy: "kodama-mcp"}, "gate.decide", target)
	if !errors.Is(err, httpapi.ErrDenied) {
		t.Fatalf("delegated gate decision = %v, want denied", err)
	}
	if auth.calls != 0 {
		t.Fatalf("authority was consulted %d times; human-only denial must fail closed first", auth.calls)
	}
	if err := z.Authorize(context.Background(), httpapi.Principal{ID: "usr_direct"}, "gate.decide", target); err != nil {
		t.Fatalf("direct human decision = %v", err)
	}
	if auth.calls != 1 {
		t.Fatalf("authority calls = %d, want 1", auth.calls)
	}
}
