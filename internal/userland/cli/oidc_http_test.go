// Tests for the real OAuth/OIDC HTTP adapter. They drive an in-test fake
// OIDC server over httptest TLS (and one plain numeric-loopback server) and
// assert the transport rules: exact endpoint use, no redirects, strict
// bounded decoding, safe error mapping, issuer-derived JWKS and the
// https-except-numeric-loopback rule.
package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// oidcHit is one request the fake OIDC server observed. It never records a
// response body or an authorization header value.
type oidcHit struct {
	Method        string
	Path          string
	Form          url.Values
	ContentType   string
	Authorization string
}

// oidcTestServer is a small in-test OIDC server with per-path handlers.
type oidcTestServer struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   []oidcHit
	routes map[string]http.HandlerFunc
}

func newOIDCTestServer(t *testing.T, tls bool) *oidcTestServer {
	t.Helper()
	server := &oidcTestServer{routes: make(map[string]http.HandlerFunc)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		_ = request.ParseForm()
		hit := oidcHit{
			Method:        request.Method,
			Path:          request.URL.Path,
			Form:          request.PostForm,
			ContentType:   request.Header.Get("Content-Type"),
			Authorization: request.Header.Get("Authorization"),
		}
		server.mu.Lock()
		server.hits = append(server.hits, hit)
		handler := server.routes[request.URL.Path]
		server.mu.Unlock()
		if handler == nil {
			http.NotFound(writer, request)
			return
		}
		handler(writer, request)
	})
	if tls {
		server.server = httptest.NewTLSServer(mux)
	} else {
		server.server = httptest.NewServer(mux)
	}
	t.Cleanup(server.server.Close)
	return server
}

func (server *oidcTestServer) handle(path string, handler http.HandlerFunc) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.routes[path] = handler
}

func (server *oidcTestServer) hitsFor(path string) []oidcHit {
	server.mu.Lock()
	defer server.mu.Unlock()
	var found []oidcHit
	for _, hit := range server.hits {
		if hit.Path == path {
			found = append(found, hit)
		}
	}
	return found
}

func (server *oidcTestServer) hitCount(path string) int {
	return len(server.hitsFor(path))
}

func (server *oidcTestServer) url(path string) string {
	return server.server.URL + path
}

func (server *oidcTestServer) client() *cli.HTTPOIDCClient {
	return &cli.HTTPOIDCClient{RoundTripper: server.server.Client().Transport}
}

// jsonHandler answers one fixed JSON body.
func jsonHandler(body string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, body)
	}
}

// statusJSONHandler answers one fixed status and JSON body.
func statusJSONHandler(status int, body string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	}
}

// oidcDocumentJSON builds one valid Kodama discovery document.
func oidcDocumentJSON(apiURL, issuer string) string {
	return fmt.Sprintf(
		`{"issuer":%q,"client_id":"kodactl","audience":"kodama-api",`+
			`"authorization_endpoint":%q,"token_endpoint":%q,`+
			`"device_authorization_endpoint":%q,"revocation_endpoint":%q,`+
			`"scopes":["openid","profile"],"api_url":%q}`,
		issuer,
		issuer+"/oauth/authorize",
		issuer+"/oauth/token",
		issuer+"/oauth/device",
		issuer+"/oauth/revoke",
		apiURL,
	)
}

const oidcTokenBody = `{"access_token":"access-1","refresh_token":"refresh-1","id_token":"id-1","token_type":"Bearer","expires_in":3600,"scope":"openid"}`

// countingRoundTripper records whether an unexpected request was attempted.
type countingRoundTripper struct {
	calls atomic.Int32
}

func (roundTripper *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	roundTripper.calls.Add(1)
	return nil, errors.New("unexpected request")
}

// stubRoundTripper answers every request with one fixed body.
type stubRoundTripper struct {
	body     string
	mu       sync.Mutex
	requests int
}

func (roundTripper *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	roundTripper.mu.Lock()
	roundTripper.requests++
	roundTripper.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(roundTripper.body)),
	}, nil
}

func (roundTripper *stubRoundTripper) count() int {
	roundTripper.mu.Lock()
	defer roundTripper.mu.Unlock()
	return roundTripper.requests
}

func TestHTTPOIDCDiscoverAcceptsBoundDocument(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle(api.DiscoveryPath, jsonHandler(oidcDocumentJSON(server.url(""), server.url(""))))
	client := server.client()
	document, err := client.Discover(context.Background(), server.url(api.DiscoveryPath))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if document.Issuer != server.url("") || document.ClientID != "kodactl" || document.Audience != "kodama-api" {
		t.Fatalf("document = %+v", document)
	}
	if document.APIURL != server.url("") {
		t.Fatalf("api_url = %q; want %q", document.APIURL, server.url(""))
	}
	hits := server.hitsFor(api.DiscoveryPath)
	if len(hits) != 1 || hits[0].Method != http.MethodGet {
		t.Fatalf("discovery hits = %+v; want one GET", hits)
	}
	if hits[0].Authorization != "" {
		t.Fatal("discovery request carried an authorization header")
	}
}

func TestHTTPOIDCDiscoverRefusals(t *testing.T) {
	cases := []struct {
		name string
		body func(apiURL, issuer string) string
	}{
		{
			name: "unknown member",
			body: func(apiURL, issuer string) string {
				return strings.Replace(oidcDocumentJSON(apiURL, issuer), `"api_url"`, `"extra":1,"api_url"`, 1)
			},
		},
		{
			name: "trailing data",
			body: func(apiURL, issuer string) string { return oidcDocumentJSON(apiURL, issuer) + `{}` },
		},
		{
			name: "non object",
			body: func(string, string) string { return `[]` },
		},
		{
			name: "empty body",
			body: func(string, string) string { return `` },
		},
		{
			name: "oversize",
			body: func(string, string) string { return strings.Repeat("x", api.MaxDiscoveryBytes+1) },
		},
		{
			name: "foreign api origin",
			body: func(_, issuer string) string { return oidcDocumentJSON("https://elsewhere.example", issuer) },
		},
		{
			name: "foreign token endpoint origin",
			body: func(apiURL, issuer string) string {
				return strings.Replace(
					oidcDocumentJSON(apiURL, issuer),
					issuer+"/oauth/token",
					"https://elsewhere.example/oauth/token",
					1,
				)
			},
		},
		{
			name: "plain http api url",
			body: func(_, issuer string) string { return oidcDocumentJSON("http://elsewhere.example", issuer) },
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newOIDCTestServer(t, true)
			server.handle(api.DiscoveryPath, jsonHandler(testCase.body(server.url(""), server.url(""))))
			document, err := server.client().Discover(context.Background(), server.url(api.DiscoveryPath))
			if err == nil {
				t.Fatalf("Discover accepted a refused document: %+v", document)
			}
		})
	}
}

func TestHTTPOIDCTokenUsesExactEndpointOnly(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/token", statusJSONHandler(http.StatusOK, oidcTokenBody))
	server.handle("/oauth/decoy", statusJSONHandler(http.StatusOK, oidcTokenBody))
	response, err := server.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     server.url("/oauth/token"),
		GrantType:    "authorization_code",
		Code:         "code-1",
		RedirectURI:  "http://127.0.0.1:43210/callback",
		ClientID:     "kodactl",
		CodeVerifier: "verifier-1",
	})
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if response.AccessToken != "access-1" || response.RefreshToken != "refresh-1" ||
		response.IDToken != "id-1" || response.TokenType != "Bearer" || response.ExpiresIn != time.Hour {
		t.Fatalf("response = %+v", response)
	}
	if got := server.hitCount("/oauth/decoy"); got != 0 {
		t.Fatalf("decoy endpoint hits = %d; want 0", got)
	}
	hits := server.hitsFor("/oauth/token")
	if len(hits) != 1 {
		t.Fatalf("token endpoint hits = %d; want 1", len(hits))
	}
	hit := hits[0]
	if hit.Method != http.MethodPost {
		t.Fatalf("token method = %q; want POST", hit.Method)
	}
	if !strings.HasPrefix(hit.ContentType, "application/x-www-form-urlencoded") {
		t.Fatalf("content type = %q", hit.ContentType)
	}
	if hit.Authorization != "" {
		t.Fatal("token request carried an authorization header")
	}
	form := hit.Form
	if _, present := form["client_secret"]; present {
		t.Fatal("token request carried a client secret")
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "code-1",
		"redirect_uri":  "http://127.0.0.1:43210/callback",
		"client_id":     "kodactl",
		"code_verifier": "verifier-1",
	}
	for name, value := range want {
		if got := form.Get(name); got != value {
			t.Fatalf("form %s = %q; want %q", name, got, value)
		}
	}
}

func TestHTTPOIDCTokenGrantForms(t *testing.T) {
	cases := []struct {
		name     string
		request  cli.TokenRequest
		wantForm map[string]string
	}{
		{
			name: "refresh token",
			request: cli.TokenRequest{
				GrantType:    "refresh_token",
				RefreshToken: "refresh-1",
				ClientID:     "kodactl",
			},
			wantForm: map[string]string{"grant_type": "refresh_token", "refresh_token": "refresh-1", "client_id": "kodactl"},
		},
		{
			name: "device code",
			request: cli.TokenRequest{
				GrantType:  "urn:ietf:params:oauth:grant-type:device_code",
				DeviceCode: "device-1",
				ClientID:   "kodactl",
			},
			wantForm: map[string]string{
				"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
				"device_code": "device-1",
				"client_id":   "kodactl",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newOIDCTestServer(t, true)
			server.handle("/oauth/token", statusJSONHandler(http.StatusOK, oidcTokenBody))
			request := testCase.request
			request.Endpoint = server.url("/oauth/token")
			if _, err := server.client().Token(context.Background(), request); err != nil {
				t.Fatalf("Token: %v", err)
			}
			hits := server.hitsFor("/oauth/token")
			if len(hits) != 1 {
				t.Fatalf("token hits = %d; want 1", len(hits))
			}
			for name, value := range testCase.wantForm {
				if got := hits[0].Form.Get(name); got != value {
					t.Fatalf("form %s = %q; want %q", name, got, value)
				}
			}
			if _, present := hits[0].Form["client_secret"]; present {
				t.Fatal("grant request carried a client secret")
			}
		})
	}
}

func TestHTTPOIDCTokenRefusesUnknownGrant(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/token", statusJSONHandler(http.StatusOK, oidcTokenBody))
	_, err := server.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:  server.url("/oauth/token"),
		GrantType: "client_credentials",
		ClientID:  "kodactl",
	})
	if err == nil {
		t.Fatal("Token accepted an unknown grant type")
	}
	if got := server.hitCount("/oauth/token"); got != 0 {
		t.Fatalf("token hits = %d; want 0", got)
	}
}

func TestHTTPOIDCTokenEndpointTransportRules(t *testing.T) {
	refused := []string{
		"",
		"http://example.com/oauth/token",
		"http://localhost:8080/oauth/token",
		"https://user:secret@example.com/oauth/token",
		"https://example.com/oauth/token?code=1",
		"https://example.com/oauth/token#fragment",
		"/oauth/token",
	}
	for _, endpoint := range refused {
		t.Run(endpoint, func(t *testing.T) {
			roundTripper := &countingRoundTripper{}
			client := &cli.HTTPOIDCClient{RoundTripper: roundTripper}
			_, err := client.Token(context.Background(), cli.TokenRequest{
				Endpoint:     endpoint,
				GrantType:    "refresh_token",
				RefreshToken: "refresh-1",
				ClientID:     "kodactl",
			})
			if err == nil {
				t.Fatalf("Token accepted endpoint %q", endpoint)
			}
			if got := roundTripper.calls.Load(); got != 0 {
				t.Fatalf("Token sent %d requests to a refused endpoint", got)
			}
		})
	}
}

func TestHTTPOIDCTokenAllowsPlainNumericLoopback(t *testing.T) {
	server := newOIDCTestServer(t, false)
	server.handle("/oauth/token", statusJSONHandler(http.StatusOK, oidcTokenBody))
	client := &cli.HTTPOIDCClient{RoundTripper: server.server.Client().Transport}
	if _, err := client.Token(context.Background(), cli.TokenRequest{
		Endpoint:     server.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: "refresh-1",
		ClientID:     "kodactl",
	}); err != nil {
		t.Fatalf("plain loopback endpoint refused: %v", err)
	}

	ipv6 := &stubRoundTripper{body: oidcTokenBody}
	if _, err := (&cli.HTTPOIDCClient{RoundTripper: ipv6}).Token(context.Background(), cli.TokenRequest{
		Endpoint:     "http://[::1]:43210/oauth/token",
		GrantType:    "refresh_token",
		RefreshToken: "refresh-1",
		ClientID:     "kodactl",
	}); err != nil {
		t.Fatalf("plain IPv6 loopback endpoint refused: %v", err)
	}
	if ipv6.count() != 1 {
		t.Fatalf("IPv6 requests = %d; want 1", ipv6.count())
	}
}

func TestHTTPOIDCTokenRefusesRedirects(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/token", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/oauth/decoy", http.StatusFound)
	})
	server.handle("/oauth/decoy", statusJSONHandler(http.StatusOK, oidcTokenBody))
	_, err := server.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     server.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: "refresh-1",
		ClientID:     "kodactl",
	})
	if err == nil {
		t.Fatal("Token followed a redirect")
	}
	if got := server.hitCount("/oauth/decoy"); got != 0 {
		t.Fatalf("redirect target hits = %d; want 0", got)
	}
}

func TestHTTPOIDCTokenResponseStrictness(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		maxSize int
	}{
		{name: "malformed", body: `{"access_token":`},
		{name: "trailing data", body: oidcTokenBody + `{}`},
		{name: "non object", body: `[]`},
		{name: "unknown member", body: `{"access_token":"a","expires_in":3600,"mystery":true}`},
		{name: "wrong expires type", body: `{"access_token":"a","expires_in":"3600"}`},
		{name: "missing access token", body: `{"expires_in":3600}`},
		{name: "negative expiry", body: `{"access_token":"a","expires_in":-1}`},
		{name: "oversized", body: strings.Repeat("x", 256), maxSize: 64},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newOIDCTestServer(t, true)
			server.handle("/oauth/token", statusJSONHandler(http.StatusOK, testCase.body))
			client := server.client()
			if testCase.maxSize > 0 {
				client.MaxBodyBytes = testCase.maxSize
			}
			if _, err := client.Token(context.Background(), cli.TokenRequest{
				Endpoint:     server.url("/oauth/token"),
				GrantType:    "refresh_token",
				RefreshToken: "refresh-1",
				ClientID:     "kodactl",
			}); err == nil {
				t.Fatalf("Token accepted a refused response: %s", testCase.body)
			}
		})
	}
}

func TestHTTPOIDCTokenErrorMappingHidesSecrets(t *testing.T) {
	const refreshSecret = "refresh-secret-canary-123"
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/token", statusJSONHandler(http.StatusBadRequest,
		fmt.Sprintf(`{"error":"authorization_pending","error_description":"wait for %s"}`, refreshSecret)))
	_, err := server.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     server.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: refreshSecret,
		ClientID:     "kodactl",
	})
	if err == nil {
		t.Fatal("Token accepted an error response")
	}
	var oauthErr *cli.OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("error %T is not an OAuthError: %v", err, err)
	}
	if oauthErr.Code != "authorization_pending" {
		t.Fatalf("oauth code = %q", oauthErr.Code)
	}
	for _, rendered := range []string{oauthErr.Description, oauthErr.Error(), err.Error()} {
		if strings.Contains(rendered, refreshSecret) {
			t.Fatalf("OAuth error rendered the refresh token: %q", rendered)
		}
	}

	long := newOIDCTestServer(t, true)
	long.handle("/oauth/token", statusJSONHandler(http.StatusBadRequest,
		fmt.Sprintf(`{"error":"invalid_grant","error_description":%q}`, strings.Repeat("z", 2000))))
	_, err = long.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     long.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: refreshSecret,
		ClientID:     "kodactl",
	})
	if !errors.As(err, &oauthErr) {
		t.Fatalf("long description error = %v", err)
	}
	if len(oauthErr.Description) > 512 {
		t.Fatalf("description length = %d; want a bounded 512", len(oauthErr.Description))
	}

	unsafe := newOIDCTestServer(t, true)
	unsafe.handle("/oauth/token", statusJSONHandler(http.StatusBadRequest,
		`{"error":"bad code<script>","error_description":"x"}`))
	_, err = unsafe.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     unsafe.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: refreshSecret,
		ClientID:     "kodactl",
	})
	if !errors.As(err, &oauthErr) || oauthErr.Code != "oauth_error" {
		t.Fatalf("unsafe code was not sanitized: %v", err)
	}

	echoed := newOIDCTestServer(t, true)
	echoed.handle("/oauth/token", statusJSONHandler(http.StatusBadRequest,
		fmt.Sprintf(`{"error":%q,"error_description":"x"}`, refreshSecret)))
	_, err = echoed.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     echoed.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: refreshSecret,
		ClientID:     "kodactl",
	})
	if !errors.As(err, &oauthErr) || oauthErr.Code != "oauth_error" {
		t.Fatalf("an echoed secret was not sanitized: %v", err)
	}
	if strings.Contains(err.Error(), refreshSecret) {
		t.Fatalf("an echoed secret reached the error: %v", err)
	}

	malformed := newOIDCTestServer(t, true)
	malformed.handle("/oauth/token", statusJSONHandler(http.StatusBadRequest, "not json "+refreshSecret))
	_, err = malformed.client().Token(context.Background(), cli.TokenRequest{
		Endpoint:     malformed.url("/oauth/token"),
		GrantType:    "refresh_token",
		RefreshToken: refreshSecret,
		ClientID:     "kodactl",
	})
	if err == nil {
		t.Fatal("malformed error body was accepted")
	}
	if errors.As(err, &oauthErr) {
		t.Fatalf("malformed error body mapped to OAuthError: %+v", oauthErr)
	}
	if strings.Contains(err.Error(), refreshSecret) {
		t.Fatalf("malformed error body rendered a secret: %v", err)
	}
}

func TestHTTPOIDCDeviceAuthorization(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/device", statusJSONHandler(http.StatusOK,
		`{"device_code":"device-1","user_code":"ABCD-EFGH","verification_uri":"https://issuer.example/device",`+
			`"verification_uri_complete":"https://issuer.example/device?code=ABCD-EFGH","expires_in":600,"interval":5}`))
	response, err := server.client().DeviceAuthorize(context.Background(), cli.DeviceAuthorizationRequest{
		Endpoint: server.url("/oauth/device"),
		ClientID: "kodactl",
		Scopes:   []string{"openid", "profile"},
	})
	if err != nil {
		t.Fatalf("DeviceAuthorize: %v", err)
	}
	if response.DeviceCode != "device-1" || response.UserCode != "ABCD-EFGH" ||
		response.VerificationURI != "https://issuer.example/device" {
		t.Fatalf("response = %+v", response)
	}
	if response.ExpiresIn != 10*time.Minute || response.Interval != 5*time.Second {
		t.Fatalf("durations = %v/%v", response.ExpiresIn, response.Interval)
	}
	hits := server.hitsFor("/oauth/device")
	if len(hits) != 1 {
		t.Fatalf("device hits = %d; want 1", len(hits))
	}
	if hits[0].Form.Get("client_id") != "kodactl" || hits[0].Form.Get("scope") != "openid profile" {
		t.Fatalf("device form = %v", hits[0].Form)
	}
	if _, present := hits[0].Form["client_secret"]; present {
		t.Fatal("device request carried a client secret")
	}

	strict := newOIDCTestServer(t, true)
	strict.handle("/oauth/device", statusJSONHandler(http.StatusOK,
		`{"device_code":"d","user_code":"u","verification_uri":"https://issuer.example/device","expires_in":600,"mystery":1}`))
	if _, err := strict.client().DeviceAuthorize(context.Background(), cli.DeviceAuthorizationRequest{
		Endpoint: strict.url("/oauth/device"), ClientID: "kodactl", Scopes: []string{"openid"},
	}); err == nil {
		t.Fatal("DeviceAuthorize accepted an unknown member")
	}

	incomplete := newOIDCTestServer(t, true)
	incomplete.handle("/oauth/device", statusJSONHandler(http.StatusOK,
		`{"device_code":"d","verification_uri":"https://issuer.example/device","expires_in":600}`))
	if _, err := incomplete.client().DeviceAuthorize(context.Background(), cli.DeviceAuthorizationRequest{
		Endpoint: incomplete.url("/oauth/device"), ClientID: "kodactl", Scopes: []string{"openid"},
	}); err == nil {
		t.Fatal("DeviceAuthorize accepted a missing user code")
	}
}

func TestHTTPOIDCRevoke(t *testing.T) {
	server := newOIDCTestServer(t, true)
	server.handle("/oauth/revoke", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	if err := server.client().Revoke(context.Background(), cli.RevocationRequest{
		Endpoint:      server.url("/oauth/revoke"),
		Token:         "refresh-1",
		ClientID:      "kodactl",
		TokenTypeHint: "refresh_token",
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	hits := server.hitsFor("/oauth/revoke")
	if len(hits) != 1 {
		t.Fatalf("revoke hits = %d; want 1", len(hits))
	}
	if hits[0].Form.Get("token") != "refresh-1" ||
		hits[0].Form.Get("token_type_hint") != "refresh_token" ||
		hits[0].Form.Get("client_id") != "kodactl" {
		t.Fatalf("revoke form = %v", hits[0].Form)
	}
	if _, present := hits[0].Form["client_secret"]; present {
		t.Fatal("revocation request carried a client secret")
	}

	failing := newOIDCTestServer(t, true)
	failing.handle("/oauth/revoke", statusJSONHandler(http.StatusBadRequest,
		`{"error":"invalid_client","error_description":"no such client"}`))
	err := failing.client().Revoke(context.Background(), cli.RevocationRequest{
		Endpoint: failing.url("/oauth/revoke"), Token: "refresh-1", ClientID: "kodactl",
	})
	var oauthErr *cli.OAuthError
	if !errors.As(err, &oauthErr) || oauthErr.Code != "invalid_client" {
		t.Fatalf("revoke error = %v", err)
	}
}

func TestHTTPOIDCJWKSFromIssuerOrigin(t *testing.T) {
	server := newOIDCTestServer(t, true)
	const keys = `{"keys":[{"kty":"RSA","kid":"k1","alg":"RS256"}]}`
	server.handle("/.well-known/openid-configuration", jsonHandler(fmt.Sprintf(
		`{"issuer":%q,"jwks_uri":%q,"token_endpoint":%q,"scopes_supported":["openid"]}`,
		server.url(""), server.url("/oauth/jwks"), server.url("/oauth/token"))))
	server.handle("/oauth/jwks", jsonHandler(keys))
	raw, err := server.client().JWKS(context.Background(), server.url(""))
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("JWKS returned invalid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(keys), &want); err != nil {
		t.Fatalf("test keys invalid: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("JWKS = %s; want %s", raw, keys)
	}
	if server.hitCount("/.well-known/openid-configuration") != 1 || server.hitCount("/oauth/jwks") != 1 {
		t.Fatalf("metadata/jwks hits = %d/%d",
			server.hitCount("/.well-known/openid-configuration"), server.hitCount("/oauth/jwks"))
	}
}

func TestHTTPOIDCJWKSRefusesForeignOrigin(t *testing.T) {
	issuerServer := newOIDCTestServer(t, true)
	foreignServer := newOIDCTestServer(t, true)
	issuerServer.handle("/.well-known/openid-configuration", jsonHandler(fmt.Sprintf(
		`{"issuer":%q,"jwks_uri":%q}`, issuerServer.url(""), foreignServer.url("/jwks"))))
	foreignServer.handle("/jwks", jsonHandler(`{"keys":[]}`))
	if _, err := issuerServer.client().JWKS(context.Background(), issuerServer.url("")); err == nil {
		t.Fatal("JWKS accepted a foreign-origin jwks_uri")
	}
	if got := foreignServer.hitCount("/jwks"); got != 0 {
		t.Fatalf("foreign jwks hits = %d; want 0", got)
	}
}

func TestHTTPOIDCJWKSRefusals(t *testing.T) {
	malformed := newOIDCTestServer(t, true)
	malformed.handle("/.well-known/openid-configuration", jsonHandler(fmt.Sprintf(
		`{"jwks_uri":%q}`, malformed.url("/oauth/jwks"))))
	malformed.handle("/oauth/jwks", jsonHandler(`{"keys":[`))
	if _, err := malformed.client().JWKS(context.Background(), malformed.url("")); err == nil {
		t.Fatal("JWKS accepted a malformed key set")
	}

	unknown := newOIDCTestServer(t, true)
	unknown.handle("/.well-known/openid-configuration", jsonHandler(fmt.Sprintf(
		`{"jwks_uri":%q}`, unknown.url("/oauth/jwks"))))
	unknown.handle("/oauth/jwks", jsonHandler(`{"keys":[],"extra":1}`))
	if _, err := unknown.client().JWKS(context.Background(), unknown.url("")); err == nil {
		t.Fatal("JWKS accepted an unknown key-set member")
	}

	missing := newOIDCTestServer(t, true)
	missing.handle("/.well-known/openid-configuration", jsonHandler(`{"issuer":"x"}`))
	if _, err := missing.client().JWKS(context.Background(), missing.url("")); err == nil {
		t.Fatal("JWKS accepted metadata without jwks_uri")
	}

	insecure := &countingRoundTripper{}
	if _, err := (&cli.HTTPOIDCClient{RoundTripper: insecure}).JWKS(context.Background(), "http://example.com"); err == nil {
		t.Fatal("JWKS accepted a plain-http non-loopback issuer")
	}
	if got := insecure.calls.Load(); got != 0 {
		t.Fatalf("insecure issuer sent %d requests", got)
	}
}
