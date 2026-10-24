package identity_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"example.invalid/inventory/internal/identity"
	"example.invalid/inventory/test/support/oidcfake"
)

const aud = "https://inventory.example.invalid/api"

func TestOIDCVerifier(t *testing.T) {
	p := oidcfake.New(t)
	v, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: p.Issuer(), Audience: aud})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := v.Verify(ctx, p.Token("alice", aud))
	if err != nil || got.Subject != "alice" {
		t.Fatalf("valid token: %v %+v", got, err)
	}
	// Role and tenant claims are inputs nobody here trusts: the principal is just a subject.
	got, err = v.Verify(ctx, p.Sign(oidcfake.Claims{Subject: "mallory", Audience: aud, Extra: map[string]any{"tenant_id": "11111111-1111-4111-8111-111111111111", "roles": []string{"admin"}}}))
	if err != nil || got.Subject != "mallory" {
		t.Fatalf("extra claims: %v %+v", got, err)
	}
	bad := map[string]string{
		"wrong audience": p.Token("alice", "https://other.example.invalid"),
		"wrong issuer":   p.Sign(oidcfake.Claims{Issuer: "https://evil.example.invalid", Subject: "alice", Audience: aud}),
		"expired":        p.Sign(oidcfake.Claims{Subject: "alice", Audience: aud, Expiry: time.Now().Add(-time.Hour)}),
		"wrong key":      p.Sign(oidcfake.Claims{Subject: "alice", Audience: aud, WrongKey: true}),
		"no subject":     p.Sign(oidcfake.Claims{NoSubject: true, Audience: aud}),
		"garbage":        "not-a-jwt",
		"empty":          "",
	}
	for name, tok := range bad {
		if _, err := v.Verify(ctx, tok); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Errorf("%s: want ErrUnauthenticated, got %v", name, err)
		}
	}
}

func TestOIDCVerifierOutageFailsClosedAndRecovers(t *testing.T) {
	p := oidcfake.New(t)
	p.SetDown(true)
	v, _ := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: p.Issuer(), Audience: aud})
	if _, err := v.Verify(context.Background(), p.Token("alice", aud)); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("an unreachable provider is unavailable, never authenticated: %v", err)
	}
	if err := v.Ready(context.Background()); err == nil {
		t.Fatal("readiness must report the outage")
	}
	p.SetDown(false)
	if _, err := v.Verify(context.Background(), p.Token("alice", aud)); err != nil {
		t.Fatalf("the verifier must recover: %v", err)
	}
	if err := v.Ready(context.Background()); err != nil {
		t.Fatalf("readiness after recovery: %v", err)
	}
	p.SetDown(true)
	if err := v.Ready(context.Background()); !errors.Is(err, identity.ErrUnavailable) {
		t.Errorf("readiness must detect an outage after successful verification: %v", err)
	}
	if got, err := v.Verify(context.Background(), p.Token("alice", aud)); err != nil || got.Subject != "alice" {
		t.Errorf("readiness must preserve cached token verification: %+v %v", got, err)
	}
	p.SetDown(false)
	if err := v.Ready(context.Background()); err != nil {
		t.Fatalf("the same verifier must become ready again: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOIDCVerifierReadiness(t *testing.T) {
	const issuer = "https://idp.example.invalid/tenant"
	t.Run("fresh discovery uses configured client and exact issuer", func(t *testing.T) {
		document := `{"issuer":"` + issuer + `","jwks_uri":"` + issuer + `/jwks"}`
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.String() != issuer+"/.well-known/openid-configuration" {
				t.Errorf("discovery URL: %s", r.URL)
			}
			if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 10*time.Second {
				t.Error("readiness must bound discovery even when the client has no timeout")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(document))}, nil
		})}
		v, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: issuer, Audience: aud, HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Even a trailing slash is a different issuer, not a healthy provider.
		document = `{"issuer":"` + issuer + `/","jwks_uri":"` + issuer + `/jwks"}`
		if err := v.Ready(context.Background()); !errors.Is(err, identity.ErrUnavailable) {
			t.Errorf("mismatched discovery issuer: %v", err)
		}
		if calls != 2 {
			t.Errorf("want fresh discovery on each call, got %d requests", calls)
		}
	})

	for _, mode := range []string{"cancellation", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			p := oidcfake.New(t)
			client := &http.Client{}
			v, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: p.Issuer(), Audience: aud, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if err := v.Ready(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if mode == "cancellation" {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			start := time.Now()
			if err := v.Ready(ctx); !errors.Is(err, identity.ErrUnavailable) {
				t.Errorf("readiness must report interrupted discovery: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Error("readiness ignored caller cancellation/deadline")
			}
		})
	}
}

func TestOIDCConfigValidation(t *testing.T) {
	for name, cfg := range map[string]identity.OIDCConfig{
		"empty issuer":      {Audience: aud},
		"no audience":       {Issuer: "https://idp.example.invalid"},
		"insecure remote":   {Issuer: "http://idp.example.invalid", Audience: aud},
		"credentials":       {Issuer: "https://u:p@idp.example.invalid", Audience: aud},
		"ftp":               {Issuer: "ftp://idp.example.invalid", Audience: aud},
		"issuer with query": {Issuer: "https://idp.example.invalid/?a=b", Audience: aud},
	} {
		if _, err := identity.NewOIDCVerifier(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := identity.NewOIDCVerifier(identity.OIDCConfig{Issuer: "http://127.0.0.1:9999", Audience: aud}); err != nil {
		t.Errorf("loopback http is allowed: %v", err)
	}
}
