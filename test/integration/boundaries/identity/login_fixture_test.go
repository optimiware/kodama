//go:build integration

package identity_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// fixtureAuthorizationURL builds one valid browser authorization request.
func fixtureAuthorizationURL(provider *idfix.Provider, verifier, state, nonce string) string {
	values := url.Values{
		"response_type":         {"code"},
		"client_id":             {"kodactl"},
		"redirect_uri":          {"http://127.0.0.1:49152/callback"},
		"code_challenge":        {cli.PKCEChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"nonce":                 {nonce},
		"scope":                 {"openid profile"},
	}
	return provider.AuthorizationEndpoint() + "?" + values.Encode()
}

func fixtureGet(t *testing.T, provider *idfix.Provider, target string) *http.Response {
	t.Helper()
	client := *provider.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(target)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	return response
}

func fixturePostForm(t *testing.T, provider *idfix.Provider, endpoint string, form url.Values) *http.Response {
	t.Helper()
	response, err := provider.Client().PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	return response
}

func fixtureCodeFromLocation(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location %q: %v", location, err)
	}
	code := parsed.Query().Get("code")
	if code == "" {
		t.Fatalf("Location %q has no code", location)
	}
	return code
}

// TestFixtureAuthorizationEndpointValidatesTheRequest proves the fixture
// authorization endpoint enforces the exact loopback redirect, the PKCE S256
// challenge, the client and the required protocol members before it issues a
// one-time code.
func TestFixtureAuthorizationEndpointValidatesTheRequest(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterPublicClient("kodactl", map[string]any{"sub": "auth0|fixture"}, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}
	verifier := strings.Repeat("v", 64)

	response := fixtureGet(t, provider, fixtureAuthorizationURL(provider, verifier, "state-1", "nonce-1"))
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorization status = %d; want 302", response.StatusCode)
	}
	location := response.Header.Get("Location")
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if parsed.Query().Get("state") != "state-1" {
		t.Fatalf("state echoed = %q; want state-1", parsed.Query().Get("state"))
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/callback" {
		t.Fatalf("redirect = %q; want the exact loopback callback", location)
	}
	code := fixtureCodeFromLocation(t, location)

	// The code is bound to the challenge: a wrong verifier is refused.
	bad := fixturePostForm(t, provider, provider.TokenEndpoint(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:49152/callback"},
		"code_verifier": {strings.Repeat("w", 64)},
	})
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-verifier status = %d; want 400", bad.StatusCode)
	}

	// A fresh code with the right verifier returns a nonce-bound ID token. The
	// login_hint is accepted and copied into the minted claims.
	second := fixtureGet(t, provider, fixtureAuthorizationURL(provider, verifier, "state-2", "nonce-2")+"&login_hint=alice")
	defer second.Body.Close()
	code = fixtureCodeFromLocation(t, second.Header.Get("Location"))
	good := fixturePostForm(t, provider, provider.TokenEndpoint(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:49152/callback"},
		"code_verifier": {verifier},
	})
	defer good.Body.Close()
	if good.StatusCode != http.StatusOK {
		t.Fatalf("valid exchange status = %d; want 200", good.StatusCode)
	}
	var tokens idfix.TokenResponse
	decodeJSON(t, readBody(t, good), &tokens)
	if tokens.AccessToken == "" || tokens.RefreshToken == "" || tokens.IDToken == "" {
		t.Fatalf("token response = %+v; want access, refresh and ID tokens", tokens)
	}
	claims := idTokenClaimsJSON(t, tokens.IDToken)
	if claims["nonce"] != "nonce-2" {
		t.Fatalf("ID token nonce = %v; want nonce-2", claims["nonce"])
	}
	if claims["iss"] != provider.Issuer() || claims["aud"] != "kodactl" {
		t.Fatalf("ID token claims = %v; want iss and client_id aud", claims)
	}
	if claims["login_hint"] != "alice" {
		t.Fatalf("ID token login_hint = %v; want alice", claims["login_hint"])
	}

	// The array option mints aud = [client_id, extra].
	provider.SetIDTokenAudience(idfix.IDTokenAudienceArray, "other-aud")
	arrayAuth := fixtureGet(t, provider, fixtureAuthorizationURL(provider, verifier, "state-4", "nonce-4"))
	defer arrayAuth.Body.Close()
	arrayCode := fixtureCodeFromLocation(t, arrayAuth.Header.Get("Location"))
	arrayExchange := fixturePostForm(t, provider, provider.TokenEndpoint(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {arrayCode},
		"redirect_uri":  {"http://127.0.0.1:49152/callback"},
		"code_verifier": {verifier},
	})
	defer arrayExchange.Body.Close()
	var arrayTokens idfix.TokenResponse
	decodeJSON(t, readBody(t, arrayExchange), &arrayTokens)
	arrayClaims := idTokenClaimsJSON(t, arrayTokens.IDToken)
	audience, ok := arrayClaims["aud"].([]any)
	if !ok || len(audience) != 2 || audience[0] != "kodactl" || audience[1] != "other-aud" {
		t.Fatalf("array ID token aud = %v; want [kodactl other-aud]", arrayClaims["aud"])
	}
	provider.SetIDTokenAudience(idfix.IDTokenAudienceClient, "")

	// Replaying the consumed code is refused.
	replay := fixturePostForm(t, provider, provider.TokenEndpoint(), url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"kodactl"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:49152/callback"},
		"code_verifier": {verifier},
	})
	_ = replay.Body.Close()
	if replay.StatusCode != http.StatusBadRequest {
		t.Fatalf("code replay status = %d; want 400", replay.StatusCode)
	}

	refused := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"unknown client", func(values url.Values) { values.Set("client_id", "other") }},
		{"localhost redirect", func(values url.Values) { values.Set("redirect_uri", "http://localhost:49152/callback") }},
		{"wrong path", func(values url.Values) { values.Set("redirect_uri", "http://127.0.0.1:49152/evil") }},
		{"wrong response type", func(values url.Values) { values.Set("response_type", "token") }},
		{"missing challenge", func(values url.Values) { values.Del("code_challenge") }},
		{"plain challenge", func(values url.Values) { values.Set("code_challenge_method", "plain") }},
		{"missing state", func(values url.Values) { values.Del("state") }},
		{"missing nonce", func(values url.Values) { values.Del("nonce") }},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			values := url.Values{
				"response_type":         {"code"},
				"client_id":             {"kodactl"},
				"redirect_uri":          {"http://127.0.0.1:49152/callback"},
				"code_challenge":        {cli.PKCEChallenge(verifier)},
				"code_challenge_method": {"S256"},
				"state":                 {"state-3"},
				"nonce":                 {"nonce-3"},
			}
			testCase.mutate(values)
			response := fixtureGet(t, provider, provider.AuthorizationEndpoint()+"?"+values.Encode())
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", response.StatusCode)
			}
		})
	}
}

// TestFixtureAuthorizationManualApproval proves the manual-approve hook can
// refuse and approve a browser authorization.
func TestFixtureAuthorizationManualApproval(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterPublicClient("kodactl", map[string]any{"sub": "auth0|fixture"}, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}
	provider.SetAutoApprove(false)
	provider.SetAuthorizationHook(func(idfix.AuthorizationRequest) error { return errors.New("denied") })
	target := fixtureAuthorizationURL(provider, strings.Repeat("v", 64), "state-1", "nonce-1")
	response := fixtureGet(t, provider, target)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("denied authorization status = %d; want 403", response.StatusCode)
	}

	provider.SetAuthorizationHook(func(idfix.AuthorizationRequest) error { return nil })
	approved := fixtureGet(t, provider, target)
	_ = approved.Body.Close()
	if approved.StatusCode != http.StatusFound {
		t.Fatalf("approved authorization status = %d; want 302", approved.StatusCode)
	}
}

// TestFixtureDevicePollingIntervalAndApproval proves the fixture returns
// authorization_pending before approval, slow_down when polled faster than the
// interval, and a token after approval at the next interval.
func TestFixtureDevicePollingIntervalAndApproval(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterPublicClient("kodactl", map[string]any{"sub": "auth0|fixture"}, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}
	provider.SetDeviceInterval(time.Hour)
	provider.SetDeviceExpiresIn(24 * time.Hour)

	response := fixturePostForm(t, provider, provider.DeviceAuthorizationEndpoint(), url.Values{"client_id": {"kodactl"}})
	defer response.Body.Close()
	var authorization struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		Interval   int    `json:"interval"`
	}
	decodeJSON(t, readBody(t, response), &authorization)
	if authorization.DeviceCode == "" || authorization.UserCode == "" || authorization.Interval != 3600 {
		t.Fatalf("device authorization = %+v", authorization)
	}

	poll := func() int {
		response := fixturePostForm(t, provider, provider.TokenEndpoint(), url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {authorization.DeviceCode},
			"client_id":   {"kodactl"},
		})
		_ = response.Body.Close()
		return response.StatusCode
	}
	if status := poll(); status != http.StatusBadRequest {
		t.Fatalf("first poll status = %d; want 400 authorization_pending", status)
	}
	if status := poll(); status != http.StatusBadRequest {
		t.Fatalf("fast second poll status = %d; want 400 slow_down", status)
	}
	if !provider.ApproveDevice(authorization.UserCode) {
		t.Fatal("the fixture did not approve the pending device code")
	}
	provider.Clock().Advance(2 * time.Hour)
	if status := poll(); status != http.StatusOK {
		t.Fatalf("approved poll status = %d; want 200", status)
	}
}

// TestFixtureRevocationRevokesTheRefreshFamily proves RFC 7009 revocation
// invalidates the recorded refresh token and records the exact request.
func TestFixtureRevocationRevokesTheRefreshFamily(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterPublicClient("kodactl", nil, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}
	refresh, err := provider.IssueRefreshToken("kodactl", nil)
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	response := fixturePostForm(t, provider, provider.RevocationEndpoint(), url.Values{
		"token":           {refresh},
		"client_id":       {"kodactl"},
		"token_type_hint": {"refresh_token"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("revocation status = %d; want 200", response.StatusCode)
	}
	records := provider.Revocations()
	if len(records) != 1 || records[0].Token != refresh || records[0].TokenTypeHint != "refresh_token" {
		t.Fatalf("revocation records = %+v", records)
	}
	if !provider.RefreshFamilyRevoked(refresh) {
		t.Fatal("the fixture did not revoke the refresh-token family")
	}
	if _, err := provider.ExchangeRefreshToken(t.Context(), refresh, "kodactl", ""); err == nil {
		t.Fatal("a revoked refresh token was accepted")
	}
}

// TestFixtureRefreshTokenLifetimeFollowsTheClock proves the refresh-token
// lifetime is controlled by the fixture clock.
func TestFixtureRefreshTokenLifetimeFollowsTheClock(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterPublicClient("kodactl", nil, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}
	provider.SetRefreshTokenTTL(time.Minute)
	token, err := provider.IssueRefreshToken("kodactl", nil)
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	provider.Clock().Advance(2 * time.Minute)
	if _, err := provider.ExchangeRefreshToken(t.Context(), token, "kodactl", ""); err == nil {
		t.Fatal("an expired refresh token was accepted")
	}
}

// TestFixtureDiscoveryFailureInjection proves the discovery fixture serves the
// document and injects 5xx, delay and a dropped connection.
func TestFixtureDiscoveryFailureInjection(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	server, err := idfix.NewDiscoveryServer(idfix.DiscoveryOptions{
		Document: idfix.DiscoveryDocumentFor(provider, "https://api.example", "kodactl"),
	})
	if err != nil {
		t.Fatalf("NewDiscoveryServer: %v", err)
	}
	t.Cleanup(server.Close)

	response, err := server.Client().Get(server.URL() + "/.well-known/kodama-configuration")
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	var document map[string]any
	decodeJSON(t, readBody(t, response), &document)
	if document["issuer"] != provider.Issuer() {
		t.Fatalf("discovery issuer = %v; want %q", document["issuer"], provider.Issuer())
	}

	server.SetStatus(http.StatusBadGateway)
	response, err = server.Client().Get(server.URL() + "/.well-known/kodama-configuration")
	if err != nil {
		t.Fatalf("GET failing discovery: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("injected discovery status = %d; want 502", response.StatusCode)
	}

	server.SetStatus(0)
	server.SetDelay(50 * time.Millisecond)
	started := time.Now()
	response, err = server.Client().Get(server.URL() + "/.well-known/kodama-configuration")
	if err != nil {
		t.Fatalf("GET delayed discovery: %v", err)
	}
	_ = response.Body.Close()
	if elapsed := time.Since(started); elapsed < 40*time.Millisecond {
		t.Fatalf("delayed discovery returned in %s", elapsed)
	}

	server.SetDelay(0)
	server.SetDrop(true)
	if _, err := server.Client().Get(server.URL() + "/.well-known/kodama-configuration"); err == nil {
		t.Fatal("a dropped discovery connection returned a response")
	}
}

// idTokenClaimsJSON decodes one ID token payload without verifying it. The
// fixture tests only assert the minted nonce and issuer.
func idTokenClaimsJSON(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("ID token has %d parts; want 3", len(parts))
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode ID token claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("unmarshal ID token claims: %v", err)
	}
	return claims
}
