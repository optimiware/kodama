// Login vectors: deterministic reference fakes.
//
// These are compile-time consumers of the login declaration surface. The
// fakes are vector fixtures, never product code: they perform no I/O beyond
// in-memory bookkeeping.
package publiccontract

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// loginFakeRandom is a deterministic cli.Random. It records every requested
// read so the vectors can pin the entropy bound of the generators.
type loginFakeRandom struct {
	mu       sync.Mutex
	counter  byte
	requests []int
	err      error
}

func (random *loginFakeRandom) Read(p []byte) (int, error) {
	random.mu.Lock()
	defer random.mu.Unlock()
	random.requests = append(random.requests, len(p))
	if random.err != nil {
		return 0, random.err
	}
	for index := range p {
		random.counter++
		p[index] = random.counter
	}
	return len(p), nil
}

func (random *loginFakeRandom) requested() []int {
	random.mu.Lock()
	defer random.mu.Unlock()
	return append([]int(nil), random.requests...)
}

// loginFakeClock is an injected cancellable clock. Sleep advances the fake now
// so an entire device-polling schedule runs without wall-clock delays.
type loginFakeClock struct {
	mu         sync.Mutex
	now        time.Time
	sleeps     []time.Duration
	sleepLimit int
	sleepErr   error
}

func newLoginFakeClock(now time.Time) *loginFakeClock {
	return &loginFakeClock{now: now}
}

func (clock *loginFakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *loginFakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.sleeps = append(clock.sleeps, duration)
	clock.now = clock.now.Add(duration)
	if clock.sleepLimit > 0 && len(clock.sleeps) > clock.sleepLimit {
		return errors.New("login fake clock: sleep limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return clock.sleepErr
}

func (clock *loginFakeClock) slept() []time.Duration {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return append([]time.Duration(nil), clock.sleeps...)
}

// loginFakeBrowser records the authorization URL that the flow opens and never
// launches a real browser.
type loginFakeBrowser struct {
	mu   sync.Mutex
	urls []string
	err  error
}

func (browser *loginFakeBrowser) Open(ctx context.Context, rawURL string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	browser.urls = append(browser.urls, rawURL)
	return browser.err
}

func (browser *loginFakeBrowser) opened() []string {
	browser.mu.Lock()
	defer browser.mu.Unlock()
	return append([]string(nil), browser.urls...)
}

func (browser *loginFakeBrowser) lastURL(t *testing.T) *url.URL {
	t.Helper()
	opened := browser.opened()
	if len(opened) == 0 {
		t.Fatal("login flow never opened the system browser")
	}
	parsed, err := url.Parse(opened[len(opened)-1])
	if err != nil {
		t.Fatalf("authorization URL is invalid: %v", err)
	}
	return parsed
}

// loginFakeLoopback is a deterministic cli.LoopbackListener. The callback
// function is invoked by whatever wait function the flow requests.
type loginFakeLoopback struct {
	mu        sync.Mutex
	address   string
	callback  func(context.Context) (cli.CallbackParams, error)
	closeErr  error
	listens   int
	waits     int
	closes    int
	waitError error
}

func (loopback *loginFakeLoopback) Listen(ctx context.Context) (string, func(context.Context) (cli.CallbackParams, error), func() error, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, nil, err
	}
	loopback.mu.Lock()
	loopback.listens++
	address := loopback.address
	callback := loopback.callback
	loopback.mu.Unlock()
	return address, func(waitCtx context.Context) (cli.CallbackParams, error) {
			loopback.mu.Lock()
			loopback.waits++
			waitError := loopback.waitError
			loopback.mu.Unlock()
			if waitError != nil {
				return cli.CallbackParams{}, waitError
			}
			if callback == nil {
				return cli.CallbackParams{}, errors.New("login fake loopback: no callback configured")
			}
			return callback(waitCtx)
		}, func() error {
			loopback.mu.Lock()
			loopback.closes++
			closeErr := loopback.closeErr
			loopback.mu.Unlock()
			return closeErr
		}, nil
}

func (loopback *loginFakeLoopback) counts() (listens, waits, closes int) {
	loopback.mu.Lock()
	defer loopback.mu.Unlock()
	return loopback.listens, loopback.waits, loopback.closes
}

// loginFakeOIDC is the reference fake OIDC server. Every method records the
// request and delegates to an optional function so a vector can choose the
// response schedule without any real HTTP traffic.
type loginFakeOIDC struct {
	mu           sync.Mutex
	discovery    api.DiscoveryDocument
	discoveryErr error
	tokenFn      func(context.Context, cli.TokenRequest) (cli.TokenResponse, error)
	deviceFn     func(context.Context, cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error)
	revokeFn     func(context.Context, cli.RevocationRequest) error
	jwks         json.RawMessage
	jwksErr      error

	discoveryCalls []string
	tokenCalls     []cli.TokenRequest
	deviceCalls    []cli.DeviceAuthorizationRequest
	revokeCalls    []cli.RevocationRequest
	jwksCalls      []string
}

func (server *loginFakeOIDC) Discover(ctx context.Context, discoveryURL string) (api.DiscoveryDocument, error) {
	if err := ctx.Err(); err != nil {
		return api.DiscoveryDocument{}, err
	}
	server.mu.Lock()
	server.discoveryCalls = append(server.discoveryCalls, discoveryURL)
	document, discoveryErr := server.discovery, server.discoveryErr
	server.mu.Unlock()
	if discoveryErr != nil {
		return api.DiscoveryDocument{}, discoveryErr
	}
	return document, nil
}

func (server *loginFakeOIDC) Token(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
	if err := ctx.Err(); err != nil {
		return cli.TokenResponse{}, err
	}
	server.mu.Lock()
	server.tokenCalls = append(server.tokenCalls, request)
	tokenFn := server.tokenFn
	server.mu.Unlock()
	if tokenFn == nil {
		return cli.TokenResponse{}, errors.New("login fake OIDC: no token handler")
	}
	return tokenFn(ctx, request)
}

func (server *loginFakeOIDC) DeviceAuthorize(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
	if err := ctx.Err(); err != nil {
		return cli.DeviceAuthorizationResponse{}, err
	}
	server.mu.Lock()
	server.deviceCalls = append(server.deviceCalls, request)
	deviceFn := server.deviceFn
	server.mu.Unlock()
	if deviceFn == nil {
		return cli.DeviceAuthorizationResponse{}, errors.New("login fake OIDC: no device handler")
	}
	return deviceFn(ctx, request)
}

func (server *loginFakeOIDC) Revoke(ctx context.Context, request cli.RevocationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	server.mu.Lock()
	server.revokeCalls = append(server.revokeCalls, request)
	revokeFn := server.revokeFn
	server.mu.Unlock()
	if revokeFn == nil {
		return nil
	}
	return revokeFn(ctx, request)
}

func (server *loginFakeOIDC) JWKS(ctx context.Context, issuer string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	server.mu.Lock()
	server.jwksCalls = append(server.jwksCalls, issuer)
	keys, jwksErr := server.jwks, server.jwksErr
	server.mu.Unlock()
	if jwksErr != nil {
		return nil, jwksErr
	}
	return append(json.RawMessage(nil), keys...), nil
}

func (server *loginFakeOIDC) issuedTokens() []cli.TokenRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cli.TokenRequest(nil), server.tokenCalls...)
}

func (server *loginFakeOIDC) revokedTokens() []cli.RevocationRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cli.RevocationRequest(nil), server.revokeCalls...)
}

func (server *loginFakeOIDC) jwksFetched() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.jwksCalls...)
}

func (server *loginFakeOIDC) discovered() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.discoveryCalls...)
}

// loginFakeStore is an in-memory cli.SessionRecordStore.
type loginFakeStore struct {
	mu        sync.Mutex
	record    cli.SessionRecord
	present   bool
	loadErr   error
	saveErr   error
	deleteErr error
	saves     []cli.SessionRecord
	deletes   int
}

func (store *loginFakeStore) LoadRecord(ctx context.Context) (cli.SessionRecord, error) {
	if err := ctx.Err(); err != nil {
		return cli.SessionRecord{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.loadErr != nil {
		return cli.SessionRecord{}, store.loadErr
	}
	if !store.present {
		return cli.SessionRecord{}, fs.ErrNotExist
	}
	return store.record, nil
}

func (store *loginFakeStore) SaveRecord(ctx context.Context, record cli.SessionRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saveErr != nil {
		return store.saveErr
	}
	store.record = record
	store.present = true
	store.saves = append(store.saves, record)
	return nil
}

func (store *loginFakeStore) DeleteRecord(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.deleteErr != nil {
		return store.deleteErr
	}
	store.present = false
	store.deletes++
	return nil
}

func (store *loginFakeStore) current() (cli.SessionRecord, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.record, store.present
}

func (store *loginFakeStore) saved() []cli.SessionRecord {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]cli.SessionRecord(nil), store.saves...)
}

func (store *loginFakeStore) deleteCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.deletes
}

// loginFakeLock is a real cross-goroutine lock with a recorded acquisition
// count. It serializes the reload → refresh → save section so a vector can pin
// that only one rotation happens.
type loginFakeLock struct {
	serial       sync.Mutex
	state        sync.Mutex
	acquisitions int
	acquireErr   error
	releases     int
}

func (lock *loginFakeLock) Acquire(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock.serial.Lock()
	lock.state.Lock()
	lock.acquisitions++
	acquireErr := lock.acquireErr
	lock.state.Unlock()
	if acquireErr != nil {
		lock.serial.Unlock()
		return nil, acquireErr
	}
	var once sync.Once
	return func() error {
		once.Do(func() {
			lock.state.Lock()
			lock.releases++
			lock.state.Unlock()
			lock.serial.Unlock()
		})
		return nil
	}, nil
}

func (lock *loginFakeLock) counts() (acquisitions, releases int) {
	lock.state.Lock()
	defer lock.state.Unlock()
	return lock.acquisitions, lock.releases
}

// loginDiscovery is the canonical valid discovery document used by the vectors.
func loginDiscovery() api.DiscoveryDocument {
	return api.DiscoveryDocument{
		Issuer:                      "https://auth.example.com",
		ClientID:                    "kodactl",
		Audience:                    "kodama-api",
		AuthorizationEndpoint:       "https://auth.example.com/authorize",
		TokenEndpoint:               "https://auth.example.com/token",
		DeviceAuthorizationEndpoint: "https://auth.example.com/device",
		RevocationEndpoint:          "https://auth.example.com/revoke",
		Scopes:                      []string{"openid", "profile", "email", "offline_access"},
		APIURL:                      "https://api.example.com",
	}
}

// loginRecord returns one valid v2 session bound to loginDiscovery.
func loginRecord(now time.Time) cli.SessionRecord {
	discovery := loginDiscovery()
	return cli.SessionRecord{
		Version:            cli.SessionFileVersion,
		Kind:               "session",
		Token:              "AT-CANARY",
		ExpiresAt:          now.Add(30 * time.Second),
		RefreshToken:       "RT-CANARY",
		Issuer:             discovery.Issuer,
		ClientID:           discovery.ClientID,
		Audience:           discovery.Audience,
		APIURL:             discovery.APIURL,
		TokenEndpoint:      discovery.TokenEndpoint,
		RevocationEndpoint: discovery.RevocationEndpoint,
	}
}

// loginTestKey lazily generates one RSA key for the ID-token vectors.
var (
	loginKeyOnce sync.Once
	loginKey     *rsa.PrivateKey
	loginKeyErr  error
)

func loginGenerateKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test RSA key: %v", err)
	}
	return key
}

func loginTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	loginKeyOnce.Do(func() {
		loginKey, loginKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if loginKeyErr != nil {
		t.Fatalf("generate test RSA key: %v", loginKeyErr)
	}
	return loginKey
}

func loginBase64(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// loginSignIDToken signs one RS256 ID token with the given claims.
func loginSignIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "login-test-key"})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	signingInput := loginBase64(header) + "." + loginBase64(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + loginBase64(signature)
}

// loginJWKS renders the public half of key as a JWKS document.
func loginJWKS(t *testing.T, key *rsa.PrivateKey) json.RawMessage {
	t.Helper()
	keys := map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": "login-test-key",
		"alg": "RS256",
		"use": "sig",
		"n":   loginBase64(key.N.Bytes()),
		"e":   loginBase64(big.NewInt(int64(key.E)).Bytes()),
	}}}
	encoded, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}
	return encoded
}
