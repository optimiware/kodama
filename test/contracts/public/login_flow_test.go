// Login flow vectors.
//
// One `kodactl login` command selects either the browser authorization-code +
// PKCE (S256) flow or the RFC 8628 device authorization flow. Both run entirely
// over the injected ports: no real browser, clock, entropy source or OIDC
// network call is used, and a credential/code never reaches an error or output.
package publiccontract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestSelectLoginMode pins the automatic and forced mode selection.
func TestSelectLoginMode(t *testing.T) {
	cases := []struct {
		name        string
		environment cli.LoginEnvironment
		want        cli.LoginMode
	}{
		{name: "display uses browser", environment: cli.LoginEnvironment{Display: ":0"}, want: cli.LoginBrowser},
		{name: "no display uses device", environment: cli.LoginEnvironment{}, want: cli.LoginDevice},
		{name: "ssh uses device", environment: cli.LoginEnvironment{Display: ":0", SSHConnection: "10.0.0.1 22 10.0.0.2 22"}, want: cli.LoginDevice},
		{name: "forced device", environment: cli.LoginEnvironment{Display: ":0", ForceDevice: true}, want: cli.LoginDevice},
		{name: "forced device under ssh", environment: cli.LoginEnvironment{SSHConnection: "10.0.0.1 22 10.0.0.2 22", ForceDevice: true}, want: cli.LoginDevice},
		{name: "forced browser", environment: cli.LoginEnvironment{SSHConnection: "10.0.0.1 22 10.0.0.2 22", ForceBrowser: true}, want: cli.LoginBrowser},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			got, err := cli.SelectLoginMode(testCase.environment)
			if err != nil {
				t.Fatalf("SelectLoginMode: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("mode = %v; want %v", got, testCase.want)
			}
		})
	}
	if _, err := cli.SelectLoginMode(cli.LoginEnvironment{ForceDevice: true, ForceBrowser: true}); err == nil {
		t.Error("SelectLoginMode accepted both forced modes")
	}
}

// loginFixture bundles the deterministic ports used by one flow vector.
type loginFixture struct {
	now       time.Time
	discovery api.DiscoveryDocument
	oidc      *loginFakeOIDC
	browser   *loginFakeBrowser
	loopback  *loginFakeLoopback
	random    *loginFakeRandom
	clock     *loginFakeClock
	lock      *loginFakeLock
	store     *loginFakeStore
	out       bytes.Buffer
	errOut    bytes.Buffer
}

func newLoginFixture(now time.Time) *loginFixture {
	discovery := loginDiscovery()
	return &loginFixture{
		now:       now,
		discovery: discovery,
		oidc:      &loginFakeOIDC{discovery: discovery},
		browser:   &loginFakeBrowser{},
		loopback:  &loginFakeLoopback{address: "127.0.0.1:43871"},
		random:    &loginFakeRandom{},
		clock:     newLoginFakeClock(now),
		lock:      &loginFakeLock{},
		store:     &loginFakeStore{},
	}
}

func (fixture *loginFixture) deps(mode cli.LoginMode) cli.LoginDeps {
	return cli.LoginDeps{
		APIURL:   fixture.discovery.APIURL,
		Mode:     mode,
		OIDC:     fixture.oidc,
		Browser:  fixture.browser,
		Loopback: fixture.loopback,
		Random:   fixture.random,
		Clock:    fixture.clock,
		Lock:     fixture.lock,
		Store:    fixture.store,
		Out:      &fixture.out,
		Err:      &fixture.errOut,
		Timeout:  2 * time.Second,
	}
}

// loginValidCallback returns a callback that mirrors the state/nonce the flow
// placed in the authorization URL and hands back the given code.
func loginValidCallback(t *testing.T, fixture *loginFixture, code string) func(context.Context) (cli.CallbackParams, error) {
	return func(ctx context.Context) (cli.CallbackParams, error) {
		if err := ctx.Err(); err != nil {
			return cli.CallbackParams{}, err
		}
		authorization := fixture.browser.lastURL(t)
		return cli.CallbackParams{Code: code, State: authorization.Query().Get("state")}, nil
	}
}

// TestBrowserLoginSuccess pins the browser flow: authorization URL shape,
// loopback redirect, PKCE S256, state and nonce generation, code exchange,
// ID-token verification and the persisted v2 session.
func TestBrowserLoginSuccess(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	key := loginTestKey(t)
	fixture.oidc.jwks = loginJWKS(t, key)
	fixture.loopback.callback = loginValidCallback(t, fixture, "CODE-CANARY")
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		authorization := fixture.browser.lastURL(t)
		nonce := authorization.Query().Get("nonce")
		idToken := loginSignIDToken(t, key, map[string]any{
			"iss":   fixture.discovery.Issuer,
			"aud":   fixture.discovery.ClientID,
			"sub":   "user-1",
			"exp":   now.Add(time.Hour).Unix(),
			"nonce": nonce,
		})
		return cli.TokenResponse{
			AccessToken:  "AT-CANARY",
			RefreshToken: "RT-CANARY",
			IDToken:      idToken,
			TokenType:    "Bearer",
			ExpiresIn:    time.Hour,
		}, nil
	}

	record, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser))
	if err != nil {
		t.Fatalf("browser Login: %v", err)
	}

	// The discovery document is fetched from the API origin's fixed path.
	discovered := fixture.oidc.discovered()
	if len(discovered) != 1 || discovered[0] != fixture.discovery.APIURL+api.DiscoveryPath {
		t.Fatalf("discovery URL = %v; want %s", discovered, fixture.discovery.APIURL+api.DiscoveryPath)
	}

	// The authorization URL is the issuer's endpoint with the declared members.
	authorization := fixture.browser.lastURL(t)
	if authorization.Scheme != "https" || authorization.Host != "auth.example.com" || authorization.Path != "/authorize" {
		t.Fatalf("authorization URL = %s", authorization)
	}
	query := authorization.Query()
	if query.Get("response_type") != "code" {
		t.Errorf("response_type = %q; want code", query.Get("response_type"))
	}
	if query.Get("client_id") != fixture.discovery.ClientID {
		t.Errorf("client_id = %q; want %q", query.Get("client_id"), fixture.discovery.ClientID)
	}
	redirect := query.Get("redirect_uri")
	if redirect != "http://127.0.0.1:43871"+cli.OAuthCallbackPath {
		t.Errorf("redirect_uri = %q; want the loopback callback", redirect)
	}
	if strings.Contains(redirect, "localhost") || strings.Contains(redirect, "0.0.0.0") {
		t.Errorf("redirect_uri is not bound to the numeric loopback host: %q", redirect)
	}
	if query.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q; want S256", query.Get("code_challenge_method"))
	}
	if query.Get("code_challenge") == "" || query.Get("state") == "" || query.Get("nonce") == "" {
		t.Errorf("authorization URL is missing state, nonce or code_challenge: %s", authorization)
	}
	if scope := query.Get("scope"); !strings.Contains(scope, "openid") || !strings.Contains(scope, "offline_access") {
		t.Errorf("scope = %q; want openid and offline_access", scope)
	}

	// The code exchange carries the matching verifier and redirect URI.
	requests := fixture.oidc.issuedTokens()
	if len(requests) != 1 {
		t.Fatalf("token endpoint called %d times; want 1", len(requests))
	}
	exchange := requests[0]
	if exchange.GrantType != "authorization_code" {
		t.Errorf("grant type = %q; want authorization_code", exchange.GrantType)
	}
	if exchange.Code != "CODE-CANARY" {
		t.Errorf("exchanged code = %q", exchange.Code)
	}
	if exchange.RedirectURI != redirect {
		t.Errorf("exchange redirect_uri = %q; want %q", exchange.RedirectURI, redirect)
	}
	if exchange.ClientID != fixture.discovery.ClientID {
		t.Errorf("exchange client_id = %q", exchange.ClientID)
	}
	if exchange.Endpoint != fixture.discovery.TokenEndpoint {
		t.Errorf("exchange endpoint = %q; want the discovered %q", exchange.Endpoint, fixture.discovery.TokenEndpoint)
	}
	if got := cli.PKCEChallenge(exchange.CodeVerifier); got != query.Get("code_challenge") {
		t.Errorf("PKCE verifier challenge = %q; authorization challenge = %q", got, query.Get("code_challenge"))
	}
	jwksIssuers := fixture.oidc.jwksFetched()
	if len(jwksIssuers) != 1 || jwksIssuers[0] != fixture.discovery.Issuer {
		t.Errorf("JWKS fetches = %v; want the issuer %s", jwksIssuers, fixture.discovery.Issuer)
	}

	// The persisted session is v2 and bound to the discovery document.
	if record.Version != cli.SessionFileVersion || record.Kind != "session" {
		t.Errorf("record header = %+v; want a v2 session", record)
	}
	if record.Token != "AT-CANARY" || record.RefreshToken != "RT-CANARY" {
		t.Errorf("record tokens = %q/%q; want the exchanged values", record.Token, record.RefreshToken)
	}
	if !record.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("record expiry = %s; want %s", record.ExpiresAt, now.Add(time.Hour))
	}
	if record.Issuer != fixture.discovery.Issuer || record.ClientID != fixture.discovery.ClientID ||
		record.Audience != fixture.discovery.Audience || record.APIURL != fixture.discovery.APIURL ||
		record.TokenEndpoint != fixture.discovery.TokenEndpoint ||
		record.RevocationEndpoint != fixture.discovery.RevocationEndpoint {
		t.Errorf("record binding = %+v; want the discovery document", record)
	}
	saved := fixture.store.saved()
	if len(saved) != 1 {
		t.Fatalf("session saved %d times; want 1", len(saved))
	}
	if saved[0].RefreshToken != "RT-CANARY" {
		t.Error("persisted session lost the refresh token")
	}
	if _, _, closes := fixture.loopback.counts(); closes != 1 {
		t.Errorf("loopback listener closed %d times; want exactly 1", closes)
	}

	assertNoLoginCanaries(t, []string{"AT-CANARY", "RT-CANARY", "CODE-CANARY"}, fixture.out.String(), fixture.errOut.String())
}

// TestBrowserLoginPersistFailureRefused pins that a token exchange whose
// session cannot be persisted does not report a successful login.
func TestBrowserLoginPersistFailureRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	key := loginTestKey(t)
	fixture.oidc.jwks = loginJWKS(t, key)
	fixture.loopback.callback = loginValidCallback(t, fixture, "CODE-CANARY")
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		authorization := fixture.browser.lastURL(t)
		idToken := loginSignIDToken(t, key, map[string]any{
			"iss":   fixture.discovery.Issuer,
			"aud":   fixture.discovery.ClientID,
			"exp":   now.Add(time.Hour).Unix(),
			"nonce": authorization.Query().Get("nonce"),
		})
		return cli.TokenResponse{AccessToken: "AT-CANARY", RefreshToken: "RT-CANARY", IDToken: idToken, ExpiresIn: time.Hour}, nil
	}
	fixture.store.saveErr = errors.New("session file is not writable")
	if _, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser)); err == nil {
		t.Fatal("Login reported success although the session was not persisted")
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("failed persistence recorded %d saves", saves)
	}
	assertNoLoginCanaries(t, []string{"AT-CANARY", "RT-CANARY"}, fixture.out.String(), fixture.errOut.String())
}

// TestBrowserLoginStateMismatchRefused pins that a callback carrying another
// state value never reaches the token endpoint or the session store.
func TestBrowserLoginStateMismatchRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.loopback.callback = func(ctx context.Context) (cli.CallbackParams, error) {
		return cli.CallbackParams{Code: "CODE-CANARY", State: "attacker-state"}, nil
	}
	if _, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser)); err == nil {
		t.Fatal("Login accepted a mismatched state")
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
		t.Errorf("token endpoint called %d times after a state mismatch", calls)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times after a state mismatch", saves)
	}
	if _, _, closes := fixture.loopback.counts(); closes != 1 {
		t.Errorf("loopback listener closed %d times; want 1", closes)
	}
}

// TestBrowserLoginRedirectErrorRefused pins an authorization error response.
func TestBrowserLoginRedirectErrorRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.loopback.callback = func(ctx context.Context) (cli.CallbackParams, error) {
		return cli.CallbackParams{Error: "access_denied", ErrorDescription: "user denied", State: "any"}, nil
	}
	if _, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser)); err == nil {
		t.Fatal("Login accepted an authorization error callback")
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
		t.Errorf("token endpoint called %d times after an authorization error", calls)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times after an authorization error", saves)
	}
}

// TestBrowserLoginTimeout pins the bounded wait: a callback that never arrives
// is canceled by the flow's timeout and no session is persisted.
func TestBrowserLoginTimeout(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.loopback.callback = func(ctx context.Context) (cli.CallbackParams, error) {
		select {
		case <-ctx.Done():
			return cli.CallbackParams{}, ctx.Err()
		case <-time.After(2 * time.Second):
			return cli.CallbackParams{}, errors.New("callback wait was not bounded")
		}
	}
	deps := fixture.deps(cli.LoginBrowser)
	deps.Timeout = 30 * time.Millisecond
	started := time.Now()
	if _, err := cli.Login(context.Background(), deps); err == nil {
		t.Fatal("Login accepted a callback timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("login timeout took %s; want a bounded wait", elapsed)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times after a timeout", saves)
	}
	if _, _, closes := fixture.loopback.counts(); closes != 1 {
		t.Errorf("loopback listener closed %d times; want 1", closes)
	}
}

// TestBrowserLoginNonceMismatchRefused pins ID-token nonce verification in the
// flow: a valid signature for the wrong nonce is refused.
func TestBrowserLoginNonceMismatchRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	key := loginTestKey(t)
	fixture.oidc.jwks = loginJWKS(t, key)
	fixture.loopback.callback = loginValidCallback(t, fixture, "CODE-CANARY")
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		idToken := loginSignIDToken(t, key, map[string]any{
			"iss":   fixture.discovery.Issuer,
			"aud":   fixture.discovery.ClientID,
			"exp":   now.Add(time.Hour).Unix(),
			"nonce": "attacker-nonce",
		})
		return cli.TokenResponse{AccessToken: "AT-CANARY", RefreshToken: "RT-CANARY", IDToken: idToken, ExpiresIn: time.Hour}, nil
	}
	_, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser))
	if err == nil {
		t.Fatal("Login accepted an ID token for another nonce")
	}
	if !errors.Is(err, cli.ErrReauthenticationRequired) {
		t.Errorf("nonce mismatch error = %v; want reauthentication required", err)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times after a nonce mismatch", saves)
	}
}

// TestBrowserLoginIDTokenAudience pins the ID-token audience rule: the ID
// token `aud` must contain the OAuth client_id (as a string or as one member of
// an array), while the persisted session still records the discovered API
// audience as its binding audience. An ID token that names only the API
// audience, another application, or an array without the client_id is refused
// and persists no session.
func TestBrowserLoginIDTokenAudience(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	discovery := loginDiscovery()
	cases := []struct {
		name     string
		audience any
		accepted bool
	}{
		{name: "client id", audience: discovery.ClientID, accepted: true},
		{name: "client id in array", audience: []string{discovery.ClientID, "another-application"}, accepted: true},
		{name: "api audience only", audience: discovery.Audience, accepted: false},
		{name: "other client", audience: "another-client", accepted: false},
		{name: "array without client id", audience: []string{discovery.Audience, "another-client"}, accepted: false},
		{name: "empty array", audience: []string{}, accepted: false},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newLoginFixture(now)
			key := loginTestKey(t)
			fixture.oidc.jwks = loginJWKS(t, key)
			fixture.loopback.callback = loginValidCallback(t, fixture, "CODE-CANARY")
			fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
				authorization := fixture.browser.lastURL(t)
				idToken := loginSignIDToken(t, key, map[string]any{
					"iss":   fixture.discovery.Issuer,
					"aud":   testCase.audience,
					"sub":   "user-1",
					"exp":   now.Add(time.Hour).Unix(),
					"nonce": authorization.Query().Get("nonce"),
				})
				return cli.TokenResponse{AccessToken: "AT-CANARY", RefreshToken: "RT-CANARY", IDToken: idToken, ExpiresIn: time.Hour}, nil
			}

			record, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser))
			if !testCase.accepted {
				if err == nil {
					t.Fatalf("Login accepted an ID token whose aud = %#v", testCase.audience)
				}
				if !errors.Is(err, cli.ErrReauthenticationRequired) {
					t.Errorf("audience refusal = %v; want reauthentication required", err)
				}
				if saves := len(fixture.store.saved()); saves != 0 {
					t.Errorf("session persisted %d times after an audience refusal", saves)
				}
				if _, present := fixture.store.current(); present {
					t.Error("an audience refusal left a session in the store")
				}
				return
			}
			if err != nil {
				t.Fatalf("Login refused a valid ID token aud = %#v: %v", testCase.audience, err)
			}
			if record.ClientID != discovery.ClientID {
				t.Errorf("record client_id = %q; want %q", record.ClientID, discovery.ClientID)
			}
			// The session binding audience is the API audience: the client_id
			// rule applies only to the audience the ID token must carry.
			if record.Audience != discovery.Audience {
				t.Errorf("record audience = %q; want the API audience %q", record.Audience, discovery.Audience)
			}
			saved := fixture.store.saved()
			if len(saved) != 1 {
				t.Fatalf("session saved %d times; want 1", len(saved))
			}
			if saved[0].Audience != discovery.Audience {
				t.Errorf("persisted audience = %q; want the API audience %q", saved[0].Audience, discovery.Audience)
			}
		})
	}
}

// TestBrowserLoginDiscoveryBindingRefused pins that discovery for another API
// URL is refused before any browser or OAuth interaction.
func TestBrowserLoginDiscoveryBindingRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.oidc.discovery.APIURL = "https://other.example.com"
	if _, err := cli.Login(context.Background(), fixture.deps(cli.LoginBrowser)); err == nil {
		t.Fatal("Login accepted discovery bound to another API URL")
	}
	if opened := fixture.browser.opened(); len(opened) != 0 {
		t.Errorf("Login opened a browser for mismatched discovery: %v", opened)
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
		t.Errorf("token endpoint called %d times for mismatched discovery", calls)
	}
}

// TestDeviceLoginSuccess pins the device flow: verification URL and user code
// are printed, the schedule follows pending and slow_down, and the v2 session
// is persisted without leaking the device code or tokens.
func TestDeviceLoginSuccess(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.oidc.deviceFn = func(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
		return cli.DeviceAuthorizationResponse{
			DeviceCode:      "DEVICE-CANARY",
			UserCode:        "USER-CODE",
			VerificationURI: "https://auth.example.com/device",
			ExpiresIn:       60 * time.Second,
			Interval:        5 * time.Second,
		}, nil
	}
	calls := 0
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		calls++
		switch calls {
		case 1:
			return cli.TokenResponse{}, &cli.OAuthError{Code: "authorization_pending"}
		case 2:
			return cli.TokenResponse{}, &cli.OAuthError{Code: "slow_down"}
		default:
			return cli.TokenResponse{AccessToken: "AT-CANARY", RefreshToken: "RT-CANARY", ExpiresIn: time.Hour}, nil
		}
	}

	record, err := cli.Login(context.Background(), fixture.deps(cli.LoginDevice))
	if err != nil {
		t.Fatalf("device Login: %v", err)
	}
	if len(fixture.oidc.deviceCalls) != 1 {
		t.Fatalf("device authorization called %d times; want 1", len(fixture.oidc.deviceCalls))
	}
	deviceRequest := fixture.oidc.deviceCalls[0]
	if deviceRequest.ClientID != fixture.discovery.ClientID {
		t.Errorf("device client_id = %q", deviceRequest.ClientID)
	}
	if deviceRequest.Endpoint != fixture.discovery.DeviceAuthorizationEndpoint {
		t.Errorf("device endpoint = %q; want the discovered %q", deviceRequest.Endpoint, fixture.discovery.DeviceAuthorizationEndpoint)
	}
	if strings.Join(deviceRequest.Scopes, " ") != strings.Join(fixture.discovery.Scopes, " ") {
		t.Errorf("device scopes = %v; want %v", deviceRequest.Scopes, fixture.discovery.Scopes)
	}

	requests := fixture.oidc.issuedTokens()
	if len(requests) != 3 {
		t.Fatalf("token endpoint called %d times; want 3 (pending, slow_down, success)", len(requests))
	}
	for _, request := range requests {
		if request.GrantType != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("device grant type = %q", request.GrantType)
		}
		if request.DeviceCode != "DEVICE-CANARY" {
			t.Errorf("device code = %q; want the authorization response value", request.DeviceCode)
		}
		if request.ClientID != fixture.discovery.ClientID {
			t.Errorf("token client_id = %q", request.ClientID)
		}
		if request.Endpoint != fixture.discovery.TokenEndpoint {
			t.Errorf("device token endpoint = %q; want the discovered %q", request.Endpoint, fixture.discovery.TokenEndpoint)
		}
	}
	sleeps := fixture.clock.slept()
	wantSleeps := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("poll sleeps = %v; want %v", sleeps, wantSleeps)
	}
	for index := range wantSleeps {
		if sleeps[index] != wantSleeps[index] {
			t.Fatalf("poll sleeps = %v; want %v", sleeps, wantSleeps)
		}
	}

	output := fixture.out.String()
	if !strings.Contains(output, "https://auth.example.com/device") || !strings.Contains(output, "USER-CODE") {
		t.Errorf("device login output = %q; want the verification URL and user code", output)
	}
	if record.Token != "AT-CANARY" || record.RefreshToken != "RT-CANARY" || record.Version != 2 {
		t.Errorf("device session = %+v", record)
	}
	if saves := len(fixture.store.saved()); saves != 1 {
		t.Errorf("device session saved %d times; want 1", saves)
	}
	assertNoLoginCanaries(t, []string{"DEVICE-CANARY", "AT-CANARY", "RT-CANARY"},
		fixture.out.String(), fixture.errOut.String())
}

// TestDeviceLoginIDTokenAudienceUnchanged pins that the ID-token audience rule
// applies only to the browser flow: the device flow never verifies an ID token
// (RFC 8628 has no ID-token validation step), so a token response that carries
// an ID token whose `aud` is only the API audience still succeeds. The
// persisted session keeps the API audience binding.
func TestDeviceLoginIDTokenAudienceUnchanged(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	key := loginTestKey(t)
	fixture.oidc.jwks = loginJWKS(t, key)
	fixture.oidc.deviceFn = func(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
		return cli.DeviceAuthorizationResponse{
			DeviceCode:      "DEVICE-CANARY",
			UserCode:        "USER-CODE",
			VerificationURI: "https://auth.example.com/device",
			ExpiresIn:       time.Minute,
			Interval:        5 * time.Second,
		}, nil
	}
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		// The browser flow would refuse this token: aud is the API audience and
		// does not contain the client_id.
		idToken := loginSignIDToken(t, key, map[string]any{
			"iss": fixture.discovery.Issuer,
			"aud": fixture.discovery.Audience,
			"sub": "user-1",
			"exp": now.Add(time.Hour).Unix(),
		})
		return cli.TokenResponse{AccessToken: "AT-CANARY", RefreshToken: "RT-CANARY", IDToken: idToken, ExpiresIn: time.Hour}, nil
	}

	record, err := cli.Login(context.Background(), fixture.deps(cli.LoginDevice))
	if err != nil {
		t.Fatalf("device Login refused a token whose ID-token audience is unchecked: %v", err)
	}
	if record.ClientID != fixture.discovery.ClientID {
		t.Errorf("record client_id = %q; want %q", record.ClientID, fixture.discovery.ClientID)
	}
	if record.Audience != fixture.discovery.Audience {
		t.Errorf("record audience = %q; want the API audience %q", record.Audience, fixture.discovery.Audience)
	}
	saved := fixture.store.saved()
	if len(saved) != 1 {
		t.Fatalf("device session saved %d times; want 1", len(saved))
	}
	if saved[0].Audience != fixture.discovery.Audience {
		t.Errorf("persisted audience = %q; want the API audience %q", saved[0].Audience, fixture.discovery.Audience)
	}
}

// TestDeviceLoginTerminalErrors pins access_denied and expired_token.
func TestDeviceLoginTerminalErrors(t *testing.T) {
	for _, code := range []string{"access_denied", "expired_token"} {
		code := code
		t.Run(code, func(t *testing.T) {
			now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
			fixture := newLoginFixture(now)
			fixture.oidc.deviceFn = func(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
				return cli.DeviceAuthorizationResponse{
					DeviceCode:      "DEVICE-CANARY",
					UserCode:        "USER-CODE",
					VerificationURI: "https://auth.example.com/device",
					ExpiresIn:       60 * time.Second,
					Interval:        5 * time.Second,
				}, nil
			}
			fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
				return cli.TokenResponse{}, &cli.OAuthError{Code: code, Description: "terminal"}
			}
			_, err := cli.Login(context.Background(), fixture.deps(cli.LoginDevice))
			if err == nil {
				t.Fatalf("device Login accepted %s", code)
			}
			if !errors.Is(err, cli.ErrReauthenticationRequired) {
				t.Errorf("%s error = %v; want reauthentication required", code, err)
			}
			if saves := len(fixture.store.saved()); saves != 0 {
				t.Errorf("session persisted %d times after %s", saves, code)
			}
			if calls := len(fixture.oidc.issuedTokens()); calls != 1 {
				t.Errorf("token endpoint called %d times after %s; want 1", calls, code)
			}
		})
	}
}

// TestDeviceLoginBoundedTime pins that a permanently pending device flow stops
// at the authorization response's lifetime instead of polling forever.
func TestDeviceLoginBoundedTime(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.oidc.deviceFn = func(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
		return cli.DeviceAuthorizationResponse{
			DeviceCode:      "DEVICE-CANARY",
			UserCode:        "USER-CODE",
			VerificationURI: "https://auth.example.com/device",
			ExpiresIn:       12 * time.Second,
			Interval:        5 * time.Second,
		}, nil
	}
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, &cli.OAuthError{Code: "authorization_pending"}
	}
	_, err := cli.Login(context.Background(), fixture.deps(cli.LoginDevice))
	if err == nil {
		t.Fatal("Login polled a permanently pending device flow without a bound")
	}
	if calls := len(fixture.oidc.issuedTokens()); calls > 6 {
		t.Errorf("token endpoint called %d times; want a bounded poll", calls)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times after an expired device flow", saves)
	}
	if elapsed := fixture.clock.Now().Sub(now); elapsed < 12*time.Second {
		t.Errorf("device flow stopped after %s; want it to reach the 12s lifetime", elapsed)
	}
}

// TestDeviceLoginCanceledContext pins that a canceled context terminates the
// device flow without persisting a session.
func TestDeviceLoginCanceledContext(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.oidc.deviceFn = func(ctx context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
		return cli.DeviceAuthorizationResponse{
			DeviceCode:      "DEVICE-CANARY",
			UserCode:        "USER-CODE",
			VerificationURI: "https://auth.example.com/device",
			ExpiresIn:       time.Minute,
			Interval:        5 * time.Second,
		}, nil
	}
	fixture.oidc.tokenFn = func(ctx context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, &cli.OAuthError{Code: "authorization_pending"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cli.Login(ctx, fixture.deps(cli.LoginDevice)); err == nil {
		t.Fatal("Login ignored a canceled context")
	}
	if calls := len(fixture.oidc.issuedTokens()); calls != 0 {
		t.Errorf("token endpoint called %d times with a canceled context", calls)
	}
	if saves := len(fixture.store.saved()); saves != 0 {
		t.Errorf("session persisted %d times with a canceled context", saves)
	}
}

func assertNoLoginCanaries(t *testing.T, canaries []string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, canary := range canaries {
			if strings.Contains(text, canary) {
				t.Errorf("login output/error leaked secret %q: %q", canary, text)
			}
		}
	}
}
