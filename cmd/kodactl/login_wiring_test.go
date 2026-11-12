// Tests for the real kodactl entrypoint wiring: the production login
// runtime reaches an httptest OIDC provider and a loopback discovery document,
// every command dispatches through cli.RunWithDeps, the renewing session
// credential source refreshes under the real cross-process lock, and
// `login` never reuses a seeded session.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

const (
	seedClientID   = "kodactl"
	seedAudience   = "kodama-api"
	loginAccess    = "login-access-token-canary"
	loginRefresh   = "login-refresh-token-canary"
	freshAccess    = "fresh-access-token-canary"
	freshRefresh   = "fresh-refresh-token-canary"
	deviceCode     = "device-code-canary"
	deviceUserCode = "USER-CODE-1"
)

// fixedClock is a deterministic cli.Clock. Sleep never waits because the
// device flow's scheduling is asserted by the provider call sequence.
type fixedClock struct {
	now time.Time
}

func (clock fixedClock) Now() time.Time { return clock.now }

func (clock fixedClock) Sleep(context.Context, time.Duration) error { return nil }

// mapEnvironment builds an injected DISPLAY/SSH/WAYLAND environment.
func mapEnvironment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// fakeBrowser implements cli.BrowserOpener by actually fetching the
// authorization URL and following the provider's redirect to the loopback
// callback.
type fakeBrowser struct {
	client *http.Client
	err    error
}

func (browser *fakeBrowser) Open(ctx context.Context, target string) error {
	if browser.err != nil {
		return browser.err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := browser.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode >= 400 {
		return fmt.Errorf("the browser received status %d", response.StatusCode)
	}
	return nil
}

// fakeProvider is a deterministic TLS OIDC provider plus discovery metadata.
type fakeProvider struct {
	t       *testing.T
	server  *httptest.Server
	key     *rsa.PrivateKey
	mu      sync.Mutex
	nonces  map[string]string
	auths   []url.Values
	tokens  []url.Values
	devices int
	revokes []url.Values
}

// newFakeProvider starts the TLS provider and seeds one RSA signing key.
func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	provider := &fakeProvider{t: t, key: key, nonces: make(map[string]string)}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", provider.serveMetadata)
	mux.HandleFunc("/jwks", provider.serveJWKS)
	mux.HandleFunc("/oauth/authorize", provider.serveAuthorize)
	mux.HandleFunc("/oauth/token", provider.serveToken)
	mux.HandleFunc("/oauth/device", provider.serveDevice)
	mux.HandleFunc("/oauth/revoke", provider.serveRevoke)
	provider.server = httptest.NewTLSServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

// issuer is the TLS provider origin.
func (provider *fakeProvider) issuer() string { return provider.server.URL }

// client returns a test HTTP client that trusts the provider certificate.
func (provider *fakeProvider) client() *http.Client { return provider.server.Client() }

// document builds the public discovery document bound to one API URL.
func (provider *fakeProvider) document(apiURL string) api.DiscoveryDocument {
	issuer := provider.issuer()
	return api.DiscoveryDocument{
		Issuer:                      issuer,
		ClientID:                    seedClientID,
		Audience:                    seedAudience,
		AuthorizationEndpoint:       issuer + "/oauth/authorize",
		TokenEndpoint:               issuer + "/oauth/token",
		DeviceAuthorizationEndpoint: issuer + "/oauth/device",
		RevocationEndpoint:          issuer + "/oauth/revoke",
		Scopes:                      []string{"openid", "profile"},
		APIURL:                      apiURL,
	}
}

func (provider *fakeProvider) serveMetadata(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, map[string]string{"jwks_uri": provider.issuer() + "/jwks"})
}

func (provider *fakeProvider) serveJWKS(writer http.ResponseWriter, _ *http.Request) {
	exponent := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(provider.key.E)).Bytes())
	modulus := base64.RawURLEncoding.EncodeToString(provider.key.N.Bytes())
	writeJSON(writer, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "test-key", "alg": "RS256", "use": "sig", "n": modulus, "e": exponent,
	}}})
}

func (provider *fakeProvider) serveAuthorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	provider.mu.Lock()
	provider.auths = append(provider.auths, query)
	provider.nonces["login-code-canary"] = query.Get("nonce")
	provider.mu.Unlock()
	redirect := query.Get("redirect_uri")
	if redirect == "" {
		http.Error(writer, "missing redirect_uri", http.StatusBadRequest)
		return
	}
	target, err := url.Parse(redirect)
	if err != nil {
		http.Error(writer, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	values := target.Query()
	values.Set("code", "login-code-canary")
	values.Set("state", query.Get("state"))
	target.RawQuery = values.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

func (provider *fakeProvider) serveToken(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	form := request.Form
	provider.mu.Lock()
	provider.tokens = append(provider.tokens, form)
	nonce := provider.nonces[form.Get("code")]
	provider.mu.Unlock()
	switch form.Get("grant_type") {
	case "authorization_code":
		writeJSON(writer, map[string]any{
			"access_token": loginAccess, "refresh_token": loginRefresh, "id_token": provider.idToken(nonce),
			"token_type": "Bearer", "expires_in": 3600,
		})
	case "refresh_token":
		writeJSON(writer, map[string]any{
			"access_token": freshAccess, "refresh_token": freshRefresh,
			"token_type": "Bearer", "expires_in": 3600,
		})
	case "urn:ietf:params:oauth:grant-type:device_code":
		writeJSON(writer, map[string]any{
			"access_token": loginAccess, "refresh_token": loginRefresh,
			"token_type": "Bearer", "expires_in": 3600,
		})
	default:
		writer.WriteHeader(http.StatusBadRequest)
		writeJSON(writer, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (provider *fakeProvider) serveDevice(writer http.ResponseWriter, _ *http.Request) {
	provider.mu.Lock()
	provider.devices++
	provider.mu.Unlock()
	writeJSON(writer, map[string]any{
		"device_code": deviceCode, "user_code": deviceUserCode,
		"verification_uri": provider.issuer() + "/device",
		"expires_in":       600, "interval": 1,
	})
}

func (provider *fakeProvider) serveRevoke(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "invalid form", http.StatusBadRequest)
		return
	}
	provider.mu.Lock()
	provider.revokes = append(provider.revokes, request.Form)
	provider.mu.Unlock()
	writeJSON(writer, map[string]any{})
}

// idToken signs one RS256 compact JWS ID token for the recorded nonce.
func (provider *fakeProvider) idToken(nonce string) string {
	header := []byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`)
	now := time.Now()
	claims, err := json.Marshal(map[string]any{
		"iss":   provider.issuer(),
		"aud":   seedClientID,
		"nonce": nonce,
		"exp":   now.Add(10 * time.Minute).Unix(),
		"nbf":   now.Add(-time.Minute).Unix(),
	})
	if err != nil {
		panic("fake provider: marshal claims: " + err.Error())
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, provider.key, crypto.SHA256, digest[:])
	if err != nil {
		panic("fake provider: sign ID token: " + err.Error())
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (provider *fakeProvider) authorizations() []url.Values {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]url.Values(nil), provider.auths...)
}

func (provider *fakeProvider) tokenCalls() []url.Values {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]url.Values(nil), provider.tokens...)
}

func (provider *fakeProvider) revokeCalls() []url.Values {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]url.Values(nil), provider.revokes...)
}

func (provider *fakeProvider) deviceCalls() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.devices
}

// writeJSON writes one JSON object response.
func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// newPublicServer serves the public discovery document and one successful
// operation envelope. The document pointer is filled in after the server URL is
// known; no request is served before it is assigned.
func newPublicServer(t *testing.T, operation string, document *api.DiscoveryDocument, recorder *resultRecorder) *httptest.Server {
	t.Helper()
	payload, err := json.Marshal(api.Result{Operation: operation, Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == api.DiscoveryPath {
			if recorder != nil {
				recorder.recordDiscovery()
			}
			writeJSON(writer, *document)
			return
		}
		if recorder != nil {
			recorder.record(request)
		}
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(api.HTTPStatusSuccess)
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

// seedRecord writes one version 2 session file with the restrictive modes.
func seedRecord(t *testing.T, path string, record cli.SessionRecord) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile session: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod session: %v", err)
	}
}

// recordFor builds one version 2 session bound to the discovery document.
func recordFor(document api.DiscoveryDocument, expiresAt time.Time) cli.SessionRecord {
	return cli.SessionRecord{
		Version:            cli.SessionFileVersion,
		Kind:               "session",
		Token:              loginAccess,
		ExpiresAt:          expiresAt,
		RefreshToken:       loginRefresh,
		Issuer:             document.Issuer,
		ClientID:           document.ClientID,
		Audience:           document.Audience,
		APIURL:             document.APIURL,
		TokenEndpoint:      document.TokenEndpoint,
		RevocationEndpoint: document.RevocationEndpoint,
	}
}

// sessionPaths returns one isolated 0700 session directory and its file.
func sessionPaths(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	return directory, filepath.Join(directory, "session.json")
}

// runEntry runs the real entry with injected ports and returns the streams.
func runEntry(t *testing.T, args []string, deps cliDeps) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, deps)
	return code, stdout.String(), stderr.String()
}

// assertNoSecrets fails when any rendered stream contains a credential.
func assertNoSecrets(t *testing.T, streams []string, secrets ...string) {
	t.Helper()
	for _, stream := range streams {
		for _, secret := range secrets {
			if strings.Contains(stream, secret) {
				t.Fatalf("output rendered a credential %q: %q", secret, stream)
			}
		}
	}
}

// TestKodactlBrowserLoginRunsTheRealRuntime drives the production login runtime
// through the real browser authorization-code + PKCE flow against a TLS fake
// provider. The fake opener fetches the authorization URL and follows the
// provider redirect to the real loopback listener.
func TestKodactlBrowserLoginRunsTheRealRuntime(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)

	clock := fixedClock{now: time.Now()}
	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.browser = &fakeBrowser{client: provider.client()}
	deps.clock = clock
	deps.environment = mapEnvironment(map[string]string{"DISPLAY": ":0"})

	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"login",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged in") {
		t.Fatalf("stdout = %q; want a login acknowledgement", stdout)
	}

	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	record, err := cli.ParseSessionRecord(data)
	if err != nil {
		t.Fatalf("ParseSessionRecord: %v", err)
	}
	if record.Version != cli.SessionFileVersion || record.Token != loginAccess || record.RefreshToken != loginRefresh {
		t.Fatalf("persisted record = %+v", record)
	}
	if record.Issuer != document.Issuer || record.ClientID != document.ClientID ||
		record.Audience != document.Audience || record.APIURL != document.APIURL ||
		record.TokenEndpoint != document.TokenEndpoint || record.RevocationEndpoint != document.RevocationEndpoint {
		t.Fatalf("persisted binding = %+v", record)
	}
	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("Stat session: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %o; want 600", info.Mode().Perm())
	}

	authorizations := provider.authorizations()
	if len(authorizations) != 1 {
		t.Fatalf("authorization requests = %d; want 1", len(authorizations))
	}
	query := authorizations[0]
	if query.Get("response_type") != "code" || query.Get("client_id") != seedClientID ||
		query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
		query.Get("state") == "" || query.Get("nonce") == "" {
		t.Fatalf("authorization query = %v", query)
	}
	if !strings.HasPrefix(query.Get("redirect_uri"), "http://127.0.0.1:") ||
		!strings.HasSuffix(query.Get("redirect_uri"), cli.OAuthCallbackPath) {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	tokens := provider.tokenCalls()
	if len(tokens) != 1 || tokens[0].Get("grant_type") != "authorization_code" ||
		tokens[0].Get("code") != "login-code-canary" || tokens[0].Get("code_verifier") == "" {
		t.Fatalf("token calls = %v", tokens)
	}
	if recorder.count() != 0 {
		t.Fatalf("login sent %d public operations", recorder.count())
	}
	assertNoSecrets(t, []string{stdout, stderr},
		loginAccess, loginRefresh, deviceCode, "login-code-canary")
}

// TestKodactlDeviceLoginRunsTheRealRuntime drives the RFC 8628 device flow
// through the real entry and deterministic clock.
func TestKodactlDeviceLoginRunsTheRealRuntime(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.browser = &fakeBrowser{client: provider.client()}
	deps.clock = fixedClock{now: time.Now()}
	deps.environment = mapEnvironment(nil)

	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"login", "--device",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, deviceUserCode) || !strings.Contains(stdout, "logged in") {
		t.Fatalf("stdout = %q; want the device instructions and a login acknowledgement", stdout)
	}
	if provider.deviceCalls() != 1 {
		t.Fatalf("device requests = %d; want 1", provider.deviceCalls())
	}
	tokens := provider.tokenCalls()
	if len(tokens) != 1 || tokens[0].Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" ||
		tokens[0].Get("device_code") != deviceCode {
		t.Fatalf("token calls = %v", tokens)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	record, err := cli.ParseSessionRecord(data)
	if err != nil {
		t.Fatalf("ParseSessionRecord: %v", err)
	}
	if record.Token != loginAccess || record.RefreshToken != loginRefresh {
		t.Fatalf("persisted record = %+v", record)
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh, deviceCode)
}

// TestKodactlLogoutRevokesThenDeletes drives the real logout through the real
// lock, provider revocation endpoint and session file deletion.
func TestKodactlLogoutRevokesThenDeletes(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	recorder := &resultRecorder{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: time.Now()}
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"logout",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged out") {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, err := os.Stat(sessionPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session file survived logout: %v", err)
	}
	revocations := provider.revokeCalls()
	if len(revocations) != 1 || revocations[0].Get("token") != loginRefresh ||
		revocations[0].Get("client_id") != seedClientID {
		t.Fatalf("revocation calls = %v", revocations)
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh)
}

// TestKodactlStatusRendersNoToken drives status through the real runtime.
func TestKodactlStatusRendersNoToken(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, &resultRecorder{})
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"-output", "json",
		"status",
	}, deps)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("status JSON: %v (%q)", err, stdout)
	}
	if view["issuer"] != document.Issuer || view["refreshable"] != true {
		t.Fatalf("status view = %v", view)
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh)
}

// TestKodactlClientRenewsExpiringSession pins that a client command renews a
// session inside the refresh leeway under the real cross-process lock with
// exactly one token call.
func TestKodactlClientRenewsExpiringSession(t *testing.T) {
	isolateUserConfig(t)
	directory, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	base := time.Now().Truncate(time.Second)
	seedRecord(t, sessionPath, recordFor(document, base.Add(30*time.Second)))

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: base}
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if recorder.count() != 1 {
		t.Fatalf("public operations = %d; want 1", recorder.count())
	}
	if got := recorder.authorization(0); got != "Bearer "+freshAccess {
		t.Fatalf("authorization = %q; want the rotated access token", got)
	}
	if recorder.discoveryCount() != 1 {
		t.Fatalf("discovery requests = %d; want exactly 1", recorder.discoveryCount())
	}
	tokens := provider.tokenCalls()
	if len(tokens) != 1 || tokens[0].Get("grant_type") != "refresh_token" ||
		tokens[0].Get("refresh_token") != loginRefresh {
		t.Fatalf("token calls = %v; want exactly one refresh", tokens)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	record, err := cli.ParseSessionRecord(data)
	if err != nil {
		t.Fatalf("ParseSessionRecord: %v", err)
	}
	if record.Token != freshAccess || record.RefreshToken != freshRefresh {
		t.Fatalf("rotated record = %+v", record)
	}
	lockInfo, err := os.Stat(filepath.Join(directory, ".session.lock"))
	if err != nil {
		t.Fatalf("Stat lock file: %v", err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock file mode = %o; want 600", lockInfo.Mode().Perm())
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh, freshAccess, freshRefresh)
}

// blockingLock holds the lock until its (bounded) context is done.
type blockingLock struct{}

func (blockingLock) Acquire(ctx context.Context) (func() error, error) {
	if ctx == nil {
		return nil, errors.New("nil context")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestKodactlHungLockHolderTimesOut pins that the credential path hands the
// lock a deadline-bearing context, so a live lock holder produces a bounded
// failure rather than an indefinite hang.
func TestKodactlHungLockHolderTimesOut(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	base := time.Now().Truncate(time.Second)
	seedRecord(t, sessionPath, recordFor(document, base.Add(30*time.Second)))
	before, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: base}
	deps.sessionLock = blockingLock{}
	deps.lockTimeout = 50 * time.Millisecond

	started := time.Now()
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, deps)
	elapsed := time.Since(started)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want a bounded failure", code)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the hung lock took %s; want the bounded timeout", elapsed)
	}
	if stderr == "" {
		t.Fatal("the bounded lock failure was silent")
	}
	if recorder.count() != 0 {
		t.Fatalf("public operations = %d; want none behind a held lock", recorder.count())
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the bounded lock failure rewrote the session")
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh)
}

// deadlineLock records the deadline of the context it receives.
type deadlineLock struct {
	mu       sync.Mutex
	deadline time.Time
	has      bool
}

func (lock *deadlineLock) Acquire(ctx context.Context) (func() error, error) {
	deadline, has := ctx.Deadline()
	lock.mu.Lock()
	lock.deadline = deadline
	lock.has = has
	lock.mu.Unlock()
	return func() error { return nil }, nil
}

func (lock *deadlineLock) recorded() (time.Time, bool) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.deadline, lock.has
}

// TestBoundedSessionLockAddsAndPreservesDeadlines pins the bound itself.
func TestBoundedSessionLockAddsAndPreservesDeadlines(t *testing.T) {
	recorder := &deadlineLock{}
	lock := newBoundedSessionLock(recorder, 100*time.Millisecond)
	release, err := lock.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	deadline, has := recorder.recorded()
	if !has {
		t.Fatal("the lock did not receive a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 250*time.Millisecond {
		t.Fatalf("bounded deadline is %s away; want about 100ms", remaining)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	caller, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	callerDeadline, _ := caller.Deadline()
	if _, err := lock.Acquire(caller); err != nil {
		t.Fatalf("Acquire with a caller deadline: %v", err)
	}
	preserved, has := recorder.recorded()
	if !has || !preserved.Equal(callerDeadline) {
		t.Fatalf("caller deadline = %s; lock saw %s/%t", callerDeadline, preserved, has)
	}
}

// failingOIDC reports one discovery failure and refuses every other call.
type failingOIDC struct {
	mu       sync.Mutex
	discover int
	err      error
}

func (client *failingOIDC) Discover(context.Context, string) (api.DiscoveryDocument, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.discover++
	return api.DiscoveryDocument{}, client.err
}

func (client *failingOIDC) Token(context.Context, cli.TokenRequest) (cli.TokenResponse, error) {
	return cli.TokenResponse{}, errors.New("unexpected token call")
}

func (client *failingOIDC) DeviceAuthorize(context.Context, cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
	return cli.DeviceAuthorizationResponse{}, errors.New("unexpected device call")
}

func (client *failingOIDC) Revoke(context.Context, cli.RevocationRequest) error {
	return errors.New("unexpected revoke call")
}

func (client *failingOIDC) JWKS(context.Context, string) (json.RawMessage, error) {
	return nil, errors.New("unexpected JWKS call")
}

func (client *failingOIDC) discoveries() int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.discover
}

// TestKodactlLoginNeverRepersistsASeededSession pins that the real entry always
// runs the OAuth runtime: a seeded session plus a failed discovery is a failure
// and is never reported as "logged in".
func TestKodactlLoginNeverRepersistsASeededSession(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, &resultRecorder{})
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))
	before, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}

	oidc := &failingOIDC{err: errors.New("discovery down")}
	deps := defaultCLIDeps()
	deps.oidc = oidc
	deps.browser = &fakeBrowser{client: provider.client()}
	deps.clock = fixedClock{now: time.Now()}
	deps.environment = mapEnvironment(map[string]string{"DISPLAY": ":0"})

	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"login",
	}, deps)
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want a discovery failure", code)
	}
	if oidc.discoveries() != 1 {
		t.Fatalf("discovery calls = %d; want the configured login runtime", oidc.discoveries())
	}
	if strings.Contains(stdout, "logged in") || strings.Contains(stderr, "logged in") {
		t.Fatal("the legacy seeded-session login path is still reachable")
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session after login: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed login rewrote the seeded session")
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh)
}

// TestKodactlExecutionCredentialFileStillWins pins the selection
// order: a configured mounted execution credential file is used instead of the
// human session for client commands.
func TestKodactlExecutionCredentialFileStillWins(t *testing.T) {
	isolateUserConfig(t)
	directory, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))
	credentialPath := filepath.Join(directory, "execution-token")
	if err := os.WriteFile(credentialPath, []byte("execution-token-canary\n"), 0o600); err != nil {
		t.Fatalf("WriteFile credential: %v", err)
	}

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"-credential-file", credentialPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if recorder.count() != 1 {
		t.Fatalf("public operations = %d; want 1", recorder.count())
	}
	if got := recorder.authorization(0); got != "Bearer execution-token-canary" {
		t.Fatalf("authorization = %q; want the mounted execution credential", got)
	}
	assertNoSecrets(t, []string{stdout, stderr}, "execution-token-canary", loginAccess, loginRefresh)
}

// TestKodactlRefreshCommandRotatesTheSession drives the exposed refresh command
// through the real lock and provider.
func TestKodactlRefreshCommandRotatesTheSession(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	recorder := &resultRecorder{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	base := time.Now().Truncate(time.Second)
	seedRecord(t, sessionPath, recordFor(document, base.Add(30*time.Second)))

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: base}
	code, stdout, stderr := runEntry(t, []string{
		"-server", apiServer.URL,
		"-session", sessionPath,
		"refresh",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "session refreshed") {
		t.Fatalf("stdout = %q; want a refresh acknowledgement", stdout)
	}
	tokens := provider.tokenCalls()
	if len(tokens) != 1 || tokens[0].Get("grant_type") != "refresh_token" {
		t.Fatalf("token calls = %v; want one refresh", tokens)
	}
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	record, err := cli.ParseSessionRecord(data)
	if err != nil {
		t.Fatalf("ParseSessionRecord: %v", err)
	}
	if record.Token != freshAccess || record.RefreshToken != freshRefresh {
		t.Fatalf("rotated record = %+v", record)
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh, freshAccess, freshRefresh)
}

// TestOIDCTransportUsesTLSDefaultsAndNoLoopbackProxy pins the production
// transport construction: certificate verification stays enabled and loopback
// endpoints never use a proxy.
func TestOIDCTransportUsesTLSDefaultsAndNoLoopbackProxy(t *testing.T) {
	transport, ok := newOIDCTransport().(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("newOIDCTransport returned %T", newOIDCTransport())
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("the OIDC transport has no TLS configuration")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("the OIDC transport disables certificate verification")
	}
	if transport.Proxy == nil {
		t.Fatal("the OIDC transport has no proxy policy")
	}
	for _, target := range []string{"https://127.0.0.1:8443/oauth/token", "https://[::1]:8443/oauth/token", "http://localhost:8080/token"} {
		request, err := http.NewRequest(http.MethodPost, target, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", target, err)
		}
		proxy, err := transport.Proxy(request)
		if err != nil || proxy != nil {
			t.Fatalf("loopback target %s used proxy %v/%v", target, proxy, err)
		}
	}
}

// TestBrowserInvocationPassesURLAsOneArgument pins the shell-free launcher argv.
func TestBrowserInvocationPassesURLAsOneArgument(t *testing.T) {
	target := "https://issuer.example/oauth/authorize?response_type=code&state=a%20b"
	cases := map[string][]string{
		"linux":   {"xdg-open", target},
		"darwin":  {"open", target},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", target},
	}
	for goos, want := range cases {
		name, argv := browserInvocation(goos, target)
		if name != want[0] {
			t.Fatalf("%s launcher = %q; want %q", goos, name, want[0])
		}
		if len(argv) != len(want)-1 {
			t.Fatalf("%s argv = %v; want %v", goos, argv, want[1:])
		}
		for index := range argv {
			if argv[index] != want[index+1] {
				t.Fatalf("%s argv = %v; want %v", goos, argv, want[1:])
			}
		}
		if argv[len(argv)-1] != target {
			t.Fatalf("%s passed the URL as more than one argument: %v", goos, argv)
		}
	}
	if err := validateBrowserTarget("javascript:alert(1)"); err == nil {
		t.Fatal("a script URL was accepted")
	}
	if err := validateBrowserTarget("https://issuer.example/oauth/authorize"); err != nil {
		t.Fatalf("a valid authorization URL was refused: %v", err)
	}
	if err := (systemBrowser{}).Open(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("the system browser accepted a non-http URL")
	}
	if browserEnvironment() == nil {
		t.Fatal("the system browser would run with a nil environment")
	}
}

// TestKodactlSessionFlagAndServerAliases pins the --session/--server names
// plus their aliases and environment overrides.
func TestKodactlSessionFlagAndServerAliases(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	t.Cleanup(apiServer.Close)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: time.Now()}
	t.Setenv("KODAMA_SERVER", apiServer.URL)
	t.Setenv("KODAMA_SESSION", sessionPath)
	code, _, stderr := runEntry(t, []string{"call", "task.read", "--input={\"task_id\":\"task-1\"}"}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("environment aliases exit = %d; stderr=%q", code, stderr)
	}
	if recorder.count() != 1 || recorder.authorization(0) != "Bearer "+loginAccess {
		t.Fatalf("environment aliases did not select the session: %d/%q", recorder.count(), recorder.authorization(0))
	}
}
