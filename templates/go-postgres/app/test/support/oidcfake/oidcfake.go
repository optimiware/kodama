// Package oidcfake is a minimal in-process OpenID Provider for tests: discovery,
// JWKS and signed access tokens. It issues whatever claims a test asks for,
// including role and tenant claims, to prove the application ignores them.
package oidcfake

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Provider is a running fake issuer.
type Provider struct {
	Server *httptest.Server
	key    *rsa.PrivateKey
	other  *rsa.PrivateKey
	down   atomic.Bool
}

// New starts a provider closed when the test ends.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{key: key, other: other}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if p.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.Server.URL, "jwks_uri": p.Server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Issuer is the issuer URL.
func (p *Provider) Issuer() string { return p.Server.URL }

// SetDown makes discovery fail, simulating a provider outage.
func (p *Provider) SetDown(down bool) { p.down.Store(down) }

// Claims customizes a token.
type Claims struct {
	Issuer    string
	Subject   string
	Audience  string
	Expiry    time.Time
	Extra     map[string]any
	WrongKey  bool
	NoSubject bool
}

// Token signs an access token for subject and audience, valid for an hour.
func (p *Provider) Token(subject, audience string) string {
	return p.Sign(Claims{Subject: subject, Audience: audience})
}

// Sign signs custom claims; zero fields default to a valid token.
func (p *Provider) Sign(c Claims) string {
	if c.Issuer == "" {
		c.Issuer = p.Server.URL
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(time.Hour)
	}
	key := p.key
	if c.WrongKey {
		key = p.other
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		panic(err)
	}
	std := jwt.Claims{Issuer: c.Issuer, Subject: c.Subject, Audience: jwt.Audience{c.Audience}, Expiry: jwt.NewNumericDate(c.Expiry), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}
	if c.NoSubject {
		std.Subject = ""
	}
	b := jwt.Signed(signer).Claims(std)
	if len(c.Extra) > 0 {
		b = b.Claims(c.Extra)
	}
	raw, err := b.Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}
