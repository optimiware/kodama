// OIDC adapter API pinned by these tests.
//
// The declared identity seam is identity.OIDCVerifier / identity.OIDCConfig /
// identity.OIDCRequest. The concrete adapter adds transport, key and claim
// configuration:
//
//	type Config struct {
//	    identity.OIDCConfig // Issuer, Audience, ClockSkew
//	    JWKSURL        string
//	    Algorithms     []string
//	    HTTPClient     *http.Client
//	    MaxJWKSBytes   int64
//	    MaxTokenBytes  int
//	    CacheTTL       time.Duration
//	    Clock          func() time.Time
//	    SubjectClaim   string
//	    TenantClaim    string
//	    ProjectClaim   string
//	    Logf           func(format string, args ...any)
//	}
//	func (Config) Validate() error
//	func NewVerifier(Config) (*Verifier, error)
//	func (*Verifier) Verify(context.Context, identity.OIDCRequest) (identity.Principal, error)
//	func PrincipalIDFor(issuer, subject string) (id.PrincipalID, error)
//
// Semantics pinned by the tests:
//   - Only access tokens are verified. A successful verification returns a
//     structurally valid human Principal whose id is derived from the issuer
//     and subject; token principal-id or role claims are never trusted.
//   - exp is valid only while request.Now is strictly before exp+ClockSkew.
//     nbf is valid when request.Now+ClockSkew is at or after nbf. iat is valid
//     when it is not later than request.Now+ClockSkew. exp is required.
//   - The token algorithm must be in Algorithms; "none" and every HS* value
//     are refused both as configuration and as a token header.
//   - JWKS is fetched through HTTPClient. A cached key set is reused while
//     Clock() is before fetchedAt+CacheTTL. An unknown kid triggers at most one
//     refetch per Verify; the refetch may replace the cache. Every provider
//     failure (transport, status, malformed/oversized body, timeout,
//     cancellation) is dependency_unavailable and never a successful result.
//   - Logf is optional. No token value, provider body or private cause is ever
//     rendered by an error or a log line.
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
)

const (
	testIssuer       = "https://issuer.example"
	testAudience     = "kodama-api"
	testJWKSURL      = "https://issuer.example/.well-known/jwks.json"
	testTenantClaim  = "https://kodama/tenant"
	testProjectClaim = "https://kodama/project"
	testSubject      = "auth0|abc123"
)

// testVerifier is the declared identity.OIDCVerifier shape. The production
// *Verifier must satisfy it; see the compile-time assertion below.
type testVerifier interface {
	Verify(context.Context, identity.OIDCRequest) (identity.Principal, error)
}

var _ identity.OIDCVerifier = (*Verifier)(nil)

type testConfig struct {
	issuer             string
	audience           string
	skew               time.Duration
	jwksURL            string
	algorithms         []string
	httpClient         *http.Client
	maxJWKSBytes       int64
	maxTokenBytes      int
	cacheTTL           time.Duration
	minRefreshInterval time.Duration
	clock              func() time.Time
	subjectClaim       string
	tenantClaim        string
	projectClaim       string
	logf               func(format string, args ...any)
}

func defaultTestConfig() testConfig {
	return testConfig{
		issuer:        testIssuer,
		audience:      testAudience,
		jwksURL:       testJWKSURL,
		algorithms:    []string{"RS256", "ES256"},
		httpClient:    &http.Client{Transport: roundTripFunc(staticJSON(`{"keys":[]}`))},
		maxJWKSBytes:  1 << 20,
		maxTokenBytes: 1 << 20,
		cacheTTL:      time.Hour,
		subjectClaim:  "sub",
		tenantClaim:   testTenantClaim,
		projectClaim:  testProjectClaim,
	}
}

// buildConfig is the only place that names the production Config type.
func buildConfig(config testConfig) Config {
	return Config{
		OIDCConfig: identity.OIDCConfig{
			Issuer:    config.issuer,
			Audience:  config.audience,
			ClockSkew: config.skew,
		},
		JWKSURL:            config.jwksURL,
		Algorithms:         config.algorithms,
		HTTPClient:         config.httpClient,
		MaxJWKSBytes:       config.maxJWKSBytes,
		MaxTokenBytes:      config.maxTokenBytes,
		CacheTTL:           config.cacheTTL,
		MinRefreshInterval: config.minRefreshInterval,
		Clock:              config.clock,
		SubjectClaim:       config.subjectClaim,
		TenantClaim:        config.tenantClaim,
		ProjectClaim:       config.projectClaim,
		Logf:               config.logf,
	}
}

func buildVerifier(config testConfig) (testVerifier, error) {
	return NewVerifier(buildConfig(config))
}

func validateConfig(config testConfig) error {
	return buildConfig(config).Validate()
}

func newVerifier(t *testing.T, config testConfig) testVerifier {
	t.Helper()
	verifier, err := buildVerifier(config)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if verifier == nil {
		t.Fatal("NewVerifier returned a nil verifier")
	}
	return verifier
}

func newVerifierWithJWKS(t *testing.T, config testConfig, keys ...map[string]any) (testVerifier, *fakeJWKS) {
	t.Helper()
	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, keys...))}
	config.httpClient = &http.Client{Transport: transport}
	return newVerifier(t, config), transport
}

func derivedPrincipalID(issuer, subject string) (id.PrincipalID, error) {
	return PrincipalIDFor(issuer, subject)
}

func expectedPrincipalID(issuer, subject string) id.PrincipalID {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return id.PrincipalID("oidc-" + hex.EncodeToString(sum[:16]))
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}

func verifyRequest(verifier testVerifier, token string, now time.Time) (identity.Principal, error) {
	return verifier.Verify(context.Background(), identity.OIDCRequest{AccessToken: token, Now: now})
}

func verifyRequestContext(ctx context.Context, verifier testVerifier, token string, now time.Time) (identity.Principal, error) {
	return verifier.Verify(ctx, identity.OIDCRequest{AccessToken: token, Now: now})
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type fakeJWKS struct {
	mu      sync.Mutex
	calls   int
	handler roundTripFunc
}

func (fake *fakeJWKS) RoundTrip(request *http.Request) (*http.Response, error) {
	fake.mu.Lock()
	fake.calls++
	handler := fake.handler
	fake.mu.Unlock()
	return handler(request)
}

func (fake *fakeJWKS) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeJWKS) setHandler(handler roundTripFunc) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.handler = handler
}

func staticJSON(body string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return jsonResponse(body, http.StatusOK), nil
	}
}

func jsonResponse(body string, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type fakeClock struct {
	mu      sync.Mutex
	current time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{current: now}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.current
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(duration)
}

var (
	rsaKeysOnce  sync.Once
	rsaPrimary   *rsa.PrivateKey
	rsaSecondary *rsa.PrivateKey
	rsaKeysErr   error
)

func primaryRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	rsaKeysOnce.Do(func() {
		rsaPrimary, rsaKeysErr = rsa.GenerateKey(rand.Reader, 2048)
		if rsaKeysErr == nil {
			rsaSecondary, rsaKeysErr = rsa.GenerateKey(rand.Reader, 2048)
		}
	})
	if rsaKeysErr != nil {
		t.Fatalf("generating RSA keys: %v", rsaKeysErr)
	}
	return rsaPrimary
}

func secondaryRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	primaryRSAKey(t)
	return rsaSecondary
}

func mustECKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	return key
}

func rsaJWK(kid string, key *rsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

func ecJWK(kid string, key *ecdsa.PrivateKey) map[string]any {
	return map[string]any{
		"kty": "EC",
		"kid": kid,
		"use": "sig",
		"alg": "ES256",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		"y":   base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

func jwksJSON(t *testing.T, keys ...map[string]any) string {
	t.Helper()
	if keys == nil {
		keys = []map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatalf("marshalling JWKS: %v", err)
	}
	return string(payload)
}

type jwtSigner func(signingInput []byte) ([]byte, error)

func rsaSHA256Signer(key *rsa.PrivateKey) jwtSigner {
	return func(signingInput []byte) ([]byte, error) {
		digest := sha256.Sum256(signingInput)
		return rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	}
}

func ecSHA256Signer(key *ecdsa.PrivateKey) jwtSigner {
	return func(signingInput []byte) ([]byte, error) {
		digest := sha256.Sum256(signingInput)
		return ecdsa.SignASN1(rand.Reader, key, digest[:])
	}
}

func hmacSHA256Signer(secret []byte) jwtSigner {
	return func(signingInput []byte) ([]byte, error) {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(signingInput)
		return mac.Sum(nil), nil
	}
}

func noSigner() jwtSigner {
	return func([]byte) ([]byte, error) { return nil, nil }
}

func makeJWT(t *testing.T, alg, kid string, claims map[string]any, sign jwtSigner) string {
	t.Helper()
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshalling JWT header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling JWT claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)
	signature, err := sign([]byte(signingInput))
	if err != nil {
		t.Fatalf("signing JWT: %v", err)
	}
	if signature == nil {
		return signingInput + "."
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testClaimsMap(now time.Time) map[string]any {
	return map[string]any{
		"iss":            testIssuer,
		"aud":            testAudience,
		"sub":            testSubject,
		"exp":            now.Add(time.Hour).Unix(),
		"nbf":            now.Add(-time.Minute).Unix(),
		"iat":            now.Add(-time.Minute).Unix(),
		testTenantClaim:  "tenant-a",
		testProjectClaim: "project-1",
	}
}

func publicKeyDER(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshalling RSA public key: %v", err)
	}
	return encoded
}
