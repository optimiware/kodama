// Package oidc provides the OIDC fixtures of the identity tests.
//
// Two fixtures exist, and they are not interchangeable:
//
//   - Fixture drives the real, pinned oauthsonas developer issuer through its
//     discovery document and the authorization-code flow with S256 PKCE, and
//     returns genuinely issued access and ID tokens. Acceptance evidence uses it.
//   - Stub is an isolated in-process issuer that signs arbitrary claims. It
//     exists to produce malformed, expired, mis-signed and rotated tokens that a
//     real issuer will not mint, and to simulate issuer outages. It never
//     substitutes for the real-issuer acceptance tests.
package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// StubAudience is the API audience the stub issues tokens for by default.
const StubAudience = "https://kodama.example.test/api"

// Stub is an isolated issuer serving discovery and a JWKS.
type Stub struct {
	URL      string
	Audience string

	srv *httptest.Server
	mu  sync.Mutex

	current    *ecdsa.PrivateKey
	currentKID string
	published  []jose.JSONWebKey

	failJWKS      int // when non-zero, the JWKS endpoint answers this HTTP status
	failDiscovery int
	jwksHits      int
	discoveryHits int
}

// NewStub starts the issuer; it is closed when the test ends.
func NewStub(t testing.TB) *Stub {
	t.Helper()
	s := &Stub{Audience: StubAudience}
	s.newKey(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.discoveryHits++
		fail := s.failDiscovery
		s.mu.Unlock()
		if fail != 0 {
			http.Error(w, "boom", fail)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": s.URL, "jwks_uri": s.URL + "/jwks", "authorization_endpoint": s.URL + "/auth", "token_endpoint": s.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.jwksHits++
		fail := s.failJWKS
		keys := append([]jose.JSONWebKey(nil), s.published...)
		s.mu.Unlock()
		if fail != 0 {
			http.Error(w, "secret internal detail", fail)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: keys})
	})
	s.srv = httptest.NewServer(mux)
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Stub) newKey(t testing.TB) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	kid := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.current, s.currentKID = k, kid
	s.published = []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: kid, Algorithm: "ES256", Use: "sig"}}
	s.mu.Unlock()
}

// Rotate replaces the signing key; the old key disappears from the JWKS.
func (s *Stub) Rotate(t testing.TB) { t.Helper(); s.newKey(t) }

// FailJWKS makes the key endpoint answer with the HTTP status (0 restores it).
func (s *Stub) FailJWKS(status int) { s.mu.Lock(); s.failJWKS = status; s.mu.Unlock() }

// FailDiscovery makes the discovery endpoint answer with the HTTP status.
func (s *Stub) FailDiscovery(status int) { s.mu.Lock(); s.failDiscovery = status; s.mu.Unlock() }

// JWKSHits and DiscoveryHits count requests served.
func (s *Stub) JWKSHits() int      { s.mu.Lock(); defer s.mu.Unlock(); return s.jwksHits }
func (s *Stub) DiscoveryHits() int { s.mu.Lock(); defer s.mu.Unlock(); return s.discoveryHits }

// Claims returns valid access-token claims for subject.
func (s *Stub) Claims(subject string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": s.URL, "aud": []string{s.Audience}, "sub": subject,
		"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "jti": "jti-" + subject,
	}
}

// Token signs the claims with the current key (ES256).
func (s *Stub) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	s.mu.Lock()
	key, kid := s.current, s.currentKID
	s.mu.Unlock()
	return sign(t, jose.ES256, key, kid, claims)
}

// TokenSignedBy signs with a freshly generated, unpublished key that claims the
// current key ID: a forged signature.
func (s *Stub) TokenSignedBy(t testing.TB, claims map[string]any) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	kid := s.currentKID
	s.mu.Unlock()
	return sign(t, jose.ES256, k, kid, claims)
}

// HMACToken signs with HS256 using the given secret (algorithm-confusion probe).
func HMACToken(t testing.TB, secret []byte, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	return serialize(t, signer, claims)
}

// UnsignedToken builds an alg=none token.
func UnsignedToken(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	p, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(p) + "."
}

func sign(t testing.TB, alg jose.SignatureAlgorithm, key crypto.Signer, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	return serialize(t, signer, claims)
}

func serialize(t testing.TB, signer jose.Signer, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TokenSignedByUnknownKey signs with a fresh key under a key ID the issuer never
// published.
func (s *Stub) TokenSignedByUnknownKey(t testing.TB, claims map[string]any) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return sign(t, jose.ES256, k, "unknown-"+time.Now().Format("150405.000000000"), claims)
}
