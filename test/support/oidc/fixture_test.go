package oidc_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/identity"
	idoidc "github.com/optimiware/kodama/internal/identity/oidc"
	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

// TestFixtureIssuesIndependentTokens exercises the real oauthsonas flow and the
// stub issuer's own contract, so the helpers the acceptance tests rely on are
// themselves checked.
func TestFixtureIssuesIndependentTokens(t *testing.T) {
	f := oidctest.Start(t)
	v, err := idoidc.NewVerifier(idoidc.Config{Issuer: f.Issuer, Audience: f.Audience, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.Login("reviewer"), f.Login("reviewer")
	if a.Access == b.Access || a.Nonce == b.Nonce {
		t.Fatal("tokens of two logins are not independent")
	}
	p, err := v.Verify(context.Background(), a.Access)
	if err != nil || p != (identity.Principal{Issuer: f.Issuer, Subject: oidctest.Subject("reviewer")}) {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := v.Verify(context.Background(), a.ID); err == nil {
		t.Fatal("an ID token verified as an API token")
	}
	other := f.Login("viewer")
	if q, err := v.Verify(context.Background(), other.Access); err != nil || q.Subject == p.Subject {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestStubIssuer(t *testing.T) {
	s := oidctest.NewStub(t)
	v, err := idoidc.NewVerifier(idoidc.Config{Issuer: s.URL, Audience: s.Audience, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), s.Token(t, s.Claims("a"))); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), s.TokenSignedBy(t, s.Claims("a"))); err == nil {
		t.Fatal("forged token verified")
	}
}
