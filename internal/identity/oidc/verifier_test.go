package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/oidc"
	oidctest "github.com/optimiware/kodama/test/support/oidc"
)

func newVerifier(t *testing.T, s *oidctest.Stub, mut func(*oidc.Config)) *oidc.Verifier {
	t.Helper()
	cfg := oidc.Config{Issuer: s.URL, Audience: s.Audience, AllowInsecureHTTP: true, MinKeyRefresh: time.Millisecond}
	if mut != nil {
		mut(&cfg)
	}
	v, err := oidc.NewVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantUnauthenticated(t *testing.T, name string, err error, token string) {
	t.Helper()
	if !errors.Is(err, identity.ErrUnauthenticated) || errors.Is(err, identity.ErrUnavailable) {
		t.Errorf("%s: want unauthenticated, got %v", name, err)
	}
	if err != nil && token != "" && strings.Contains(err.Error(), token[:20]) {
		t.Errorf("%s: error quotes the token: %v", name, err)
	}
}

func TestVerifierFailures(t *testing.T) {
	ctx := context.Background()
	s := oidctest.NewStub(t)
	v := newVerifier(t, s, nil)

	t.Run("a valid access token maps issuer and subject only", func(t *testing.T) {
		claims := s.Claims("user|1")
		claims["roles"] = []string{"admin"}
		claims["email"] = "admin@example.test"
		p, err := v.Verify(ctx, s.Token(t, claims))
		if err != nil {
			t.Fatal(err)
		}
		if p != (identity.Principal{Issuer: s.URL, Subject: "user|1"}) {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("expired, not yet valid and missing expiry", func(t *testing.T) {
		c := s.Claims("u")
		c["exp"] = time.Now().Add(-time.Minute).Unix()
		tok := s.Token(t, c)
		_, err := v.Verify(ctx, tok)
		wantUnauthenticated(t, "expired", err, tok)
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Errorf("expiry reason lost: %v", err)
		}
		c = s.Claims("u")
		c["nbf"] = time.Now().Add(time.Hour).Unix()
		tok = s.Token(t, c)
		_, err = v.Verify(ctx, tok)
		wantUnauthenticated(t, "nbf", err, tok)
		c = s.Claims("u")
		delete(c, "exp")
		tok = s.Token(t, c)
		_, err = v.Verify(ctx, tok)
		wantUnauthenticated(t, "no exp", err, tok)
	})

	t.Run("missing or malformed subject", func(t *testing.T) {
		for name, mut := range map[string]func(map[string]any){
			"absent":  func(c map[string]any) { delete(c, "sub") },
			"empty":   func(c map[string]any) { c["sub"] = "" },
			"control": func(c map[string]any) { c["sub"] = "a\nb" },
			"huge":    func(c map[string]any) { c["sub"] = strings.Repeat("s", 513) },
			"number":  func(c map[string]any) { c["sub"] = 42 },
		} {
			c := s.Claims("u")
			mut(c)
			tok := s.Token(t, c)
			_, err := v.Verify(ctx, tok)
			wantUnauthenticated(t, name, err, tok)
		}
	})

	t.Run("wrong issuer and wrong audience", func(t *testing.T) {
		c := s.Claims("u")
		c["iss"] = "https://evil.example.test"
		tok := s.Token(t, c)
		_, err := v.Verify(ctx, tok)
		wantUnauthenticated(t, "issuer", err, tok)

		for name, aud := range map[string]any{"other": []string{"https://other.example.test"}, "client id": []string{"kodama-cli"}, "empty": []string{}, "string": "kodama-cli"} {
			c = s.Claims("u")
			c["aud"] = aud
			tok = s.Token(t, c)
			_, err = v.Verify(ctx, tok)
			wantUnauthenticated(t, "audience "+name, err, tok)
		}
		c = s.Claims("u")
		c["aud"] = []string{"kodama-cli", s.Audience} // multi-audience tokens are fine when ours is listed
		if _, err := v.Verify(ctx, s.Token(t, c)); err != nil {
			t.Errorf("listed audience rejected: %v", err)
		}
	})

	t.Run("ID tokens are not API tokens", func(t *testing.T) {
		// Same issuer, but the client audience: rejected by the audience check.
		c := s.Claims("u")
		c["aud"] = []string{"kodama-cli"}
		c["nonce"] = "n-0S6_WzA2Mj"
		c["at_hash"] = "x"
		tok := s.Token(t, c)
		_, err := v.Verify(ctx, tok)
		wantUnauthenticated(t, "client-audience id token", err, tok)

		// Even if the audience were misconfigured to the API audience, the ID-token
		// claims reject it.
		c = s.Claims("u")
		c["nonce"] = "n-0S6_WzA2Mj"
		tok = s.Token(t, c)
		_, err = v.Verify(ctx, tok)
		wantUnauthenticated(t, "api-audience id token", err, tok)

		lax := newVerifier(t, s, func(c *oidc.Config) { c.AcceptIDTokenClaims = true })
		if _, err := lax.Verify(ctx, tok); err != nil {
			t.Errorf("explicit option did not apply: %v", err)
		}
		c["aud"] = []string{"kodama-cli"}
		if _, err := lax.Verify(ctx, s.Token(t, c)); err == nil {
			t.Error("the option must never relax the audience check")
		}
	})

	t.Run("signature tampering and unsupported algorithms", func(t *testing.T) {
		good := s.Token(t, s.Claims("u"))
		parts := strings.Split(good, ".")
		// Replace the payload with another subject but keep the signature.
		forgedPayload := s.Token(t, s.Claims("admin"))
		swapped := parts[0] + "." + strings.Split(forgedPayload, ".")[1] + "." + parts[2]
		_, err := v.Verify(ctx, swapped)
		wantUnauthenticated(t, "payload swap", err, swapped)

		flipped := good[:len(good)-4] + "AAAA"
		_, err = v.Verify(ctx, flipped)
		wantUnauthenticated(t, "signature flip", err, flipped)

		forged := s.TokenSignedBy(t, s.Claims("u"))
		_, err = v.Verify(ctx, forged)
		wantUnauthenticated(t, "wrong key", err, forged)

		none := oidctest.UnsignedToken(s.Claims("u"))
		_, err = v.Verify(ctx, none)
		wantUnauthenticated(t, "alg none", err, none)

		hs := oidctest.HMACToken(t, []byte("0123456789abcdef0123456789abcdef"), s.Claims("u"))
		_, err = v.Verify(ctx, hs)
		wantUnauthenticated(t, "HS256", err, hs)

		for name, tok := range map[string]string{"empty": "", "garbage": "not-a-jwt", "two parts": "a.b", "huge": strings.Repeat("a", oidc.DefaultMaxTokenBytes+1), "bearer prefix": "Bearer " + good} {
			_, err := v.Verify(ctx, tok)
			if !errors.Is(err, identity.ErrUnauthenticated) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("key rotation is picked up and the old key stops verifying", func(t *testing.T) {
		rs := oidctest.NewStub(t)
		rv := newVerifier(t, rs, nil)
		old := rs.Token(t, rs.Claims("u"))
		if _, err := rv.Verify(ctx, old); err != nil {
			t.Fatal(err)
		}
		rs.Rotate(t)
		time.Sleep(5 * time.Millisecond) // past the minimum refresh interval
		fresh := rs.Token(t, rs.Claims("u"))
		if _, err := rv.Verify(ctx, fresh); err != nil {
			t.Fatalf("token from the rotated key rejected: %v", err)
		}
		// The old key left the published set; once the cache refreshed it no longer verifies.
		_, err := rv.Verify(ctx, old)
		wantUnauthenticated(t, "retired key", err, old)
	})

	t.Run("an unavailable issuer fails closed and is not reported as a bad credential", func(t *testing.T) {
		us := oidctest.NewStub(t)
		us.FailDiscovery(http.StatusInternalServerError)
		uv := newVerifier(t, us, nil)
		tok := us.Token(t, us.Claims("u"))
		_, err := uv.Verify(ctx, tok)
		if !errors.Is(err, identity.ErrUnavailable) || errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("discovery outage: %v", err)
		}
		us.FailDiscovery(0)
		time.Sleep(5 * time.Millisecond)
		us.FailJWKS(http.StatusBadGateway)
		_, err = uv.Verify(ctx, tok)
		if !errors.Is(err, identity.ErrUnavailable) || errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("jwks outage: %v", err)
		}
		if strings.Contains(err.Error(), "secret internal detail") {
			t.Fatalf("provider response body leaked: %v", err)
		}
		// Recovery needs no restart (after the failure-repeat interval).
		us.FailJWKS(0)
		time.Sleep(5 * time.Millisecond)
		if _, err := uv.Verify(ctx, tok); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})

	t.Run("cached keys survive a short outage but not forever", func(t *testing.T) {
		cs := oidctest.NewStub(t)
		cv := newVerifier(t, cs, func(c *oidc.Config) { c.KeyCacheTTL = 20 * time.Millisecond; c.StaleKeyLimit = 150 * time.Millisecond })
		tok := cs.Token(t, cs.Claims("u"))
		if _, err := cv.Verify(ctx, tok); err != nil {
			t.Fatal(err)
		}
		cs.FailJWKS(http.StatusServiceUnavailable)
		time.Sleep(40 * time.Millisecond)
		if _, err := cv.Verify(ctx, tok); err != nil {
			t.Fatalf("stale cache within the limit: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if _, err := cv.Verify(ctx, tok); !errors.Is(err, identity.ErrUnavailable) {
			t.Fatalf("stale cache past the limit must fail closed: %v", err)
		}
	})

	t.Run("a cancelled context is an outage, not a bad credential", func(t *testing.T) {
		cs := oidctest.NewStub(t)
		cv := newVerifier(t, cs, nil)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := cv.Verify(cctx, cs.Token(t, cs.Claims("u")))
		if !errors.Is(err, identity.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("unknown key IDs do not stampede the issuer", func(t *testing.T) {
		ss := oidctest.NewStub(t)
		sv := newVerifier(t, ss, func(c *oidc.Config) { c.MinKeyRefresh = time.Hour })
		if _, err := sv.Verify(ctx, ss.Token(t, ss.Claims("u"))); err != nil {
			t.Fatal(err)
		}
		before := ss.JWKSHits()
		for i := 0; i < 20; i++ {
			_, _ = sv.Verify(ctx, ss.TokenSignedByUnknownKey(t, ss.Claims("u")))
		}
		if hits := ss.JWKSHits() - before; hits > 0 {
			t.Fatalf("%d JWKS refreshes inside the refresh interval", hits)
		}
	})
}

func TestVerifierConfiguration(t *testing.T) {
	bad := []oidc.Config{
		{Issuer: "https://issuer.example", Audience: ""},
		{Issuer: "", Audience: "a"},
		{Issuer: "http://issuer.example", Audience: "a"}, // http without the explicit development flag
		{Issuer: "https://user:pw@issuer.example", Audience: "a"},
		{Issuer: "https://issuer.example?x=1", Audience: "a"},
		{Issuer: "https://issuer.example", Audience: "a", SigningAlgs: []string{"HS256"}},
		{Issuer: "https://issuer.example", Audience: "a", SigningAlgs: []string{"none"}},
	}
	for _, cfg := range bad {
		if _, err := oidc.NewVerifier(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
	if _, err := oidc.NewVerifier(oidc.Config{Issuer: "https://issuer.example", Audience: "a"}); err != nil {
		t.Fatalf("valid production configuration: %v", err)
	}
	if _, err := oidc.NewVerifier(oidc.Config{Issuer: "http://127.0.0.1:1", Audience: "a", AllowInsecureHTTP: true}); err != nil {
		t.Fatalf("development configuration: %v", err)
	}
}

func TestVerifyAccessTokenReturnsExpiryAndScopes(t *testing.T) {
	s := oidctest.NewStub(t)
	v := newVerifier(t, s, nil)
	c := s.Claims("user|1")
	c["scope"] = "openid profile"
	got, err := v.VerifyAccessToken(context.Background(), s.Token(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if got.Principal.Subject != "user|1" || got.Expiry.IsZero() {
		t.Fatalf("token = %+v", got)
	}
	if len(got.Scopes) != 2 || got.Scopes[0] != "openid" || got.Scopes[1] != "profile" {
		t.Fatalf("scopes = %v", got.Scopes)
	}
}
